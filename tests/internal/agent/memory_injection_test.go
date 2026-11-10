package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"diegoc-agent/internal/memory"
	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

// Keyword-only retrieval exercises the real Manager/FileStore boundary without
// requiring embedding network calls or ChromaDB in middleware integration tests.
type unavailableEmbedding struct{}

func (unavailableEmbedding) Embed(context.Context, string) ([]float32, error) {
	return nil, errors.New("vector unavailable")
}
func (unavailableEmbedding) EmbedBatch(context.Context, []string) ([][]float32, error) {
	return nil, errors.New("vector unavailable")
}

type injectionKeywordStore struct {
	calls  atomic.Int32
	search func(context.Context, string, int) ([]memory.SearchResult, error)
}

func (*injectionKeywordStore) EnsureIndex(context.Context) error                       { return nil }
func (*injectionKeywordStore) IndexChunks(context.Context, []memory.MemoryChunk) error { return nil }
func (s *injectionKeywordStore) Search(ctx context.Context, q string, n int) ([]memory.SearchResult, error) {
	s.calls.Add(1)
	return s.search(ctx, q, n)
}
func (*injectionKeywordStore) Delete(context.Context, []string) error { return nil }
func (*injectionKeywordStore) Close() error                           { return nil }

func injectionTestManager(t *testing.T, search func(context.Context, string, int) ([]memory.SearchResult, error), client forkClientFunc) *memory.Manager {
	t.Helper()
	cfg := memory.DefaultConfig()
	cfg.WorkingDir = t.TempDir()
	cfg.MaxInputLength = 100000
	mgr := memory.NewManager(cfg, client)
	mgr.SetFileStore(memory.NewFileStore(unavailableEmbedding{}, nil, &injectionKeywordStore{search: search}, memory.NewRuleTokenCounter(), "test", 400, 80))
	t.Cleanup(func() { mgr.Close() })
	return mgr
}

