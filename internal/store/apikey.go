package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"
)

// APIKeyPrefix marks a memory-vault key on sight, so a leaked one is
// greppable in logs and recognizable to a secret scanner.
const APIKeyPrefix = "mv_"

// GenerateAPIKey returns a new API key and the hash to store for it. The
// plaintext is returned exactly once, here — only the hash is persisted, so
// a database dump cannot be replayed as credentials.
func GenerateAPIKey() (key, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	key = APIKeyPrefix + base64.RawURLEncoding.EncodeToString(b)
	return key, HashAPIKey(key), nil
}

// HashAPIKey is a plain SHA-256, not bcrypt/argon2, on purpose. Those exist
// to slow guessing of low-entropy human passwords; an API key is 256 bits of
// CSPRNG output, so there is nothing to guess. A fast hash is what lets
// authentication be one indexed equality lookup per request instead of a
// scan that re-derives a slow hash against every stored key.
func HashAPIKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// The tenants and api_keys tables are the two that deliberately carry no RLS
// policy, and everything in this file talks to the pool directly rather than
// through tenantDB. That is not an oversight: authentication has to read
// api_keys *before* it knows which tenant the request belongs to, so there is
// no app.tenant_id to filter on yet. Every other table is reachable only via
// tenantDB, and memories stays fully policy-enforced.
//
// A consequence worth stating plainly: the administrative calls below
// (CreateTenant, Tenants, CreateAPIKey, APIKeys, RevokeAPIKey) are NOT scoped
// by the receiver's tenant binding — s.ForTenant(x).RevokeAPIKey(y) will
// happily revoke tenant y's key. That is safe today only because every caller
// is the operator CLI, which is already fully privileged. Anything that ever
// exposes these over HTTP must scope them itself; neither the compiler nor
// RLS will catch it.

// TenantForKey resolves a plaintext API key to the tenant that owns it.
// Returns "" with no error when the key is unknown or revoked — an unknown
// key is a routine failed authentication, not a server fault.
func (s *Store) TenantForKey(key string) (string, error) {
	var tenantID string
	err := s.db.pool.QueryRow(
		`SELECT tenant_id::text FROM api_keys WHERE key_hash = $1 AND revoked_at IS NULL`,
		HashAPIKey(key),
	).Scan(&tenantID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return tenantID, nil
}

// HasMintedAPIKey reports whether an API key has ever been issued, revoked
// ones included. It is what closes anonymous access: a vault that has never
// had a key is a single-tenant one, where unauthenticated access to the
// bootstrap tenant is the pre-multi-tenancy behavior; the first minted key
// means real tenants exist, and serving them to an anonymous caller is a leak.
//
// Deliberately not "any *live* key": counting only unrevoked keys would mean
// revoking the last outstanding key silently re-opens the vault to
// unauthenticated callers — the exact opposite of what an operator revoking a
// leaked key is trying to achieve. Issuing a key is a one-way door out of
// single-tenant mode.
func (s *Store) HasMintedAPIKey() (bool, error) {
	var exists bool
	err := s.db.pool.QueryRow(`SELECT EXISTS (SELECT 1 FROM api_keys)`).Scan(&exists)
	return exists, err
}

// CreateTenant registers a tenant and returns its id. An empty plan defaults
// to 'free' (the column default); an invalid one is rejected by the table's
// CHECK constraint.
func (s *Store) CreateTenant(email, plan string) (string, error) {
	if email == "" {
		return "", fmt.Errorf("email is required")
	}
	if plan == "" {
		plan = "free"
	}
	var id string
	err := s.db.pool.QueryRow(
		`INSERT INTO tenants (email, plan) VALUES ($1, $2) RETURNING id::text`,
		email, plan,
	).Scan(&id)
	return id, err
}

// TenantInfo is one tenant as listed by Tenants.
type TenantInfo struct {
	ID        string
	Email     string
	Plan      string
	CreatedAt time.Time
}

// Tenants lists every registered tenant, oldest first — the operator's way to
// find the id that CreateAPIKey needs.
func (s *Store) Tenants() ([]TenantInfo, error) {
	rows, err := s.db.pool.Query(`SELECT id::text, email, plan, created_at FROM tenants ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TenantInfo
	for rows.Next() {
		var t TenantInfo
		if err := rows.Scan(&t.ID, &t.Email, &t.Plan, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CreateAPIKey mints a key for a tenant and returns the plaintext. This is
// the only moment it exists outside the caller's hands: only its hash is
// stored, so a lost key is reissued, never recovered.
func (s *Store) CreateAPIKey(tenantID, label string) (string, error) {
	key, hash, err := GenerateAPIKey()
	if err != nil {
		return "", err
	}
	if _, err := s.db.pool.Exec(
		`INSERT INTO api_keys (tenant_id, key_hash, label) VALUES ($1, $2, $3)`,
		tenantID, hash, label,
	); err != nil {
		return "", err
	}
	return key, nil
}

// APIKeyInfo is one key's metadata — never its plaintext, which is
// unrecoverable by design.
type APIKeyInfo struct {
	ID        string
	Label     string
	CreatedAt time.Time
	RevokedAt sql.NullTime
}

// APIKeys lists a tenant's keys, including revoked ones, so an operator can
// see what was issued and when it was withdrawn.
func (s *Store) APIKeys(tenantID string) ([]APIKeyInfo, error) {
	rows, err := s.db.pool.Query(
		`SELECT id::text, label, created_at, revoked_at FROM api_keys WHERE tenant_id = $1 ORDER BY created_at`,
		tenantID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKeyInfo
	for rows.Next() {
		var k APIKeyInfo
		if err := rows.Scan(&k.ID, &k.Label, &k.CreatedAt, &k.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeAPIKey withdraws a key by id, keeping the row as a record of what
// was issued. The bool reports whether a live key was actually revoked, so
// revoking an already-revoked or unknown id isn't reported as success.
func (s *Store) RevokeAPIKey(id string) (bool, error) {
	res, err := s.db.pool.Exec(
		`UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, id)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	return affected > 0, err
}
