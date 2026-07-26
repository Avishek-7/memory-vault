// Package store holds the Postgres/pgvector-backed memory CRUD, chunking,
// and embedding logic shared by the MCP server (main.go) and the
// memory-vault-tui browser (cmd/memory-vault-tui), so both talk to the
// same schema through one code path instead of duplicating SQL.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"

	"memory-vault/internal/embed"
)

// DefaultEmbedDim is the dimension of Ollama's all-minilm model, the
// default Embedder. A different Embedder/EMBED_DIM is an escape hatch —
// see Config.EmbedDim.
const DefaultEmbedDim = 384

// chunkTargetWords/chunkOverlapWords approximate all-minilm's 256-token
// budget via word count (~0.75 words/token), leaving headroom. Other
// embedding models may have a different real budget; this is a fixed
// approximation regardless of the configured Embedder.
const chunkTargetWords = 150
const chunkOverlapWords = 15

const DefaultSpace = "default"

// DefaultSource is recorded on a memory when no source is given — it's
// provenance metadata (which agent wrote it), not an isolation boundary
// like space.
const DefaultSource = "unspecified"

// DefaultKind is recorded on a memory when no kind is given.
const DefaultKind = "note"

// ValidKinds are the only structured memory types save_memory accepts.
// Enforced at the application level (not a DB constraint) so validation
// errors are a clear message instead of an opaque SQL error, and so the
// set can grow without a migration.
var ValidKinds = []string{"fact", "decision", "preference", "task", "note"}

// IsValidKind reports whether k is one of ValidKinds.
func IsValidKind(k string) bool {
	for _, v := range ValidKinds {
		if k == v {
			return true
		}
	}
	return false
}

// migrationSQL is templated on the embedding dimension so a non-default
// EMBED_DIM is reflected in the vector column at table-creation time.
func migrationSQL(dim int) string {
	return fmt.Sprintf(`
		CREATE EXTENSION IF NOT EXISTS vector;
		CREATE TABLE IF NOT EXISTS memories (
			space       TEXT NOT NULL DEFAULT 'default',
			name        TEXT NOT NULL,
			chunk_index INT NOT NULL DEFAULT 0,
			content     TEXT NOT NULL,
			embedding   vector(%d) NOT NULL,
			updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
			PRIMARY KEY (space, name, chunk_index)
		);
		ALTER TABLE memories ADD COLUMN IF NOT EXISTS chunk_index INT NOT NULL DEFAULT 0;
		ALTER TABLE memories ADD COLUMN IF NOT EXISTS space TEXT NOT NULL DEFAULT 'default';
		ALTER TABLE memories ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'unspecified';
		ALTER TABLE memories ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'note';
		DO $$
		BEGIN
			IF (SELECT array_length(conkey, 1) FROM pg_constraint
				WHERE conrelid = 'memories'::regclass AND contype = 'p') < 3 THEN
				ALTER TABLE memories DROP CONSTRAINT memories_pkey;
				ALTER TABLE memories ADD PRIMARY KEY (space, name, chunk_index);
			END IF;
		END $$;
		CREATE INDEX IF NOT EXISTS memories_embedding_idx
			ON memories USING hnsw (embedding vector_cosine_ops);
		ALTER TABLE memories ADD COLUMN IF NOT EXISTS content_tsv tsvector
			GENERATED ALWAYS AS (to_tsvector('english', content)) STORED;
		CREATE INDEX IF NOT EXISTS memories_content_tsv_idx ON memories USING GIN (content_tsv);
	`, dim)
}

// Config configures a Store: how to reach Postgres and which Embedder to
// use for turning memory content into vectors.
type Config struct {
	DatabaseURL string
	// Embedder defaults to an OllamaEmbedder against localhost:11434
	// running all-minilm if left nil.
	Embedder embed.Embedder
	// EmbedDim must match the dimension Embedder actually returns; it's
	// baked into the vector column at table-creation time. Defaults to
	// DefaultEmbedDim if <= 0.
	EmbedDim        int
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
}

