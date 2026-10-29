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

	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

func TestGoalRejectsPrematureStopThenCompletesWithIsolatedJudge(t *testing.T) {
	mainCalls, judgeCalls := 0, 0
	main := forkClientFunc(func(_ context.Context, messages []schema.Message, _ []tools.Tool) (*schema.LLMResponse, error) {
		mainCalls++
		if !strings.Contains(contentString(messages[0]), "Active user goal") || !strings.Contains(contentString(messages[0]), "tests pass") {
			t.Fatal("main request lost active condition")
		}
		if mainCalls == 2 && !strings.Contains(contentString(messages[len(messages)-1]), "run the missing test") {
			t.Fatal("judge's reason was not fed back to the main agent")
		}
		return &schema.LLMResponse{Content: "candidate answer", Thinking: "PRIVATE_THINKING", Usage: &schema.TokenUsage{TotalTokens: 20}}, nil
	})
	judge := forkClientFunc(func(_ context.Context, messages []schema.Message, list []tools.Tool) (*schema.LLMResponse, error) {
		judgeCalls++
		if len(list) != 0 || len(messages) != 2 || contentString(messages[0]) != goalEvaluatorPrompt {
			t.Fatal("judge inherited tools or main system instructions")
		}
		payload := contentString(messages[1])
		if !strings.Contains(payload, "tests pass") || !strings.Contains(payload, "candidate answer") || strings.Contains(payload, "PRIVATE_THINKING") {
			t.Fatal("incorrect evaluator evidence")
		}
		decision := `{"satisfied":false,"reason":"run the missing test","needs_user_input":false}`
		if judgeCalls == 2 {
			decision = `{"satisfied":true,"reason":"tests passed","needs_user_input":false}`
		}
		return &schema.LLMResponse{Content: decision, Usage: &schema.TokenUsage{TotalTokens: 5}}, nil
	})
	a := New(main, "SYSTEM", 10, 100000, nil)
	a.ConfigureGoals(judge, "small-checker", 3)
	if err := a.SetGoal("tests pass"); err != nil {
		t.Fatal(err)
	}
	a.AddUserMessage("implement and test")
	out, err := a.RunWithHITL(context.Background(), nil, nil)
	if err != nil || out != "candidate answer" || mainCalls != 2 || judgeCalls != 2 || a.goal != nil {
		t.Fatalf("stop behavior: %q %v main=%d judge=%d goal=%+v", out, err, mainCalls, judgeCalls, a.goal)
	}
	status := a.GoalStatus()
	if status.Status != "completed" || status.Checks != 2 || status.Tokens != 50 || a.APITotalTokens != 50 {
		t.Fatalf("goal accounting: %+v total=%d", status, a.APITotalTokens)
	}
	if len(a.Checkpoints()) != 1 || contentString(a.Messages[0]) != "SYSTEM" {
		t.Fatal("continuations created checkpoints or mutated stored system prompt")
	}
	// A completed goal does not recheck or attach an instruction on later turns.
	a.LLM = forkClientFunc(func(_ context.Context, messages []schema.Message, _ []tools.Tool) (*schema.LLMResponse, error) {
		if strings.Contains(contentString(messages[0]), "Active user goal") {
			t.Fatal("completed goal leaked into later turn")
		}
		return &schema.LLMResponse{Content: "ordinary reply"}, nil
	})
	a.AddUserMessage("unrelated question")
	if _, err := a.RunWithHITL(context.Background(), nil, nil); err != nil || judgeCalls != 2 {
		t.Fatalf("completed goal restarted: %v", err)
	}
}

func TestGoalCheckBudgetPausesAndRequiresExplicitResume(t *testing.T) {
	main := forkClientFunc(func(context.Context, []schema.Message, []tools.Tool) (*schema.LLMResponse, error) {
		return &schema.LLMResponse{Content: "not yet"}, nil
	})
	checks := 0
	judge := forkClientFunc(func(context.Context, []schema.Message, []tools.Tool) (*schema.LLMResponse, error) {
		checks++
		return &schema.LLMResponse{Content: `{"satisfied":false,"reason":"remaining work"}`}, nil
	})
	a := New(main, "system", 50, 100000, nil)
	a.ConfigureGoals(judge, "checker", 2)
	a.SetGoal("finish")
	a.AddUserMessage("start")
	_, err := a.RunWithHITL(context.Background(), nil, nil)
	var paused *GoalPausedError
	if !errors.As(err, &paused) || a.GoalStatus().Status != "paused" || checks != 2 {
		t.Fatalf("limit did not pause: %v %+v checks=%d", err, a.GoalStatus(), checks)
	}
	a.AddUserMessage("status question")
	if _, err := a.RunWithHITL(context.Background(), nil, nil); err != nil || checks != 2 {
		t.Fatal("ordinary message silently reactivated paused goal")
	}
	if err := a.ResumeGoal(); err != nil {
		t.Fatal(err)
	}
	a.AddUserMessage("resume")
	_, err = a.RunWithHITL(context.Background(), nil, nil)
	if !errors.As(err, &paused) || checks != 4 || a.GoalStatus().Checks != 4 {
		t.Fatalf("resume did not start a new bounded attempt: %v checks=%d", err, checks)
	}
}

