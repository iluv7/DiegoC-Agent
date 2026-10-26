package tools

import (
	"context"
	"encoding/json"
	"strings"

	"diegoc-agent/internal/loop"
	"diegoc-agent/internal/permission"
)

type loopSessionKey struct{}
type loopSession struct {
	scheduler *loop.Scheduler
	id        string
}

// WithLoopSession binds tools to this foreground run, not to shared tool
// objects. Fork workers do not inherit this context or scheduling authority.
func WithLoopSession(ctx context.Context, scheduler *loop.Scheduler, id string) context.Context {
	return context.WithValue(ctx, loopSessionKey{}, loopSession{scheduler, id})
}

// LoopTool exposes local scheduling without granting permissions to the work
// performed on a scheduled turn. The Agent checks those permissions normally.
type LoopTool struct{ Operation string }

func LoopTools() []Tool {
	return []Tool{&LoopTool{"create"}, &LoopTool{"list"}, &LoopTool{"cancel"}}
}

func (t *LoopTool) Name() string { return "loop_" + t.Operation }
func (t *LoopTool) Description() string {
	switch t.Operation {
	case "create":
		return "Schedule a prompt to repeat in the current foreground conversation, only when the user requests recurring work. interval is a positive integer plus s/m/h/d (default 10m; minimum 1 minute). First run is after one interval. Requires this process and session to be active; expires after 7 days. Does not grant tool permissions."
	case "list":
		return "List the current conversation's recurring prompts with task IDs, next run times, interval_ns, and expiry."
	default:
		return "Cancel a recurring prompt by ID, or id=all for this conversation. Cancel the current scheduled loop when its requested goal is complete; its ID is included in the scheduled message."
	}
}
func (t *LoopTool) Parameters() map[string]interface{} {
	properties := map[string]interface{}{}
	required := []string{}
	switch t.Operation {
	case "create":
		properties["prompt"] = map[string]interface{}{"type": "string", "minLength": 1, "description": "Task to repeat, including any completion or stopping condition."}
		properties["interval"] = map[string]interface{}{"type": "string", "description": "e.g. 5m, 2h, 1d; defaults to 10m."}
		required = []string{"prompt"}
	case "cancel":
		properties["id"] = map[string]interface{}{"type": "string", "description": "Task ID, e.g. loop-1, or all."}
		required = []string{"id"}
	}
	return map[string]interface{}{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}
func (t *LoopTool) Execute(ctx context.Context, args map[string]interface{}) (*ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	session, _ := ctx.Value(loopSessionKey{}).(loopSession)
	if session.scheduler == nil || session.id == "" {
		return &ToolResult{Error: "loop tools require an active foreground interactive session; unavailable to background workers"}, nil
	}
	var output interface{}
	switch t.Operation {
	case "create":
		prompt, _ := args["prompt"].(string)
		interval := loop.DefaultInterval
		if value, exists := args["interval"]; exists {
			text, ok := value.(string)
			if !ok {
				return &ToolResult{Error: "interval must be a string, e.g. 5m"}, nil
			}
			var err error
			interval, err = loop.ParseInterval(text)
			if err != nil {
				return &ToolResult{Error: err.Error()}, nil
			}
		}
		task, err := session.scheduler.Create(session.id, prompt, interval)
		if err != nil {
			return &ToolResult{Error: err.Error()}, nil
		}
		output = task
	case "list":
		output = session.scheduler.List(session.id)
	case "cancel":
		id, _ := args["id"].(string)
		if strings.TrimSpace(id) == "" {
			return &ToolResult{Error: "id is required"}, nil
		}
		count := session.scheduler.Cancel(session.id, strings.TrimSpace(id))
		if count == 0 && id != "all" {
			return &ToolResult{Error: "loop not found in this session"}, nil
		}
		output = map[string]int{"cancelled": count}
	default:
		return &ToolResult{Error: "unknown loop operation"}, nil
	}
	data, err := json.Marshal(output)
	if err != nil {
		return nil, err
	}
	return &ToolResult{Success: true, Content: string(data)}, nil
}
func (t *LoopTool) CheckPermissions(_ map[string]interface{}, _ *permission.Context) permission.Decision {
	return permission.Decision{Behavior: permission.BehaviorALLOW}
}
func (t *LoopTool) IsConcurrencySafe() bool { return true }
func (t *LoopTool) IsReadOnly() bool        { return t.Operation == "list" }
func (t *LoopTool) IsExternalTool() bool    { return false }
