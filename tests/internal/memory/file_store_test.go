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

func (s *mockVectorStore) Query(_ context.Context, _ string, queryEmbedding []float32, nResults int, where map[string]interface{}) ([]SearchResult, error) {
	// Phase 8c: 按 conversation_date 过滤
	dateFilter, _ := where["conversation_date"].(string)

	type scored struct {
		id    string
		score float64
	}
	var scoredChunks []scored
	for id, vec := range s.vecs {
		c := s.chunks[id]
		// 日期过滤
		if dateFilter != "" && c.ConversationDate != "" && c.ConversationDate != dateFilter {
			continue
		}
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
			DateLabel: c.ConversationDate,
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
			DateLabel: c.ConversationDate,
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

// ---- Phase 8c: Time Metadata Anchoring Tests ----

func TestExtractDateConstraint_Absolute(t *testing.T) {
	tests := []struct {
		query      string
		wantDate   string
		wantFilter bool
	}{
		{"What happened on 2026-07-04?", "2026-07-04", true},
		{"changes from 2025/12/01", "2025-12-01", true},
		{"find decisions from 2024-01-15 about architecture", "2024-01-15", true},
		{"no date mentioned here", "", false},
		{"version 2.0 released 2023-06-30", "2023-06-30", true},
	}

	for _, tt := range tests {
		date, hasFilter := ExtractDateConstraint(tt.query)
		if date != tt.wantDate {
			t.Errorf("ExtractDateConstraint(%q): date = %q, want %q", tt.query, date, tt.wantDate)
		}
		if hasFilter != tt.wantFilter {
			t.Errorf("ExtractDateConstraint(%q): hasFilter = %v, want %v", tt.query, hasFilter, tt.wantFilter)
		}
	}
}

func TestExtractDateConstraint_Relative(t *testing.T) {
	// 相对时间：返回空日期但 hasFilter=true
	relQueries := []string{
		"what did we discuss today",
		"yesterday's decisions",
		"last week summary",
		"this month goals",
		"今天讨论了什么",
		"昨天的决策",
		"上周的工作",
		"最近的项目进展",
		"本月的计划",
	}

	for _, q := range relQueries {
		date, hasFilter := ExtractDateConstraint(q)
		if !hasFilter {
			t.Errorf("ExtractDateConstraint(%q): expected hasFilter=true", q)
		}
		if date != "" {
			t.Errorf("ExtractDateConstraint(%q): relative date should return empty string, got %q", q, date)
		}
	}
}

func TestExtractDateConstraint_InvalidDate(t *testing.T) {
	// 不符合日期格式的数字不应被提取
	invalidQueries := []string{
		"version 2.0.1 released",
		"port 8080 timeout",
		"size is 1234-5678-90",
	}

	for _, q := range invalidQueries {
		_, hasFilter := ExtractDateConstraint(q)
		if hasFilter {
			t.Errorf("ExtractDateConstraint(%q): expected hasFilter=false for invalid date", q)
		}
	}
}

func TestBuildDateMetadataFilter(t *testing.T) {
	filter := BuildDateMetadataFilter("2026-07-04")
	if filter == nil {
		t.Fatal("filter should not be nil")
	}
	if filter["conversation_date"] != "2026-07-04" {
		t.Errorf("conversation_date = %v", filter["conversation_date"])
	}

	// 空日期返回 nil
	if BuildDateMetadataFilter("") != nil {
		t.Error("empty date should return nil")
	}
}

func TestFormatDateLabels(t *testing.T) {
	results := []SearchResult{
		{ID: "a", Text: "Build agent with Go", DateLabel: "2026-07-04"},
		{ID: "b", Text: "Use ChromaDB for vectors", DateLabel: ""},
		{ID: "c", Text: "[2026-07-05] Already has prefix", DateLabel: "2026-07-05"},
	}

	formatted := FormatDateLabels(results)

	// 有 DateLabel → 加前缀
	if !strings.HasPrefix(formatted[0].Text, "[2026-07-04]") {
		t.Errorf("expected date prefix, got: %s", formatted[0].Text)
	}
	// 无 DateLabel → 不变
	if formatted[1].Text != "Use ChromaDB for vectors" {
		t.Errorf("expected unchanged text, got: %s", formatted[1].Text)
	}
	// 已有前缀 → 不重复
	if formatted[2].Text != "[2026-07-05] Already has prefix" {
		t.Errorf("expected no duplicate prefix, got: %s", formatted[2].Text)
	}
}

func TestFileStore_IndexFileWithDate_DateLabel(t *testing.T) {
	fs := NewFileStore(&mockEmbedder{dim: 8}, newMockVectorStore(), newMockKeywordStore(),
		&RuleTokenCounter{Divisor: 3.75}, "test_date_label", 100, 20)

	ctx := context.Background()

	// 给多个日期的文件建索引
	_ = fs.IndexFileWithDate(ctx, "memory/2026-07-01.md", "## Initial\nProject started", "2026-07-01")
	_ = fs.IndexFileWithDate(ctx, "memory/2026-07-04.md", "## Architecture\nUsing ChromaDB for memory storage", "2026-07-04")

	// 搜索应返回带 DateLabel 的结果
	results, err := fs.HybridSearch(ctx, "ChromaDB", 5)
	if err != nil {
		t.Fatalf("HybridSearch: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected results")
	}
	// 搜索结果应该包含日期标签前缀
	for _, r := range results {
		if r.DateLabel != "" && !strings.HasPrefix(r.Text, "["+r.DateLabel+"]") {
			t.Errorf("result should have date prefix: DateLabel=%q, Text=%q", r.DateLabel, r.Text)
		}
	}
}

func TestFileStore_Integration_DateMetadataFlow(t *testing.T) {
	// 端到端测试：IndexFileWithDate → VectorSearch/KeywordSearch/HybridSearch 都有 DateLabel
	fs := NewFileStore(&mockEmbedder{dim: 8}, newMockVectorStore(), newMockKeywordStore(),
		&RuleTokenCounter{Divisor: 3.75}, "test_date_flow", 200, 40)

	ctx := context.Background()

	content := "---\nconversation_date: 2026-07-15\n---\n## Architecture Decision\nUse microservices with Go."
	date, body := ParseFrontmatter(content)
	if date != "2026-07-15" {
		t.Fatalf("ParseFrontmatter: date = %q", date)
	}

	err := fs.IndexFileWithDate(ctx, "memory/2026-07-15.md", body, date)
	if err != nil {
		t.Fatalf("IndexFileWithDate: %v", err)
	}

	// 验证 meta
	meta := fs.GetFileMeta("memory/2026-07-15.md")
	if meta == nil {
		t.Fatal("meta should exist")
	}
	if meta.ConversationDate != "2026-07-15" {
		t.Errorf("meta.ConversationDate = %q", meta.ConversationDate)
	}

	// Vector search should get DateLabel
	vecRes, _ := fs.VectorSearch(ctx, "microservices", 3)
	if len(vecRes) == 0 {
		t.Fatal("vector search: no results")
	}
	hasDateLabel := false
	for _, r := range vecRes {
		if r.DateLabel != "" {
			hasDateLabel = true
			break
		}
	}
	if !hasDateLabel {
		t.Error("vector search results should have DateLabel")
	}

	// Hybrid search should get DateLabel too
	hyRes, _ := fs.HybridSearch(ctx, "Go microservices", 3)
	if len(hyRes) == 0 {
		t.Fatal("hybrid search: no results")
	}
	for _, r := range hyRes {
		if r.DateLabel != "" && !strings.HasPrefix(r.Text, "["+r.DateLabel+"]") {
			t.Errorf("hybrid result Text should have date prefix: got %q, DateLabel=%q", r.Text, r.DateLabel)
		}
	}
}

func TestFileStore_DateFilter_Search(t *testing.T) {
	fs := NewFileStore(&mockEmbedder{dim: 8}, newMockVectorStore(), newMockKeywordStore(),
		&RuleTokenCounter{Divisor: 3.75}, "test_date_filter", 200, 40)

	ctx := context.Background()

	// 写入不同日期的记忆
	_ = fs.IndexFileWithDate(ctx, "memory/2026-06-01.md", "## Decision\nUse Python", "2026-06-01")
	_ = fs.IndexFileWithDate(ctx, "memory/2026-07-15.md", "## Decision\nUse Go", "2026-07-15")

	// 搜索 "Decision" 应返回两个 chunk
	results1, _ := fs.HybridSearch(ctx, "Decision", 10)
	if len(results1) < 2 {
		t.Logf("hybrid search returned %d results for 'Decision'", len(results1))
	}

	// 带日期约束的搜索：只返回 2026-06-01 的
	results2, err := fs.HybridSearchWithDate(ctx, "Decision on 2026-06-01", 10)
	if err != nil {
		t.Fatalf("HybridSearchWithDate: %v", err)
	}
	if len(results2) == 0 {
		t.Fatal("expected at least 1 result for dated search")
	}
	foundOther := false
	for _, r := range results2 {
		if r.DateLabel == "2026-07-15" {
			foundOther = true
		}
	}
	if foundOther {
		t.Error("date filter should exclude 2026-07-15 results when searching 2026-06-01")
	}
}

func TestIsDatePattern(t *testing.T) {
	if !isDatePattern("2026-07-04", '-') {
		t.Error("valid date with dash")
	}
	if !isDatePattern("2026/07/04", '/') {
		t.Error("valid date with slash")
	}
	if isDatePattern("2026-7-04", '-') {
		t.Error("invalid: single digit month")
	}
	if isDatePattern("not-a-date-", '-') {
		t.Error("invalid: letters")
	}
	if isDatePattern("2026-07-04extra", '-') {
		t.Error("invalid: too long (checked in substring context)")
	}
}

func TestParseFrontmatter_Complex(t *testing.T) {
	content := "---\nconversation_date: 2026-07-04\nother_field: value\n---\n## Memory\nSome content here"
	date, body := ParseFrontmatter(content)
	if date != "2026-07-04" {
		t.Errorf("date = %q", date)
	}
	if !strings.HasPrefix(body, "## Memory") {
		t.Errorf("body should start with ## Memory, got: %s", body)
	}
}

func TestParseFrontmatter_OnlyDate(t *testing.T) {
	content := "---\nconversation_date: 2026-07-04\n---\nContent"
	date, body := ParseFrontmatter(content)
	if date != "2026-07-04" {
		t.Errorf("date = %q", date)
	}
	if body != "Content" {
		t.Errorf("body = %q", body)
	}
}
