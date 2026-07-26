#!/bin/sh
# memory-vault DNS failover watcher
#
# Runs on the STANDBY host (never the primary — if the primary is down,
# anything running on it is down too) and polls the primary's /healthz.
# After FAILOVER_THRESHOLD consecutive failures it points the Cloudflare
# DNS record clients use at the standby's IP; after RECOVERY_THRESHOLD
# consecutive successes with DNS still pointed at the standby, it points
# it back at the primary. A cooldown after any flip stops it flapping
# back and forth if health is borderline.
#
# This is DNS-based failover, not instant: it's bounded by the DNS
# record's TTL and client-side caching (set a low TTL on the record — see
# README), and the standby's data can lag the primary by
# deploy/standby-sync.sh's sync interval. See the README's Fallback /
# failover section for the full picture.
#
# Deployment shape: by default this script loops forever (sleep
# CHECK_INTERVAL_SECONDS between checks) and is meant to run as a
# long-running systemd service (memory-vault-failover.service,
# Restart=always) rather than a timer — systemd timers don't reliably
# hit sub-minute intervals (AccuracySec defaults to 1 minute, and each
# firing pays unit-start overhead), which matters here since
# CHECK_INTERVAL_SECONDS defaults to 30s. Pass --once to run a single
# check-and-maybe-flip cycle and exit instead, if you'd rather drive this
# from cron/a timer at whatever interval you're comfortable with — state
# persists across invocations either way via FAILOVER_STATE_DIR.
#
# Required env (e.g. via /etc/memory-vault-failover.env, loaded by the
# systemd unit):
#   HEALTH_URL     - the primary's health endpoint, e.g.
#                    https://memory-vault-primary.example.com/healthz
#   STANDBY_IP     - this host's IP, to fail over to
#   PRIMARY_IP     - the primary's IP, to fail back to (kept as a fixed
#                    value rather than read back from DNS, so the script
#                    doesn't need DNS to already be correct to know what
#                    "correct" is)
#   CF_ZONE_ID     - Cloudflare zone ID containing the record
#   CF_RECORD_ID   - Cloudflare DNS record ID to flip
#   CF_API_TOKEN   - a token scoped ONLY to DNS edit on that zone/record —
#                    separate from any broader DNS-01 cert-issuance token
# Optional env:
#   FAILOVER_THRESHOLD    - consecutive health failures before failing over (default: 3)
#   RECOVERY_THRESHOLD    - consecutive health successes before failing back (default: 3)
#   CHECK_INTERVAL_SECONDS - seconds between checks in loop mode (default: 30)
#   FAILOVER_COOLDOWN_MIN - minutes to wait after any flip before flipping again (default: 10)
#   NOTIFY_WEBHOOK_URL    - if set, POST a small JSON payload here on every transition
#   CF_API_BASE_URL       - Cloudflare API base (default: https://api.cloudflare.com/client/v4;
#                           override to point at a mock for testing)
#   FAILOVER_STATE_DIR    - where state persists across runs (default: /var/lib/memory-vault-failover)
#   FAILOVER_LOG          - log file (default: /var/log/memory-vault-failover.log)
#
# All Cloudflare API calls and state transitions fail loudly to
# FAILOVER_LOG and stderr on error — a script that silently fails to fail
# over is worse than no script at all.

set -eu

: "${HEALTH_URL:?HEALTH_URL is required}"
: "${STANDBY_IP:?STANDBY_IP is required}"
: "${PRIMARY_IP:?PRIMARY_IP is required}"
: "${CF_ZONE_ID:?CF_ZONE_ID is required}"
: "${CF_RECORD_ID:?CF_RECORD_ID is required}"
: "${CF_API_TOKEN:?CF_API_TOKEN is required}"

FAILOVER_THRESHOLD="${FAILOVER_THRESHOLD:-3}"
RECOVERY_THRESHOLD="${RECOVERY_THRESHOLD:-3}"
CHECK_INTERVAL_SECONDS="${CHECK_INTERVAL_SECONDS:-30}"
FAILOVER_COOLDOWN_MIN="${FAILOVER_COOLDOWN_MIN:-10}"
CF_API_BASE_URL="${CF_API_BASE_URL:-https://api.cloudflare.com/client/v4}"
FAILOVER_STATE_DIR="${FAILOVER_STATE_DIR:-/var/lib/memory-vault-failover}"
FAILOVER_LOG="${FAILOVER_LOG:-/var/log/memory-vault-failover.log}"

mkdir -p "$FAILOVER_STATE_DIR"
state_file="$FAILOVER_STATE_DIR/state"
# State file format (space-separated, one line): <pointer> <consecutive_failures> <consecutive_successes> <last_flip_epoch>
# pointer is "primary" or "standby": which side DNS currently points at,
# as far as this script's own history knows (it doesn't query Cloudflare
# to confirm on every loop — the last flip it made is authoritative for
# its own state machine; if DNS was changed out-of-band, fix it up by
# hand-editing $state_file's first field).

