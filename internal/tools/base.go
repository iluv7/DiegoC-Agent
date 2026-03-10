package tools

import "context"

// ToolResult is the result of a tool execution.
type ToolResult struct {
	Success bool   `json:"success"`
	Content string `json:"content"`
	Error   string `json:"error,omitempty"`
}

// Tool is the interface all tools implement.
type Tool interface {
	Name() string
	Description() string
	Parameters() map[string]interface{}
	Execute(ctx context.Context, args map[string]interface{}) (*ToolResult, error)
}

// ToAnthropicSchema returns the tool in Anthropic API format.
func ToAnthropicSchema(t Tool) map[string]interface{} {
	return map[string]interface{}{
		"name":         t.Name(),
		"description":  t.Description(),
		"input_schema": t.Parameters(),
	}
}

// ToOpenAISchema returns the tool in OpenAI API format.
func ToOpenAISchema(t Tool) map[string]interface{} {
	return map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{
			"name":        t.Name(),
			"description": t.Description(),
			"parameters": t.Parameters(),
		},
	}
}
