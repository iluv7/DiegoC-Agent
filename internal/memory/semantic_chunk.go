package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// SemanticChunk 按 embedding 相似度突变点确定 chunk 边界。
//
// 算法：
//  1. 按行拆分 → 逐行 embedding
//  2. 计算相邻行 cosine 相似度
//  3. 取 25 百分位为自适应阈值
//  4. 相似度低于阈值处切开
//  5. 每个组拼成一个 chunk
//
// 返回的 chunk 数量通常比固定大小切分更少、更语义连贯。
// 成本：每行一次 embedding 调用。
func SemanticChunk(ctx context.Context, text, path, source string, embedder Embedder) ([]MemoryChunk, error) {
	lines := strings.Split(text, "\n")
	if len(lines) == 0 {
		return nil, nil
	}

	// 过滤纯空行（保留结构）
	type indexedLine struct {
		idx  int
		text string
	}
	var validLines []indexedLine
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			validLines = append(validLines, indexedLine{idx: i, text: trimmed})
		}
	}
	if len(validLines) == 0 {
		return nil, nil
	}

	// 逐行 embedding
	lineTexts := make([]string, len(validLines))
	for i, vl := range validLines {
		lineTexts[i] = vl.text
	}
	embeddings, err := embedder.EmbedBatch(ctx, lineTexts)
	if err != nil {
		return nil, fmt.Errorf("semanticchunk: embed lines: %w", err)
	}

	// 计算相邻行相似度
	similarities := make([]float64, len(embeddings)-1)
	for i := 0; i < len(embeddings)-1; i++ {
		similarities[i] = cosineSimilarity64(embeddings[i], embeddings[i+1])
	}

	// 取 25 百分位为切割阈值
	threshold := percentile25(similarities)

	// 按阈值切割
	breakPoints := make(map[int]bool) // 在 validLines[i] 后切割（即 validLines[i+1] 开始新 chunk）
	for i, sim := range similarities {
		if sim < threshold {
			breakPoints[i] = true
		}
	}

	// 按 breakPoints 分组
	chunks := make([]MemoryChunk, 0)
	groupStart := 0
	for i := 0; i < len(validLines); i++ {
		if breakPoints[i] || i == len(validLines)-1 {
			// 收集 groupStart..i 的行
			groupLines := make([]string, 0)
			for j := groupStart; j <= i; j++ {
				groupLines = append(groupLines, validLines[j].text)
			}
			content := strings.Join(groupLines, "\n")
			startLine := validLines[groupStart].idx + 1
			endLine := validLines[i].idx + 1

			chunk := MemoryChunk{
				ID:        chunkID(path, len(chunks)),
				Path:      path,
				Source:    source,
				StartLine: startLine,
				EndLine:   endLine,
				Text:      content,
				Hash:      contentHash(content),
			}
			chunks = append(chunks, chunk)
			groupStart = i + 1
		}
	}

	return chunks, nil
}

// percentile25 取 float64 切片的 25 百分位（用于自适应阈值）。
func percentile25(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := make([]float64, len(values))
	copy(sorted, values)
	sort.Float64s(sorted)
	idx := int(float64(len(sorted)) * 0.25)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// cosineSimilarity64 计算两个 float32 向量的 cosine 相似度（float64 精度）。
func cosineSimilarity64(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
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
