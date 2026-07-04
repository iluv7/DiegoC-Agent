package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ChromaClient 对接 ChromaDB HTTP API（OSS 版本）。
// 默认 endpoint: http://localhost:8000
type ChromaClient struct {
	endpoint   string
	httpClient *http.Client
	tenant     string
	database   string
}

// NewChromaClient 创建 ChromaDB HTTP 客户端。
// endpoint 示例: "http://localhost:8000"
func NewChromaClient(endpoint string) *ChromaClient {
	return &ChromaClient{
		endpoint: strings.TrimRight(endpoint, "/"),
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		tenant:   "default_tenant",
		database: "default_database",
	}
}

var _ VectorStore = (*ChromaClient)(nil)

// ---- VectorStore 实现 ----

// EnsureCollection 确保 collection 存在。
func (c *ChromaClient) EnsureCollection(ctx context.Context, name string) error {
	// 先查是否已存在
	cols, err := c.listCollections(ctx)
	if err != nil {
		return err
	}
	for _, col := range cols {
		if col.Name == name {
			return nil
		}
	}
	// 不存在则创建
	return c.createCollection(ctx, name)
}

// Upsert 插入/更新 chunks。
func (c *ChromaClient) Upsert(ctx context.Context, collection string, chunks []MemoryChunk, embeddings [][]float32) error {
	col, err := c.getCollection(ctx, collection)
	if err != nil {
		return err
	}

	ids := make([]string, len(chunks))
	documents := make([]string, len(chunks))
	metadatas := make([]map[string]interface{}, len(chunks))
	embeds := make([][]float32, len(chunks))

	for i, ch := range chunks {
		ids[i] = ch.ID
		documents[i] = ch.Text
		metadatas[i] = map[string]interface{}{
			"path":       ch.Path,
			"source":     ch.Source,
			"start_line": ch.StartLine,
			"end_line":   ch.EndLine,
			"hash":       ch.Hash,
		}
		if i < len(embeddings) {
			embeds[i] = embeddings[i]
		}
	}

	body := map[string]interface{}{
		"ids":        ids,
		"documents":  documents,
		"embeddings": embeds,
		"metadatas":  metadatas,
	}

	url := fmt.Sprintf("%s/api/v2/tenants/%s/databases/%s/collections/%s/upsert",
		c.endpoint, c.tenant, c.database, col.ID)
	_, err = c.doPost(ctx, url, body)
	return err
}

// Query 向量搜索。
func (c *ChromaClient) Query(ctx context.Context, collection string, queryEmbedding []float32, nResults int, where map[string]interface{}) ([]SearchResult, error) {
	if nResults <= 0 {
		nResults = 5
	}

	col, err := c.getCollection(ctx, collection)
	if err != nil {
		return nil, err
	}

	body := map[string]interface{}{
		"query_embeddings": [][]float32{queryEmbedding},
		"n_results":        nResults,
		"include":          []string{"documents", "metadatas", "distances"},
	}
	if where != nil {
		body["where_document"] = where
	}

	url := fmt.Sprintf("%s/api/v2/tenants/%s/databases/%s/collections/%s/query",
		c.endpoint, c.tenant, c.database, col.ID)

	resp, err := c.doPost(ctx, url, body)
	if err != nil {
		return nil, err
	}

	return c.parseQueryResults(resp), nil
}

// Delete 按 ID 删除 chunks。
func (c *ChromaClient) Delete(ctx context.Context, collection string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}

	col, err := c.getCollection(ctx, collection)
	if err != nil {
		return err
	}

	body := map[string]interface{}{
		"ids": ids,
	}

	url := fmt.Sprintf("%s/api/v2/tenants/%s/databases/%s/collections/%s/delete",
		c.endpoint, c.tenant, c.database, col.ID)
	_, err = c.doPost(ctx, url, body)
	return err
}

// ---- 内部 ----

type chromaCollection struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type chromaListResponse struct {
	Data []chromaCollection `json:"data"`
}

