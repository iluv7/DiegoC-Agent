package memory

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"diegoc-agent/internal/llm"
	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

// fixedMockLLM returns the same response on every Generate call.
type fixedMockLLM struct{ resp *schema.LLMResponse }

func (m *fixedMockLLM) Generate(_ context.Context, _ []schema.Message, _ []tools.Tool) (*schema.LLMResponse, error) {
	return m.resp, nil
}

var _ llm.Client = (*fixedMockLLM)(nil)

// errorMockLLM always fails.
type errorMockLLM struct{}

func (m *errorMockLLM) Generate(_ context.Context, _ []schema.Message, _ []tools.Tool) (*schema.LLMResponse, error) {
	return nil, errors.New("llm unavailable")
}

var _ llm.Client = (*errorMockLLM)(nil)

// indexTestMemoryFiles 把几条测试记忆写入 FileStore，返回 store。
func indexTestMemoryFiles(t *testing.T) *FileStore {
	t.Helper()
	store := newTestFileStore()
	ctx := context.Background()

	files := map[string]string{
		"2026-08-01.md": `---
conversation_date: 2026-08-01
---

## User Preferences
- User prefers Python 3.12 for backend projects
- User likes concise answers
`,
		"2026-08-05.md": `---
conversation_date: 2026-08-05
---

## Project Decisions
- Chose ChromaDB as the vector storage backend
- Memory index uses SQLite FTS5 for keyword search
`,
	}

	for name, content := range files {
		convDate, body := ParseFrontmatter(content)
		if err := store.IndexFileWithDate(ctx, name, body, convDate); err != nil {
			t.Fatalf("index %s: %v", name, err)
		}
	}
	return store
}

