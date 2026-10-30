package agent

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"

	"diegoc-agent/internal/memory"
	"diegoc-agent/internal/permission"
	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

func TestInteractiveForkIsAnExactSnapshotWithoutATaskOrAPICall(t *testing.T) {
	var calls atomic.Int32
	client := forkClientFunc(func(context.Context, []schema.Message, []tools.Tool) (*schema.LLMResponse, error) {
		calls.Add(1)
		return &schema.LLMResponse{Content: "answer"}, nil
	})
	root := New(client, "rendered prompt", 8, 10000, nil)
	root.AddUserMessage("original context")
	root.Messages = append(root.Messages, schema.Message{Role: "assistant", Content: "analysis", ToolCalls: []schema.ToolCall{forkCall("call", "nested arguments")}}, schema.Message{Role: "tool", ToolCallID: "call", Content: "result"})
	root.PermissionCtx.AddRule(permission.Rule{ToolName: "write_file", Behavior: permission.BehaviorALLOW, RuleContent: "original"})
	cfg := memory.DefaultConfig()
	cfg.WorkingDir = t.TempDir()
	mgr := memory.NewManager(cfg, client)
	defer mgr.Close()
	mgr.SetCompressedSummary("summary at fork")
	root.SetMemoryManager(mgr)
	background := NewForkManager(context.Background())
	defer background.Close()
	root.EnableForks(background)
	sessions := NewConversationSessions(root)
	child := mustForkSession(t, sessions)
	if calls.Load() != 0 || child.ID != "session-1" || child.ParentID != "main" || sessions.Current() != child {
		t.Fatal("fork must create and enter a session without an API call")
	}
	if !reflect.DeepEqual(child.Agent.Messages, cloneMessages(root.Messages)) {
		t.Fatal("fork added a task or changed the inherited context")
	}
	if child.Agent.isFork || child.Agent.contextState.manager != nil || child.Agent.contextState.summaryText() != "summary at fork" {
		t.Fatal("foreground clone inherited worker limits or shared mutable memory")
	}
	child.Agent.Messages[2].ToolCalls[0].Function.Arguments["prompt"] = "child only"
	child.Agent.PermissionCtx.AllowRules["write_file"][0].RuleContent = "child only"
	if root.Messages[2].ToolCalls[0].Function.Arguments["prompt"] != "nested arguments" || root.PermissionCtx.AllowRules["write_file"][0].RuleContent != "original" {
		t.Fatal("mutable state leaked to main")
	}
	tool := child.Agent.toolByName["Agent"].(*ForkTool)
	if tool.owner != child.Agent {
		t.Fatal("Agent tool still bound to main")
	}
	if child.Agent.permissionEngine.Context != &child.Agent.PermissionCtx {
		t.Fatal("permissions not session-local")
	}
}

func TestSwitchPreservesSeparateConversationsAndNestedForks(t *testing.T) {
	root := New(nil, "system", 8, 10000, nil)
	root.AddUserMessage("shared")
	sessions := NewConversationSessions(root)
	first := mustForkSession(t, sessions)
	first.Agent.AddUserMessage("first branch")
	nested := mustForkSession(t, sessions)
	if nested.ParentID != first.ID {
		t.Fatal("nested fork lost parent")
	}
	nested.Agent.AddUserMessage("nested branch")
	if err := sessions.Switch("main"); err != nil {
		t.Fatal(err)
	}
	root.AddUserMessage("main branch")
	if err := sessions.Switch(first.ID); err != nil {
		t.Fatal(err)
	}
	if last := sessions.Current().Agent.Messages[len(first.Agent.Messages)-1]; last.Content != "first branch" {
		t.Fatal("switch changed first branch")
	}
	if err := sessions.Switch(nested.ID); err != nil {
		t.Fatal(err)
	}
	if nested.Agent.Messages[len(nested.Agent.Messages)-1].Content != "nested branch" {
		t.Fatal("nested conversation lost")
	}
	if err := sessions.Switch("missing"); err == nil || sessions.Current() != nested {
		t.Fatal("invalid selection changed active session")
	}
	if len(root.Messages) != 3 || root.Messages[2].Content != "main branch" {
		t.Fatal("fork messages were synced to main")
	}
}

func TestForegroundRewindAndClearAffectOnlyActiveSession(t *testing.T) {
	root := New(nil, "system", 8, 10000, nil)
	root.AddUserMessage("shared")
	sessions := NewConversationSessions(root)
	child := mustForkSession(t, sessions)
	child.Agent.AddUserMessage("child only")
	if len(child.Agent.Checkpoints()) != 1 {
		t.Fatal("fork should own its future rewind points")
	}
	id := child.Agent.Checkpoints()[0].ID
	if _, err := child.Agent.Rewind(id, "conversation"); err != nil {
		t.Fatal(err)
	}
	if len(root.Messages) != 2 || len(root.Checkpoints()) != 1 {
		t.Fatal("child rewind changed main")
	}
	child.Agent.ClearConversation()
	if len(root.Messages) != 2 || len(child.Agent.Messages) != 1 {
		t.Fatal("child clear changed main")
	}
}

func mustForkSession(t *testing.T, sessions *ConversationSessions) *ConversationSession {
	t.Helper()
	entry, err := sessions.Fork()
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func TestForegroundForkLimitIncludesNestedForks(t *testing.T) {
	root := New(nil, "system", 5, 1000, nil)
	sessions := NewConversationSessions(root)
	for i := 0; i < MaxForkSessions; i++ {
		// Alternate main-origin and nested forks; all share the same limit.
		if i%2 == 0 {
			if err := sessions.Switch("main"); err != nil {
				t.Fatal(err)
			}
		}
		mustForkSession(t, sessions)
	}
	before := sessions.Current()
	if entry, err := sessions.Fork(); err == nil || entry != nil {
		t.Fatal("eighth fork was accepted")
	}
	if sessions.Current() != before || len(sessions.List()) != MaxForkSessions+1 {
		t.Fatal("rejected fork mutated session state")
	}
	if err := sessions.Switch("main"); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Fork(); err == nil {
		t.Fatal("switching to main bypassed the limit")
	}
}
