package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"diegoc-agent/internal/llm"
	"diegoc-agent/internal/permission"
	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

// MemorySearchTool 记忆搜索工具。LLM 可调用它来检索长期记忆文件。
//
// 流程（对应 ReMe 的 MemorySearch / multi-query search）：
//  1. 同步调一次 LLM 生成 4 个语义变体子 query（失败则退化为单 query 搜索）
//  2. 原 query + 4 变体 = 5 个 query 并发调 FileStore.HybridSearchWithDate
//  3. 跨子 query RRF 合并：原 query 全权重，变体统一衰减 30（rank +30）
//  4. 去重 → 排序 → minScore 过滤 → top-N
//
// 返回 JSON：{path, start_line, end_line, score, snippet}
type MemorySearchTool struct {
	fileStore *FileStore
	llmClient llm.Client // 可为 nil → 跳过 query 变体生成
	language  string     // "zh" 中文，空字符串英文

	// maxVariations 生成变体子 query 的数量（默认 4）。
	maxVariations int
	// variationPenalty 变体子 query 在 RRF 合并时的 rank 衰减（默认 30）。
	variationPenalty int
}

// NewMemorySearchTool 创建记忆搜索工具。
// llmClient 为 nil 时跳过语义变体生成，只做原 query 搜索。
func NewMemorySearchTool(fileStore *FileStore, llmClient llm.Client, language string) *MemorySearchTool {
	return &MemorySearchTool{
		fileStore:        fileStore,
		llmClient:        llmClient,
		language:         language,
		maxVariations:    4,
		variationPenalty: 30,
	}
}

var _ tools.Tool = (*MemorySearchTool)(nil)

// ---- tools.Tool 接口 ----

func (t *MemorySearchTool) Name() string { return "memory_search" }

func (t *MemorySearchTool) Description() string {
	return "Search long-term memory files for information relevant to the current conversation. " +
		"Use this to recall past decisions, user preferences, project context, and facts recorded " +
		"in previous sessions. Returns ranked results with file path, line numbers, score, and snippet."
}

func (t *MemorySearchTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"query": map[string]interface{}{
				"type":        "string",
				"description": "The search question or keywords to look up in memory",
			},
			"max_results": map[string]interface{}{
				"type":        "integer",
				"description": "Maximum number of results to return (1-100, default 5)",
			},
			"min_score": map[string]interface{}{
				"type":        "number",
				"description": "Minimum relevance score to include a result (0.001-0.999, default 0.1)",
			},
		},
		"required": []interface{}{"query"},
	}
}

func (t *MemorySearchTool) CheckPermissions(_ map[string]interface{}, _ *permission.Context) permission.Decision {
	return permission.Decision{Behavior: permission.BehaviorPASSTHROUGH} // 纯读，永远安全
}

func (t *MemorySearchTool) IsConcurrencySafe() bool { return true }
func (t *MemorySearchTool) IsReadOnly() bool        { return true }
func (t *MemorySearchTool) IsExternalTool() bool    { return false }

// ---- Execute ----

// Execute 执行记忆搜索。
// 参数：
//
//	query      (required) 搜索问题
//	maxResults 1~100，默认 5
//	minScore   0.001~0.999，默认 0.1
func (t *MemorySearchTool) Execute(ctx context.Context, args map[string]interface{}) (*tools.ToolResult, error) {
	if t.fileStore == nil {
		return &tools.ToolResult{Success: false, Error: "memory search is not available: no file store configured"}, nil
	}

	// ---- 参数解析与校验 ----
	query, _ := args["query"].(string)
	if strings.TrimSpace(query) == "" {
		return &tools.ToolResult{Success: false, Error: "query is required"}, nil
	}

	maxResults := 5
	if v, ok := args["max_results"].(float64); ok {
		maxResults = int(v)
	}
	if maxResults < 1 {
		maxResults = 1
	}
	if maxResults > 100 {
		maxResults = 100
	}

	minScore := 0.1
	if v, ok := args["min_score"].(float64); ok {
		minScore = v
	}
	if minScore < 0.001 {
		minScore = 0.001
	}
	if minScore > 0.999 {
		minScore = 0.999
	}

	// ---- Step 1: LLM 生成语义变体子 query（best-effort）----
	queries := []string{query}
	queries = append(queries, t.generateVariations(ctx, query)...)

	// ---- Step 2: 并发搜索 ----
	// 每个子 query 取 maxResults*3 候选，供 RRF 合并。
	fetchLimit := maxResults * 3
	resultsByQuery := make([][]SearchResult, len(queries))

	var wg sync.WaitGroup
	for i, q := range queries {
		wg.Add(1)
		go func(idx int, qq string) {
			defer wg.Done()
			res, err := t.fileStore.HybridSearchWithDate(ctx, qq, fetchLimit)
			if err == nil {
				resultsByQuery[idx] = res
			}
			// 搜索失败 → 该路结果留空，不影响其他路
		}(i, q)
	}
	wg.Wait()

	// ---- Step 3: 跨子 query RRF 合并 ----
	// queries[0] 是原 query（全权重），其余是变体（rank +30 衰减）。
	merged := t.rrfAcrossQueries(queries, resultsByQuery)

	// ---- Step 4: 去重已完成（map 键）→ 排序 → 过滤 → top-N ----
	sort.Slice(merged, func(i, j int) bool {
		return merged[i].Score > merged[j].Score
	})

	filtered := make([]SearchResult, 0, len(merged))
	for _, r := range merged {
		if r.Score < minScore {
			continue
		}
		filtered = append(filtered, r)
		if len(filtered) >= maxResults {
			break
		}
	}

	// ---- Step 5: JSON 输出 ----
	content := formatSearchResults(filtered, query)
	return &tools.ToolResult{Success: true, Content: content}, nil
}

