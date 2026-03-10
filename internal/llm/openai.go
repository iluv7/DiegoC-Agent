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

// OpenAIClient implements Client for OpenAI-compatible API.
type OpenAIClient struct {
	apiKey     string
	apiBase    string
	model      string
	httpClient *http.Client
	retry      RetryConfig
}

// NewOpenAIClient creates an OpenAI-compatible client.
func NewOpenAIClient(apiKey, apiBase, model string, retry RetryConfig) *OpenAIClient {
	return &OpenAIClient{
		apiKey:     apiKey,
		apiBase:    strings.TrimSuffix(apiBase, "/"),
		model:      model,
		httpClient: &http.Client{Timeout: 120 * time.Second},
		retry:      retry,
	}
}

func (c *OpenAIClient) url() string {
	return c.apiBase + "/chat/completions"
}

// Generate calls the API with retry.
func (c *OpenAIClient) Generate(ctx context.Context, messages []schema.Message, toolList []tools.Tool) (*schema.LLMResponse, error) {
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

func (c *OpenAIClient) doGenerate(ctx context.Context, messages []schema.Message, toolList []tools.Tool) (*schema.LLMResponse, error) {
	apiMessages := c.convertMessages(messages)
	body := map[string]interface{}{
		"model":    c.model,
		"messages": apiMessages,
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
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

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

func (c *OpenAIClient) convertMessages(messages []schema.Message) []map[string]interface{} {
	var out []map[string]interface{}
	for _, msg := range messages {
		m := map[string]interface{}{"role": msg.Role}
		if msg.Role == "assistant" && msg.Thinking != "" {
			m["reasoning_details"] = []map[string]interface{}{{"text": msg.Thinking}}
		}
		if msg.Role == "assistant" && len(msg.ToolCalls) > 0 {
			var tcs []map[string]interface{}
			for _, tc := range msg.ToolCalls {
				argBytes, _ := json.Marshal(tc.Function.Arguments)
				tcs = append(tcs, map[string]interface{}{
					"id": "tool_call_id", "type": "function",
					"function": map[string]interface{}{"name": tc.Function.Name, "arguments": string(argBytes)},
				})
			}
			m["tool_calls"] = tcs
		}
		if msg.Role == "tool" {
			m["tool_call_id"] = msg.ToolCallID
		}
		m["content"] = msg.Content
		out = append(out, m)
	}
	return out
}

func (c *OpenAIClient) convertTools(toolList []tools.Tool) []map[string]interface{} {
	if len(toolList) == 0 {
		return nil
	}
	out := make([]map[string]interface{}, 0, len(toolList))
	for _, t := range toolList {
		out = append(out, tools.ToOpenAISchema(t))
	}
	return out
}

type openAIChoice struct {
	Message struct {
		Content    string `json:"content"`
		ToolCalls  []struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
		ReasoningDetails []struct {
			Text string `json:"text"`
		} `json:"reasoning_details"`
	} `json:"message"`
	FinishReason string `json:"finish_reason"`
}

type openAIResponse struct {
	Choices []openAIChoice `json:"choices"`
	Usage   *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

func (c *OpenAIClient) parseResponse(data []byte) (*schema.LLMResponse, error) {
	var oar openAIResponse
	if err := json.Unmarshal(data, &oar); err != nil {
		return nil, err
	}
	if len(oar.Choices) == 0 {
		return nil, fmt.Errorf("no choices in response")
	}
	ch := oar.Choices[0]
	content := ch.Message.Content
	var thinking string
	for _, d := range ch.Message.ReasoningDetails {
		thinking += d.Text
	}
	var toolCalls []schema.ToolCall
	for _, tc := range ch.Message.ToolCalls {
		var args map[string]interface{}
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		toolCalls = append(toolCalls, schema.ToolCall{
			ID:   tc.ID,
			Type: "function",
			Function: schema.FunctionCall{
				Name:      tc.Function.Name,
				Arguments: args,
			},
		})
	}
	finish := ch.FinishReason
	if finish == "" {
		finish = "stop"
	}
	var usage *schema.TokenUsage
	if oar.Usage != nil {
		usage = &schema.TokenUsage{
			PromptTokens:     oar.Usage.PromptTokens,
			CompletionTokens: oar.Usage.CompletionTokens,
			TotalTokens:      oar.Usage.TotalTokens,
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
