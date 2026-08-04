package store

import (
	"encoding/hex"
	"strings"
	"testing"
)

// TestGenerateAPIKey needs no database: it covers the parts of key minting
// that are pure — that keys are prefixed, unique, and that what gets stored
// is a hash rather than the key itself.
func TestGenerateAPIKey(t *testing.T) {
	key, hash, err := GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	if !strings.HasPrefix(key, APIKeyPrefix) {
		t.Errorf("key %q lacks the %q prefix", key, APIKeyPrefix)
	}
	// 32 random bytes as unpadded base64url is 43 characters. A short key
	// here would mean the entropy assumption behind using a fast hash
	// (HashAPIKey's comment) had quietly stopped holding.
	if body := strings.TrimPrefix(key, APIKeyPrefix); len(body) != 43 {
		t.Errorf("key body is %d chars, want 43 (32 random bytes, base64url)", len(body))
	}
	if strings.Contains(hash, key) || hash == key {
		t.Fatal("stored hash contains the plaintext key")
	}
	if hash != HashAPIKey(key) {
		t.Error("HashAPIKey is not deterministic for the key it was generated with")
	}

	other, _, err := GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	if other == key {
		t.Fatal("two generated keys are identical; the CSPRNG is not being used")
	}
}

// clearAPIKeys empties the table so a test can assert on the "no keys exist
// yet" state regardless of what ran before it. Safe because the tests here
// only ever run against a disposable DATABASE_URL — rls_test.go already
// drops and rebuilds `memories` outright.
func clearAPIKeys(t *testing.T, st *Store) {
	t.Helper()
	if _, err := st.db.pool.Exec(`DELETE FROM api_keys`); err != nil {
		t.Fatalf("clearing api_keys: %v", err)
	}
}

func TestAPIKeyResolvesToItsTenant(t *testing.T) {
	st := setupTenantDB(t)
	clearAPIKeys(t, st)
	makeTenant(t, st, tenantAID, "a@example.test")

	key, err := st.CreateAPIKey(tenantAID, "laptop")
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	got, plan, err := st.TenantForKey(key)
	if err != nil {
		t.Fatalf("TenantForKey: %v", err)
	}
	if got != tenantAID {
		t.Errorf("TenantForKey resolved to %q, want %q", got, tenantAID)
	}
	// The plan comes back from the same lookup, so the caller can apply the
	// tenant's limits without a second query.
	if plan != "free" {
		t.Errorf("TenantForKey returned plan %q, want the tenant's default %q", plan, "free")
	}

	// The plaintext must not be recoverable from the table — that is the
	// whole point of storing only a hash.
	var stored string
	if err := st.db.pool.QueryRow(`SELECT key_hash FROM api_keys WHERE tenant_id = $1`, tenantAID).Scan(&stored); err != nil {
		t.Fatalf("reading stored key: %v", err)
	}
	if stored == key {
		t.Fatal("api_keys stores the plaintext key; a database dump would be a set of live credentials")
	}
	// Not merely "different from the key": a reversible encoding of it would
	// also be different, and would still hand an attacker every live
	// credential. A SHA-256 digest is exactly 64 hex characters, so anything
	// carrying the key's own length has not been through a hash.
	if len(stored) != 64 {
		t.Errorf("stored credential is %d chars, want a 64-char SHA-256 digest — this looks like an encoding, not a hash", len(stored))
	}
	if _, err := hex.DecodeString(stored); err != nil {
		t.Errorf("stored credential %q is not hex; want a SHA-256 digest", stored)
	}
}

// TestUnknownAndRevokedKeysAreRejected covers the two ways a key must fail:
// never issued, and withdrawn. Both have to come back as "no tenant" rather
// than an error, or authenticate() cannot tell them apart from a fault.
func TestUnknownAndRevokedKeysAreRejected(t *testing.T) {
	st := setupTenantDB(t)
	clearAPIKeys(t, st)
	makeTenant(t, st, tenantAID, "a@example.test")

	if got, _, err := st.TenantForKey("mv_this-key-was-never-issued"); err != nil || got != "" {
		t.Errorf("TenantForKey(unknown) = (%q, %v), want (\"\", nil)", got, err)
	}
	if got, _, err := st.TenantForKey(""); err != nil || got != "" {
		t.Errorf("TenantForKey(empty) = (%q, %v), want (\"\", nil)", got, err)
	}

	key, err := st.CreateAPIKey(tenantAID, "leaked")
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	keys, err := st.APIKeys(tenantAID)
	if err != nil || len(keys) != 1 {
		t.Fatalf("APIKeys = (%v, %v), want exactly one key", keys, err)
	}

	revoked, err := st.RevokeAPIKey(keys[0].ID)
	if err != nil || !revoked {
		t.Fatalf("RevokeAPIKey = (%v, %v), want (true, nil)", revoked, err)
	}
	if got, _, err := st.TenantForKey(key); err != nil || got != "" {
		t.Fatalf("revoked key still authenticates as %q (err %v)", got, err)
	}
	// Revoking twice is not a success: an operator re-running it should not
	// be told they just withdrew access they had already withdrawn.
	if revoked, err := st.RevokeAPIKey(keys[0].ID); err != nil || revoked {
		t.Errorf("second RevokeAPIKey = (%v, %v), want (false, nil)", revoked, err)
	}
}

