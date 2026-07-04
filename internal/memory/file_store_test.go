package memory

import (
	"context"
	"strings"
	"testing"
)

// ---- mock Embedder ----

type mockEmbedder struct{ dim int }

func (m *mockEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	vec := make([]float32, m.dim)
	for i, r := range text {
		idx := i % m.dim
		vec[idx] += float32(r) / 1000.0
	}
	return vec, nil
}

func (m *mockEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	vecs := make([][]float32, len(texts))
	for i, t := range texts {
		v, _ := m.Embed(context.Background(), t)
		vecs[i] = v
	}
	return vecs, nil
}

var _ Embedder = (*mockEmbedder)(nil)

// ---- mock VectorStore ----

type mockVectorStore struct {
	chunks map[string]MemoryChunk
	vecs   map[string][]float32
}

func newMockVectorStore() *mockVectorStore {
	return &mockVectorStore{
		chunks: make(map[string]MemoryChunk),
		vecs:   make(map[string][]float32),
	}
}

func (s *mockVectorStore) EnsureCollection(_ context.Context, _ string) error { return nil }

func (s *mockVectorStore) Upsert(_ context.Context, _ string, chunks []MemoryChunk, embeddings [][]float32) error {
	for i, c := range chunks {
		s.chunks[c.ID] = c
		if i < len(embeddings) {
			s.vecs[c.ID] = embeddings[i]
		}
	}
	return nil
}

func (s *mockVectorStore) Query(_ context.Context, _ string, queryEmbedding []float32, nResults int, _ map[string]interface{}) ([]SearchResult, error) {
	type scored struct {
		id    string
		score float64
	}
	var scoredChunks []scored
	for id, vec := range s.vecs {
		sim := cosineSimilarity(queryEmbedding, vec)
		scoredChunks = append(scoredChunks, scored{id: id, score: sim})
	}
	// sort desc
	for i := 0; i < len(scoredChunks); i++ {
		for j := i + 1; j < len(scoredChunks); j++ {
			if scoredChunks[j].score > scoredChunks[i].score {
				scoredChunks[i], scoredChunks[j] = scoredChunks[j], scoredChunks[i]
			}
		}
	}
	if nResults > len(scoredChunks) {
		nResults = len(scoredChunks)
	}
	results := make([]SearchResult, nResults)
	for i := 0; i < nResults; i++ {
		c := s.chunks[scoredChunks[i].id]
		results[i] = SearchResult{
			ID:        c.ID,
			Path:      c.Path,
			StartLine: c.StartLine,
			EndLine:   c.EndLine,
			Text:      c.Text,
			Score:     scoredChunks[i].score,
		}
	}
	return results, nil
}

func (s *mockVectorStore) Delete(_ context.Context, _ string, ids []string) error {
	for _, id := range ids {
		delete(s.chunks, id)
		delete(s.vecs, id)
	}
	return nil
}

var _ VectorStore = (*mockVectorStore)(nil)

// ---- mock KeywordStore (in-memory) ----

type mockKeywordStore struct {
	entries map[string]*SearchResult // merge_key → result
}

func newMockKeywordStore() *mockKeywordStore {
	return &mockKeywordStore{entries: make(map[string]*SearchResult)}
}

func (s *mockKeywordStore) EnsureIndex(_ context.Context) error { return nil }

func (s *mockKeywordStore) IndexChunks(_ context.Context, chunks []MemoryChunk) error {
	for _, c := range chunks {
		s.entries[c.ID] = &SearchResult{
			ID:        c.ID,
			Path:      c.Path,
			StartLine: c.StartLine,
			EndLine:   c.EndLine,
			Text:      c.Text,
		}
	}
	return nil
}

func (s *mockKeywordStore) Search(_ context.Context, query string, limit int) ([]SearchResult, error) {
	lowerQ := strings.ToLower(query)
	var results []SearchResult
	for _, r := range s.entries {
		if strings.Contains(strings.ToLower(r.Text), lowerQ) {
			results = append(results, *r)
		}
	}
	// BM25-like score: count query words matched
	for i := range results {
		words := strings.Fields(lowerQ)
		match := 0
		for _, w := range words {
			if strings.Contains(strings.ToLower(results[i].Text), w) {
				match++
			}
		}
		results[i].Score = float64(match) / float64(len(words))
	}
	// sort desc
	for i := 0; i < len(results); i++ {
		for j := i + 1; j < len(results); j++ {
			if results[j].Score > results[i].Score {
				results[i], results[j] = results[j], results[i]
			}
		}
	}
	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

func (s *mockKeywordStore) Delete(_ context.Context, ids []string) error {
	for _, id := range ids {
		delete(s.entries, id)
	}
	return nil
}

func (s *mockKeywordStore) Close() error { return nil }

var _ KeywordStore = (*mockKeywordStore)(nil)

// ---- helper ----

func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (normA * normB)
}