func TestMemoryInjectionFreezesSnapshotAcrossTurnsAndForks(t *testing.T) {
	var queries []string
	mgr := injectionTestManager(t, func(_ context.Context, q string, _ int) ([]memory.SearchResult, error) {
		queries = append(queries, q)
		return []memory.SearchResult{{Path: "preferences.md", StartLine: 3, EndLine: 4, Text: "Prefer concise Chinese replies."}}, nil
	}, nil)
	modelCalls := 0
	client := forkClientFunc(func(_ context.Context, msgs []schema.Message, _ []tools.Tool) (*schema.LLMResponse, error) {
		modelCalls++
		if msgs[0].Content != "system rules" || !memory.IsInjectedMemory(msgs[1]) || msgs[1].Role != "user" {
			t.Fatalf("incorrect message order/authority: %+v", msgs)
		}
		count := 0
		for _, msg := range msgs {
			if memory.IsInjectedMemory(msg) {
				count++
			}
		}
		if count != 1 {
			t.Fatal("duplicate injection")
		}
		return &schema.LLMResponse{Content: "done"}, nil
	})
	a := New(client, "system rules", 5, 100000, nil)
	a.SetMemoryManager(mgr)
	// Keep only injection and compaction: this test concerns recall, not extraction.
	a.SetMiddlewareFactories(func() any { return &MemoryInjectionMiddleware{} }, func() any { return &ContextCompactionMiddleware{} })
	for _, q := range []string{"first question", "different question"} {
		a.AddUserMessage(q)
		if _, err := a.RunWithHITL(context.Background(), nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := a.Messages[1]
	if len(queries) != 1 || queries[0] != "first question" {
		t.Fatalf("lookup repeated or used wrong query: %v", queries)
	}
	usage := a.ContextUsage()
	if usage.Memory <= 0 || usage.Total != usage.System+usage.ToolDefinitions+usage.Summary+usage.Memory+usage.Conversation+usage.ToolResults {
		t.Fatalf("missing recall budget: %+v", usage)
	}
	for _, kind := range []sessionCloneKind{foregroundSessionClone, backgroundTaskClone} {
		child := a.cloneSession(kind, nil, "task")
		if child.memoryManager != nil || !reflect.DeepEqual(child.Messages[1], snapshot) {
			t.Fatal("fork snapshot inheritance changed")
		}
		if _, err := child.RunWithHITL(context.Background(), nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(queries) != 1 || modelCalls != 4 {
		t.Fatal("fork queried parent memory")
	}
	if _, err := a.Rewind(1, "conversation"); err != nil {
		t.Fatal(err)
	}
	a.AddUserMessage("rewound question")
	if _, err := a.RunWithHITL(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(queries) != 2 || queries[1] != "rewound question" {
		t.Fatal("rewind before injection did not refresh snapshot")
	}
	a.ClearConversation()
	a.AddUserMessage("fresh question")
	if _, err := a.RunWithHITL(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(queries) != 3 || queries[2] != "fresh question" {
		t.Fatal("clear did not refresh snapshot")
	}
}

func TestMemoryInjectionTimeoutRetryAndCancellation(t *testing.T) {
	calls := 0
	mgr := injectionTestManager(t, func(ctx context.Context, _ string, _ int) ([]memory.SearchResult, error) {
		calls++
		if calls == 1 {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return []memory.SearchResult{{Text: "recovered memory", Path: "memory.md"}}, nil
	}, nil)
	a := New(nil, "system", 5, 100000, nil)
	a.SetMemoryManager(mgr)
	a.AddUserMessage("question")
	hook := &MemoryInjectionMiddleware{Timeout: time.Millisecond}
	terminals := 0
	next := func(context.Context) (string, error) { terminals++; return "done", nil }
	if _, err := hook.OnReply(context.Background(), a, next); err != nil || hook.loaded {
		t.Fatal("optional lookup timeout blocked reply or froze failure")
	}
	if _, err := hook.OnReply(context.Background(), a, next); err != nil || !hook.loaded {
		t.Fatal("later lookup did not recover")
	}
	if terminals != 2 || !memory.IsInjectedMemory(a.Messages[1]) {
		t.Fatal("lookup retry failed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := hook.OnReply(ctx, a, next); !errors.Is(err, context.Canceled) || terminals != 2 {
		t.Fatal("canceled reply reached model")
	}
	disabled := &MemoryInjectionMiddleware{Disabled: true}
	if _, err := disabled.OnReply(context.Background(), a, next); err != nil {
		t.Fatal(err)
	}
	for _, msg := range a.Messages {
		if memory.IsInjectedMemory(msg) {
			t.Fatal("disabled middleware retained recalled context")
		}
	}
}

func TestMemoryInjectionEmptySnapshotAndNoStore(t *testing.T) {
	calls := 0
	mgr := injectionTestManager(t, func(context.Context, string, int) ([]memory.SearchResult, error) { calls++; return nil, nil }, nil)
	a := New(nil, "system", 5, 100000, nil)
	a.SetMemoryManager(mgr)
	a.AddUserMessage("question")
	hook := &MemoryInjectionMiddleware{}
	next := func(context.Context) (string, error) { return "done", nil }
	for i := 0; i < 2; i++ {
		if _, err := hook.OnReply(context.Background(), a, next); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 || len(a.Messages) != 2 {
		t.Fatal("empty lookup repeated or created an empty message")
	}
	a.SetMemoryManager(nil)
	if _, err := hook.OnReply(context.Background(), a, next); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("unconfigured memory still queried")
	}
}

func TestMemoryInjectionPreservedByCompactionWithoutExtractionFeedback(t *testing.T) {
	for _, mode := range []string{"manual", "legacy", "memory"} {
		t.Run(mode, func(t *testing.T) {
			var summaryCalls int
			client := forkClientFunc(func(_ context.Context, msgs []schema.Message, ts []tools.Tool) (*schema.LLMResponse, error) {
				if strings.Contains(contentString(msgs[1]), "FROZEN_MEMORY_FACT") {
					t.Error("injected memory was fed back to summarization/extraction")
				}
				summaryCalls++
				return &schema.LLMResponse{Content: compactTestSummary}, nil
			})
			a := New(client, "system", 5, 50000, nil)
			frozen := memory.InjectedMemoryMessage("FROZEN_MEMORY_FACT")
			a.Messages = append(a.Messages, frozen)
			longConversation(a)
			switch mode {
			case "manual":
				if _, err := a.Compact(context.Background(), ""); err != nil {
					t.Fatal(err)
				}
			case "legacy":
				a.TokenLimit = 100
				if err := a.summarizeIfNeeded(context.Background()); err != nil {
					t.Fatal(err)
				}
			case "memory":
				cfg := memory.DefaultConfig()
				cfg.WorkingDir = t.TempDir()
				cfg.MaxInputLength = 600
				cfg.CompactReserve = 80
				mgr := memory.NewManager(cfg, client)
				defer mgr.Close()
				messages, summary, err := mgr.CompactContext(context.Background(), a.Messages, "system")
				if err != nil || summary == "" {
					t.Fatalf("automatic compact: %v", err)
				}
				a.Messages = messages
			}
			if summaryCalls == 0 || !reflect.DeepEqual(a.Messages[1], frozen) {
				t.Fatal("compaction lost frozen memory")
			}
			for _, msg := range memoryTurns(a.Messages) {
				if memory.IsInjectedMemory(msg) || strings.Contains(contentString(msg), "FROZEN_MEMORY_FACT") {
					t.Fatal("memory extraction included recalled facts")
				}
			}
		})
	}
}

func TestMemoryInjectionBudgetAndMultimodalQuery(t *testing.T) {
	text := strings.Repeat("中文记忆🙂", 1000) + "</memory>"
	results := []memory.SearchResult{{Path: "facts.md", Text: text, StartLine: 1}, {Path: "duplicate.md", Text: text}}
	msg, ok := renderInjectedMemory(results, 150)
	if !ok || memory.NewRuleTokenCounter().CountMessages([]schema.Message{msg}) > 150 || !utf8.ValidString(msg.Content.(string)) || !strings.Contains(msg.Content.(string), "facts.md") {
		t.Fatal("invalid or over-budget memory")
	}
	if msg, ok := renderInjectedMemory(results, 1); ok || memory.IsInjectedMemory(msg) {
		t.Fatal("tiny budget created a message")
	}
	query := injectionQuery([]schema.Message{{Role: "user", Content: []interface{}{map[string]interface{}{"type": "image", "source": "image data"}, map[string]interface{}{"type": "text", "text": strings.Repeat("问", 1200)}}}})
	if len([]rune(query)) != 1000 || strings.Contains(query, "image data") {
		t.Fatal("query not bounded to user text")
	}
}

func TestMemoryInjectionDoesNotSplitToolMessagePairs(t *testing.T) {
	mgr := injectionTestManager(t, func(context.Context, string, int) ([]memory.SearchResult, error) {
		return []memory.SearchResult{{Path: "prefs.md", Text: "Use Chinese."}}, nil
	}, nil)
	a := New(nil, "system", 5, 100000, nil)
	a.SetMemoryManager(mgr)
	a.Messages = append(a.Messages, schema.Message{Role: "user", Content: "old task"}, schema.Message{Role: "assistant", ToolCalls: []schema.ToolCall{executorTestCall("call", "read", nil)}}, schema.Message{Role: "tool", ToolCallID: "call", Content: "result"})
	a.AddUserMessage("current question")
	original := cloneMessages(a.Messages)
	hook := &MemoryInjectionMiddleware{}
	for i := 0; i < 2; i++ {
		if _, err := hook.OnReply(context.Background(), a, func(context.Context) (string, error) { return "done", nil }); err != nil {
			t.Fatal(err)
		}
	}
	if !memory.IsInjectedMemory(a.Messages[1]) || !reflect.DeepEqual(original[1:], a.Messages[2:]) {
		t.Fatal("late or repeated injection changed tool message sequence")
	}
}
