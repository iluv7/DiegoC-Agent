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

// OpenAIEmbeddingClient 调用 OpenAI 兼容的 embeddings API。
// 兼容 deepseek、minimax、openai 等厂商。
type OpenAIEmbeddingClient struct {
	apiKey  string
	apiBase string
	model   string
	client  *http.Client
}

// NewOpenAIEmbeddingClient 创建 embedding 客户端。
// apiBase 示例: "https://api.deepseek.com"
// model 示例: "text-embedding-v4"（按厂商不同）
func NewOpenAIEmbeddingClient(apiKey, apiBase, model string) *OpenAIEmbeddingClient {
	apiBase = strings.TrimRight(apiBase, "/")
	return &OpenAIEmbeddingClient{
		apiKey:  apiKey,
		apiBase: apiBase,
		model:   model,
		client: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

var _ Embedder = (*OpenAIEmbeddingClient)(nil)

// Embed 生成单个文本的 embedding。
func (c *OpenAIEmbeddingClient) Embed(ctx context.Context, text string) ([]float32, error) {
	vecs, err := c.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, fmt.Errorf("embed: empty response")
	}
	return vecs[0], nil
}

// EmbedBatch 批量生成 embeddings。
func (c *OpenAIEmbeddingClient) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	body := map[string]interface{}{
		"model": c.model,
		"input": texts,
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("embed: marshal: %w", err)
	}

	url := c.apiBase + "/embeddings"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed: request: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("embed: read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("embed: %s (status %d): %s", url, resp.StatusCode, string(data))
	}

	return parseEmbeddingResponse(data)
}

// ---- 解析 OpenAI 格式响应 ----

type embeddingResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
}

func parseEmbeddingResponse(data []byte) ([][]float32, error) {
	var resp embeddingResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("embed: parse response: %w", err)
	}

	// 按 index 排序
	vecs := make([][]float32, len(resp.Data))
	for _, d := range resp.Data {
		if d.Index >= 0 && d.Index < len(vecs) {
			vecs[d.Index] = d.Embedding
		}
	}
	return vecs, nil
}
