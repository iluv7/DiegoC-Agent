package agent

import (
	"context"
	"encoding/json"

	"diegoc-agent/internal/permission"
	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

// toolExecutor owns one conversation's permission engine, approval correlation
// and call states. It uses the owning Agent's live tools, messages, logger and
// checkpoints, so forks cannot retain another session's execution state.
type toolExecutor struct {
	agent       *Agent
	permissions *permission.Engine
	replyID     string
	states      map[string]permission.ToolCallState
}

func newToolExecutor(a *Agent) *toolExecutor {
	return &toolExecutor{
		agent: a, permissions: permission.NewEngine(&a.PermissionCtx),
		states: make(map[string]permission.ToolCallState),
	}
}

func (e *toolExecutor) reset() {
	e.states = make(map[string]permission.ToolCallState)
}

// executeCalls preserves model order and stops the batch on cancellation. Tool
// and permission failures remain tool messages for the next model request.
func (e *toolExecutor) executeCalls(ctx context.Context, calls []schema.ToolCall, input <-chan permission.HITLConfirmResponse, events chan<- permission.HITLConfirmRequest) error {
	for _, call := range calls {
		if err := ctx.Err(); err != nil {
			e.agent.cleanupIncompleteMessages()
			return err
		}
		args := toolArgsFromRaw(call.Function.Arguments)
		if args == nil {
			args = make(map[string]interface{})
		}
		tool := e.agent.toolByName[call.Function.Name]
		if tool == nil {
			e.appendError(call, "Unknown tool: "+call.Function.Name)
			continue
		}
		decision := e.checkPermission(call, tool, args)
		switch decision.Behavior {
		case permission.BehaviorALLOW:
			e.states[call.ID] = permission.ToolCallAllowed
			e.execute(ctx, call, tool, args)
		case permission.BehaviorASK:
			if input == nil || events == nil {
				e.reject(call, "Tool requires user approval; no permission handler is available")
				continue
			}
			if err := e.confirmAndExecute(ctx, call, tool, args, decision, input, events); err != nil {
				return err
			}
		case permission.BehaviorDENY:
			e.reject(call, decision.Message)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return nil
}

func (e *toolExecutor) checkPermission(call schema.ToolCall, tool tools.Tool, args map[string]interface{}) permission.Decision {
	a := e.agent
	interactive, _ := tool.(tools.UserInteractionTool)
	if interactive != nil && interactive.RequiresUserInteraction() && (a.isFork || a.PermissionCtx.Mode == permission.ModeDontAsk) {
		return permission.Decision{Behavior: permission.BehaviorDENY, Message: "Interactive questions require a foreground attended run; report the question to the user or parent agent instead"}
	}
	if a.PlanMode() {
		// Plan mode remains a hard boundary even with persisted allow rules.
		if call.Function.Name == "write_plan" || tool.IsReadOnly() {
			return permission.Decision{Behavior: permission.BehaviorALLOW, Message: "EXPLORE mode: planning/read-only tool allowed"}
		}
		return permission.Decision{Behavior: permission.BehaviorDENY, Message: "EXPLORE mode: mutating tools are disabled until the plan is approved"}
	}
	return e.permissions.CheckPermission(call.Function.Name, func() permission.Decision {
		return tool.CheckPermissions(args, &a.PermissionCtx)
	}, args)
}

func (e *toolExecutor) confirmAndExecute(ctx context.Context, call schema.ToolCall, tool tools.Tool, args map[string]interface{}, decision permission.Decision, input <-chan permission.HITLConfirmResponse, events chan<- permission.HITLConfirmRequest) error {
	e.states[call.ID] = permission.ToolCallAsking
	request := permission.HITLConfirmRequest{
		ReplyID: e.replyID,
		ToolCalls: []permission.PendingToolCall{{
			ID: call.ID, Name: call.Function.Name, Args: args, SuggestedRules: decision.SuggestedRules,
		}},
	}
	select {
	case events <- request:
	case <-ctx.Done():
		return ctx.Err()
	}
	var response permission.HITLConfirmResponse
	select {
	case response = <-input:
	case <-ctx.Done():
		return ctx.Err()
	}
	if response.ReplyID != e.replyID {
		e.reject(call, "HITL reply_id 不匹配")
		return nil
	}
	confirmed := false
	for _, result := range response.Results {
		if result.ToolCall.ID != call.ID {
			continue
		}
		confirmed = result.Confirmed
		if confirmed {
			e.states[call.ID] = permission.ToolCallAllowed
			for _, rule := range result.Rules {
				e.permissions.AddRule(rule)
			}
			e.execute(ctx, call, tool, result.ToolCall.Args)
		} else {
			e.reject(call, "操作被用户拒绝")
		}
		break
	}
	if !confirmed {
		e.states[call.ID] = permission.ToolCallFinished
	}
	return nil
}

// execute runs an already permitted call, recording file versions and the same
// result in the log and conversation. Waiting for human input never holds the
// shared tool lock; background Agent spawning also retains its existing bypass.
func (e *toolExecutor) execute(ctx context.Context, call schema.ToolCall, tool tools.Tool, args map[string]interface{}) {
	a := e.agent
	interactive, _ := tool.(tools.UserInteractionTool)
	waitsForUser := interactive != nil && interactive.RequiresUserInteraction()
	if a.forks != nil && tool.Name() != "Agent" && !waitsForUser {
		a.forks.toolMu.Lock()
		defer a.forks.toolMu.Unlock()
	}
	if err := ctx.Err(); err != nil {
		e.appendError(call, err.Error())
		return
	}
	path, backupErr := a.trackBefore(tool, args)
	if backupErr != nil {
		e.reject(call, "Cannot create rewind backup: "+backupErr.Error())
		return
	}
	result, execErr := a.act(ctx, ToolExecution{ID: call.ID, Name: call.Function.Name}, func(ctx context.Context) (*tools.ToolResult, error) {
		return tool.Execute(ctx, args)
	})
	a.trackAfter(path)
	if execErr != nil {
		result = &tools.ToolResult{Success: false, Error: execErr.Error()}
	}
	content := result.Content
	if !result.Success {
		content = "Error: " + result.Error
	}
	if a.Logger != nil {
		a.Logger.LogToolResult(call.Function.Name, args, result.Success, result.Content, result.Error)
	}
	a.Messages = append(a.Messages, schema.Message{
		Role: "tool", Content: content, ToolCallID: call.ID, Name: call.Function.Name,
	})
	e.states[call.ID] = permission.ToolCallFinished
}

func (e *toolExecutor) reject(call schema.ToolCall, reason string) {
	e.appendError(call, reason)
	e.states[call.ID] = permission.ToolCallFinished
}

func (e *toolExecutor) appendError(call schema.ToolCall, reason string) {
	e.agent.Messages = append(e.agent.Messages, schema.Message{
		Role: "tool", Content: "Error: " + reason, ToolCallID: call.ID, Name: call.Function.Name,
	})
}

// toolArgsFromRaw normalises tool arguments (e.g. if API sent them as a JSON string).
func toolArgsFromRaw(raw map[string]interface{}) map[string]interface{} {
	if raw == nil {
		return nil
	}
	if s, ok := raw["arguments"].(string); ok {
		var args map[string]interface{}
		if json.Unmarshal([]byte(s), &args) == nil {
			return args
		}
	}
	return raw
}
