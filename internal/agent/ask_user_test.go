package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"diegoc-agent/internal/permission"
	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

func askUserCall() schema.ToolCall {
	return schema.ToolCall{ID: "question", Type: "function", Function: schema.FunctionCall{Name: "ask_user", Arguments: map[string]interface{}{
		"question": "存储方式？", "options": []tools.QuestionOption{{Label: "SQLite"}, {Label: "PostgreSQL"}},
	}}}
}

func TestAskUserContinuesInPlanModeAndAcrossForegroundSessions(t *testing.T) {
	client := forkClientFunc(func(_ context.Context, messages []schema.Message, _ []tools.Tool) (*schema.LLMResponse, error) {
		last := messages[len(messages)-1]
		if last.Role == "user" {
			return &schema.LLMResponse{ToolCalls: []schema.ToolCall{askUserCall()}}, nil
		}
		var answer tools.UserAnswer
		if err := json.Unmarshal([]byte(last.Content.(string)), &answer); err != nil {
			t.Fatal(err)
		}
		return &schema.LLMResponse{Content: "继续使用 " + answer.Answer}, nil
	})
	root := New(client, "system", 5, 100000, []tools.Tool{&tools.AskUserTool{}})
	root.SetPlanMode(true)
	run := func(a *Agent, answer string) {
		a.AddUserMessage("开始")
		ctx := tools.WithUserQuestionHandler(context.Background(), func(context.Context, tools.UserQuestion) (tools.UserAnswer, error) {
			return tools.UserAnswer{Answer: answer, Source: "option"}, nil
		})
		out, err := a.RunWithHITL(ctx, nil, nil)
		if err != nil || out != "继续使用 "+answer {
			t.Fatalf("run: %q %v", out, err)
		}
	}
	run(root, "SQLite")
	sessions := NewConversationSessions(root)
	defer sessions.Close()
	child := mustForkSession(t, sessions)
	before := len(root.Messages)
	// Even bypass permissions must collect an actual human answer.
	child.Agent.PermissionCtx.Mode = permission.ModeBypass
	run(child.Agent, "PostgreSQL")
	if len(root.Messages) != before || root.Messages[before-1].Content != "继续使用 SQLite" {
		t.Fatal("child answer leaked to main")
	}
	if err := sessions.Switch("main"); err != nil {
		t.Fatal(err)
	}
	run(root, "SQLite")
}

func TestCancelledQuestionStopsRemainingToolsAndRepairsConversation(t *testing.T) {
	dir := t.TempDir()
	client := forkClientFunc(func(context.Context, []schema.Message, []tools.Tool) (*schema.LLMResponse, error) {
		return &schema.LLMResponse{ToolCalls: []schema.ToolCall{askUserCall(), {
			ID: "write", Type: "function", Function: schema.FunctionCall{Name: "write_file", Arguments: map[string]interface{}{"path": "unexpected", "content": "should not execute"}},
		}}}, nil
	})
	a := New(client, "system", 1, 100000, []tools.Tool{&tools.AskUserTool{}, &tools.WriteTool{WorkspaceDir: dir}})
	a.PermissionCtx.Mode = permission.ModeBypass
	a.AddUserMessage("ask then write")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = tools.WithUserQuestionHandler(ctx, func(context.Context, tools.UserQuestion) (tools.UserAnswer, error) {
		cancel()
		return tools.UserAnswer{}, context.Canceled
	})
	if _, err := a.RunWithHITL(ctx, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled final step returned success: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "unexpected")); !os.IsNotExist(err) {
		t.Fatal("executed a mutation after question cancellation")
	}
	a.AddUserMessage("new task")
	if len(a.Messages) != 6 || a.Messages[4].ToolCallID != "write" || a.Messages[5].Role != "user" {
		t.Fatalf("interrupted tool batch was not repaired: %+v", a.Messages)
	}
}

func TestQuestionDoesNotHoldSharedToolLock(t *testing.T) {
	a := New(nil, "system", 1, 100000, []tools.Tool{&tools.AskUserTool{}})
	manager := NewForkManager(context.Background())
	defer manager.Close()
	a.EnableForks(manager)
	acquired := make(chan struct{})
	ctx := tools.WithUserQuestionHandler(context.Background(), func(context.Context, tools.UserQuestion) (tools.UserAnswer, error) {
		go func() {
			manager.toolMu.Lock()
			defer manager.toolMu.Unlock()
			close(acquired)
		}()
		select {
		case <-acquired:
		case <-time.After(time.Second):
			t.Error("question blocked other agents' file/tool execution")
		}
		return tools.UserAnswer{Answer: "SQLite", Source: "option"}, nil
	})
	tc := askUserCall()
	a.executeAndAppend(ctx, tc, a.toolByName["ask_user"], tc.Function.Arguments)
}

func TestBackgroundAndUnattendedQuestionsNeverOpenUI(t *testing.T) {
	for _, background := range []bool{false, true} {
		client := forkClientFunc(func(context.Context, []schema.Message, []tools.Tool) (*schema.LLMResponse, error) {
			return &schema.LLMResponse{ToolCalls: []schema.ToolCall{askUserCall()}}, nil
		})
		a := New(client, "system", 1, 100000, []tools.Tool{&tools.AskUserTool{}})
		a.isFork = background
		a.PermissionCtx.Mode = permission.ModeDontAsk
		// A remembered allow rule must not bypass the attended-run requirement.
		a.PermissionCtx.AddRule(permission.Rule{ToolName: "ask_user", Behavior: permission.BehaviorALLOW})
		if background {
			a.PermissionCtx.Mode = permission.ModeBypass
		}
		a.AddUserMessage("ask")
		ctx := tools.WithUserQuestionHandler(context.Background(), func(context.Context, tools.UserQuestion) (tools.UserAnswer, error) {
			t.Fatal("non-foreground run opened UI")
			return tools.UserAnswer{}, nil
		})
		a.RunWithHITL(ctx, nil, nil)
		if last := a.Messages[len(a.Messages)-1]; last.Role != "tool" || !strings.Contains(last.Content.(string), "foreground attended") {
			t.Fatalf("missing explicit rejection: %+v", last)
		}
	}
}
