package store

import "testing"

type stubEmbedder struct {
	vec []float32
	err error
}

func (s *stubEmbedder) Embed(text string) ([]float32, error) { return s.vec, s.err }

func TestEmbedDimensionMismatch(t *testing.T) {
	st := &Store{Embedder: &stubEmbedder{vec: []float32{0.1, 0.2}}, EmbedDim: 3}
	if _, err := st.Embed("hello"); err == nil {
		t.Error("Embed() with mismatched dimension = nil error, want error")
	}
}

func TestEmbedDimensionMatch(t *testing.T) {
	st := &Store{Embedder: &stubEmbedder{vec: []float32{0.1, 0.2, 0.3}}, EmbedDim: 3}
	vec, err := st.Embed("hello")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vec) != 3 {
		t.Errorf("Embed() = %v, want length 3", vec)
	}
}

func TestChunkContentUnchangedForShortInput(t *testing.T) {
	short := "a few words"
	got := ChunkContent(short)
	if len(got) != 1 || got[0] != short {
		t.Errorf("ChunkContent(%q) = %v, want single unchanged chunk", short, got)
	}
}
