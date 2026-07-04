package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
)

// MemoryChunk 是 markdown 文件的一个分段。
type MemoryChunk struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	Source    string `json:"source"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Text      string `json:"text"`
	Hash      string `json:"hash"`

	// Phase 8c: 时间元数据
	ConversationDate string `json:"conversation_date,omitempty"`
}

// SearchResult 是一次搜索的结果条目。
type SearchResult struct {
	ID        string  `json:"id"`
	Path      string  `json:"path"`
	StartLine int     `json:"start_line"`
	EndLine   int     `json:"end_line"`
	Text      string  `json:"text"`
	Score     float64 `json:"score"`

	// Phase 8c: 日期标签
	DateLabel string `json:"date_label,omitempty"`
}

// FileMeta 内存中的文件元数据缓存。
type FileMeta struct {
	Path   string   `json:"path"`
	Hash   string   `json:"hash"`
	Size   int64    `json:"size"`
	Chunks []string `json:"chunks"`
	// Phase 8c
	ConversationDate string `json:"conversation_date,omitempty"`
}

// Embedder 生成文本向量。
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)
}

// VectorStore 向量数据库后端（ChromaDB 默认实现）。
type VectorStore interface {
	EnsureCollection(ctx context.Context, name string) error
	Upsert(ctx context.Context, collection string, chunks []MemoryChunk, embeddings [][]float32) error
	Query(ctx context.Context, collection string, queryEmbedding []float32, nResults int, where map[string]interface{}) ([]SearchResult, error)
	Delete(ctx context.Context, collection string, ids []string) error
}

// FileStore 管理 markdown 记忆文件的索引与搜索。
// 向量走 ChromaDB，关键词走 SQLite FTS5 BM25，混合搜索走 RRF。
type FileStore struct {
	embedder    Embedder
	vecStore    VectorStore
	kwStore     KeywordStore
	counter     TokenCounter
	collection  string
	chunkTokens int
	overlap     int

	meta map[string]*FileMeta
	mu   sync.RWMutex
}

// NewFileStore 创建文件索引管理器。
func NewFileStore(embedder Embedder, vecStore VectorStore, kwStore KeywordStore, counter TokenCounter, collection string, chunkTokens, overlap int) *FileStore {
	if chunkTokens <= 0 {
		chunkTokens = 400
	}
	if overlap <= 0 {
		overlap = 80
	}
	return &FileStore{
		embedder:    embedder,
		vecStore:    vecStore,
		kwStore:     kwStore,
		counter:     counter,
		collection:  collection,
		chunkTokens: chunkTokens,
		overlap:     overlap,
		meta:        make(map[string]*FileMeta),
	}
}

// ---- 文件索引 ----

// IndexFile 分块 → embedding → 双写 ChromaDB + FTS5 → 更新缓存。
func (fs *FileStore) IndexFile(ctx context.Context, path, content string) error {
	return fs.IndexFileWithDate(ctx, path, content, "")
}

// IndexFileWithDate 同 IndexFile，但附加 conversation_date（Phase 8c）。
func (fs *FileStore) IndexFileWithDate(ctx context.Context, path, content, conversationDate string) error {
	// 删除旧 chunks
	fs.mu.RLock()
	old, exists := fs.meta[path]
	fs.mu.RUnlock()
	if exists && len(old.Chunks) > 0 {
		_ = fs.vecStore.Delete(ctx, fs.collection, old.Chunks)
		_ = fs.kwStore.Delete(ctx, old.Chunks)
	}

	// 分块（若有日期 → 注入每个 chunk）
	chunks := chunkMarkdown(content, path, path, fs.chunkTokens, fs.overlap, fs.counter)
	if len(chunks) == 0 {
		return nil
	}
	if conversationDate != "" {
		for i := range chunks {
			chunks[i].ConversationDate = conversationDate
		}
	}

	// 批量 embedding
	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = c.Text
	}
	embeddings, err := fs.embedder.EmbedBatch(ctx, texts)
	if err != nil {
		return fmt.Errorf("filestore: embed batch: %w", err)
	}

	// 确保后端就绪
	if err := fs.vecStore.EnsureCollection(ctx, fs.collection); err != nil {
		return fmt.Errorf("filestore: ensure vector collection: %w", err)
	}
	if err := fs.kwStore.EnsureIndex(ctx); err != nil {
		return fmt.Errorf("filestore: ensure fts5 index: %w", err)
	}

	// 双写
	if err := fs.vecStore.Upsert(ctx, fs.collection, chunks, embeddings); err != nil {
		return fmt.Errorf("filestore: vector upsert: %w", err)
	}
	if err := fs.kwStore.IndexChunks(ctx, chunks); err != nil {
		return fmt.Errorf("filestore: fts5 index: %w", err)
	}

	// 更新缓存
	chunkIDs := make([]string, len(chunks))
	for i, c := range chunks {
		chunkIDs[i] = c.ID
	}
	fs.mu.Lock()
	fs.meta[path] = &FileMeta{
		Path:             path,
		Hash:             contentHash(content),
		Size:             int64(len(content)),
		Chunks:           chunkIDs,
		ConversationDate: conversationDate,
	}
	fs.mu.Unlock()

	return nil
}

// RemoveFile 从索引中删除文件（双后端）。
func (fs *FileStore) RemoveFile(ctx context.Context, path string) error {
	fs.mu.Lock()
	meta, exists := fs.meta[path]
	if exists {
		delete(fs.meta, path)
	}
	fs.mu.Unlock()

	if exists && len(meta.Chunks) > 0 {
		if err := fs.vecStore.Delete(ctx, fs.collection, meta.Chunks); err != nil {
			return err
		}
		if err := fs.kwStore.Delete(ctx, meta.Chunks); err != nil {
			return err
		}
	}
	return nil
}

// HasChanged 检查文件是否变更。
func (fs *FileStore) HasChanged(path, content string) bool {
	fs.mu.RLock()
	meta, exists := fs.meta[path]
	fs.mu.RUnlock()
	if !exists {
		return true
	}
	return meta.Hash != contentHash(content)
}

// GetFileMeta 返回文件缓存。
func (fs *FileStore) GetFileMeta(path string) *FileMeta {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	m, _ := fs.meta[path]
	return m
}

// ---- 搜索 ----

// VectorSearch 纯向量语义搜索。
func (fs *FileStore) VectorSearch(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 5
	}
	vec, err := fs.embedder.Embed(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("filestore: embed query: %w", err)
	}
	return fs.vecStore.Query(ctx, fs.collection, vec, limit, nil)
}

// KeywordSearch 纯 BM25 关键词搜索。
func (fs *FileStore) KeywordSearch(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 5
	}
	return fs.kwStore.Search(ctx, query, limit)
}

// HybridSearch RRF 混合搜索：向量 + BM25 关键词。
// 两路各取 limit*3 候选，RRF 合并排名，取 top-N。
func (fs *FileStore) HybridSearch(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 5
	}
	fetchLimit := limit * 3

	// 并行搜索
	var vecResults, kwResults []SearchResult
	var vecErr, kwErr error
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		vec, err := fs.embedder.Embed(ctx, query)
		if err != nil {
			vecErr = err
			return
		}
		vecResults, vecErr = fs.vecStore.Query(ctx, fs.collection, vec, fetchLimit, nil)
	}()

	go func() {
		defer wg.Done()
		kwResults, kwErr = fs.kwStore.Search(ctx, query, fetchLimit)
	}()

	wg.Wait()

	// 一路失败 → 退回另一路
	if vecErr != nil && kwErr != nil {
		return nil, fmt.Errorf("filestore: hybrid both failed: vec=%v kw=%v", vecErr, kwErr)
	}
	if vecErr != nil {
		return kwResults, nil
	}
	if kwErr != nil {
		return vecResults, nil
	}

	// RRF 合并
	merged := rrfMerge(vecResults, kwResults, 60)

	// 排序取 top-N
	sort.Slice(merged, func(i, j int) bool {
		return merged[i].Score > merged[j].Score
	})
	if len(merged) > limit {
		merged = merged[:limit]
	}

	return merged, nil
}

// ---- RRF 合并 ----

// rrfMerge 用 Reciprocal Rank Fusion 合并两个排名列表。
// k=60 是标准值（论文默认）。
// score(d) = sum_i 1/(k + rank_i(d))
func rrfMerge(ranked1, ranked2 []SearchResult, k int) []SearchResult {
	// 用 merge_key 去重（ID + path）
	byKey := make(map[string]*SearchResult)

	addList := func(ranked []SearchResult, listIdx int) {
		for rank, r := range ranked {
			key := r.ID
			if key == "" {
				key = fmt.Sprintf("%s:%d-%d", r.Path, r.StartLine, r.EndLine)
			}
			if existing, ok := byKey[key]; ok {
				existing.Score += 1.0 / float64(k+rank+1)
			} else {
				cp := r
				cp.Score = 1.0 / float64(k+rank+1)
				byKey[key] = &cp
			}
			_ = listIdx
		}
	}

	addList(ranked1, 0)
	addList(ranked2, 1)

	results := make([]SearchResult, 0, len(byKey))
	for _, r := range byKey {
		results = append(results, *r)
	}
	return results
}

// ---- 分块 ----

// chunkMarkdown 把 markdown 文本按 token 数分块。
func chunkMarkdown(text, path, source string, chunkTokens, overlap int, counter TokenCounter) []MemoryChunk {
	if text == "" {
		return nil
	}

	charsPerToken := 4
	maxChars := chunkTokens * charsPerToken
	overlapChars := overlap * charsPerToken

	lines := strings.Split(text, "\n")
	chunks := make([]MemoryChunk, 0)

	currentStart := 1
	currentLines := make([]string, 0)
	currentChars := 0

	flush := func(endLine int) {
		content := strings.Join(currentLines, "\n")
		if strings.TrimSpace(content) == "" {
			currentLines = nil
			currentChars = 0
			currentStart = endLine + 1
			return
		}
		chunk := MemoryChunk{
			ID:        chunkID(path, len(chunks)),
			Path:      path,
			Source:    source,
			StartLine: currentStart,
			EndLine:   endLine,
			Text:      content,
			Hash:      contentHash(content),
		}
		chunks = append(chunks, chunk)

		if overlapChars > 0 && len(currentLines) > 0 {
			overlapText := ""
			overlapLines := make([]string, 0)
			for i := len(currentLines) - 1; i >= 0; i-- {
				line := currentLines[i]
				if len(overlapText)+len(line)+1 > overlapChars {
					break
				}
				overlapLines = append([]string{line}, overlapLines...)
				overlapText += line + "\n"
			}
			currentLines = overlapLines
			currentChars = len(overlapText)
			if len(overlapLines) > 0 {
				currentStart = endLine - len(overlapLines) + 1
			}
		} else {
			currentLines = nil
			currentChars = 0
			currentStart = endLine + 1
		}
	}

	for i, line := range lines {
		lineNum := i + 1
		lineChars := len(line) + 1

		if currentChars+lineChars > maxChars && len(currentLines) > 0 {
			flush(lineNum - 1)
		}

		currentLines = append(currentLines, line)
		currentChars += lineChars
	}

	if len(currentLines) > 0 {
		flush(len(lines))
	}

	return chunks
}

// ---- 工具函数 ----

func chunkID(path string, index int) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", path, index)))
	return hex.EncodeToString(h[:])[:16]
}

func contentHash(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])[:16]
}

func distanceToSimilarity(distance float64) float64 {
	return 1.0 / (1.0 + distance)
}

func clamp(v, minV, maxV float64) float64 {
	return math.Max(minV, math.Min(maxV, v))
}

// ---- Phase 8c: Frontmatter 解析 ----

// ParseFrontmatter 解析 YAML-style 简易 frontmatter。
// 只提取 conversation_date 字段。
func ParseFrontmatter(content string) (conversationDate string, body string) {
	if !strings.HasPrefix(content, "---\n") {
		return "", content
	}
	end := strings.Index(content[4:], "\n---\n")
	if end == -1 {
		return "", content
	}
	fm := content[4 : 4+end]
	body = content[4+end+5:] // skip "---\n" + fm + "\n---\n"

	for _, line := range strings.Split(fm, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "conversation_date:") {
			conversationDate = strings.TrimSpace(strings.TrimPrefix(line, "conversation_date:"))
		}
	}
	return conversationDate, body
}