type Store struct {
	DB       *sql.DB
	Embedder embed.Embedder
	EmbedDim int
}

// Open connects to Postgres, applies pool settings, and runs the (idempotent)
// schema migration.
func Open(cfg Config) (*Store, error) {
	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("DatabaseURL is not set")
	}
	dim := cfg.EmbedDim
	if dim <= 0 {
		dim = DefaultEmbedDim
	}
	embedder := cfg.Embedder
	if embedder == nil {
		embedder = &embed.OllamaEmbedder{URL: "http://localhost:11434", Model: "all-minilm"}
	}
	db, err := sql.Open("postgres", cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	if _, err := db.Exec(migrationSQL(dim)); err != nil {
		return nil, err
	}
	if err := checkEmbedDim(db, dim); err != nil {
		return nil, err
	}
	return &Store{DB: db, Embedder: embedder, EmbedDim: dim}, nil
}

// checkEmbedDim fails fast, with a clear message, if memories.embedding
// already exists at a different dimension than the configured EmbedDim.
// CREATE TABLE IF NOT EXISTS is a no-op against an existing table, so
// without this check a dimension change on an existing database would
// otherwise surface as a confusing Postgres error on the first insert.
func checkEmbedDim(db *sql.DB, dim int) error {
	var actual int
	err := db.QueryRow(`
		SELECT atttypmod FROM pg_attribute
		WHERE attrelid = 'memories'::regclass AND attname = 'embedding'
	`).Scan(&actual)
	if err != nil {
		return fmt.Errorf("checking memories.embedding dimension: %w", err)
	}
	if actual > 0 && actual != dim {
		return fmt.Errorf(
			"memories.embedding is %d-dimensional in the database but EMBED_DIM=%d — "+
				"switching embedding dimension/model on an existing database isn't supported "+
				"(see README's \"Using a different embedding backend\"); start from a fresh database instead",
			actual, dim)
	}
	return nil
}

func (s *Store) Close() error { return s.DB.Close() }

// ChunkContent splits content into overlapping word-based chunks that
// safely fit all-minilm's 256-token context window. Reassembly (Reassemble)
// simply concatenates chunks back together; the small overlap is kept for
// embedding quality and is not deduplicated on read.
func ChunkContent(content string) []string {
	words := strings.Fields(content)
	if len(words) <= chunkTargetWords {
		return []string{content}
	}
	var chunks []string
	start := 0
	for start < len(words) {
		end := start + chunkTargetWords
		if end > len(words) {
			end = len(words)
		}
		chunks = append(chunks, strings.Join(words[start:end], " "))
		if end == len(words) {
			break
		}
		start = end - chunkOverlapWords
	}
	return chunks
}

// Embed turns text into a vector via the configured Embedder, checking
// its length against EmbedDim immediately so a misconfigured/mismatched
// embedding backend fails here with a clear message rather than deep
// inside a SQL insert.
func (s *Store) Embed(text string) ([]float32, error) {
	vec, err := s.Embedder.Embed(text)
	if err != nil {
		return nil, err
	}
	if len(vec) != s.EmbedDim {
		return nil, fmt.Errorf("embedder returned a %d-dimension vector, want %d (set EMBED_DIM to match, or check the embedding model)", len(vec), s.EmbedDim)
	}
	return vec, nil
}

// VectorLiteral formats a float32 slice as pgvector's text input format, e.g. "[0.1,0.2,0.3]".
func VectorLiteral(v []float32) string {
	parts := make([]string, len(v))
	for i, f := range v {
		parts[i] = fmt.Sprintf("%g", f)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// ParseVectorLiteral parses pgvector's text output format ("[0.1,0.2,0.3]")
// back into a float64 slice.
func ParseVectorLiteral(s string) ([]float64, error) {
	s = strings.Trim(s, "[]")
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	out := make([]float64, len(parts))
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			return nil, err
		}
		out[i] = f
	}
	return out, nil
}

