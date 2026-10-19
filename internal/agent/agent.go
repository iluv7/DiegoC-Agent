package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"diegoc-agent/internal/llm"
	"diegoc-agent/internal/logger"
	"diegoc-agent/internal/memory"
	"diegoc-agent/internal/permission"
	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

// Agent runs the conversation loop with optional tools and HITL support.
type Agent struct {
	PlanMode        bool
	needsToolRepair bool
	forks           *ForkManager
	isFork          bool
	forkBatch       []schema.Message
	inboxMu         sync.Mutex
	forkReports     []ForkEvent

	checkpoints        []checkpoint
	checkpointSequence int
	LLM                llm.Client
	SystemPrompt       string
	Messages           []schema.Message
	MaxSteps           int
	TokenLimit         int
	Tools              []tools.Tool
	toolByName         map[string]tools.Tool
	APITotalTokens     int
	skipNextTokenCheck bool
	Logger             *logger.AgentLogger

	// Memory pipeline (Phase 11-12). When set, replaces the legacy summarizeIfNeeded.
	memoryManager     *memory.Manager
	compressedSummary string
	contextLimit      int
	compactLanguage   string

	// HITL 权限系统
	PermissionCtx    permission.Context
	permissionEngine *permission.Engine
	replyID          string                              // 当前回复的 ID，关联 HITL request 和 response
	toolCallStates   map[string]permission.ToolCallState // tool_call_id → 状态
}

// SetPlanMode switches the same ReAct agent between read-only planning and execution.
// The permission context is restored to its prior mode when planning is approved.
func (a *Agent) SetPlanMode(enabled bool) {
	if enabled == a.PlanMode {
		return
	}
	if enabled {
		a.PermissionCtx.PrePlanMode = a.PermissionCtx.Mode
		a.PermissionCtx.Mode = permission.ModeExplore
	} else {
		mode := a.PermissionCtx.PrePlanMode
		if mode == "" || mode == permission.ModeExplore {
			mode = permission.ModeDefault
		}
		a.PermissionCtx.Mode = mode
	}
	a.PlanMode = enabled
	if len(a.Messages) > 0 {
		base := a.SystemPrompt
		if enabled {
			base += "\n\n## Plan Mode\nExplore and reason before editing. Do not modify project files or run commands that change state. Write the complete proposed implementation plan to `.agent_plan.md` using `write_plan` when ready. Then summarize the plan and tell the user to approve it with `/execute`. If the request changes materially, update the plan first."
		} else {
			base += "\n\n## Execute Mode\nFollow the approved plan in `.agent_plan.md`. Implement it, verify the changes, and adapt to new user instructions."
		}
		a.Messages[0].Content = base
	}
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
	msgs := []schema.Message{
		{Role: "system", Content: systemPrompt},
	}
	ag := &Agent{
		LLM:             client,
		SystemPrompt:    systemPrompt,
		Messages:        msgs,
		MaxSteps:        maxSteps,
		TokenLimit:      tokenLimit,
		contextLimit:    tokenLimit,
		compactLanguage: "zh",
		Tools:           toolList,
		toolByName:      byName,
		PermissionCtx:   permCtx,
		toolCallStates:  make(map[string]permission.ToolCallState),
	}
	ag.permissionEngine = permission.NewEngine(&ag.PermissionCtx)
	return ag
}

// AddUserMessage appends a user message.
func (a *Agent) AddUserMessage(content string) {
	if a.needsToolRepair {
		a.cleanupPendingToolCalls()
		a.needsToolRepair = false
	}
	a.deliverForkReports()
	a.makeCheckpoint(content)
	a.Messages = append(a.Messages, schema.Message{Role: "user", Content: content})
}

