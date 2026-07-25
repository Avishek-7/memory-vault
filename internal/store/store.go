// Package store holds the Postgres/pgvector-backed memory CRUD, chunking,
// and embedding logic shared by the MCP server (main.go) and the
// memory-vault-tui browser (cmd/memory-vault-tui), so both talk to the
// same schema through one code path instead of duplicating SQL.
package store

import (
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "github.com/lib/pq"

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
	return &Store{DB: db, Embedder: embedder, EmbedDim: dim}, nil
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

// Reassemble fetches all chunks for (space, name) ordered by chunk_index
// and joins them back into the original content. Returns "" with no error
// if the memory doesn't exist.
func (s *Store) Reassemble(space, name string) (string, error) {
	rows, err := s.DB.Query(`SELECT content FROM memories WHERE space = $1 AND name = $2 ORDER BY chunk_index`, space, name)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var parts []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return "", err
		}
		parts = append(parts, c)
	}
	return strings.Join(parts, " "), rows.Err()
}

// SaveMemory chunks, embeds, and (re)writes content under (space, name),
// replacing any existing chunks for that name. Returns the chunk count.
func (s *Store) SaveMemory(space, name, content string) (int, error) {
	chunks := ChunkContent(content)
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`DELETE FROM memories WHERE space = $1 AND name = $2`, space, name); err != nil {
		tx.Rollback()
		return 0, err
	}
	for i, chunk := range chunks {
		vec, err := s.Embed(chunk)
		if err != nil {
			tx.Rollback()
			return 0, err
		}
		if _, err := tx.Exec(`
			INSERT INTO memories (space, name, chunk_index, content, embedding, updated_at)
			VALUES ($1, $2, $3, $4, $5::vector, now())
		`, space, name, i, chunk, VectorLiteral(vec)); err != nil {
			tx.Rollback()
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(chunks), nil
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

// ListMemoryNames lists the distinct memory names within a space.
func (s *Store) ListMemoryNames(space string) ([]string, error) {
	rows, err := s.DB.Query(`SELECT DISTINCT name FROM memories WHERE space = $1 ORDER BY name`, space)
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
	Space string
	Name  string
}

// ListAll lists every (space, name) pair across all spaces, ordered by
// space then name.
func (s *Store) ListAll() ([]SpaceName, error) {
	rows, err := s.DB.Query(`SELECT DISTINCT space, name FROM memories ORDER BY space, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SpaceName
	for rows.Next() {
		var sn SpaceName
		if err := rows.Scan(&sn.Space, &sn.Name); err != nil {
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

// SearchWeights controls how SearchMemories blends its three signals;
// defaults elsewhere reproduce semantic-only ranking.
type SearchWeights struct {
	Semantic     float64
	Keyword      float64
	Recency      float64
	HalfLifeDays float64
}

type SearchMatch struct {
	Name    string
	Score   float64
	Content string
}

// SearchMemories runs hybrid (semantic + keyword + recency) search within a
// space and returns up to limit matches, highest score first, with each
// match's content already reassembled.
func (s *Store) SearchMemories(space, query string, limit int, w SearchWeights) ([]SearchMatch, error) {
	vec, err := s.Embed(query)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(`
		SELECT name, embedding <=> $1::vector AS distance,
		       ts_rank(content_tsv, plainto_tsquery('english', $2)) AS rank,
		       updated_at
		FROM memories
		WHERE space = $3
		ORDER BY distance ASC
		LIMIT $4
	`, VectorLiteral(vec), query, space, limit*5)
	if err != nil {
		return nil, err
	}
	type nameScore struct {
		name  string
		score float64
	}
	best := map[string]nameScore{}
	for rows.Next() {
		var n string
		var dist, rank float64
		var updatedAt time.Time
		if err := rows.Scan(&n, &dist, &rank, &updatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		days := time.Since(updatedAt).Hours() / 24
		recency := math.Exp(-math.Ln2 * days / w.HalfLifeDays)
		score := w.Semantic*(1-dist) + w.Keyword*rank + w.Recency*recency
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
	Centroid    []float64
	LastUpdated time.Time
}

// MemoryCentroids returns every memory name in a space with its centroid
// embedding (the average of its chunk embeddings) and most recent update time.
func (s *Store) MemoryCentroids(space string) ([]MemoryCentroid, error) {
	rows, err := s.DB.Query(`SELECT name, avg(embedding)::text, max(updated_at) FROM memories WHERE space = $1 GROUP BY name`, space)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemoryCentroid
	for rows.Next() {
		var name, centroidStr string
		var lastUpdated time.Time
		if err := rows.Scan(&name, &centroidStr, &lastUpdated); err != nil {
			return nil, err
		}
		centroid, err := ParseVectorLiteral(centroidStr)
		if err != nil {
			return nil, err
		}
		out = append(out, MemoryCentroid{Name: name, Centroid: centroid, LastUpdated: lastUpdated})
	}
	return out, rows.Err()
}
