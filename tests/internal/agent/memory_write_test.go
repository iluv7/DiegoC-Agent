package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"diegoc-agent/internal/memory"
	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

func writeTestManager(t *testing.T) (*memory.Manager, chan string) {
	t.Helper()
	writes := make(chan string, 128)
	cfg := memory.DefaultConfig()
	cfg.WorkingDir = t.TempDir()
	cfg.MaxInputLength = 100000
	mgr := memory.NewManager(cfg, forkClientFunc(func(_ context.Context, msgs []schema.Message, _ []tools.Tool) (*schema.LLMResponse, error) {
		writes <- contentString(msgs[1])
		return &schema.LLMResponse{Content: "saved"}, nil
	}))
	t.Cleanup(func() { mgr.Close() })
	return mgr, writes
}

// FIFO barrier lets assertions inspect every accepted job without sleeping.
func collectedWrites(t *testing.T, mgr *memory.Manager, writes <-chan string) []string {
	t.Helper()
	const barrier = "MEMORY_TEST_BARRIER_1729"
	if err := mgr.QueueSummary([]schema.Message{{Role: "user", Content: barrier}}); err != nil {
		t.Fatal(err)
	}
	var result []string
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case text := <-writes:
			if strings.Contains(text, barrier) {
				return result
			}
			result = append(result, text)
		case <-timer.C:
			t.Fatal("memory worker did not drain")
			return nil
		}
	}
}