func TestGoalEvaluationFailureNeverClaimsSuccess(t *testing.T) {
	for _, value := range []string{"not JSON", `{}`, `{"satisfied":null,"reason":"ok"}`, `{"satisfied":true,"reason":""}`, `{"satisfied":true,"reason":"ok"} {}`, `{"satisfied":true,"reason":"ok","extra":1}`, `{"satisfied":true,"reason":"ok","needs_user_input":true}`} {
		t.Run(value, func(t *testing.T) {
			a := New(forkClientFunc(func(context.Context, []schema.Message, []tools.Tool) (*schema.LLMResponse, error) {
				return &schema.LLMResponse{Content: "candidate"}, nil
			}), "system", 5, 100000, nil)
			a.ConfigureGoals(forkClientFunc(func(context.Context, []schema.Message, []tools.Tool) (*schema.LLMResponse, error) {
				return &schema.LLMResponse{Content: value}, nil
			}), "judge", 2)
			a.SetGoal("finish")
			a.AddUserMessage("start")
			out, err := a.RunWithHITL(context.Background(), nil, nil)
			if err == nil || out != "candidate" || a.GoalStatus().Status != "paused" || a.goal == nil {
				t.Fatalf("invalid verdict accepted: %q %v %+v", out, err, a.GoalStatus())
			}
		})
	}
	for _, response := range []*schema.LLMResponse{nil, {ToolCalls: []schema.ToolCall{{ID: "unexpected"}}}, {Content: `{"satisfied":false,"reason":"credentials required","needs_user_input":true}`}} {
		a := New(forkClientFunc(func(context.Context, []schema.Message, []tools.Tool) (*schema.LLMResponse, error) {
			return &schema.LLMResponse{Content: "candidate"}, nil
		}), "system", 5, 100000, nil)
		a.ConfigureGoals(forkClientFunc(func(context.Context, []schema.Message, []tools.Tool) (*schema.LLMResponse, error) {
			return response, nil
		}), "judge", 2)
		a.SetGoal("finish")
		a.AddUserMessage("start")
		if _, err := a.RunWithHITL(context.Background(), nil, nil); err == nil || a.GoalStatus().Status != "paused" {
			t.Fatalf("unsafe verdict did not pause: %v", err)
		}
	}
}