// ---- tests ----

func TestChunkMarkdown_Basic(t *testing.T) {
	counter := &RuleTokenCounter{Divisor: 3.75}
	text := "# Project Goals\n- Build AI\n- Support tools\n\n## Architecture\n- Go backend\n- ChromaDB\n\n## Notes\n- Handle errors"

	chunks := chunkMarkdown(text, "test.md", "memory", 20, 5, counter)
	if len(chunks) == 0 {
		t.Fatal("expected at least 1 chunk")
	}
	for i, c := range chunks {
		if c.ID == "" {
			t.Errorf("chunk %d: missing ID", i)
		}
		if c.Path != "test.md" {
			t.Errorf("chunk %d: path mismatch", i)
		}
		if c.Text == "" {
			t.Errorf("chunk %d: empty text", i)
		}
	}
}

func TestChunkMarkdown_Empty(t *testing.T) {
	counter := &RuleTokenCounter{Divisor: 3.75}
	chunks := chunkMarkdown("", "t.md", "src", 100, 20, counter)
	if chunks != nil {
		t.Error("empty should return nil")
	}
}

func TestChunkMarkdown_SingleLine(t *testing.T) {
	counter := &RuleTokenCounter{Divisor: 3.75}
	chunks := chunkMarkdown("one line", "t.md", "src", 100, 20, counter)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
}

func TestFileStore_IndexAndDualSearch(t *testing.T) {
	mockEmb := &mockEmbedder{dim: 8}
	mockVec := newMockVectorStore()
	mockKW := newMockKeywordStore()
	counter := &RuleTokenCounter{Divisor: 3.75}
	fs := NewFileStore(mockEmb, mockVec, mockKW, counter, "test_mem", 100, 20)

	content := "## Goal\nBuild agent.\n\n## Decision\nUse Go and ChromaDB."
	ctx := context.Background()

	err := fs.IndexFile(ctx, "memory/test.md", content)
	if err != nil {
		t.Fatalf("IndexFile: %v", err)
	}

	// Vector search
	vecRes, err := fs.VectorSearch(ctx, "Go", 3)
	if err != nil {
		t.Fatalf("VectorSearch: %v", err)
	}
	if len(vecRes) == 0 {
		t.Error("vector search: no results")
	}

	// Keyword search
	kwRes, err := fs.KeywordSearch(ctx, "ChromaDB", 3)
	if err != nil {
		t.Fatalf("KeywordSearch: %v", err)
	}
	if len(kwRes) == 0 {
		t.Error("keyword search: no results")
	}

	// Hybrid search
	hyRes, err := fs.HybridSearch(ctx, "agent Go", 3)
	if err != nil {
		t.Fatalf("HybridSearch: %v", err)
	}
	if len(hyRes) == 0 {
		t.Error("hybrid search: no results")
	}
}

func TestFileStore_HasChanged(t *testing.T) {
	fs := NewFileStore(&mockEmbedder{dim: 8}, newMockVectorStore(), newMockKeywordStore(),
		&RuleTokenCounter{Divisor: 3.75}, "test", 100, 20)

	_ = fs.IndexFile(context.Background(), "f.md", "v1")
	if fs.HasChanged("f.md", "v1") {
		t.Error("same content: should not change")
	}
	if !fs.HasChanged("f.md", "v2") {
		t.Error("different content: should change")
	}
}

func TestFileStore_RemoveFile(t *testing.T) {
	fs := NewFileStore(&mockEmbedder{dim: 8}, newMockVectorStore(), newMockKeywordStore(),
		&RuleTokenCounter{Divisor: 3.75}, "test", 100, 20)

	_ = fs.IndexFile(context.Background(), "f.md", "test")
	if fs.GetFileMeta("f.md") == nil {
		t.Fatal("expected meta after index")
	}
	_ = fs.RemoveFile(context.Background(), "f.md")
	if fs.GetFileMeta("f.md") != nil {
		t.Error("meta should be nil after remove")
	}
}

func TestRRFMerge(t *testing.T) {
	vec := []SearchResult{
		{ID: "a", Text: "chunk A", Score: 0.8},
		{ID: "b", Text: "chunk B", Score: 0.5},
	}
	kw := []SearchResult{
		{ID: "b", Text: "chunk B", Score: 0.9},
		{ID: "c", Text: "chunk C", Score: 0.6},
	}

	merged := rrfMerge(vec, kw, 60)
	byID := make(map[string]float64)
	for _, r := range merged {
		byID[r.ID] = r.Score
	}

	// RRF: a appears only in vec (rank 1), b appears in both, c appears only in kw (rank 2)
	// a: 1/(60+1) = 1/61 ≈ 0.01639
	// b: 1/(60+2) + 1/(60+1) = 1/62 + 1/61 ≈ 0.03253
	// c: 1/(60+2) = 1/62 ≈ 0.01613
	if byID["b"] < byID["a"] {
		t.Error("b should rank higher than a (appears in both lists)")
	}
}