func TestMemorySearchTool_Basic(t *testing.T) {
	store := indexTestMemoryFiles(t)

	// Mock LLM 返回 2 个语义变体。
	mock := &fixedMockLLM{resp: &schema.LLMResponse{
		Content: "- Python version preference\n- backend programming language\n- python 3.12 setup\n",
	}}
	tool := NewMemorySearchTool(store, mock, "")

	result, err := tool.Execute(context.Background(), map[string]interface{}{
		"query": "What Python version does the user prefer?",
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !result.Success {
		t.Fatalf("Execute not successful: %v", result.Error)
	}

	// 结果应为 JSON 数组。
	var parsed []map[string]interface{}
	if err := json.Unmarshal([]byte(result.Content), &parsed); err != nil {
		t.Fatalf("result is not valid JSON: %v\ncontent: %s", err, result.Content)
	}
	if len(parsed) == 0 {
		t.Fatal("expected at least one result")
	}

	// 第一个结果应命中 Python 偏好的文件。
	first := parsed[0]
	if path, _ := first["path"].(string); !strings.Contains(path, "2026-08-01") {
		t.Errorf("top result path = %q, want file containing user preference", path)
	}
	for _, field := range []string{"path", "start_line", "end_line", "score", "snippet"} {
		if _, ok := first[field]; !ok {
			t.Errorf("result missing field %q: %v", field, first)
		}
	}
	t.Logf("top result: %+v", first)
}

func TestMemorySearchTool_LLMFailure_FallsBackToSingleQuery(t *testing.T) {
	store := indexTestMemoryFiles(t)

	// LLM 报错 → 退化为原 query 单路搜索，仍应返回结果。
	tool := NewMemorySearchTool(store, &errorMockLLM{}, "")

	result, err := tool.Execute(context.Background(), map[string]interface{}{
		"query": "ChromaDB",
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !result.Success {
		t.Fatalf("Execute not successful: %v", result.Error)
	}

	var parsed []map[string]interface{}
	if err := json.Unmarshal([]byte(result.Content), &parsed); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if len(parsed) == 0 {
		t.Fatal("expected results even when LLM variations fail")
	}
	t.Logf("fallback search returned %d results", len(parsed))
}

func TestMemorySearchTool_NilLLM_SkipsVariations(t *testing.T) {
	store := indexTestMemoryFiles(t)

	// llmClient 为 nil → 跳过变体生成，不 panic。
	tool := NewMemorySearchTool(store, nil, "")

	result, err := tool.Execute(context.Background(), map[string]interface{}{
		"query": "vector storage",
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !result.Success {
		t.Fatalf("Execute not successful: %v", result.Error)
	}
	var parsed []map[string]interface{}
	if err := json.Unmarshal([]byte(result.Content), &parsed); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if len(parsed) == 0 {
		t.Fatal("expected results with nil LLM client")
	}
}

func TestMemorySearchTool_NilFileStore(t *testing.T) {
	tool := NewMemorySearchTool(nil, nil, "")

	result, err := tool.Execute(context.Background(), map[string]interface{}{
		"query": "anything",
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if result.Success {
		t.Error("expected failure when fileStore is nil")
	}
	if !strings.Contains(result.Error, "not available") {
		t.Errorf("error message = %q, want 'not available' hint", result.Error)
	}
}

func TestMemorySearchTool_MissingQuery(t *testing.T) {
	store := newTestFileStore()
	tool := NewMemorySearchTool(store, nil, "")

	result, err := tool.Execute(context.Background(), map[string]interface{}{})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if result.Success {
		t.Error("expected failure when query is missing")
	}
}

func TestMemorySearchTool_MaxResultsClamped(t *testing.T) {
	store := indexTestMemoryFiles(t)
	tool := NewMemorySearchTool(store, nil, "")

	// maxResults 超上限 100 → 钳制到 100（此处结果数受索引大小限制）。
	result, err := tool.Execute(context.Background(), map[string]interface{}{
		"query":       "memory",
		"max_results": float64(9999),
		"min_score":   float64(0.0),
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !result.Success {
		t.Fatalf("Execute not successful: %v", result.Error)
	}

	// maxResults = 1 时只返回 1 条。
	result2, err := tool.Execute(context.Background(), map[string]interface{}{
		"query":       "memory",
		"max_results": float64(1),
		"min_score":   float64(0.0),
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	var parsed []map[string]interface{}
	if err := json.Unmarshal([]byte(result2.Content), &parsed); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if len(parsed) > 1 {
		t.Errorf("max_results=1 but got %d results", len(parsed))
	}
}

func TestMemorySearchTool_MinScoreFilter(t *testing.T) {
	store := indexTestMemoryFiles(t)
	tool := NewMemorySearchTool(store, nil, "")

	// minScore = 0.999（最高）→ 只保留与最高分几乎相同的结果或全部过滤。
	result, err := tool.Execute(context.Background(), map[string]interface{}{
		"query":     "ChromaDB",
		"min_score": float64(0.999),
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !result.Success {
		t.Fatalf("Execute not successful: %v", result.Error)
	}

	if strings.HasPrefix(result.Content, "No memory found") {
		t.Log("high min_score filtered all results (acceptable)")
		return
	}

	var parsed []map[string]interface{}
	if err := json.Unmarshal([]byte(result.Content), &parsed); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	for _, r := range parsed {
		score, _ := r["score"].(float64)
		if score < 0.999 {
			t.Errorf("result score %v below min_score 0.999", score)
		}
	}
}

func TestMemorySearchTool_NoMatch(t *testing.T) {
	store := indexTestMemoryFiles(t)
	tool := NewMemorySearchTool(store, nil, "")

	result, err := tool.Execute(context.Background(), map[string]interface{}{
		"query": "zzzzz quantum blockchain unicorn",
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !result.Success {
		t.Fatalf("Execute not successful: %v", result.Error)
	}
	if !strings.Contains(result.Content, "No memory found") {
		t.Logf("query returned some weak results: %s", result.Content)
	}
}

func TestParseVariations(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		maxN     int
		expected []string
	}{
		{
			name:     "standard format",
			content:  "- variation one\n- variation two\n- variation three\n- variation four\n- variation five",
			maxN:     4,
			expected: []string{"variation one", "variation two", "variation three", "variation four"},
		},
		{
			name:     "with prose around",
			content:  "Here are some variations:\n- first query\n\n- second query\nSome extra text\n- third query",
			maxN:     4,
			expected: []string{"first query", "second query", "third query"},
		},
		{
			name:     "empty lines skipped",
			content:  "- \n- valid one\n\n- valid two",
			maxN:     4,
			expected: []string{"valid one", "valid two"},
		},
		{
			name:     "no valid lines",
			content:  "No variations available.",
			maxN:     4,
			expected: nil,
		},
		{
			name:     "max variations respected",
			content:  "- a\n- b\n- c\n- d\n- e\n- f",
			maxN:     2,
			expected: []string{"a", "b"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseVariations(tt.content, tt.maxN)
			if len(got) != len(tt.expected) {
				t.Fatalf("got %d variations %v, want %d %v", len(got), got, len(tt.expected), tt.expected)
			}
			for i := range got {
				if got[i] != tt.expected[i] {
					t.Errorf("variation[%d] = %q, want %q", i, got[i], tt.expected[i])
				}
			}
		})
	}
}

func TestRRFAcrossQueries_VariationDecay(t *testing.T) {
	tool := NewMemorySearchTool(nil, nil, "")

	chunkA := SearchResult{ID: "A", Path: "a.md", Text: "chunk A"}
	chunkB := SearchResult{ID: "B", Path: "b.md", Text: "chunk B"}
	chunkC := SearchResult{ID: "C", Path: "c.md", Text: "chunk C"}

	// 原 query：A 排第一；变体 query：A 也排第一，B 排第二。
	queries := []string{"original", "variation"}
	results := [][]SearchResult{
		{chunkA, chunkB, chunkC}, // original
		{chunkA, chunkB},         // variation (decayed)
	}

	merged := tool.rrfAcrossQueries(queries, results)
	if len(merged) != 3 {
		t.Fatalf("merged %d results, want 3 (deduped)", len(merged))
	}

	// A 应排第一（两路都命中）。
	// B 第二（原 query rank2 + 变体 rank2 衰减）。
	// C 第三（只有原 query rank3）。
	scores := map[string]float64{}
	for _, r := range merged {
		scores[r.ID] = r.Score
	}
	if !(scores["A"] >= scores["B"] && scores["B"] >= scores["C"]) {
		t.Errorf("rank order wrong: A=%v B=%v C=%v", scores["A"], scores["B"], scores["C"])
	}
	if scores["A"] != 1.0 {
		t.Errorf("top result should be normalized to 1.0, got %v", scores["A"])
	}

	// 验证衰减确实生效：变体 rank1 的贡献 < 原 query rank1 的贡献。
	originalRank1 := 1.0 / float64(60+1)
	variationRank1 := 1.0 / float64(60+1+tool.variationPenalty)
	if variationRank1 >= originalRank1 {
		t.Errorf("variation penalty not applied: %v >= %v", variationRank1, originalRank1)
	}
	t.Logf("A=%v B=%v C=%v", scores["A"], scores["B"], scores["C"])
}

func TestMemorySearchTool_ChineseVariationPrompt(t *testing.T) {
	store := newTestFileStore()

	// 中文模式：LLM 返回中文变体，应被正常解析。
	mock := &fixedMockLLM{resp: &schema.LLMResponse{
		Content: "- 用户偏好哪种编程语言\n- Python 版本选择\n- 后端技术栈\n",
	}}
	tool := NewMemorySearchTool(store, mock, "zh")

	variations := tool.generateVariations(context.Background(), "用户喜欢什么语言")
	if len(variations) != 3 {
		t.Fatalf("got %d variations, want 3: %v", len(variations), variations)
	}
	if variations[0] != "用户偏好哪种编程语言" {
		t.Errorf("variation[0] = %q", variations[0])
	}
}