func TestCancelledGoalEvaluationPreservesGoalEvenIfJudgeReturnsSuccess(t *testing.T) {
	a := New(forkClientFunc(func(context.Context, []schema.Message, []tools.Tool) (*schema.LLMResponse, error) {
		return &schema.LLMResponse{Content: "candidate"}, nil
	}), "system", 5, 100000, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.ConfigureGoals(forkClientFunc(func(context.Context, []schema.Message, []tools.Tool) (*schema.LLMResponse, error) {
		cancel()
		return &schema.LLMResponse{Content: `{"satisfied":true,"reason":"done"}`}, nil
	}), "judge", 2)
	a.SetGoal("finish")
	a.AddUserMessage("start")
	if _, err := a.RunWithHITL(ctx, nil, nil); !errors.Is(err, context.Canceled) || a.GoalStatus().Status != "paused" {
		t.Fatalf("cancellation claimed completion: %v %+v", err, a.GoalStatus())
	}
}

func TestGoalRetainsPermissionsAndStopsAtAgentStepLimit(t *testing.T) {
	workspace := t.TempDir()
	judgeCalled := false
	a := New(forkClientFunc(func(context.Context, []schema.Message, []tools.Tool) (*schema.LLMResponse, error) {
		return &schema.LLMResponse{ToolCalls: []schema.ToolCall{{ID: "write", Type: "function", Function: schema.FunctionCall{Name: "write_file", Arguments: map[string]interface{}{"path": "blocked", "content": "data"}}}}}, nil
	}), "system", 2, 100000, []tools.Tool{&tools.WriteTool{WorkspaceDir: workspace}})
	a.ConfigureGoals(forkClientFunc(func(context.Context, []schema.Message, []tools.Tool) (*schema.LLMResponse, error) {
		judgeCalled = true
		return nil, nil
	}), "judge", 10)
	a.SetGoal("write file")
	a.SetPlanMode(true)
	a.AddUserMessage("start")
	_, err := a.RunWithHITL(context.Background(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "step limit") || a.GoalStatus().Status != "paused" || judgeCalled {
		t.Fatalf("step limit bypassed or judge ran mid-tools: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "blocked")); !os.IsNotExist(err) {
		t.Fatal("goal bypassed Plan Mode")
	}
}

func TestGoalSessionLifecycleAndCompactionEvidence(t *testing.T) {
	a := New(nil, "system", 5, 100000, nil)
	baseUsage := a.ContextUsage().Total
	a.ConfigureGoals(nil, "checker", 3)
	if err := a.SetGoal(" "); err == nil {
		t.Fatal("empty goal accepted")
	}
	a.SetGoal("finish")
	if a.ContextUsage().Total <= baseUsage {
		t.Fatal("active goal instruction was excluded from context accounting")
	}
	a.AddUserMessage("start")
	a.compressedSummary = "important earlier evidence"
	a.Messages = append(a.Messages, schema.Message{Role: "tool", Content: strings.Repeat("中文 evidence ", 15000)})
	request := a.goalEvaluationMessages()
	payload := contentString(request[1])
	if !json.Valid([]byte(payload)) || !strings.Contains(payload, "important earlier evidence") || !strings.Contains(payload, `"evidence_truncated":true`) || len(payload) > 65000 {
		t.Fatalf("invalid/unbounded evidence payload: %d bytes", len(payload))
	}
	sessions := NewConversationSessions(a)
	child, err := sessions.Fork()
	if err != nil || child.Agent.GoalStatus().Status != "" || child.Agent.goalConfig.model != "checker" {
		t.Fatal("goal state leaked to fork or evaluator settings were lost")
	}
	child.Agent.SetGoal("different target")
	if a.GoalStatus().Condition != "finish" {
		t.Fatal("fork mutated parent target")
	}
	if _, err := a.Rewind(1, "conversation"); err != nil || a.GoalStatus().Status != "paused" {
		t.Fatalf("rewind did not pause: %v", err)
	}
	a.ClearConversation()
	if a.GoalStatus().Status != "" || len(a.Messages) != 1 {
		t.Fatal("clear retained goal")
	}
	a.isFork = true
	if err := a.SetGoal("background target"); err == nil {
		t.Fatal("background worker accepted goal")
	}
}

func TestGoalPauseEventsNeverBlockRunner(t *testing.T) {
	a := New(forkClientFunc(func(context.Context, []schema.Message, []tools.Tool) (*schema.LLMResponse, error) {
		return nil, errors.New("main API failed")
	}), "system", 5, 100000, nil)
	a.SetGoal("finish")
	a.AddUserMessage("start")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	events := make(chan GoalEvent) // no UI consumer
	if _, err := a.RunWithHITL(WithGoalEvents(ctx, events), nil, nil); err == nil || a.GoalStatus().Status != "paused" {
		t.Fatalf("main failure did not pause: %v", err)
	}
}

func TestGoalDefaultsToSeparateToolFreeCallOnMainClient(t *testing.T) {
	calls, evaluations := 0, 0
	a := New(forkClientFunc(func(_ context.Context, messages []schema.Message, list []tools.Tool) (*schema.LLMResponse, error) {
		calls++
		if contentString(messages[0]) == goalEvaluatorPrompt {
			evaluations++
			if len(list) != 0 || len(messages) != 2 {
				t.Fatal("default evaluator inherited agent state/tools")
			}
			return &schema.LLMResponse{Content: `{"satisfied":true,"reason":"verified"}`}, nil
		}
		return &schema.LLMResponse{Content: "done"}, nil
	}), "system", 5, 100000, []tools.Tool{&tools.AskUserTool{}})
	a.SetGoal("finish")
	a.AddUserMessage("start")
	if _, err := a.RunWithHITL(context.Background(), nil, nil); err != nil || calls != 2 || evaluations != 1 || a.GoalStatus().Status != "completed" {
		t.Fatalf("default evaluator failed: %v calls=%d checks=%d", err, calls, evaluations)
	}
}
