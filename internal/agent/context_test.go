package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"diegoc-agent/internal/memory"
	"diegoc-agent/internal/permission"
	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

const compactTestSummary = "## Goal\nBuild storage.\n## Constraints\nKeep the API contract.\n## Progress\nFiles updated.\n## Key Decisions\nUse SQLite.\n## Next Steps\nRun tests.\n## Critical Context\nKeep src/storage.go."

func longConversation(a *Agent) {
	a.AddUserMessage("保留接口约定：" + strings.Repeat("用户需求与约定。", 500))
	a.Messages = append(a.Messages, schema.Message{Role: "assistant", Content: strings.Repeat("已完成的实现细节。", 500)})
}

func TestContextUsageIsReadOnlyAndCountsAllCategories(t *testing.T) {
	a := New(nil, "system prompt", 5, 10000, []tools.Tool{&tools.AskUserTool{}})
	a.Messages = append(a.Messages, memory.SummaryMessage("stored summary"), schema.Message{Role: "user", Content: "hello"}, schema.Message{Role: "assistant", Thinking: "reasoning", ToolCalls: []schema.ToolCall{{ID: "call", Function: schema.FunctionCall{Name: "read_file", Arguments: map[string]interface{}{"path": "long-file.go"}}}}}, schema.Message{Role: "tool", Content: strings.Repeat("result ", 20), ToolCallID: "call", Name: "read_file"})
	a.APITotalTokens = 999999
	before := cloneMessages(a.Messages)
	usage := a.ContextUsage()
	if usage.System <= 0 || usage.Summary <= 0 || usage.Conversation <= 0 || usage.ToolDefinitions <= 0 || usage.ToolResults <= 0 {
		t.Fatalf("missing category: %+v", usage)
	}
	if usage.Total != usage.System+usage.Summary+usage.Conversation+usage.ToolDefinitions+usage.ToolResults || usage.Total >= a.APITotalTokens {
		t.Fatalf("incorrect total: %+v", usage)
	}
	if !reflect.DeepEqual(before, cloneMessages(a.Messages)) || a.APITotalTokens != 999999 {
		t.Fatal("context command changed history or counters")
	}
	// Accumulated API cost alone must not invoke a summarizer.
	if err := a.summarizeIfNeeded(context.Background()); err != nil || !reflect.DeepEqual(before, cloneMessages(a.Messages)) {
		t.Fatal("billing tokens triggered compaction of a small context")
	}
}

func TestCompactPreservesPlanPermissionsRewindAndSessionIsolation(t *testing.T) {
	client := forkClientFunc(func(_ context.Context, msgs []schema.Message, ts []tools.Tool) (*schema.LLMResponse, error) {
		if len(ts) > 0 || !strings.Contains(msgs[1].Content.(string), "保留接口约定") {
			t.Fatal("compaction lost user's preservation instruction or enabled tools")
		}
		return &schema.LLMResponse{Content: compactTestSummary, Usage: &schema.TokenUsage{TotalTokens: 80}}, nil
	})
	root := New(client, "system", 5, 20000, nil)
	cfg := memory.DefaultConfig()
	cfg.WorkingDir, cfg.MaxInputLength = t.TempDir(), 30000
	mgr := memory.NewManager(cfg, client)
	defer mgr.Close()
	root.SetMemoryManager(mgr)
	root.SetPlanMode(true)
	longConversation(root)
	before := cloneMessages(root.Messages)
	sessions := NewConversationSessions(root)
	child := mustForkSession(t, sessions).Agent
	if child.ContextUsage().Limit != 30000 {
		t.Fatal("foreground clone lost configured context budget")
	}
	stats, err := child.Compact(context.Background(), "保留接口约定")
	if err != nil || stats.After.Total >= stats.Before.Total || child.APITotalTokens != 80 {
		t.Fatalf("compact child: %+v %v", stats, err)
	}
	if !reflect.DeepEqual(before, cloneMessages(root.Messages)) || mgr.CompressedSummary() != "" {
		t.Fatal("child compaction changed parent conversation or memory")
	}
	stats, err = root.Compact(context.Background(), "保留接口约定")
	if err != nil || stats.After.Summary == 0 || stats.After.Total >= stats.Before.Total {
		t.Fatalf("compact root: %+v %v", stats, err)
	}
	if root.Messages[0].Content != before[0].Content || root.PermissionCtx.Mode != permission.ModeExplore || !root.PlanMode || len(root.Checkpoints()) != 1 || mgr.CompressedSummary() != compactTestSummary {
		t.Fatal("compaction changed plan, permissions, checkpoints, or failed to sync summary")
	}
	if len(root.Messages) != 2 || root.Messages[1].Role != "user" || !memory.IsSummaryMessage(root.Messages[1]) {
		t.Fatal("summary could replace system prompt or leave orphaned tool calls")
	}
	root.AddUserMessage("next turn")
	if _, err := root.Rewind(2, "conversation"); err != nil || len(root.Messages) != 2 || mgr.CompressedSummary() != compactTestSummary {
		t.Fatal("rewind after compaction did not restore compacted context")
	}
	if _, err := root.Rewind(1, "conversation"); err != nil || len(root.Messages) != 1 || mgr.CompressedSummary() != "" {
		t.Fatal("compaction destroyed earlier rewind point")
	}
}

