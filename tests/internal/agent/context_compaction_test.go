package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"diegoc-agent/internal/memory"
	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

func TestCompactionMiddlewarePreparesModelRequest(t *testing.T) {
	for _, backed := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "memory"}[backed], func(t *testing.T) {
			summaries, models := 0, 0
			client := forkClientFunc(func(_ context.Context, msgs []schema.Message, ts []tools.Tool) (*schema.LLMResponse, error) {
				if len(ts) > 0 {
					return &schema.LLMResponse{Content: "nothing to save"}, nil
				}
				if strings.Contains(contentString(msgs[0]), "context compaction assistant") || strings.Contains(contentString(msgs[0]), "skilled at summarizing") {
					summaries++
					return &schema.LLMResponse{Content: compactTestSummary}, nil
				}
				models++
				found := false
				for _, msg := range msgs {
					if strings.Contains(contentString(msg), compactTestSummary) {
						found = true
					}
				}
				if !found || msgs[0].Content != "system" {
					t.Errorf("model did not receive prepared context: %+v", msgs)
				}
				return &schema.LLMResponse{Content: "done"}, nil
			})
			a := New(client, "system", 5, 100, nil)
			if backed {
				cfg := memory.DefaultConfig()
				cfg.WorkingDir = t.TempDir()
				cfg.MaxInputLength = 600
				cfg.CompactReserve = 80
				cfg.Language = ""
				// Background memory work uses a separate client to keep request assertions local.
				mgr := memory.NewManager(cfg, client)
				defer mgr.Close()
				a.SetMemoryManager(mgr)
			}
			a.AddUserMessage(strings.Repeat("old conversation ", 300))
			a.Messages = append(a.Messages, schema.Message{Role: "assistant", Content: "previous result"})
			a.AddUserMessage("current question")
			out, err := a.RunWithHITL(context.Background(), nil, nil)
			if err != nil || out != "done" || summaries == 0 || models != 1 {
				t.Fatalf("out=%q err=%v summaries=%d models=%d", out, err, summaries, models)
			}
			if backed && a.compressedSummary != compactTestSummary {
				t.Fatal("summary not synchronized")
			}
		})
	}
}

func TestCompactionMiddlewareCanBeDisabledAndPropagatesFailures(t *testing.T) {
	a := New(nil, "system", 5, 1, nil)
	a.AddUserMessage(strings.Repeat("large history ", 100))
	before := cloneMessages(a.Messages)
	a.SetMiddlewareFactories(func() any { return &testModelHook{} })
	if _, err := a.RunWithHITL(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, a.Messages[:len(before)]) {
		t.Fatal("unregistered compaction still changed history")
	}
	cfg := memory.DefaultConfig()
	cfg.WorkingDir = t.TempDir()
	mgr := memory.NewManager(cfg, nil)
	mgr.Close()
	a.SetMemoryManager(mgr)
	hook := &ContextCompactionMiddleware{}
	called := false
	terminal := func(context.Context) (*schema.LLMResponse, error) { called = true; return &schema.LLMResponse{}, nil }
	if _, err := hook.OnReasoning(context.Background(), a, terminal); err == nil || called {
		t.Fatal("closed manager failure reached model")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := hook.OnReasoning(ctx, a, terminal); !errors.Is(err, context.Canceled) || called {
		t.Fatal("cancellation reached compaction/model")
	}
}