func TestParseFrontmatter(t *testing.T) {
	content := "---\nconversation_date: 2026-07-04\n---\n## Goal\nBuild something"
	date, body := ParseFrontmatter(content)
	if date != "2026-07-04" {
		t.Errorf("expected date 2026-07-04, got %q", date)
	}
	if !strings.Contains(body, "## Goal") {
		t.Errorf("body should contain ## Goal, got %q", body)
	}
}

func TestParseFrontmatter_NoFrontmatter(t *testing.T) {
	content := "## Goal\nJust content no frontmatter"
	date, body := ParseFrontmatter(content)
	if date != "" {
		t.Errorf("expected empty date, got %q", date)
	}
	if body != content {
		t.Errorf("body should be unchanged")
	}
}

func TestFileStore_IndexWithDate(t *testing.T) {
	fs := NewFileStore(&mockEmbedder{dim: 8}, newMockVectorStore(), newMockKeywordStore(),
		&RuleTokenCounter{Divisor: 3.75}, "test", 100, 20)

	ctx := context.Background()
	err := fs.IndexFileWithDate(ctx, "memory/2026-07-04.md", "## Goal\nTest", "2026-07-04")
	if err != nil {
		t.Fatalf("IndexFileWithDate: %v", err)
	}

	meta := fs.GetFileMeta("memory/2026-07-04.md")
	if meta == nil {
		t.Fatal("meta is nil")
	}
	if meta.ConversationDate != "2026-07-04" {
		t.Errorf("ConversationDate = %q", meta.ConversationDate)
	}
}

func TestFTS5Store_Integration(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/fts5_test.db"

	store, err := NewFTS5Store(dbPath)
	if err != nil {
		t.Fatalf("NewFTS5Store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	if err := store.EnsureIndex(ctx); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}

	chunks := []MemoryChunk{
		{ID: "c1", Path: "test.md", StartLine: 1, EndLine: 2, Text: "Build agent with Go"},
		{ID: "c2", Path: "test.md", StartLine: 3, EndLine: 4, Text: "Use ChromaDB for vectors"},
	}

	if err := store.IndexChunks(ctx, chunks); err != nil {
		t.Fatalf("IndexChunks: %v", err)
	}

	// Search
	results, err := store.Search(ctx, "ChromaDB", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected search results for 'ChromaDB'")
	}
	found := false
	for _, r := range results {
		if strings.Contains(r.Text, "ChromaDB") {
			found = true
			break
		}
	}
	if !found {
		t.Error("FTS5 search should find ChromaDB")
	}

	// Delete
	if err := store.Delete(ctx, []string{"c1"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	results2, _ := store.Search(ctx, "Go", 5)
	for _, r := range results2 {
		if r.ID == "c1" {
			t.Error("c1 should be deleted")
		}
	}
}

func TestSemanticChunk_Basic(t *testing.T) {
	emb := &mockEmbedder{dim: 8}
	text := `## Introduction
This is the first section about the project.

## Architecture
The system uses microservices.

## Conclusion
We learned a lot from this experiment.`

	ctx := context.Background()
	chunks, err := SemanticChunk(ctx, text, "test.md", "memory", emb)
	if err != nil {
		t.Fatalf("SemanticChunk: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatal("expected at least 1 chunk")
	}
	for _, c := range chunks {
		if c.Text == "" {
			t.Error("chunk text should not be empty")
		}
	}
}

func TestDistanceToSimilarity(t *testing.T) {
	if distanceToSimilarity(0) != 1.0 {
		t.Error("d=0 -> s=1")
	}
	if distanceToSimilarity(1.0) != 0.5 {
		t.Error("d=1 -> s=0.5")
	}
}

func TestContentHash(t *testing.T) {
	if contentHash("a") != contentHash("a") {
		t.Error("same -> same hash")
	}
	if contentHash("a") == contentHash("b") {
		t.Error("diff -> diff hash")
	}
}

func TestToFTS5Query(t *testing.T) {
	if toFTS5Query("hello") != `"hello"` {
		t.Errorf("single word: %q", toFTS5Query("hello"))
	}
	q := toFTS5Query("build agent")
	if !strings.Contains(q, "AND") {
		t.Errorf("multi-word should use AND: %q", q)
	}
}
