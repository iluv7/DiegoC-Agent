package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"diegoc-agent/internal/logger"
	"diegoc-agent/internal/permission"
	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

type executorTestTool struct {
	name     string
	decision permission.Decision
	readOnly bool
	onCheck  func()
	run      func(context.Context, map[string]interface{}) (*tools.ToolResult, error)
}

func (t *executorTestTool) Name() string        { return t.name }
func (t *executorTestTool) Description() string { return "executor test tool" }
func (t *executorTestTool) Parameters() map[string]interface{} {
	return map[string]interface{}{"type": "object"}
}
func (t *executorTestTool) Execute(ctx context.Context, args map[string]interface{}) (*tools.ToolResult, error) {
	return t.run(ctx, args)
}
func (t *executorTestTool) CheckPermissions(map[string]interface{}, *permission.Context) permission.Decision {
	if t.onCheck != nil {
		t.onCheck()
	}
	return t.decision
}
func (t *executorTestTool) IsReadOnly() bool        { return t.readOnly }
func (t *executorTestTool) IsConcurrencySafe() bool { return false }
func (t *executorTestTool) IsExternalTool() bool    { return false }

func executorTestCall(id, name string, args map[string]interface{}) schema.ToolCall {
	return schema.ToolCall{ID: id, Type: "function", Function: schema.FunctionCall{Name: name, Arguments: args}}
}

func executorTestAgent(calls []schema.ToolCall, list ...tools.Tool) *Agent {
	client := forkClientFunc(func(_ context.Context, messages []schema.Message, _ []tools.Tool) (*schema.LLMResponse, error) {
		if messages[len(messages)-1].Role == "user" {
			return &schema.LLMResponse{ToolCalls: calls}, nil
		}
		return &schema.LLMResponse{Content: "done"}, nil
	})
	a := New(client, "system", 3, 100000, list)
	a.AddUserMessage("run tools")
	return a
}

func TestToolExecutorPreservesOrderArgumentsErrorsAndLogs(t *testing.T) {
	var executed []string
	makeTool := func(name string) *executorTestTool {
		return &executorTestTool{name: name, decision: permission.Decision{Behavior: permission.BehaviorALLOW}, run: func(_ context.Context, args map[string]interface{}) (*tools.ToolResult, error) {
			executed = append(executed, name)
			if args == nil {
				t.Fatal("nil arguments reached tool execution")
			}
			switch name {
			case "success":
				if args["value"] != "normalized" {
					t.Fatalf("arguments were not normalized: %+v", args)
				}
				return &tools.ToolResult{Success: true, Content: "result"}, nil
			case "error":
				return nil, errors.New("execution failed")
			default:
				return &tools.ToolResult{Error: "reported failure"}, nil
			}
		}}
	}
	calls := []schema.ToolCall{
		executorTestCall("one", "success", map[string]interface{}{"arguments": `{"value":"normalized"}`}),
		executorTestCall("two", "missing", nil),
		executorTestCall("three", "error", nil),
		executorTestCall("four", "failure", nil),
	}
	a := executorTestAgent(calls, makeTool("success"), makeTool("error"), makeTool("failure"))
	a.Logger = logger.NewInDirectory(t.TempDir())
	defer a.Logger.Close()
	if out, err := a.RunWithHITL(context.Background(), nil, nil); err != nil || out != "done" {
		t.Fatalf("tool failure interrupted the conversation: %q %v", out, err)
	}
	if !reflect.DeepEqual(executed, []string{"success", "error", "failure"}) {
		t.Fatalf("execution order changed: %v", executed)
	}
	want := []string{"result", "Error: Unknown tool: missing", "Error: execution failed", "Error: reported failure"}
	for i, content := range want {
		msg := a.Messages[3+i]
		if msg.Role != "tool" || msg.ToolCallID != calls[i].ID || msg.Name != calls[i].Function.Name || msg.Content != content {
			t.Fatalf("result correlation changed: %+v", msg)
		}
	}
	data, err := os.ReadFile(a.Logger.GetLogFilePath())
	if err != nil || strings.Count(string(data), `"tool_name"`) != 3 || !strings.Contains(string(data), `"value": "normalized"`) || !strings.Contains(string(data), "execution failed") || !strings.Contains(string(data), "reported failure") {
		t.Fatalf("tool results were not logged consistently: %v\n%s", err, data)
	}
}

func TestToolExecutorPreservesPermissionBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, tool string
		mode       permission.Mode
		behavior   permission.Behavior
		rule       permission.Behavior
		readOnly   bool
		allowed    bool
	}{
		{name: "plan rejects remembered writes", tool: "mutate", mode: permission.ModeExplore, behavior: permission.BehaviorALLOW, rule: permission.BehaviorALLOW},
		{name: "plan permits reads", tool: "read", mode: permission.ModeExplore, readOnly: true, allowed: true},
		{name: "plan permits plan file", tool: "write_plan", mode: permission.ModeExplore, allowed: true},
		{name: "deny overrides allow", tool: "mutate", mode: permission.ModeBypass, behavior: permission.BehaviorALLOW, rule: permission.BehaviorDENY},
		{name: "remembered allow overrides ask", tool: "mutate", behavior: permission.BehaviorASK, rule: permission.BehaviorALLOW, allowed: true},
		{name: "unattended passthrough denied", tool: "mutate", mode: permission.ModeDontAsk, behavior: permission.BehaviorPASSTHROUGH},
		{name: "bypass passthrough allowed", tool: "mutate", mode: permission.ModeBypass, behavior: permission.BehaviorPASSTHROUGH, allowed: true},
		{name: "approval without handler denied", tool: "mutate", behavior: permission.BehaviorASK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executions := 0
			tool := &executorTestTool{name: tc.tool, readOnly: tc.readOnly, decision: permission.Decision{Behavior: tc.behavior}, run: func(context.Context, map[string]interface{}) (*tools.ToolResult, error) {
				executions++
				return &tools.ToolResult{Success: true, Content: "executed"}, nil
			}}
			a := executorTestAgent([]schema.ToolCall{executorTestCall("call", tc.tool, nil)}, tool)
			if tc.mode == permission.ModeExplore {
				a.SetPlanMode(true)
			} else if tc.mode != "" {
				a.PermissionCtx.Mode = tc.mode
			}
			if tc.rule != "" {
				a.PermissionCtx.AddRule(permission.Rule{ToolName: tc.tool, Behavior: tc.rule})
			}
			if _, err := a.RunWithHITL(context.Background(), nil, nil); err != nil {
				t.Fatal(err)
			}
			if (executions == 1) != tc.allowed || a.Messages[3].Content == "executed" != tc.allowed {
				t.Fatalf("permission boundary changed: executions=%d result=%+v", executions, a.Messages[3])
			}
		})
	}
}

func TestToolExecutorApprovalsKeepEditedArgumentsAndRulesSessionLocal(t *testing.T) {
	for _, action := range []string{"approve", "reject", "stale reply"} {
		t.Run(action, func(t *testing.T) {
			var values []string
			rule := permission.Rule{ToolName: "approved_tool", Behavior: permission.BehaviorALLOW, Source: "userConfirm"}
			tool := &executorTestTool{name: rule.ToolName, decision: permission.Decision{Behavior: permission.BehaviorASK, SuggestedRules: []permission.Rule{rule}}, run: func(_ context.Context, args map[string]interface{}) (*tools.ToolResult, error) {
				values = append(values, args["value"].(string))
				return &tools.ToolResult{Success: true, Content: "executed"}, nil
			}}
			calls := []schema.ToolCall{executorTestCall("one", tool.name, map[string]interface{}{"value": "original"})}
			if action == "approve" {
				calls = append(calls, executorTestCall("two", tool.name, map[string]interface{}{"value": "next call"}))
			}
			a := executorTestAgent(calls, tool)
			sibling := a.cloneConversation()
			a.executor.replyID = "run-one"
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			requests := make(chan permission.HITLConfirmRequest, 1)
			responses := make(chan permission.HITLConfirmResponse, 1)
			done := make(chan error, 1)
			go func() { _, err := a.RunWithHITL(ctx, responses, requests); done <- err }()
			var request permission.HITLConfirmRequest
			select {
			case request = <-requests:
			case <-ctx.Done():
				t.Fatal("approval request was not delivered")
			}
			if request.ReplyID != "run-one" || request.ToolCalls[0].ID != "one" || !reflect.DeepEqual(request.ToolCalls[0].SuggestedRules, []permission.Rule{rule}) {
				t.Fatalf("approval metadata changed: %+v", request)
			}
			call := request.ToolCalls[0]
			call.Args = map[string]interface{}{"value": "approved edit"}
			reply := permission.HITLConfirmResponse{ReplyID: request.ReplyID, Results: []permission.ToolConfirmResult{{ToolCall: call, Confirmed: action != "reject", Rules: []permission.Rule{rule}}}}
			if action == "stale reply" {
				reply.ReplyID = "different run"
			}
			responses <- reply
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if action == "approve" {
				if !reflect.DeepEqual(values, []string{"approved edit", "next call"}) || len(a.PermissionCtx.AllowRules[tool.name]) != 1 || len(requests) != 0 {
					t.Fatal("approval lost edited arguments or did not remember the rule")
				}
			} else if len(values) != 0 || len(a.PermissionCtx.AllowRules[tool.name]) != 0 || !strings.HasPrefix(contentString(a.Messages[3]), "Error:") {
				t.Fatal("rejected/stale approval executed a tool or persisted a rule")
			}
			if len(sibling.PermissionCtx.AllowRules[tool.name]) != 0 || sibling.executor == a.executor || sibling.executor.agent != sibling || sibling.executor.permissions.Context != &sibling.PermissionCtx {
				t.Fatal("executor or approval state leaked into another session")
			}
		})
	}
}

