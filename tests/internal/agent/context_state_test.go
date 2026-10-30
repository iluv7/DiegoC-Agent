package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"diegoc-agent/internal/memory"
	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

func TestContextSummarySourceIsSharedByGoalSnapshotsAndDetachedForks(t *testing.T) {
	client := forkClientFunc(func(context.Context, []schema.Message, []tools.Tool) (*schema.LLMResponse, error) {
		return &schema.LLMResponse{Content: "answer"}, nil
	})
	root := New(client, "system", 5, 80000, nil)
	root.contextState.setSummary("standalone summary")
	cfg := memory.DefaultConfig()
	cfg.WorkingDir, cfg.MaxInputLength, cfg.Language = t.TempDir(), 32000, ""
	mgr := memory.NewManager(cfg, client)
	defer mgr.Close()
	mgr.SetCompressedSummary("loaded memory summary")
	root.SetMemoryManager(mgr)
	if root.contextState.summaryText() != "loaded memory summary" {
		t.Fatal("attaching memory replaced its existing summary")
	}
	// Simulate a summary committed by the automatic pipeline, without manually
	// synchronizing any Agent field before a goal check, checkpoint, or fork.
	mgr.SetCompressedSummary("latest memory summary")
	if err := root.SetGoal("verify context"); err != nil {
		t.Fatal(err)
	}
	var evidence struct{ Summary string }
	if err := json.Unmarshal([]byte(contentString(root.goalEvaluationMessages()[1])), &evidence); err != nil || evidence.Summary != "latest memory summary" {
		t.Fatalf("goal used a stale summary: %+v %v", evidence, err)
	}
	if root.snapshotContext().summary != "latest memory summary" {
		t.Fatal("checkpoint used a stale summary")
	}
	foreground := root.cloneConversation()
	forks := NewForkManager(context.Background())
	defer forks.Close()
	root.EnableForks(forks)
	id, err := forks.Fork(root, "inspect context")
	if err != nil {
		t.Fatal(err)
	}
	waitFork(t, forks, id, "completed")
	background := forks.sessions[id].agent
	for _, child := range []*Agent{foreground, background} {
		if child.contextState.manager != nil || child.contextState.summaryText() != "latest memory summary" {
			t.Fatal("fork shared mutable memory or inherited a stale summary")
		}
		limit, threshold, language := child.contextState.settings(child.TokenLimit)
		if limit != 32000 || threshold != child.TokenLimit || language != "" {
			t.Fatalf("detached fork lost its previous budget/language behavior: %d %d %q", limit, threshold, language)
		}
		child.ClearConversation()
	}
	if mgr.CompressedSummary() != "latest memory summary" {
		t.Fatal("clearing a fork changed its parent's summary")
	}
	root.SetMemoryManager(nil)
	mgr.SetCompressedSummary("manager changed after detachment")
	if root.contextState.summaryText() != "latest memory summary" {
		t.Fatal("detaching memory lost the effective summary or kept sharing it")
	}
	root.contextState.setSummary("standalone edit")
	if mgr.CompressedSummary() != "manager changed after detachment" {
		t.Fatal("standalone summary write reached the detached manager")
	}
	mgr.SetCompressedSummary("")
	root.SetMemoryManager(mgr)
	if root.contextState.summaryText() != "" {
		t.Fatal("an empty attached summary was replaced with stale standalone state")
	}
}