func TestCompactFailureOrCancellationKeepsOriginalState(t *testing.T) {
	for _, mode := range []string{"api error", "invalid", "cancel", "larger"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client := forkClientFunc(func(context.Context, []schema.Message, []tools.Tool) (*schema.LLMResponse, error) {
				if mode == "api error" {
					return nil, errors.New("offline")
				}
				if mode == "cancel" {
					cancel()
				}
				summary := compactTestSummary
				if mode == "invalid" {
					summary = "not structured"
				}
				if mode == "larger" {
					summary += strings.Repeat("unnecessary expanded detail ", 1000)
				}
				return &schema.LLMResponse{Content: summary, Usage: &schema.TokenUsage{TotalTokens: 40}}, nil
			})
			a := New(client, "system", 5, 20000, nil)
			longConversation(a)
			a.compressedSummary, a.needsToolRepair = "previous summary", true
			before := cloneMessages(a.Messages)
			checkpoints := a.Checkpoints()
			_, err := a.Compact(ctx, "")
			if err == nil || !reflect.DeepEqual(before, cloneMessages(a.Messages)) || a.compressedSummary != "previous summary" || !a.needsToolRepair || !reflect.DeepEqual(checkpoints, a.Checkpoints()) {
				t.Fatalf("failed compaction changed state: %v", err)
			}
			if mode != "api error" && a.APITotalTokens != 40 {
				t.Fatal("spent compaction tokens not recorded")
			}
		})
	}
}

func TestEmptyAndSummaryOnlyCompactDoNotRequestModel(t *testing.T) {
	a := New(nil, "system", 5, 10000, nil)
	if _, err := a.Compact(context.Background(), ""); err == nil {
		t.Fatal("empty conversation was compacted")
	}
	a.Messages = append(a.Messages, memory.SummaryMessage(compactTestSummary))
	if _, err := a.Compact(context.Background(), ""); err == nil || len(a.Messages) != 2 {
		t.Fatal("summary-only compaction changed context")
	}
}

func TestCompactPartialFailureDoesNotApplyIntermediateSummary(t *testing.T) {
	var calls int
	client := forkClientFunc(func(context.Context, []schema.Message, []tools.Tool) (*schema.LLMResponse, error) {
		calls++
		if calls == 2 {
			return nil, errors.New("second fragment failed")
		}
		return &schema.LLMResponse{Content: compactTestSummary, Usage: &schema.TokenUsage{TotalTokens: 20}}, nil
	})
	a := New(client, "system", 5, 1800, nil)
	longConversation(a)
	before := cloneMessages(a.Messages)
	if _, err := a.Compact(context.Background(), ""); err == nil || calls != 2 || !reflect.DeepEqual(before, cloneMessages(a.Messages)) || a.compressedSummary != "" || a.APITotalTokens != 20 {
		t.Fatalf("partial summary was applied or billed tokens lost: calls=%d tokens=%d err=%v", calls, a.APITotalTokens, err)
	}
}

func TestConversationContinuesWithSummaryAndOriginalSystem(t *testing.T) {
	client := forkClientFunc(func(_ context.Context, msgs []schema.Message, _ []tools.Tool) (*schema.LLMResponse, error) {
		if strings.Contains(msgs[0].Content.(string), "上下文压缩助手") {
			return &schema.LLMResponse{Content: compactTestSummary}, nil
		}
		if msgs[0].Content != "original system rules" || msgs[1].Role != "user" || !memory.IsSummaryMessage(msgs[1]) || msgs[len(msgs)-1].Content != "continue testing" {
			t.Fatalf("next request lost system/summary/user task: %+v", msgs)
		}
		return &schema.LLMResponse{Content: "continued"}, nil
	})
	a := New(client, "original system rules", 5, 20000, nil)
	longConversation(a)
	if _, err := a.Compact(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	a.AddUserMessage("continue testing")
	out, err := a.RunWithHITL(context.Background(), nil, nil)
	if err != nil || out != "continued" {
		t.Fatalf("continue: %s %v", out, err)
	}
}
