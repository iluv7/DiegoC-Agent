package schema

// LLMProvider is the API provider type.
type LLMProvider string

const (
	ProviderAnthropic LLMProvider = "anthropic"
	ProviderOpenAI    LLMProvider = "openai"
)

// FunctionCall holds tool call function name and arguments.
type FunctionCall struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

// ToolCall is one tool invocation from the model.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// Message is a chat message (system, user, assistant, tool).
type Message struct {
	Role       string      `json:"role"`
	Content    interface{} `json:"content,omitempty"` // string or []ContentBlock
	Thinking   string      `json:"thinking,omitempty"`
	ToolCalls  []ToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string      `json:"tool_call_id,omitempty"`
	Name       string      `json:"name,omitempty"`
}

// TokenUsage from API response.
type TokenUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens     int `json:"total_tokens"`
}

// LLMResponse is the parsed API response.
type LLMResponse struct {
	Content      string      `json:"content"`
	Thinking     string      `json:"thinking,omitempty"`
	ToolCalls    []ToolCall  `json:"tool_calls,omitempty"`
	FinishReason string      `json:"finish_reason"`
	Usage        *TokenUsage `json:"usage,omitempty"`
}
