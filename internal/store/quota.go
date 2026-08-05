package store

import (
	"database/sql"
	"fmt"
)

// PlanLimits caps what one tenant may consume. A zero or negative value means
// "no limit" for that dimension, which is how the bootstrap tenant — and any
// plan an operator chooses not to cap — opts out entirely.
type PlanLimits struct {
	// RequestsPerMinute is the sustained /mcp request rate. Enforced at the
	// HTTP handler, not here; carried on the plan so there is one place that
	// answers "what is this tenant allowed to do".
	RequestsPerMinute int
	// Burst is how many requests a previously idle tenant may make at once.
	Burst int
	// MaxMemories caps distinct memory names across all of a tenant's spaces.
	MaxMemories int
	// MaxContentBytes caps the total stored content in BYTES. The queries
	// use octet_length rather than length because Postgres's length() counts
	// CHARACTERS while Go's len() counts bytes — mixing them undercounts
	// every non-ASCII memory and lets a tenant past its cap.
	MaxContentBytes int64
}

// Unlimited is the zero-limit plan the bootstrap tenant runs under, so an
// existing self-hosted vault behaves exactly as it did before quotas existed.
var Unlimited = PlanLimits{}

// DefaultPlanLimits are the built-in caps per plan. They are deliberately
// hardcoded rather than stored in the tenants table: changing a plan's limits
// should be a reviewed deploy, not an UPDATE anyone with database access can
// make, and it keeps the migration surface unchanged. main.go allows each
// value to be overridden by environment variable for an operator who wants
// different economics.
var DefaultPlanLimits = map[string]PlanLimits{
	"free":    {RequestsPerMinute: 60, Burst: 30, MaxMemories: 200, MaxContentBytes: 10 << 20},     // 10 MB
	"builder": {RequestsPerMinute: 600, Burst: 120, MaxMemories: 5000, MaxContentBytes: 500 << 20}, // 500 MB
	"team":    {RequestsPerMinute: 3000, Burst: 600, MaxMemories: 50000, MaxContentBytes: 5 << 30}, // 5 GB
}

// FallbackPlan is what an unrecognised plan name resolves to — the
// strictest built-in plan, so a typo or a plan added to the database ahead of
// the code cannot silently grant unlimited storage.
const FallbackPlan = "free"

// The fallback plan must exist, or LimitsForPlan's map lookup would return
// the zero PlanLimits — which this package defines as UNLIMITED, the exact
// opposite of the fail-closed behaviour the fallback exists to provide.
// Renaming a tier without updating FallbackPlan would otherwise hand every
// tenant unlimited storage silently, so it fails at startup instead.
func init() {
	if _, ok := DefaultPlanLimits[FallbackPlan]; !ok {
		panic("store: FallbackPlan " + FallbackPlan + " is not present in DefaultPlanLimits, " +
			"so an unrecognised plan would resolve to unlimited instead of the strictest plan")
	}
}

// CanonicalPlan maps a plan name to one the code knows about, collapsing
// anything unrecognised to FallbackPlan.
//
// Callers need this and not just LimitsForPlan: anything deriving a name from
// the plan — an environment variable prefix, a metric label — must use the
// same name the limits actually came from, or it will read configuration that
// does not exist while believing it applied.
func CanonicalPlan(plan string) string {
	if _, ok := DefaultPlanLimits[plan]; ok {
		return plan
	}
	return FallbackPlan
}

// LimitsForPlan returns the caps for a plan name, falling back to the
// strictest built-in plan for anything unrecognised.
func LimitsForPlan(plan string) PlanLimits {
	return DefaultPlanLimits[CanonicalPlan(plan)]
}

// QuotaError is returned by a write that would take a tenant past one of its
// plan's limits. It carries the numbers so the caller can tell the user what
// they hit and by how much, rather than a bare "quota exceeded".
type QuotaError struct {
	Resource string // "memories" or "storage"
	Limit    int64
	Current  int64
	Adding   int64
}

func (e *QuotaError) Error() string {
	return fmt.Sprintf("%s quota exceeded: %d in use, adding %d, limit %d",
		e.Resource, e.Current, e.Adding, e.Limit)
}

// WithLimits returns a copy of s that enforces the given plan limits on
// writes. It shares the pool and tenant binding, so it is cheap enough to
// call per request — which is what the auth middleware does, once it knows
// the tenant's plan.
func (s *Store) WithLimits(limits PlanLimits) *Store {
	scoped := *s
	scoped.limits = limits
	return &scoped
}

// Limits reports the limits this Store enforces.
func (s *Store) Limits() PlanLimits { return s.limits }

// TenantID reports which tenant this Store is bound to. Exposed so the HTTP
// layer can key its rate-limit buckets per tenant; it is an identifier, not a
// handle — nothing outside this package can use it to reach the pool.
func (s *Store) TenantID() string { return s.db.tenant }

// Usage is a tenant's current consumption, for quota checks and for showing
// someone how close they are to their ceiling.
type Usage struct {
	Memories     int
	ContentBytes int64
}

// Usage returns what the bound tenant is currently consuming. RLS scopes the
// query, so this counts exactly one tenant with no WHERE clause of its own.
func (s *Store) Usage() (Usage, error) {
	// Goes through Query rather than a QueryRow helper because tenantDB's
	// rows own the transaction they are read from; a bare *sql.Row would
	// leave that transaction open.
	rows, err := s.db.Query(`
		SELECT count(DISTINCT (space, name)), coalesce(sum(octet_length(content)), 0) FROM memories
	`)
	if err != nil {
		return Usage{}, err
	}
	defer rows.Close()
	var u Usage
	if rows.Next() {
		if err := rows.Scan(&u.Memories, &u.ContentBytes); err != nil {
			return Usage{}, err
		}
	}
	return u, rows.Err()
}

// checkQuota rejects a write that would take the tenant past its plan's
// caps. It runs inside saveMemory's transaction so the counts it reads
// already reflect any rows that write is replacing.
//
// A zero-limit plan short-circuits before touching the database, which is the
// bootstrap tenant's path on every single write — the unlimited case must not
// cost a query.
func (s *Store) checkQuota(tx *sql.Tx, space, name string, chunks []string) error {
	if s.limits.MaxMemories <= 0 && s.limits.MaxContentBytes <= 0 {
		return nil
	}

	var adding int64
	for _, c := range chunks {
		adding += int64(len(c))
	}

	// Usage is measured EXCLUDING the memory being written, then that memory
	// is charged once below. On the overwrite path its rows were already
	// deleted, so the exclusion changes nothing. On the non-overwrite path
	// (Import without overwrite) they are still present, and counting them
	// alongside the incoming copy charged the same memory twice — enough to
	// fail an import of a backup a tenant already holds, which then aborted
	// the whole import instead of skipping the duplicate.
	var count int64
	var bytes int64
	if err := tx.QueryRow(`
		SELECT count(DISTINCT (space, name)), coalesce(sum(octet_length(content)), 0)
		FROM memories WHERE NOT (space = $1 AND name = $2)
	`, space, name).Scan(&count, &bytes); err != nil {
		return fmt.Errorf("reading quota usage: %w", err)
	}

	if s.limits.MaxMemories > 0 && count+1 > int64(s.limits.MaxMemories) {
		return &QuotaError{Resource: "memories", Limit: int64(s.limits.MaxMemories), Current: count, Adding: 1}
	}
	if s.limits.MaxContentBytes > 0 && bytes+adding > s.limits.MaxContentBytes {
		return &QuotaError{Resource: "storage", Limit: s.limits.MaxContentBytes, Current: bytes, Adding: adding}
	}
	return nil
}