log() {
	# Never echo CF_API_TOKEN — only ever the fixed fields below.
	printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$1" | tee -a "$FAILOVER_LOG" >&2
}

notify() {
	# event from_ip to_ip
	if [ -z "${NOTIFY_WEBHOOK_URL:-}" ]; then
		return 0
	fi
	payload=$(printf '{"event":"%s","from_ip":"%s","to_ip":"%s","timestamp":"%s"}' \
		"$1" "$2" "$3" "$(date -u +%Y-%m-%dT%H:%M:%SZ)")
	if ! curl -s -m 10 -X POST -H 'Content-Type: application/json' \
		--data "$payload" "$NOTIFY_WEBHOOK_URL" >/dev/null 2>&1; then
		log "WARN notify webhook failed (DNS flip already applied, this is notification-only)"
	fi
}

# cf_update_dns ip - PATCHes the DNS record's content. Logs and returns
# non-zero on any failure (network error, non-2xx, or a "success":false
# response body) without ever printing CF_API_TOKEN.
cf_update_dns() {
	target_ip="$1"
	resp="$(curl -s -m 15 -w '\n%{http_code}' -X PATCH \
		"$CF_API_BASE_URL/zones/$CF_ZONE_ID/dns_records/$CF_RECORD_ID" \
		-H "Authorization: Bearer $CF_API_TOKEN" \
		-H "Content-Type: application/json" \
		--data "{\"content\":\"$target_ip\"}")" || {
		log "FAIL cloudflare API request failed (network error)"
		return 1
	}
	http_code="$(printf '%s' "$resp" | tail -n1)"
	body="$(printf '%s' "$resp" | sed '$d')"
	case "$http_code" in
	2??) ;;
	*)
		log "FAIL cloudflare API returned HTTP $http_code: $body"
		return 1
		;;
	esac
	if printf '%s' "$body" | grep -qE '"success"[[:space:]]*:[[:space:]]*true'; then
		return 0
	fi
	log "FAIL cloudflare API reported failure: $body"
	return 1
}

health_ok() {
	code="$(curl -s -m 10 -o /dev/null -w '%{http_code}' "$HEALTH_URL" 2>/dev/null || echo 000)"
	[ "$code" = "200" ]
}

read_state() {
	if [ -f "$state_file" ]; then
		# shellcheck disable=SC2034 # read into named fields for clarity below
		read -r pointer consecutive_failures consecutive_successes last_flip <"$state_file"
	else
		pointer="primary"
		consecutive_failures=0
		consecutive_successes=0
		last_flip=0
	fi
}

write_state() {
	echo "$pointer $consecutive_failures $consecutive_successes $last_flip" >"$state_file"
}

cooldown_active() {
	now="$(date -u +%s)"
	elapsed_min=$(((now - last_flip) / 60))
	[ "$elapsed_min" -lt "$FAILOVER_COOLDOWN_MIN" ]
}

check_once() {
	read_state

	if health_ok; then
		consecutive_failures=0
		consecutive_successes=$((consecutive_successes + 1))
	else
		consecutive_successes=0
		consecutive_failures=$((consecutive_failures + 1))
	fi

	if [ "$pointer" = "primary" ] && [ "$consecutive_failures" -ge "$FAILOVER_THRESHOLD" ] && ! cooldown_active; then
		log "FAILOVER triggering: $consecutive_failures consecutive health failures against $HEALTH_URL"
		if cf_update_dns "$STANDBY_IP"; then
			pointer="standby"
			last_flip="$(date -u +%s)"
			consecutive_failures=0
			consecutive_successes=0
			log "FAILOVER complete: DNS now points at standby ($STANDBY_IP)"
			notify "failover" "$PRIMARY_IP" "$STANDBY_IP"
		else
			log "FAILOVER FAILED: could not update DNS, will retry next check"
		fi
	elif [ "$pointer" = "standby" ] && [ "$consecutive_successes" -ge "$RECOVERY_THRESHOLD" ] && ! cooldown_active; then
		log "RECOVERY triggering: $consecutive_successes consecutive health successes against $HEALTH_URL"
		if cf_update_dns "$PRIMARY_IP"; then
			pointer="primary"
			last_flip="$(date -u +%s)"
			consecutive_failures=0
			consecutive_successes=0
			log "RECOVERY complete: DNS now points at primary ($PRIMARY_IP)"
			notify "recovery" "$STANDBY_IP" "$PRIMARY_IP"
		else
			log "RECOVERY FAILED: could not update DNS, will retry next check"
		fi
	fi

	write_state
}

if [ "${1:-}" = "--once" ]; then
	check_once
	exit 0
fi

while true; do
	check_once
	sleep "$CHECK_INTERVAL_SECONDS"
done