func TestContextRewindAndClearRestoreSummaryWithAndWithoutMemory(t *testing.T) {
	for _, backend := range []string{"standalone", "memory"} {
		t.Run(backend, func(t *testing.T) {
			a := New(nil, "system", 5, 20000, nil)
			var mgr *memory.Manager
			if backend == "memory" {
				cfg := memory.DefaultConfig()
				cfg.WorkingDir = t.TempDir()
				mgr = memory.NewManager(cfg, nil)
				defer mgr.Close()
				a.SetMemoryManager(mgr)
			}
			limit := a.ContextUsage().Limit
			a.AddUserMessage("first")
			a.Messages = append(a.Messages, schema.Message{Role: "assistant", ToolCalls: []schema.ToolCall{{ID: "call", Function: schema.FunctionCall{Arguments: map[string]interface{}{"value": "saved"}}}}})
			a.contextState.setSummary("saved summary")
			saved := cloneMessages(a.Messages)
			a.AddUserMessage("second")
			a.Messages[2].ToolCalls[0].Function.Arguments["value"] = "mutated"
			a.contextState.setSummary("later summary")
			a.contextState.skipNextTokenCheck = true
			if _, err := a.Rewind(2, "files"); err != nil || a.contextState.summaryText() != "later summary" || !a.contextState.skipNextTokenCheck {
				t.Fatalf("files-only rewind changed context: %v", err)
			}
			if _, err := a.Rewind(2, "conversation"); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(saved, a.Messages) || a.contextState.summaryText() != "saved summary" || a.contextState.skipNextTokenCheck {
				t.Fatal("conversation rewind lost the snapshot or compaction skip behavior")
			}
			if mgr != nil && mgr.CompressedSummary() != "saved summary" {
				t.Fatal("rewind did not restore the memory summary")
			}
			a.ClearConversation()
			if a.contextState.summaryText() != "" || len(a.Messages) != 1 || a.Messages[0].Content != "system" || a.ContextUsage().Limit != limit {
				t.Fatal("clear left context state or changed the configured budget")
			}
			if mgr != nil && mgr.CompressedSummary() != "" {
				t.Fatal("clear did not reset the memory summary")
			}
		})
	}
}

func TestContextPreparationKeepsBothAutomaticCompactionStrategies(t *testing.T) {
	var calls int
	legacy := New(forkClientFunc(func(context.Context, []schema.Message, []tools.Tool) (*schema.LLMResponse, error) {
		calls++
		return &schema.LLMResponse{Content: "legacy execution summary"}, nil
	}), "system", 5, 1, nil)
	legacy.AddUserMessage("task")
	legacy.Messages = append(legacy.Messages, schema.Message{Role: "assistant", Content: strings.Repeat("details ", 100)})
	if err := legacy.prepareContext(context.Background()); err != nil || calls != 1 || !strings.Contains(contentString(legacy.Messages[2]), "[Assistant Execution Summary]") {
		t.Fatalf("legacy automatic summary changed: calls=%d err=%v", calls, err)
	}
	if err := legacy.prepareContext(context.Background()); err != nil || calls != 1 {
		t.Fatal("legacy compaction no longer skips the following token check")
	}

	client := forkClientFunc(func(_ context.Context, _ []schema.Message, tools []tools.Tool) (*schema.LLMResponse, error) {
		if len(tools) > 0 {
			return nil, errors.New("long-term memory writing disabled in test")
		}
		return &schema.LLMResponse{Content: compactTestSummary}, nil
	})
	cfg := memory.DefaultConfig()
	cfg.WorkingDir, cfg.MaxInputLength, cfg.CompactReserve = t.TempDir(), 600, 80
	mgr := memory.NewManager(cfg, client)
	defer mgr.Close()
	a := New(client, strings.Repeat("system constraints ", 100), 5, 20000, nil)
	a.SetMemoryManager(mgr)
	a.Messages = append(a.Messages, schema.Message{Role: "user", Content: strings.Repeat("old conversation ", 300)}, schema.Message{Role: "user", Content: "recent question"})
	if err := a.prepareContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mgr.CompressedSummary() != compactTestSummary || a.contextState.summaryText() != compactTestSummary || !memory.IsSummaryMessage(a.Messages[1]) || a.Messages[0].Content != a.SystemPrompt {
		t.Fatal("memory pipeline lost its summary or system instructions")
	}
	// A no-op pipeline pass must retain the previous summary for future forks.
	a.Messages = []schema.Message{{Role: "system", Content: a.SystemPrompt}}
	if err := a.prepareContext(context.Background()); err != nil || a.cloneConversation().contextState.summaryText() != compactTestSummary {
		t.Fatalf("a no-op memory pass erased the summary: %v", err)
	}
}