func TestMemoryWriteReplyDeltasAndForkIsolation(t *testing.T) {
	mgr, writes := writeTestManager(t)
	a := New(forkClientFunc(func(context.Context, []schema.Message, []tools.Tool) (*schema.LLMResponse, error) {
		return &schema.LLMResponse{Content: "answer"}, nil
	}), "system", 5, 100000, nil)
	a.SetMemoryManager(mgr)
	// Equal text in separate turns must not be deduplicated by content alone.
	for i := 0; i < 2; i++ {
		a.AddUserMessage("remember my preference")
		if _, err := a.RunWithHITL(context.Background(), nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	payloads := collectedWrites(t, mgr, writes)
	if len(payloads) != 2 {
		t.Fatalf("expected one delta per reply: %v", payloads)
	}
	for _, payload := range payloads {
		if strings.Count(payload, "remember my preference") != 1 || strings.Count(payload, "answer") != 1 {
			t.Fatalf("history replayed or repeated turn lost: %s", payload)
		}
	}
	for _, kind := range []sessionCloneKind{foregroundSessionClone, backgroundTaskClone} {
		child := a.cloneSession(kind, nil, "task")
		if _, err := child.RunWithHITL(context.Background(), nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := collectedWrites(t, mgr, writes); len(got) != 0 {
		t.Fatalf("fork wrote parent memory: %v", got)
	}
	a.ClearConversation()
	a.AddUserMessage("remember my preference")
	if _, err := a.RunWithHITL(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := collectedWrites(t, mgr, writes); len(got) != 1 || strings.Count(got[0], "remember my preference") != 1 {
		t.Fatalf("clear did not reset prefix: %v", got)
	}
}

func TestMemoryWriteBeforeAutomaticAndManualCompaction(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		t.Run(map[bool]string{false: "manual", true: "automatic"}[automatic], func(t *testing.T) {
			mgr, writes := writeTestManager(t)
			if automatic { // Low budget on the manager triggers real automatic compaction.
				cfg := memory.DefaultConfig()
				cfg.WorkingDir = t.TempDir()
				cfg.MaxInputLength = 600
				cfg.CompactReserve = 80
				cfg.Language = ""
				mgr = memory.NewManager(cfg, forkClientFunc(func(_ context.Context, msgs []schema.Message, ts []tools.Tool) (*schema.LLMResponse, error) {
					if len(ts) == 0 {
						return &schema.LLMResponse{Content: compactTestSummary}, nil
					}
					writes <- contentString(msgs[1])
					return &schema.LLMResponse{Content: "saved"}, nil
				}))
				defer mgr.Close()
			}
			a := New(forkClientFunc(func(_ context.Context, msgs []schema.Message, _ []tools.Tool) (*schema.LLMResponse, error) {
				if strings.Contains(contentString(msgs[0]), "上下文压缩助手") {
					return &schema.LLMResponse{Content: compactTestSummary}, nil
				}
				return &schema.LLMResponse{Content: "final answer"}, nil
			}), "system", 5, 20000, nil)
			a.SetMemoryManager(mgr)
			a.AddUserMessage("original fact " + strings.Repeat("long history ", 300))
			a.Messages = append(a.Messages, schema.Message{Role: "assistant", Content: "old answer"})
			a.AddUserMessage("recent question")
			if !automatic {
				if _, err := a.Compact(context.Background(), ""); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := a.RunWithHITL(context.Background(), nil, nil); err != nil {
				t.Fatal(err)
			}
			payloads := collectedWrites(t, mgr, writes)
			all := strings.Join(payloads, "\n")
			if len(payloads) != 2 || strings.Count(all, "original fact") != 1 || strings.Count(all, "recent question") != 1 || strings.Count(all, "final answer") != 1 || strings.Contains(all, "## Key Decisions") {
				t.Fatalf("lost original turns or replayed summary: %v", payloads)
			}
		})
	}
}

func TestMemoryWriteDisabledFailuresAndQueueRejection(t *testing.T) {
	mgr, writes := writeTestManager(t)
	a := New(nil, "system", 5, 100000, nil)
	a.SetMemoryManager(mgr)
	a.AddUserMessage("keep original")
	hook := &MemoryWriteMiddleware{}
	want := errors.New("model failed")
	if _, err := hook.OnReply(context.Background(), a, func(context.Context) (string, error) { return "", want }); !errors.Is(err, want) {
		t.Fatal(err)
	}
	if got := collectedWrites(t, mgr, writes); len(got) != 0 {
		t.Fatal("failed reply wrote partial response")
	}
	a.SetMiddlewareFactories() // No writer means no implicit writes inside compaction.
	before := cloneMessages(a.Messages)
	a.LLM = forkClientFunc(func(context.Context, []schema.Message, []tools.Tool) (*schema.LLMResponse, error) {
		return &schema.LLMResponse{Content: compactTestSummary}, nil
	})
	longConversation(a)
	if _, err := a.Compact(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if got := collectedWrites(t, mgr, writes); len(got) != 0 {
		t.Fatal("disabled writer still submitted memory")
	}
	// A queue rejection must not advance the accepted prefix or discard history.
	a.Messages = before
	a.SetMiddlewareFactories(func() any { return hook })
	mgr.Close()
	snapshot := cloneMessages(a.Messages)
	if err := a.beforeContextCompaction(context.Background(), []schema.Message{memory.SummaryMessage("replacement")}); err == nil || !reflect.DeepEqual(snapshot, a.Messages) || len(hook.covered) != 0 {
		t.Fatal("rejected submission changed state")
	}
}

type rejectingCompactionObserver struct{ reject bool }

func (m *rejectingCompactionObserver) OnContextCompaction(context.Context, *Agent, ContextCompactionEvent) error {
	if m.reject {
		return errors.New("replacement rejected")
	}
	return nil
}

func TestMemoryWriteCompactionRetryDoesNotReplayAcceptedWork(t *testing.T) {
	mgr, writes := writeTestManager(t)
	reject := &rejectingCompactionObserver{reject: true}
	writer := &MemoryWriteMiddleware{}
	a := New(forkClientFunc(func(context.Context, []schema.Message, []tools.Tool) (*schema.LLMResponse, error) {
		return &schema.LLMResponse{Content: compactTestSummary}, nil
	}), "system", 5, 20000, nil)
	a.SetMemoryManager(mgr)
	a.SetMiddlewareFactories(func() any { return writer }, func() any { return reject })
	longConversation(a)
	before := cloneMessages(a.Messages)
	if _, err := a.Compact(context.Background(), ""); err == nil || !reflect.DeepEqual(before, a.Messages) || mgr.CompressedSummary() != "" {
		t.Fatal("rejected compaction changed context")
	}
	reject.reject = false
	if _, err := a.Compact(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if got := collectedWrites(t, mgr, writes); len(got) != 1 {
		t.Fatalf("compaction retry replayed accepted messages: %v", got)
	}
}

func TestMemoryWriteWaitsForValidCompaction(t *testing.T) {
	for _, mode := range []string{"invalid", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			writes := make(chan string, 128)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cfg := memory.DefaultConfig()
			cfg.WorkingDir = t.TempDir()
			cfg.MaxInputLength = 600
			cfg.CompactReserve = 80
			mgr := memory.NewManager(cfg, forkClientFunc(func(_ context.Context, msgs []schema.Message, ts []tools.Tool) (*schema.LLMResponse, error) {
				if len(ts) > 0 {
					writes <- contentString(msgs[1])
					return &schema.LLMResponse{Content: "saved"}, nil
				}
				if mode == "cancel" {
					cancel()
					return &schema.LLMResponse{Content: compactTestSummary}, nil
				}
				return &schema.LLMResponse{Content: "invalid"}, nil
			}))
			defer mgr.Close()
			a := New(nil, "system", 5, 100000, nil)
			a.SetMemoryManager(mgr)
			a.AddUserMessage(strings.Repeat("old conversation ", 300))
			a.AddUserMessage("recent question")
			before := cloneMessages(a.Messages)
			hook := &ContextCompactionMiddleware{}
			_, err := hook.OnReasoning(ctx, a, func(context.Context) (*schema.LLMResponse, error) { return &schema.LLMResponse{}, nil })
			if mode == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
			if !reflect.DeepEqual(before, a.Messages) || mgr.CompressedSummary() != "" {
				t.Fatal("failed compaction changed context")
			}
			if got := collectedWrites(t, mgr, writes); len(got) != 0 {
				t.Fatal("failed compaction submitted memory")
			}
		})
	}
}
