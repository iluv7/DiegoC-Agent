package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

// AnthropicClient implements Client for Anthropic-compatible API.
type AnthropicClient struct {
	apiKey     string
	apiBase    string
	model      string
	httpClient *http.Client
	retry      RetryConfig
}

// NewAnthropicClient creates an Anthropic-compatible client.
func NewAnthropicClient(apiKey, apiBase, model string, retry RetryConfig) *AnthropicClient {
	return &AnthropicClient{
		apiKey:     apiKey,
		apiBase:    strings.TrimSuffix(apiBase, "/"),
		model:      model,
		httpClient: &http.Client{Timeout: 120 * time.Second},
		retry:      retry,
	}
}

func (c *AnthropicClient) url() string {
	return c.apiBase + "/v1/messages"
}

// Generate calls the API with retry and returns parsed response.
func (c *AnthropicClient) Generate(ctx context.Context, messages []schema.Message, toolList []tools.Tool) (*schema.LLMResponse, error) {
	var resp *schema.LLMResponse
	err := DoWithRetry(ctx, c.retry, func() error {
		var e error
		resp, e = c.doGenerate(ctx, messages, toolList)
		return e
	}, nil)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *AnthropicClient) doGenerate(ctx context.Context, messages []schema.Message, toolList []tools.Tool) (*schema.LLMResponse, error) {
	system, apiMessages := c.convertMessages(messages)
	body := map[string]interface{}{
		"model":      c.model,
		"max_tokens": 16384,
		"messages":   apiMessages,
	}
	if system != "" {
		body["system"] = system
	}
	if len(toolList) > 0 {
		body["tools"] = c.convertTools(toolList)
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(), bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	httpResp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()
	data, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, err
	}
	if httpResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("api error %d: %s", httpResp.StatusCode, string(data))
	}
	return c.parseResponse(data)
}

func (c *AnthropicClient) convertMessages(messages []schema.Message) (system string, apiMessages []map[string]interface{}) {
	for _, msg := range messages {
		if msg.Role == "system" {
			if s, ok := msg.Content.(string); ok {
				system = s
			}
			continue
		}
		if msg.Role == "user" || msg.Role == "assistant" {
			if msg.Role == "assistant" && (msg.Thinking != "" || len(msg.ToolCalls) > 0) {
				blocks := []map[string]interface{}{}
				if msg.Thinking != "" {
					blocks = append(blocks, map[string]interface{}{"type": "thinking", "thinking": msg.Thinking})
				}
				if msg.Content != nil {
					if s, ok := msg.Content.(string); ok && s != "" {
						blocks = append(blocks, map[string]interface{}{"type": "text", "text": s})
					}
				}
				for _, tc := range msg.ToolCalls {
					blocks = append(blocks, map[string]interface{}{
						"type":  "tool_use",
						"id":    tc.ID,
						"name":  tc.Function.Name,
						"input": tc.Function.Arguments,
					})
				}
				apiMessages = append(apiMessages, map[string]interface{}{"role": "assistant", "content": blocks})
			} else {
				apiMessages = append(apiMessages, map[string]interface{}{"role": msg.Role, "content": msg.Content})
			}
			continue
		}
		if msg.Role == "tool" {
			apiMessages = append(apiMessages, map[string]interface{}{
				"role": "user",
				"content": []map[string]interface{}{
					{"type": "tool_result", "tool_use_id": msg.ToolCallID, "content": msg.Content},
				},
			})
		}
	}
	return system, apiMessages
}

func (c *AnthropicClient) convertTools(toolList []tools.Tool) []map[string]interface{} {
	if len(toolList) == 0 {
		return nil
	}
	out := make([]map[string]interface{}, 0, len(toolList))
	for _, t := range toolList {
		out = append(out, tools.ToAnthropicSchema(t))
	}
	return out
}

type anthropicResponse struct {
	Content []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		Thinking string          `json:"thinking"`
		ID       string          `json:"id"`
		Name     string          `json:"name"`
		Input    json.RawMessage `json:"input"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func (c *AnthropicClient) parseResponse(data []byte) (*schema.LLMResponse, error) {
	var ar anthropicResponse
	if err := json.Unmarshal(data, &ar); err != nil {
		return nil, err
	}
	var content, thinking string
	var toolCalls []schema.ToolCall
	for _, block := range ar.Content {
		switch block.Type {
		case "text":
			content += block.Text
		case "thinking":
			thinking += block.Thinking
		case "tool_use":
			args := make(map[string]interface{})
			if len(block.Input) > 0 {
				_ = json.Unmarshal(block.Input, &args)
			}
			toolCalls = append(toolCalls, schema.ToolCall{
				ID:   block.ID,
				Type: "function",
				Function: schema.FunctionCall{
					Name:      block.Name,
					Arguments: args,
				},
			})
		}
	}
	finish := ar.StopReason
	if finish == "" {
		finish = "end_turn"
	}
	var usage *schema.TokenUsage
	if ar.Usage != nil {
		usage = &schema.TokenUsage{
			PromptTokens:     ar.Usage.InputTokens,
			CompletionTokens: ar.Usage.OutputTokens,
			TotalTokens:      ar.Usage.InputTokens + ar.Usage.OutputTokens,
		}
	}
	return &schema.LLMResponse{
		Content:      content,
		Thinking:     thinking,
		ToolCalls:    toolCalls,
		FinishReason: finish,
		Usage:        usage,
	}, nil
}
