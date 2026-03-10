package agent

import (
	"context"
	"encoding/json"

	"diegoc-agent/internal/llm"
	"diegoc-agent/internal/logger"
	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

// Agent runs the conversation loop with optional tools.
type Agent struct {
	LLM                 llm.Client
	SystemPrompt        string
	Messages            []schema.Message
	MaxSteps            int
	TokenLimit          int
	Tools               []tools.Tool
	toolByName          map[string]tools.Tool
	APITotalTokens      int
	skipNextTokenCheck  bool
	Logger *logger.AgentLogger // optional; when set, logs each run to ~/.diegoc-agent/log/
}

// New creates an Agent with system prompt and tools.
func New(client llm.Client, systemPrompt string, maxSteps int, tokenLimit int, toolList []tools.Tool) *Agent {
	if maxSteps <= 0 {
		maxSteps = 50
	}
	if tokenLimit <= 0 {
		tokenLimit = 80000
	}
	msgs := []schema.Message{
		{Role: "system", Content: systemPrompt},
	}
	byName := make(map[string]tools.Tool)
	for _, t := range toolList {
		byName[t.Name()] = t
	}
	return &Agent{
		LLM:          client,
		SystemPrompt: systemPrompt,
		Messages:     msgs,
		MaxSteps:     maxSteps,
		TokenLimit:   tokenLimit,
		Tools:        toolList,
		toolByName:   byName,
	}
}

// AddUserMessage appends a user message.
func (a *Agent) AddUserMessage(content string) {
	a.Messages = append(a.Messages, schema.Message{Role: "user", Content: content})
}

// Run executes the loop until the model returns end_turn (no tool calls) or context is cancelled.
func (a *Agent) Run(ctx context.Context) (string, error) {
	if a.Logger != nil {
		a.Logger.StartNewRun()
	}
	toolNames := make([]string, 0, len(a.Tools))
	for _, t := range a.Tools {
		toolNames = append(toolNames, t.Name())
	}
	for step := 0; step < a.MaxSteps; step++ {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		if err := a.summarizeIfNeeded(ctx); err != nil {
			return "", err
		}

		if a.Logger != nil {
			a.Logger.LogRequest(a.Messages, toolNames)
		}
		resp, err := a.LLM.Generate(ctx, a.Messages, a.Tools)
		if err != nil {
			return "", err
		}

		if resp.Usage != nil {
			a.APITotalTokens += resp.Usage.TotalTokens
		}
		if a.Logger != nil {
			a.Logger.LogResponse(resp.Content, resp.Thinking, resp.ToolCalls, resp.FinishReason)
		}
		a.Messages = append(a.Messages, schema.Message{
			Role:      "assistant",
			Content:   resp.Content,
			Thinking:  resp.Thinking,
			ToolCalls: resp.ToolCalls,
		})

		if len(resp.ToolCalls) == 0 {
			return resp.Content, nil
		}

		// Execute each tool call and append tool result messages
		for _, tc := range resp.ToolCalls {
			select {
			case <-ctx.Done():
				a.cleanupIncompleteMessages()
				return "", ctx.Err()
			default:
			}

			args := toolArgsFromRaw(tc.Function.Arguments)
			if args == nil {
				args = make(map[string]interface{})
			}

			tool := a.toolByName[tc.Function.Name]
			var result *tools.ToolResult
			if tool == nil {
				result = &tools.ToolResult{Success: false, Error: "Unknown tool: " + tc.Function.Name}
			} else {
				var errExec error
				result, errExec = tool.Execute(ctx, args)
				if errExec != nil {
					result = &tools.ToolResult{Success: false, Error: errExec.Error()}
				}
			}

			content := result.Content
			if !result.Success {
				content = "Error: " + result.Error
			}
			if a.Logger != nil {
				a.Logger.LogToolResult(tc.Function.Name, args, result.Success, result.Content, result.Error)
			}
			a.Messages = append(a.Messages, schema.Message{
				Role:       "tool",
				Content:    content,
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
			})
		}
	}
	return "", nil
}

// cleanupIncompleteMessages removes the last assistant message and its tool results (e.g. after cancel).
func (a *Agent) cleanupIncompleteMessages() {
	for i := len(a.Messages) - 1; i >= 0; i-- {
		if a.Messages[i].Role == "assistant" {
			a.Messages = a.Messages[:i]
			return
		}
	}
}

// toolArgsFromRaw normalises tool arguments (e.g. if API sent them as a JSON string).
func toolArgsFromRaw(raw map[string]interface{}) map[string]interface{} {
	if raw == nil {
		return nil
	}
	// If a single key holds JSON string (some APIs), parse it
	if s, ok := raw["arguments"].(string); ok {
		var m map[string]interface{}
		if json.Unmarshal([]byte(s), &m) == nil {
			return m
		}
	}
	return raw
}
