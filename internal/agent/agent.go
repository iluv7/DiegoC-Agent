package agent

import (
	"context"
	"encoding/json"

	"diegoc-agent/internal/llm"
	"diegoc-agent/internal/logger"
	"diegoc-agent/internal/permission"
	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

// Agent runs the conversation loop with optional tools and HITL support.
type Agent struct {
	LLM           llm.Client
	SystemPrompt  string
	Messages      []schema.Message
	MaxSteps      int
	TokenLimit    int
	Tools         []tools.Tool
	toolByName    map[string]tools.Tool
	APITotalTokens int
	skipNextTokenCheck bool
	Logger        *logger.AgentLogger

	// HITL 权限系统
	PermissionCtx     permission.Context
	permissionEngine  *permission.Engine
	replyID           string                 // 当前回复的 ID，关联 HITL request 和 response
	toolCallStates    map[string]permission.ToolCallState // tool_call_id → 状态
}

// New creates an Agent with system prompt and tools.
func New(client llm.Client, systemPrompt string, maxSteps int, tokenLimit int, toolList []tools.Tool) *Agent {
	return NewWithPermission(client, systemPrompt, maxSteps, tokenLimit, toolList, permission.NewContext())
}

// NewWithPermission creates an Agent with custom permission context.
func NewWithPermission(client llm.Client, systemPrompt string, maxSteps int, tokenLimit int, toolList []tools.Tool, permCtx permission.Context) *Agent {
	if maxSteps <= 0 {
		maxSteps = 50
	}
	if tokenLimit <= 0 {
		tokenLimit = 80000
	}
	byName := make(map[string]tools.Tool)
	for _, t := range toolList {
		byName[t.Name()] = t
	}
	engine := permission.NewEngine(&permCtx)
	msgs := []schema.Message{
		{Role: "system", Content: systemPrompt},
	}
	return &Agent{
		LLM:              client,
		SystemPrompt:     systemPrompt,
		Messages:         msgs,
		MaxSteps:         maxSteps,
		TokenLimit:       tokenLimit,
		Tools:            toolList,
		toolByName:       byName,
		PermissionCtx:    permCtx,
		permissionEngine: engine,
		toolCallStates:   make(map[string]permission.ToolCallState),
	}
}

// AddUserMessage appends a user message.
func (a *Agent) AddUserMessage(content string) {
	a.Messages = append(a.Messages, schema.Message{Role: "user", Content: content})
}

// RunWithHITL executes the loop with HITL support.
// 当工具需要用户确认时，通过 eventCh 发送 HITLConfirmRequest，
// 然后阻塞等待 inputCh 收到 HITLConfirmResponse。
func (a *Agent) RunWithHITL(
	ctx context.Context,
	inputCh <-chan permission.HITLConfirmResponse,
	eventCh chan<- permission.HITLConfirmRequest,
) (string, error) {
	return a.runLoop(ctx, inputCh, eventCh)
}

// runLoop 是 Run 和 RunWithHITL 共享的内部循环。
// 当 hitlInputCh == nil 时表示无 HITL 支持，所有 ASK 转为 DENY。
func (a *Agent) runLoop(
	ctx context.Context,
	hitlInputCh <-chan permission.HITLConfirmResponse,
	hitlEventCh chan<- permission.HITLConfirmRequest,
) (string, error) {

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

		// 执行每个工具调用（带权限检查）
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
			if tool == nil {
				a.appendToolError(tc, "Unknown tool: "+tc.Function.Name)
				continue
			}

			// ============== 权限检查 (HITL 插入点) ==============
			decision := a.permissionEngine.CheckPermission(
				tc.Function.Name,
				func() permission.Decision {
					return tool.CheckPermissions(args, &a.PermissionCtx)
				},
				args,
			)

			switch decision.Behavior {
			case permission.BehaviorALLOW:
				// 直接执行
				a.toolCallStates[tc.ID] = permission.ToolCallAllowed
				a.executeAndAppend(ctx, tc, tool, args)

			case permission.BehaviorASK:

				a.toolCallStates[tc.ID] = permission.ToolCallAsking

				// 发确认请求给调用者
				req := permission.HITLConfirmRequest{
					ReplyID: a.replyID,
					ToolCalls: []permission.PendingToolCall{{
						ID:             tc.ID,
						Name:           tc.Function.Name,
						Args:           args,
						SuggestedRules: decision.SuggestedRules,
					}},
				}
				select {
				case hitlEventCh <- req:
				case <-ctx.Done():
					return "", ctx.Err()
				}

				// 阻塞等待用户回复
				var response permission.HITLConfirmResponse
				select {
				case response = <-hitlInputCh:
				case <-ctx.Done():
					return "", ctx.Err()
				}

				// 校验 ReplyID
				if response.ReplyID != a.replyID {
					a.appendToolError(tc, "HITL reply_id 不匹配")
					a.toolCallStates[tc.ID] = permission.ToolCallFinished
					continue
				}

				// 处理用户决定
				confirmed := false
				for _, cr := range response.Results {
					if cr.ToolCall.ID == tc.ID {
						confirmed = cr.Confirmed
						if cr.Confirmed {
							a.toolCallStates[tc.ID] = permission.ToolCallAllowed
							// 用户选了"始终允许" → 写入规则
							for _, rule := range cr.Rules {
								a.permissionEngine.AddRule(rule)
							}
							a.executeAndAppend(ctx, tc, tool, cr.ToolCall.Args)
						} else {
							a.appendToolError(tc, "操作被用户拒绝")
							a.toolCallStates[tc.ID] = permission.ToolCallFinished
						}
						break
					}
				}
				if !confirmed {
					a.toolCallStates[tc.ID] = permission.ToolCallFinished
				}

			case permission.BehaviorDENY:
				a.appendToolError(tc, decision.Message)
				a.toolCallStates[tc.ID] = permission.ToolCallFinished
			}
		}
	}
	return "", nil
}

// executeAndAppend 执行工具并将结果追加到消息列表。
func (a *Agent) executeAndAppend(ctx context.Context, tc schema.ToolCall, tool tools.Tool, args map[string]interface{}) {
	var result *tools.ToolResult
	var execErr error
	result, execErr = tool.Execute(ctx, args)
	if execErr != nil {
		result = &tools.ToolResult{Success: false, Error: execErr.Error()}
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
	a.toolCallStates[tc.ID] = permission.ToolCallFinished
}

// appendToolError 追加一个工具错误结果到消息列表。
func (a *Agent) appendToolError(tc schema.ToolCall, errMsg string) {
	a.Messages = append(a.Messages, schema.Message{
		Role:       "tool",
		Content:    "Error: " + errMsg,
		ToolCallID: tc.ID,
		Name:       tc.Function.Name,
	})
}

// cleanupIncompleteMessages 移除最后一个 assistant 消息及之后的所有消息（取消时清理）。
func (a *Agent) cleanupIncompleteMessages() {
	for i := len(a.Messages) - 1; i >= 0; i-- {
		if a.Messages[i].Role == "assistant" {
			a.Messages = a.Messages[:i]
			return
		}
	}
	// 没找到 assistant 消息 → 回退到只剩 system prompt
	if len(a.Messages) > 1 {
		a.Messages = a.Messages[:1]
	}
}

// toolArgsFromRaw normalises tool arguments (e.g. if API sent them as a JSON string).
func toolArgsFromRaw(raw map[string]interface{}) map[string]interface{} {
	if raw == nil {
		return nil
	}
	if s, ok := raw["arguments"].(string); ok {
		var m map[string]interface{}
		if json.Unmarshal([]byte(s), &m) == nil {
			return m
		}
	}
	return raw
}