// ---- LLM 变体生成 ----

// generateVariations 调 LLM 生成语义变体子 query。
// 任何失败（llmClient 为 nil、LLM 报错、解析为空）都返回空切片 —
// 调用方退化为原 query 单路搜索。
func (t *MemorySearchTool) generateVariations(ctx context.Context, query string) []string {
	if t.llmClient == nil {
		return nil
	}

	msgs := []schema.Message{
		{Role: "system", Content: t.variationSystemPrompt()},
		{Role: "user", Content: t.variationUserPrompt(query)},
	}

	resp, err := t.llmClient.Generate(ctx, msgs, nil)
	if err != nil {
		return nil
	}

	return parseVariations(resp.Content, t.maxVariations)
}

func (t *MemorySearchTool) variationSystemPrompt() string {
	if t.language == "zh" {
		return "你是搜索查询重写助手。为给定问题生成语义变体查询，提高检索召回率。"
	}
	return "You are a search query rewriting assistant. Generate semantic variations of the given question to improve retrieval recall."
}

func (t *MemorySearchTool) variationUserPrompt(query string) string {
	if t.language == "zh" {
		return fmt.Sprintf(
			"为下面的问题生成 %d 个搜索查询变体。每个变体应使用不同的措辞、"+
				"聚焦不同的实体、或从不同角度切入。每行输出一个查询，以 \"- \" 开头。\n\n"+
				"问题: %s\n\n查询:", t.maxVariations, query)
	}
	return fmt.Sprintf(
		"Generate %d search query variations for the question below. Each variation "+
			"should use different wording, focus on different entities, or approach from "+
			"a different angle. Output one query per line, starting with \"- \".\n\n"+
			"Question: %s\n\nQueries:", t.maxVariations, query)
}

// parseVariations 从 LLM 输出中解析以 "- " 开头的变体查询行。
// 最多取 maxVariations 个，忽略空行和多余输出。
func parseVariations(content string, maxVariations int) []string {
	variations := make([]string, 0, maxVariations)
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		v := strings.TrimSpace(strings.TrimPrefix(line, "- "))
		if v == "" {
			continue
		}
		variations = append(variations, v)
		if len(variations) >= maxVariations {
			break
		}
	}
	return variations
}

// ---- 跨 query RRF 合并 ----

// rrfAcrossQueries 用 RRF 合并多个 query 的搜索结果。
// queries[0] 为原 query，全权重：score += 1/(60 + rank + 1)
// 其余变体统一衰减 variationPenalty：score += 1/(60 + rank + 1 + penalty)
// 按 chunk ID 去重（同一 chunk 出现在多路结果时分数累加）。
// 合并后分数归一化到 [0,1]（最高分 = 1.0），使 minScore 过滤有意义。
func (t *MemorySearchTool) rrfAcrossQueries(queries []string, resultsByQuery [][]SearchResult) []SearchResult {
	const k = 60

	byKey := make(map[string]*SearchResult)

	for qi, ranked := range resultsByQuery {
		penalty := 0
		if qi > 0 {
			penalty = t.variationPenalty
		}
		for rank, r := range ranked {
			key := r.ID
			if key == "" {
				key = fmt.Sprintf("%s:%d-%d", r.Path, r.StartLine, r.EndLine)
			}
			score := 1.0 / float64(k+rank+1+penalty)
			if existing, ok := byKey[key]; ok {
				existing.Score += score
			} else {
				cp := r
				cp.Score = score
				byKey[key] = &cp
			}
		}
	}

	// 归一化：最高分 = 1.0
	maxScore := 0.0
	for _, r := range byKey {
		if r.Score > maxScore {
			maxScore = r.Score
		}
	}

	merged := make([]SearchResult, 0, len(byKey))
	for _, r := range byKey {
		if maxScore > 0 {
			r.Score = r.Score / maxScore
		}
		merged = append(merged, *r)
	}
	return merged
}

// ---- 输出格式化 ----

// formatSearchResults 把搜索结果格式化为 JSON 字符串。
// 字段：path, start_line, end_line, score, snippet。
func formatSearchResults(results []SearchResult, query string) string {
	type resultJSON struct {
		Path      string  `json:"path"`
		StartLine int     `json:"start_line"`
		EndLine   int     `json:"end_line"`
		Score     float64 `json:"score"`
		Snippet   string  `json:"snippet"`
		Date      string  `json:"date,omitempty"`
	}

	out := make([]resultJSON, 0, len(results))
	for _, r := range results {
		snippet := r.Text
		if len([]rune(snippet)) > 300 {
			runes := []rune(snippet)
			snippet = string(runes[:300]) + "..."
		}
		out = append(out, resultJSON{
			Path:      r.Path,
			StartLine: r.StartLine,
			EndLine:   r.EndLine,
			Score:     roundScore(r.Score),
			Snippet:   snippet,
			Date:      r.DateLabel,
		})
	}

	if len(out) == 0 {
		return fmt.Sprintf("No memory found matching query: %q", query)
	}

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Sprintf("Found %d results but failed to serialize: %v", len(out), err)
	}
	return string(data)
}

// roundScore 把分数四舍五入到 4 位小数，减少输出噪音。
func roundScore(score float64) float64 {
	return float64(int(score*10000+0.5)) / 10000
}