// CosineDistance mirrors pgvector's <=> operator (1 - cosine similarity).
func CosineDistance(a, b []float64) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 1
	}
	return 1 - dot/(math.Sqrt(na)*math.Sqrt(nb))
}

// MemoryMeta is a reassembled memory's content plus its descriptive
// metadata (source today; kind/flags join it in later phases).
type MemoryMeta struct {
	Content string
	Source  string
	Kind    string
}

// ReassembleMeta fetches all chunks for (space, name) ordered by chunk_index,
// joins their content back into the original text, and returns the memory's
// source/kind alongside it. Returns a zero MemoryMeta with no error if the
// memory doesn't exist.
func (s *Store) ReassembleMeta(space, name string) (MemoryMeta, error) {
	rows, err := s.DB.Query(`SELECT content, source, kind FROM memories WHERE space = $1 AND name = $2 ORDER BY chunk_index`, space, name)
	if err != nil {
		return MemoryMeta{}, err
	}
	defer rows.Close()
	var parts []string
	var source, kind string
	for rows.Next() {
		var c, src, k string
		if err := rows.Scan(&c, &src, &k); err != nil {
			return MemoryMeta{}, err
		}
		parts = append(parts, c)
		source, kind = src, k
	}
	return MemoryMeta{Content: strings.Join(parts, " "), Source: source, Kind: kind}, rows.Err()
}

// Reassemble fetches all chunks for (space, name) ordered by chunk_index
// and joins them back into the original content. Returns "" with no error
// if the memory doesn't exist.
func (s *Store) Reassemble(space, name string) (string, error) {
	meta, err := s.ReassembleMeta(space, name)
	return meta.Content, err
}

// SourceConflictError is returned by SaveMemoryExpectSource when the
// caller's expectSource doesn't match an existing memory's recorded source.
type SourceConflictError struct {
	Existing string
}

func (e *SourceConflictError) Error() string {
	return fmt.Sprintf("existing memory has source %q", e.Existing)
}

// SaveMemory chunks, embeds, and (re)writes content under (space, name),
// replacing any existing chunks for that name. Returns the chunk count.
// An empty source is recorded as DefaultSource; an empty kind is recorded
// as DefaultKind. Callers must validate kind against ValidKinds themselves
// (IsValidKind) — Store trusts it here.
func (s *Store) SaveMemory(space, name, content, source, kind string) (int, error) {
	if source == "" {
		source = DefaultSource
	}
	if kind == "" {
		kind = DefaultKind
	}
	_, chunks, err := s.saveMemory(space, name, content, source, kind, true, "")
	return chunks, err
}

// SaveMemoryExpectSource is SaveMemory, but first checks (within the same
// transaction, row-locked to close the race) whether a memory already
// exists at (space, name) with a different source. If so, it returns a
// *SourceConflictError instead of overwriting it — this is how two
// differently-sourced agents avoid silently clobbering each other's
// same-named memory. An empty expectSource skips the check entirely
// (SaveMemory's behavior).
func (s *Store) SaveMemoryExpectSource(space, name, content, source, kind, expectSource string) (int, error) {
	if source == "" {
		source = DefaultSource
	}
	if kind == "" {
		kind = DefaultKind
	}
	_, chunks, err := s.saveMemory(space, name, content, source, kind, true, expectSource)
	return chunks, err
}

// isUniqueViolation reports whether err is a Postgres unique-constraint
// violation (SQLSTATE 23505) — here, a concurrent write to the same
// (space, name, chunk_index) primary key.
func isUniqueViolation(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23505"
}

