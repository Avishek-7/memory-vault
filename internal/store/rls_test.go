package store

import (
	"database/sql"
	"os"
	"strings"
	"testing"
)

// setupTenantDB opens a Store against a real Postgres+pgvector, skipping if
// DATABASE_URL is unset. Deliberately in the store package rather than
// alongside main_test.go's integration tests: those need a live Ollama, so
// they never run in CI, and an isolation test that silently skips is worse
// than no isolation test. A stub embedder needs no model server, so this one
// runs anywhere Postgres does.
func setupTenantDB(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set, skipping RLS isolation test")
	}
	st, err := Open(Config{
		DatabaseURL:  url,
		Embedder:     &stubEmbedder{vec: make([]float32, DefaultEmbedDim)},
		MaxOpenConns: 4,
		MaxIdleConns: 2,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// makeTenant inserts a tenant directly (there is no key-minting CLI until
// step 2) and removes it, and everything it owns, on cleanup.
func makeTenant(t *testing.T, st *Store, id, email string) {
	t.Helper()
	if _, err := st.db.pool.Exec(
		`INSERT INTO tenants (id, email) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`,
		id, email,
	); err != nil {
		t.Fatalf("creating tenant %s: %v", email, err)
	}
	t.Cleanup(func() { st.db.pool.Exec(`DELETE FROM tenants WHERE id = $1`, id) })
}

const (
	tenantAID = "11111111-1111-1111-1111-111111111111"
	tenantBID = "22222222-2222-2222-2222-222222222222"
)

// TestTenantIsolation is the load-bearing test for step 1: it must fail if
// RLS is ever disabled, if FORCE ROW LEVEL SECURITY is dropped (the app
// connects as the table owner, who would otherwise bypass the policy), or if
// tenantDB stops binding app.tenant_id.
func TestTenantIsolation(t *testing.T) {
	st := setupTenantDB(t)
	makeTenant(t, st, tenantAID, "a@example.test")
	makeTenant(t, st, tenantBID, "b@example.test")

	a := st.ForTenant(tenantAID)
	b := st.ForTenant(tenantBID)

	if _, err := a.SaveMemory("shared-space", "secret", "tenant A private content", DefaultSource, DefaultKind); err != nil {
		t.Fatalf("tenant A SaveMemory: %v", err)
	}
	if _, err := b.SaveMemory("shared-space", "secret", "tenant B private content", DefaultSource, DefaultKind); err != nil {
		t.Fatalf("tenant B SaveMemory: %v", err)
	}

	// Same space, same memory name: only the tenant boundary separates
	// these two rows, so a leak shows up as A reading B's content.
	got, err := a.Reassemble("shared-space", "secret")
	if err != nil {
		t.Fatalf("tenant A Reassemble: %v", err)
	}
	if !strings.Contains(got, "tenant A") {
		t.Errorf("tenant A read %q, want its own content", got)
	}
	if strings.Contains(got, "tenant B") {
		t.Fatalf("TENANT LEAK: tenant A read tenant B's content: %q", got)
	}

	// Listing and space enumeration must not reveal the other tenant either
	// — neither the rows nor the fact that a space has more in it.
	names, err := a.ListMemoryNames("shared-space", "", "")
	if err != nil {
		t.Fatalf("tenant A ListMemoryNames: %v", err)
	}
	if len(names) != 1 {
		t.Errorf("tenant A sees %d names in shared-space, want 1: %v", len(names), names)
	}

	all, err := a.ListAll("", "")
	if err != nil {
		t.Fatalf("tenant A ListAll: %v", err)
	}
	for _, sn := range all {
		if content, _ := a.Reassemble(sn.Space, sn.Name); strings.Contains(content, "tenant B") {
			t.Fatalf("TENANT LEAK: ListAll exposed tenant B's %s/%s", sn.Space, sn.Name)
		}
	}

	t.Cleanup(func() {
		a.DeleteMemory("shared-space", "secret")
		b.DeleteMemory("shared-space", "secret")
	})
}

// TestTenantIsolationAgainstSQLInjection feeds SQL metacharacters through the
// space field, which reaches every WHERE clause in the package. The queries
// are parameterized, so this asserts the belt (no injection) — and because
// RLS filters independently of the WHERE clause, the braces would hold even
// if a query were ever built by concatenation.
func TestTenantIsolationAgainstSQLInjection(t *testing.T) {
	st := setupTenantDB(t)
	makeTenant(t, st, tenantAID, "a@example.test")
	makeTenant(t, st, tenantBID, "b@example.test")

	a := st.ForTenant(tenantAID)
	b := st.ForTenant(tenantBID)

	if _, err := b.SaveMemory("b-space", "b-secret", "tenant B private content", DefaultSource, DefaultKind); err != nil {
		t.Fatalf("tenant B SaveMemory: %v", err)
	}
	t.Cleanup(func() { b.DeleteMemory("b-space", "b-secret") })

	injections := []string{
		"b-space' OR '1'='1",
		"' OR 1=1 --",
		"b-space'; SET app.tenant_id = '" + tenantBID + "'; --",
		"b-space' UNION SELECT content FROM memories --",
		"b-space",
	}
	for _, space := range injections {
		names, err := a.ListMemoryNames(space, "", "")
		if err != nil {
			t.Fatalf("ListMemoryNames(%q): %v", space, err)
		}
		if len(names) != 0 {
			t.Fatalf("TENANT LEAK: injection %q returned tenant B rows: %v", space, names)
		}
		if content, err := a.Reassemble(space, "b-secret"); err != nil {
			t.Fatalf("Reassemble(%q): %v", space, err)
		} else if content != "" {
			t.Fatalf("TENANT LEAK: injection %q read tenant B content: %q", space, content)
		}
	}
}

// TestUnboundTenantIsRejected proves the policy is what does the filtering,
// not the application's WHERE clauses: a raw pool connection with no
// app.tenant_id bound cannot read the table at all. If this test starts
// passing rows through, RLS has been disabled or lost its FORCE.
func TestUnboundTenantIsRejected(t *testing.T) {
	st := setupTenantDB(t)
	makeTenant(t, st, tenantAID, "a@example.test")

	a := st.ForTenant(tenantAID)
	if _, err := a.SaveMemory("rls-check", "row", "content", DefaultSource, DefaultKind); err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}
	t.Cleanup(func() { a.DeleteMemory("rls-check", "row") })

	// Bypassing tenantDB entirely — exactly what no code outside this
	// package can do, since Store exposes no *sql.DB.
	var count int
	err := st.db.pool.QueryRow(`SELECT count(*) FROM memories`).Scan(&count)
	if err == nil {
		t.Fatalf("unbound query succeeded and saw %d rows; RLS is not being enforced "+
			"(check ENABLE/FORCE ROW LEVEL SECURITY on memories)", count)
	}
	// Two possible shapes, both hard errors, never a silent empty result:
	// on a connection that has never bound the setting, current_setting
	// raises "unrecognized configuration parameter". On a connection
	// recycled from the pool it raises on the uuid cast instead — a
	// customized GUC stays defined for the life of the session once set, and
	// SET LOCAL reverts it at commit to its prior value, which for a
	// never-previously-set custom GUC is the empty string rather than
	// undefined. ''::uuid then fails. Either way the query cannot return rows.
	msg := err.Error()
	if !strings.Contains(msg, "app.tenant_id") && !strings.Contains(msg, "invalid input syntax for type uuid") {
		t.Errorf("unbound query failed with %v, want an unset-app.tenant_id error", err)
	}
}

// TestSuperuserIsRejected covers the trap that makes every other test in
// this file meaningless if it regresses: a superuser bypasses RLS outright,
// so the policy would be fully configured and enforce nothing. Requires
// SUPERUSER_DATABASE_URL, since it needs a privileged role to test against.
func TestSuperuserIsRejected(t *testing.T) {
	url := os.Getenv("SUPERUSER_DATABASE_URL")
	if url == "" {
		t.Skip("SUPERUSER_DATABASE_URL not set, skipping superuser rejection test")
	}
	st, err := Open(Config{
		DatabaseURL: url,
		Embedder:    &stubEmbedder{vec: make([]float32, DefaultEmbedDim)},
	})
	if err == nil {
		st.Close()
		t.Fatal("Open succeeded as a superuser; RLS cannot be enforced for such a role " +
			"and startup must refuse it")
	}
	if !strings.Contains(err.Error(), "BYPASSRLS") {
		t.Errorf("Open failed with %v, want an explanation naming the privilege problem", err)
	}
}

// TestUpgradeFromSingleTenantPreservesData is the other half of step 1's
// acceptance: an existing self-hosted vault, whose rows predate tenant_id
// entirely, must still be readable after the migration rather than
// disappearing behind the new policy.
//
// It builds the pre-multi-tenancy shape of `memories` by hand (three-column
// primary key, no tenant_id), writes a row into it, and then lets Open
// migrate it in place.
func TestUpgradeFromSingleTenantPreservesData(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set, skipping upgrade test")
	}
	raw, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer raw.Close()

	// Isolate from whatever the other tests left behind.
	if _, err := raw.Exec(`DROP TABLE IF EXISTS memories CASCADE`); err != nil {
		t.Fatalf("dropping memories: %v", err)
	}
	t.Cleanup(func() { raw.Exec(`DROP TABLE IF EXISTS memories CASCADE`) })

	if _, err := raw.Exec(`
		CREATE EXTENSION IF NOT EXISTS vector;
		CREATE TABLE memories (
			space       TEXT NOT NULL DEFAULT 'default',
			name        TEXT NOT NULL,
			chunk_index INT NOT NULL DEFAULT 0,
			content     TEXT NOT NULL,
			embedding   vector(384) NOT NULL,
			updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
			source      TEXT NOT NULL DEFAULT 'unspecified',
			kind        TEXT NOT NULL DEFAULT 'note',
			PRIMARY KEY (space, name, chunk_index)
		);
		INSERT INTO memories (space, name, chunk_index, content, embedding)
			VALUES ('default', 'pre-existing', 0, 'written before multi-tenancy',
			        array_fill(0::real, ARRAY[384])::vector);
	`); err != nil {
		t.Fatalf("building pre-multi-tenancy schema: %v", err)
	}

	st, err := Open(Config{
		DatabaseURL: url,
		Embedder:    &stubEmbedder{vec: make([]float32, DefaultEmbedDim)},
	})
	if err != nil {
		t.Fatalf("Open on a pre-multi-tenancy database: %v", err)
	}
	defer st.Close()

	// Open binds the bootstrap tenant, which the migration hands the
	// orphaned rows to — so the vault reads exactly as it did before.
	got, err := st.Reassemble("default", "pre-existing")
	if err != nil {
		t.Fatalf("Reassemble after upgrade: %v", err)
	}
	if got != "written before multi-tenancy" {
		t.Errorf("after upgrade read %q, want the pre-existing content intact", got)
	}
}

// TestBootstrapTenantExists covers the single-tenant upgrade path: the
// migration must create the tenant that adopts pre-multi-tenancy rows, or an
// existing self-hosted deploy loses its data behind the new policy.
func TestBootstrapTenantExists(t *testing.T) {
	st := setupTenantDB(t)

	var plan string
	err := st.db.pool.QueryRow(`SELECT plan FROM tenants WHERE id = $1`, BootstrapTenantID).Scan(&plan)
	if err == sql.ErrNoRows {
		t.Fatal("bootstrap tenant missing; pre-multi-tenancy rows would be orphaned")
	}
	if err != nil {
		t.Fatalf("querying bootstrap tenant: %v", err)
	}

	// Open's default binding is the bootstrap tenant, so an unmodified
	// single-tenant deploy keeps working with no configuration change.
	if st.db.tenant != BootstrapTenantID {
		t.Errorf("Open bound tenant %q, want the bootstrap tenant %q", st.db.tenant, BootstrapTenantID)
	}
}