func TestToolExecutorApprovalCancellationStopsBatchAndRepairsHistory(t *testing.T) {
	for _, phase := range []string{"sending request", "waiting for reply"} {
		t.Run(phase, func(t *testing.T) {
			checked := make(chan struct{})
			executions := 0
			tool := &executorTestTool{name: "approval", decision: permission.Decision{Behavior: permission.BehaviorASK}, onCheck: func() { close(checked) }, run: func(context.Context, map[string]interface{}) (*tools.ToolResult, error) {
				executions++
				return &tools.ToolResult{Success: true}, nil
			}}
			a := executorTestAgent([]schema.ToolCall{executorTestCall("first", tool.name, nil), executorTestCall("remaining", tool.name, nil)}, tool)
			if err := a.SetGoal("finish task"); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			requests := make(chan permission.HITLConfirmRequest)
			responses := make(chan permission.HITLConfirmResponse)
			done := make(chan error, 1)
			go func() { _, err := a.RunWithHITL(ctx, responses, requests); done <- err }()
			select {
			case <-checked:
			case <-ctx.Done():
				t.Fatal("permission check was not reached")
			}
			if phase == "waiting for reply" {
				select {
				case <-requests:
				case <-ctx.Done():
					t.Fatal("approval request was not delivered")
				}
			}
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) || executions != 0 || a.GoalStatus().Status != "paused" {
				t.Fatalf("cancelled approval continued the batch: %v", err)
			}
			a.AddUserMessage("followup")
			if len(a.Messages) != 6 || a.Messages[3].ToolCallID != "first" || a.Messages[4].ToolCallID != "remaining" || a.Messages[5].Content != "followup" {
				t.Fatalf("cancelled batch was not repaired: %+v", a.Messages)
			}
		})
	}
}

func TestToolExecutorWritesRemainRewindableAndBackupFailuresDoNotWrite(t *testing.T) {
	for _, target := range []string{"file.txt", "directory"} {
		t.Run(target, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, target)
			if target == "directory" {
				if err := os.Mkdir(path, 0755); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, []byte("original"), 0644); err != nil {
				t.Fatal(err)
			}
			call := executorTestCall("write", "write_file", map[string]interface{}{"path": target, "content": "updated"})
			a := executorTestAgent([]schema.ToolCall{call}, &tools.WriteTool{WorkspaceDir: dir})
			a.PermissionCtx.AddRule(permission.Rule{ToolName: "write_file", Behavior: permission.BehaviorALLOW, RuleContent: target})
			if _, err := a.RunWithHITL(context.Background(), nil, nil); err != nil {
				t.Fatal(err)
			}
			changes, err := a.FileChanges(0)
			if err != nil {
				t.Fatal(err)
			}
			if target == "directory" {
				if !strings.Contains(contentString(a.Messages[3]), "Cannot create rewind backup") || len(changes.Files) != 0 {
					t.Fatal("backup failure did not stop execution")
				}
				info, err := os.Stat(path)
				if err != nil || !info.IsDir() {
					t.Fatal("backup failure changed the target")
				}
				return
			}
			requireFile(t, path, "updated")
			if len(changes.Files) != 1 || string(changes.Files[0].Before.Data) != "original" || string(changes.Files[0].After.Data) != "updated" {
				t.Fatal("executor lost before/after file versions")
			}
			if _, err := a.Rewind(1, "both"); err != nil {
				t.Fatal(err)
			}
			requireFile(t, path, "original")
			if len(a.Messages) != 1 || len(a.executor.states) != 0 {
				t.Fatal("rewind did not reset conversation/executor state")
			}
		})
	}
}