// saveMemory chunks, embeds, and writes content under (space, name) in a
// single transaction. If overwrite is true, any existing chunks for that
// name are deleted first (SaveMemory's behavior, unconditional replace).
// If overwrite is false, existing rows are left alone: the insert's own
// primary-key uniqueness is what enforces "don't clobber" atomically
// (rather than a separate existence check before the write, which a
// concurrent writer could race between) — a conflict there, whether from
// a memory that already existed before this call or one written by a
// concurrent caller mid-transaction, means wrote=false with no error.
//
// If expectSource is non-empty, an existing memory at (space, name) is
// row-locked and its source compared against expectSource before any
// write happens; a mismatch returns a *SourceConflictError and writes
// nothing.
func (s *Store) saveMemory(space, name, content, source, kind string, overwrite bool, expectSource string) (wrote bool, chunks int, err error) {
	chunkList := ChunkContent(content)
	tx, err := s.DB.Begin()
	if err != nil {
		return false, 0, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	if expectSource != "" {
		var existing string
		err := tx.QueryRow(`SELECT source FROM memories WHERE space = $1 AND name = $2 LIMIT 1 FOR UPDATE`, space, name).Scan(&existing)
		if err != nil && err != sql.ErrNoRows {
			return false, 0, err
		}
		if err == nil && existing != expectSource {
			return false, 0, &SourceConflictError{Existing: existing}
		}
	}

	if overwrite {
		if _, err := tx.Exec(`DELETE FROM memories WHERE space = $1 AND name = $2`, space, name); err != nil {
			return false, 0, err
		}
	}
	for i, chunk := range chunkList {
		vec, err := s.Embed(chunk)
		if err != nil {
			return false, 0, err
		}
		_, err = tx.Exec(`
			INSERT INTO memories (space, name, chunk_index, content, embedding, source, kind, updated_at)
			VALUES ($1, $2, $3, $4, $5::vector, $6, $7, now())
		`, space, name, i, chunk, VectorLiteral(vec), source, kind)
		if err != nil {
			if !overwrite && isUniqueViolation(err) {
				return false, 0, nil
			}
			return false, 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, 0, err
	}
	return true, len(chunkList), nil
}

// DeleteMemory deletes a memory by (space, name); the bool reports whether
// anything was deleted.
func (s *Store) DeleteMemory(space, name string) (bool, error) {
	res, err := s.DB.Exec(`DELETE FROM memories WHERE space = $1 AND name = $2`, space, name)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	return affected > 0, err
}

// ListMemoryNames lists the distinct memory names within a space, optionally
// filtered by source and/or kind (either "" means no filter on that field).
func (s *Store) ListMemoryNames(space, source, kind string) ([]string, error) {
	query := `SELECT DISTINCT name FROM memories WHERE space = $1`
	args := []interface{}{space}
	if source != "" {
		args = append(args, source)
		query += fmt.Sprintf(" AND source = $%d", len(args))
	}
	if kind != "" {
		args = append(args, kind)
		query += fmt.Sprintf(" AND kind = $%d", len(args))
	}
	query += ` ORDER BY name`
	rows, err := s.DB.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

type SpaceName struct {
	Space  string
	Name   string
	Source string
	Kind   string
}

// ListAll lists every (space, name) pair across all spaces, ordered by
// space then name, optionally filtered by source and/or kind (either ""
// means no filter on that field).
func (s *Store) ListAll(source, kind string) ([]SpaceName, error) {
	query := `SELECT DISTINCT space, name, source, kind FROM memories`
	var clauses []string
	var args []interface{}
	if source != "" {
		args = append(args, source)
		clauses = append(clauses, fmt.Sprintf("source = $%d", len(args)))
	}
	if kind != "" {
		args = append(args, kind)
		clauses = append(clauses, fmt.Sprintf("kind = $%d", len(args)))
	}
	if len(clauses) > 0 {
		query += ` WHERE ` + strings.Join(clauses, " AND ")
	}
	query += ` ORDER BY space, name`
	rows, err := s.DB.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SpaceName
	for rows.Next() {
		var sn SpaceName
		if err := rows.Scan(&sn.Space, &sn.Name, &sn.Source, &sn.Kind); err != nil {
			return nil, err
		}
		out = append(out, sn)
	}
	return out, rows.Err()
}

// Spaces lists the distinct spaces that currently hold any memory.
func (s *Store) Spaces() ([]string, error) {
	rows, err := s.DB.Query(`SELECT DISTINCT space FROM memories ORDER BY space`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sp string
		if err := rows.Scan(&sp); err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

// SearchWeights controls how SearchMemories blends its signals; defaults
// elsewhere reproduce semantic-only ranking. KindBoost, if set, adds a flat
// per-kind amount to a match's score (keyed by kind, e.g. "decision");
// a nil/empty map or missing key adds nothing.
type SearchWeights struct {
	Semantic     float64
	Keyword      float64
	Recency      float64
	HalfLifeDays float64
	KindBoost    map[string]float64
}

type SearchMatch struct {
	Name    string
	Score   float64
	Content string
}

// SearchMemories runs hybrid (semantic + keyword + recency + per-kind boost)
// search within a space and returns up to limit matches, highest score
// first, with each match's content already reassembled. Optional source/kind
// filter candidates ("" means no filter on that field).
func (s *Store) SearchMemories(space, query string, limit int, w SearchWeights, source, kind string) ([]SearchMatch, error) {
	vec, err := s.Embed(query)
	if err != nil {
		return nil, err
	}
	sqlQuery := `
		SELECT name, kind, embedding <=> $1::vector AS distance,
		       ts_rank(content_tsv, plainto_tsquery('english', $2)) AS rank,
		       updated_at
		FROM memories
		WHERE space = $3`
	args := []interface{}{VectorLiteral(vec), query, space}
	if source != "" {
		args = append(args, source)
		sqlQuery += fmt.Sprintf(" AND source = $%d", len(args))
	}
	if kind != "" {
		args = append(args, kind)
		sqlQuery += fmt.Sprintf(" AND kind = $%d", len(args))
	}
	sqlQuery += fmt.Sprintf(` ORDER BY distance ASC LIMIT $%d`, len(args)+1)
	args = append(args, limit*5)
	rows, err := s.DB.Query(sqlQuery, args...)
	if err != nil {
		return nil, err
	}
	type nameScore struct {
		name  string
		score float64
	}
	best := map[string]nameScore{}
	for rows.Next() {
		var n, k string
		var dist, rank float64
		var updatedAt time.Time
		if err := rows.Scan(&n, &k, &dist, &rank, &updatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		days := time.Since(updatedAt).Hours() / 24
		recency := math.Exp(-math.Ln2 * days / w.HalfLifeDays)
		score := w.Semantic*(1-dist) + w.Keyword*rank + w.Recency*recency + w.KindBoost[k]
		if cur, ok := best[n]; !ok || score > cur.score {
			best[n] = nameScore{name: n, score: score}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	matches := make([]nameScore, 0, len(best))
	for _, m := range best {
		matches = append(matches, m)
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].score > matches[j].score })
	if len(matches) > limit {
		matches = matches[:limit]
	}
	out := make([]SearchMatch, 0, len(matches))
	for _, m := range matches {
		content, err := s.Reassemble(space, m.name)
		if err != nil {
			return nil, err
		}
		out = append(out, SearchMatch{Name: m.name, Score: m.score, Content: content})
	}
	return out, nil
}

// MemoryCentroid is one memory's average chunk embedding and freshness,
// used by compact_memories to find near-duplicate or stale candidates.
type MemoryCentroid struct {
	Name        string
	Source      string
	Kind        string
	Centroid    []float64
	LastUpdated time.Time
}

// MemoryCentroids returns every memory name in a space with its centroid
// embedding (the average of its chunk embeddings), source, kind, and most
// recent update time.
func (s *Store) MemoryCentroids(space string) ([]MemoryCentroid, error) {
	rows, err := s.DB.Query(`SELECT name, max(source), max(kind), avg(embedding)::text, max(updated_at) FROM memories WHERE space = $1 GROUP BY name`, space)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemoryCentroid
	for rows.Next() {
		var name, source, kind, centroidStr string
		var lastUpdated time.Time
		if err := rows.Scan(&name, &source, &kind, &centroidStr, &lastUpdated); err != nil {
			return nil, err
		}
		centroid, err := ParseVectorLiteral(centroidStr)
		if err != nil {
			return nil, err
		}
		out = append(out, MemoryCentroid{Name: name, Source: source, Kind: kind, Centroid: centroid, LastUpdated: lastUpdated})
	}
	return out, rows.Err()
}

// MemoryExport is one memory's portable representation: no embeddings
// (cheap to regenerate on Import, and including them would tie the
// export to a specific embedding model/dimension).
type MemoryExport struct {
	Name      string    `json:"name"`
	Space     string    `json:"space"`
	Source    string    `json:"source,omitempty"`
	Kind      string    `json:"kind,omitempty"`
	Content   string    `json:"content"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Export reassembles every memory in space into its portable form,
// ordered by space then name. Exports every space if space == "", and
// every source if source == "".
func (s *Store) Export(space, source string) ([]MemoryExport, error) {
	query := `SELECT space, name, source, kind, content, updated_at FROM memories`
	var clauses []string
	var args []interface{}
	if space != "" {
		args = append(args, space)
		clauses = append(clauses, fmt.Sprintf("space = $%d", len(args)))
	}
	if source != "" {
		args = append(args, source)
		clauses = append(clauses, fmt.Sprintf("source = $%d", len(args)))
	}
	if len(clauses) > 0 {
		query += ` WHERE ` + strings.Join(clauses, " AND ")
	}
	query += ` ORDER BY space, name, chunk_index`
	rows, err := s.DB.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []MemoryExport
	for rows.Next() {
		var sp, name, src, kind, content string
		var updatedAt time.Time
		if err := rows.Scan(&sp, &name, &src, &kind, &content, &updatedAt); err != nil {
			return nil, err
		}
		if n := len(out); n > 0 && out[n-1].Space == sp && out[n-1].Name == name {
			out[n-1].Content += " " + content // rejoin chunks, same as Reassemble
		} else {
			out = append(out, MemoryExport{Space: sp, Name: name, Source: src, Kind: kind, Content: content, UpdatedAt: updatedAt})
		}
	}
	return out, rows.Err()
}

// ImportResult reports what Import did with each memory in the payload:
// which were written, and which were skipped because they already
// existed and overwrite was false.
type ImportResult struct {
	Imported []string // "space/name"
	Skipped  []string // "space/name"
}

// Import re-chunks and re-embeds every memory in data through SaveMemory
// (the normal save path), so chunking and whichever Embedder is
// currently configured are applied consistently rather than writing rows
// directly. spaceOverride, if non-empty, sends every memory to that
// space regardless of what's recorded in data; otherwise each memory
// keeps its own recorded space. sourceOverride works the same way for
// source; a memory whose export predates the source field (or has one
// blank) falls back to DefaultSource. A (space, name) pair that already
// exists is skipped unless overwrite is true.
func (s *Store) Import(data []MemoryExport, spaceOverride, sourceOverride string, overwrite bool) (ImportResult, error) {
	var res ImportResult
	for _, m := range data {
		space := m.Space
		if spaceOverride != "" {
			space = spaceOverride
		}
		if space == "" {
			space = DefaultSpace
		}
		source := m.Source
		if sourceOverride != "" {
			source = sourceOverride
		}
		if source == "" {
			source = DefaultSource
		}
		kind := m.Kind
		if kind == "" || !IsValidKind(kind) {
			kind = DefaultKind // backward-compat with pre-Phase-2 exports, and a safety net against a hand-edited payload
		}
		wrote, _, err := s.saveMemory(space, m.Name, m.Content, source, kind, overwrite, "")
		if err != nil {
			return res, err
		}
		if wrote {
			res.Imported = append(res.Imported, space+"/"+m.Name)
		} else {
			res.Skipped = append(res.Skipped, space+"/"+m.Name)
		}
	}
	return res, nil
}