// TestHasMintedAPIKeyGatesAnonymousAccess covers the rule that closes the
// open door: a vault that has never issued a key is single-tenant and may
// serve anonymous callers, and minting the first key must flip that off
// immediately, with no restart.
//
// The revocation case is the one that matters most. Revoking the last
// outstanding key must NOT re-open the vault: an operator revoking a leaked
// key would otherwise turn a closed vault into one serving the bootstrap
// tenant to any unauthenticated caller, at exactly the worst moment.
func TestHasMintedAPIKeyGatesAnonymousAccess(t *testing.T) {
	st := setupTenantDB(t)
	clearAPIKeys(t, st)
	makeTenant(t, st, tenantAID, "a@example.test")

	if has, err := st.HasMintedAPIKey(); err != nil || has {
		t.Fatalf("HasMintedAPIKey on a vault that never issued one = (%v, %v), want (false, nil)", has, err)
	}
	if _, err := st.CreateAPIKey(tenantAID, "first"); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if has, err := st.HasMintedAPIKey(); err != nil || !has {
		t.Fatalf("HasMintedAPIKey after minting = (%v, %v), want (true, nil); anonymous access would stay open", has, err)
	}

	keys, err := st.APIKeys(tenantAID)
	if err != nil {
		t.Fatalf("APIKeys: %v", err)
	}
	if _, err := st.RevokeAPIKey(keys[0].ID); err != nil {
		t.Fatalf("RevokeAPIKey: %v", err)
	}
	if has, err := st.HasMintedAPIKey(); err != nil || !has {
		t.Errorf("HasMintedAPIKey after revoking the last key = (%v, %v), want (true, nil): "+
			"revoking a leaked key must not re-open the vault to anonymous callers", has, err)
	}
}

// TestKeyScopedStoresStayIsolated is step 2's end-to-end acceptance: two
// tenants, each authenticated by their own key exactly as authenticate()
// does it, must not be able to read each other's memories. If this fails,
// the key lookup and the RLS boundary have come apart.
func TestKeyScopedStoresStayIsolated(t *testing.T) {
	st := setupTenantDB(t)
	clearAPIKeys(t, st)
	makeTenant(t, st, tenantAID, "a@example.test")
	makeTenant(t, st, tenantBID, "b@example.test")

	keyA, err := st.CreateAPIKey(tenantAID, "")
	if err != nil {
		t.Fatalf("CreateAPIKey(A): %v", err)
	}
	keyB, err := st.CreateAPIKey(tenantBID, "")
	if err != nil {
		t.Fatalf("CreateAPIKey(B): %v", err)
	}

	// Exactly the production path: key -> tenant id -> scoped store.
	scoped := func(key string) *Store {
		t.Helper()
		id, _, err := st.TenantForKey(key)
		if err != nil {
			t.Fatalf("TenantForKey: %v", err)
		}
		if id == "" {
			t.Fatal("freshly minted key did not authenticate")
		}
		return st.ForTenant(id)
	}
	a, b := scoped(keyA), scoped(keyB)

	if _, err := a.SaveMemory("keyed", "secret", "tenant A private content", DefaultSource, DefaultKind); err != nil {
		t.Fatalf("tenant A SaveMemory: %v", err)
	}
	if _, err := b.SaveMemory("keyed", "secret", "tenant B private content", DefaultSource, DefaultKind); err != nil {
		t.Fatalf("tenant B SaveMemory: %v", err)
	}
	t.Cleanup(func() {
		a.DeleteMemory("keyed", "secret")
		b.DeleteMemory("keyed", "secret")
	})

	got, err := b.Reassemble("keyed", "secret")
	if err != nil {
		t.Fatalf("tenant B Reassemble: %v", err)
	}
	if strings.Contains(got, "tenant A") {
		t.Fatalf("TENANT LEAK: tenant B's key read tenant A's content: %q", got)
	}
	if !strings.Contains(got, "tenant B") {
		t.Errorf("tenant B read %q, want its own content", got)
	}
}