// SetMemoryManager configures the memory pipeline.
// When set, PreReasoningHook replaces the legacy summarizeIfNeeded in the agent loop.
func (a *Agent) SetMemoryManager(mgr *memory.Manager) {
	a.memoryManager = mgr
	if mgr != nil {
		a.contextLimit, _, a.compactLanguage = mgr.ContextSettings()
	}
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
) (output string, runErr error) {

	defer func() {
		a.forkBatch = nil
		if runErr != nil {
			a.needsToolRepair = true
		}
	}()
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

		a.deliverForkReports()
		if a.memoryManager != nil {
			// Phase 11-12: full memory pipeline
			keptMsgs, summary, err := a.memoryManager.PreReasoningHook(ctx, a.Messages, a.SystemPrompt)
			if err != nil {
				return "", err
			}
			a.Messages = keptMsgs
			if summary != "" {
				a.compressedSummary = summary
			}
		} else {
			// Legacy fallback: simple per-round summarization
			if err := a.summarizeIfNeeded(ctx); err != nil {
				return "", err
			}
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

		if a.forks != nil {
			a.forkBatch = cloneMessages(a.Messages)
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
			var decision permission.Decision
			interactive, _ := tool.(tools.UserInteractionTool)
			if interactive != nil && interactive.RequiresUserInteraction() && (a.isFork || a.PermissionCtx.Mode == permission.ModeDontAsk) {
				decision = permission.Decision{Behavior: permission.BehaviorDENY, Message: "Interactive questions require a foreground attended run; report the question to the user or parent agent instead"}
			} else if a.PermissionCtx.Mode == permission.ModeExplore {
				// Plan mode is a hard boundary: persisted allow rules must not make
				// a mutating tool available while the agent is planning.
				if tc.Function.Name == "write_plan" || tool.IsReadOnly() {
					decision = permission.Decision{Behavior: permission.BehaviorALLOW, Message: "EXPLORE mode: planning/read-only tool allowed"}
				} else {
					decision = permission.Decision{Behavior: permission.BehaviorDENY, Message: "EXPLORE mode: mutating tools are disabled until the plan is approved"}
				}
			} else {
				decision = a.permissionEngine.CheckPermission(
					tc.Function.Name,
					func() permission.Decision { return tool.CheckPermissions(args, &a.PermissionCtx) },
					args,
				)
			}

			switch decision.Behavior {
			case permission.BehaviorALLOW:
				// 直接执行
				a.toolCallStates[tc.ID] = permission.ToolCallAllowed
				a.executeAndAppend(ctx, tc, tool, args)

			case permission.BehaviorASK:
				if hitlInputCh == nil || hitlEventCh == nil {
					a.appendToolError(tc, "Tool requires user approval; no permission handler is available")
					a.toolCallStates[tc.ID] = permission.ToolCallFinished
					continue
				}

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
			if err := ctx.Err(); err != nil {
				return "", err
			}
		}
		a.forkBatch = nil
	}
	if a.isFork {
		return "", fmt.Errorf("fork reached maximum steps (%d) without a final response", a.MaxSteps)
	}
	return "", nil
}

// executeAndAppend 执行工具并将结果追加到消息列表。
func (a *Agent) executeAndAppend(ctx context.Context, tc schema.ToolCall, tool tools.Tool, args map[string]interface{}) {
	interactive, _ := tool.(tools.UserInteractionTool)
	waitsForUser := interactive != nil && interactive.RequiresUserInteraction()
	if a.forks != nil && tool.Name() != "Agent" && !waitsForUser {
		a.forks.toolMu.Lock()
		defer a.forks.toolMu.Unlock()
	}
	if err := ctx.Err(); err != nil {
		a.appendToolError(tc, err.Error())
		return
	}
	var result *tools.ToolResult
	var execErr error
	path, backupErr := a.trackBefore(tool, args)
	if backupErr != nil {
		a.appendToolError(tc, "Cannot create rewind backup: "+backupErr.Error())
		a.toolCallStates[tc.ID] = permission.ToolCallFinished
		return
	}
	result, execErr = tool.Execute(ctx, args)
	if path != "" {
		version, err := readVersion(path)
		if err == nil {
			a.checkpoints[len(a.checkpoints)-1].After[path] = version
		}
	}
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