func (c *ChromaClient) listCollections(ctx context.Context) ([]chromaCollection, error) {
	url := fmt.Sprintf("%s/api/v2/tenants/%s/databases/%s/collections",
		c.endpoint, c.tenant, c.database)
	resp, err := c.doGet(ctx, url)
	if err != nil {
		return nil, err
	}
	var list chromaListResponse
	if err := json.Unmarshal(resp, &list); err != nil {
		return nil, fmt.Errorf("chroma: parse list collections: %w", err)
	}
	return list.Data, nil
}

func (c *ChromaClient) getCollection(ctx context.Context, name string) (*chromaCollection, error) {
	cols, err := c.listCollections(ctx)
	if err != nil {
		return nil, err
	}
	for _, col := range cols {
		if col.Name == name {
			return &col, nil
		}
	}
	return nil, fmt.Errorf("chroma: collection %q not found", name)
}

func (c *ChromaClient) createCollection(ctx context.Context, name string) error {
	url := fmt.Sprintf("%s/api/v2/tenants/%s/databases/%s/collections",
		c.endpoint, c.tenant, c.database)
	body := map[string]interface{}{
		"name": name,
		"metadata": map[string]interface{}{
			"description": "DiegoC-Agent memory index",
		},
	}
	_, err := c.doPost(ctx, url, body)
	return err
}

// ---- HTTP 请求 ----

func (c *ChromaClient) doGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("chroma: GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("chroma: read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("chroma: %s (status %d): %s", url, resp.StatusCode, string(data))
	}

	return data, nil
}

func (c *ChromaClient) doPost(ctx context.Context, url string, body interface{}) ([]byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("chroma: marshal body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("chroma: POST %s: %w", url, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("chroma: read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("chroma: %s (status %d): %s", url, resp.StatusCode, string(data))
	}

	return data, nil
}

// ---- 解析 ChromaDB 响应 ----

// chromaQueryResponse 对应 ChromaDB query API 的返回格式。
// 返回的是二维数组：外层是 query 数量（我们发 1 个），内层是 results。
type chromaQueryResponse struct {
	IDs       [][]string                 `json:"ids"`
	Documents [][]string                 `json:"documents"`
	Metadatas [][]map[string]interface{} `json:"metadatas"`
	Distances [][]float64                `json:"distances"`
}

func (c *ChromaClient) parseQueryResults(data []byte) []SearchResult {
	var resp chromaQueryResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil
	}

	if len(resp.IDs) == 0 {
		return nil
	}

	// 取第一个（也是唯一一个）query 的结果
	batchIDs := resp.IDs[0]
	batchDocs := safeGetDocBatch(resp.Documents, 0)
	batchMeta := safeGetMetaBatch(resp.Metadatas, 0)
	batchDist := safeGetDistBatch(resp.Distances, 0)

	results := make([]SearchResult, 0, len(batchIDs))
	for i, id := range batchIDs {
		r := SearchResult{
			ID:   id,
			Text: safeGet(batchDocs, i),
		}
		if i < len(batchMeta) {
			r.Path, _ = batchMeta[i]["path"].(string)
			r.StartLine, _ = toIntFromMeta(batchMeta[i]["start_line"])
			r.EndLine, _ = toIntFromMeta(batchMeta[i]["end_line"])
		}
		if i < len(batchDist) {
			r.Score = distanceToSimilarity(batchDist[i])
		}
		results = append(results, r)
	}
	return results
}

// ---- 辅助 ----

func safeGet(slice []string, i int) string {
	if i < len(slice) {
		return slice[i]
	}
	return ""
}

func safeGetDocBatch(slice [][]string, i int) []string {
	if i < len(slice) {
		return slice[i]
	}
	return nil
}

func safeGetMetaBatch(slice [][]map[string]interface{}, i int) []map[string]interface{} {
	if i < len(slice) {
		return slice[i]
	}
	return nil
}

func safeGetDistBatch(slice [][]float64, i int) []float64 {
	if i < len(slice) {
		return slice[i]
	}
	return nil
}

func toIntFromMeta(v interface{}) (int, bool) {
	switch x := v.(type) {
	case float64:
		return int(x), true
	case int:
		return x, true
	default:
		return 0, false
	}
}
