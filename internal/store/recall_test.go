package store

import (
	"database/sql"
	"os"
	"strings"
	"testing"
)

// unitVector returns a DefaultEmbedDim vector pointing along the first axis,
// optionally tilted toward the second. tilt=0 is exactly the query vector
// (cosine distance 0); a larger tilt is farther away.
func unitVector(tilt float32) []float32 {
	v := make([]float32, DefaultEmbedDim)
	v[0] = 1
	if len(v) > 1 {
		v[1] = tilt
	}
	return v
}

// insertAs bulk-inserts n rows for one tenant with a fixed embedding,
// bypassing SaveMemory so the test can control distances precisely and load
// thousands of rows in one statement. It still goes through a tenant-bound
// transaction, so RLS applies exactly as in production.
func insertAs(t *testing.T, st *Store, tenantID, space, namePrefix string, vec []float32, n int) {
	t.Helper()
	tx, err := st.db.pool.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`SELECT set_config('app.tenant_id', $1, true)`, tenantID); err != nil {
		t.Fatalf("binding tenant: %v", err)
	}
	if _, err := tx.Exec(`
		INSERT INTO memories (space, name, chunk_index, content, embedding)
		SELECT $1, $2 || g, 0, 'recall test row', $3::vector
		FROM generate_series(1, $4) g
	`, space, namePrefix, VectorLiteral(vec), n); err != nil {
		t.Fatalf("bulk insert: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestIterativeScanSettingIsReal guards the subtlest part of this fix.
//
// pgvector's GUCs only come into existence once its shared library is loaded
// into the backend, and a connection fresh from the pool has not run any
// vector code yet. On such a connection, setting hnsw.iterative_scan SILENTLY
// SUCCEEDS as a custom placeholder GUC and has no effect whatsoever —
// current_setting even reads the value back, so nothing looks wrong. The
// first version of this fix was a complete no-op for exactly that reason.
//
// Asserting vartype='enum' is what distinguishes the real setting from a
// placeholder: a placeholder is always vartype='string'.
//
// The test must run on a connection that has NEVER executed vector code,
// which is why it opens its own pool rather than borrowing the Store's. By
// the time Open has migrated the schema, its pooled connections have all
// loaded the library and the bug becomes invisible — an earlier version of
// this test borrowed the Store's pool and passed happily against the broken
// implementation.
func TestIterativeScanSettingIsReal(t *testing.T) {
	st := setupTenantDB(t)
	supported, err := supportsIterativeScan(st.db.pool)
	if err != nil {
		t.Fatalf("checking pgvector version: %v", err)
	}
	if !supported {
		t.Skip("pgvector < 0.8, iterative scan unavailable")
	}

	raw, err := sql.Open("postgres", os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer raw.Close()
	cold := &tenantDB{pool: raw, tenant: BootstrapTenantID, bindSQL: bindStatement(true)}

	tx, err := cold.begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	var setting, vartype string
	if err := tx.QueryRow(
		`SELECT setting, vartype FROM pg_settings WHERE name = 'hnsw.iterative_scan'`,
	).Scan(&setting, &vartype); err != nil {
		t.Fatalf("hnsw.iterative_scan is not a known setting in this transaction: %v", err)
	}
	if vartype != "enum" {
		t.Errorf("hnsw.iterative_scan has vartype %q, want \"enum\" — it is being set as a "+
			"placeholder GUC before pgvector's library is loaded, which does nothing at all", vartype)
	}
	if setting != "relaxed_order" {
		t.Errorf("hnsw.iterative_scan = %q, want \"relaxed_order\"", setting)
	}

	var tenant string
	if err := tx.QueryRow(`SELECT current_setting('app.tenant_id')`).Scan(&tenant); err != nil {
		t.Fatalf("reading app.tenant_id: %v", err)
	}
	if tenant != BootstrapTenantID {
		t.Errorf("app.tenant_id = %q, want %q", tenant, BootstrapTenantID)
	}
}

// TestTenantSearchIsPreFilteredByTenant pins down what actually protects
// recall: step 1 widened the primary key to (tenant_id, space, name,
// chunk_index), and the RLS predicate is an indexable condition on its
// leading column. So a search plans as an exact scan over only the querying
// tenant's rows, rather than an approximate scan of a vector index shared
// with every other tenant.
//
// This is worth a test because it is load-bearing but invisible: nothing in
// the search code asks for it, and reordering the primary key — or dropping
// tenant_id from its front — would silently move searches onto the shared
// HNSW index, where a small tenant among large ones measurably loses matches.
func TestTenantSearchIsPreFilteredByTenant(t *testing.T) {
	st := setupTenantDB(t)
	makeTenant(t, st, tenantAID, "a@example.test")
	makeTenant(t, st, tenantBID, "b@example.test")

	const space = "recall"
	st.Embedder = &stubEmbedder{vec: unitVector(0)}
	// B's rows sit exactly on the query vector, A's slightly off it: on a
	// global top-k scan, every candidate would belong to B.
	insertAs(t, st, tenantBID, space, "b-noise-", unitVector(0), 2000)
	insertAs(t, st, tenantAID, space, "a-own-", unitVector(0.5), 5)
	t.Cleanup(func() {
		for _, id := range []string{tenantAID, tenantBID} {
			tx, err := st.db.pool.Begin()
			if err != nil {
				continue
			}
			tx.Exec(`SELECT set_config('app.tenant_id', $1, true)`, id)
			tx.Exec(`DELETE FROM memories WHERE space = $1`, space)
			tx.Commit()
		}
	})
	if _, err := st.db.pool.Exec(`ANALYZE memories`); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}

	a := st.ForTenant(tenantAID)

	// The plan must pre-filter on tenant_id rather than post-filter a global
	// vector scan.
	tx, err := a.db.begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	rows, err := tx.Query(`
		EXPLAIN SELECT name FROM memories WHERE space = $1
		ORDER BY embedding <=> $2::vector LIMIT 25`, space, VectorLiteral(unitVector(0)))
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan.WriteString(line + "\n")
	}
	rows.Close()
	if !strings.Contains(plan.String(), "tenant_id") {
		t.Errorf("search plan does not filter on tenant_id via an index condition, so it is "+
			"relying on the shared vector index and post-filtering — recall is no longer "+
			"guaranteed for a small tenant. Plan:\n%s", plan.String())
	}

	// And the user-visible contract: A gets all of its own matches back,
	// despite being outnumbered 400:1 in the shared index.
	got, err := a.SearchMemories(space, "anything", 5, SearchWeights{Semantic: 1, HalfLifeDays: 30}, "", "")
	if err != nil {
		t.Fatalf("SearchMemories: %v", err)
	}
	if len(got) != 5 {
		t.Errorf("tenant A got %d/5 of its own matches while sharing the index with 2000 of "+
			"tenant B's rows", len(got))
	}
	for _, m := range got {
		if !strings.HasPrefix(m.Name, "a-own-") {
			t.Fatalf("TENANT LEAK: tenant A's search returned %q", m.Name)
		}
	}
}
