package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

func awaitMemoryTask(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for memory task")
	}
}

func TestManagerCloseCancelsAndWaitsForBackgroundSummary(t *testing.T) {
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	cfg := DefaultConfig()
	cfg.WorkingDir = t.TempDir()
	cfg.MaxInputLength, cfg.CompactReserve = 600, 80
	marker := filepath.Join(cfg.WorkingDir, "finished")
	client := manualClientFunc(func(ctx context.Context, _ []schema.Message, ts []tools.Tool) (*schema.LLMResponse, error) {
		if len(ts) == 0 {
			return &schema.LLMResponse{Content: manualSummary}, nil
		}
		close(started)
		<-ctx.Done()
		close(canceled)
		<-release
		if err := os.WriteFile(marker, []byte("finished"), 0600); err != nil {
			return nil, err
		}
		return nil, ctx.Err()
	})
	mgr := NewManager(cfg, client)
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); mgr.Close() })
	turn, cancelTurn := context.WithCancel(context.Background())
	messages := []schema.Message{{Role: "system", Content: "system"}, {Role: "user", Content: strings.Repeat("old conversation ", 300)}, {Role: "user", Content: "recent question"}}
	if _, _, err := mgr.CompactContext(turn, messages, "system"); err != nil {
		t.Fatal(err)
	}
	if err := mgr.QueueSummary(messages); err != nil {
		t.Fatal(err)
	}
	awaitMemoryTask(t, started)
	cancelTurn()
	select {
	case <-canceled:
		t.Fatal("ending a foreground turn canceled independent memory work")
	default:
	}
	done := make(chan struct{})
	go func() { mgr.Close(); close(done) }()
	awaitMemoryTask(t, canceled)
	select {
	case <-done:
		t.Fatal("Close returned before the worker finished")
	default:
	}
	releaseOnce.Do(func() { close(release) })
	awaitMemoryTask(t, done)
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("worker did not finish its final write before Close", err)
	}
	if err := mgr.Close(); err != nil || mgr.BackgroundError() != nil {
		t.Fatalf("normal shutdown cancellation was reported as failure: %v", err)
	}
	if _, _, err := mgr.CompactContext(context.Background(), messages, "system"); err == nil {
		t.Fatal("closed manager accepted another turn")
	}
}

func TestManagerRetainsBackgroundFailure(t *testing.T) {
	cfg := DefaultConfig()
	cfg.WorkingDir = t.TempDir()
	want := errors.New("summary service unavailable")
	mgr := NewManager(cfg, manualClientFunc(func(context.Context, []schema.Message, []tools.Tool) (*schema.LLMResponse, error) {
		return nil, want
	}))
	t.Cleanup(func() { mgr.Close() })
	mgr.QueueSummary([]schema.Message{{Role: "user", Content: "remember this"}})
	mgr.tasks.Wait()
	if !errors.Is(mgr.BackgroundError(), want) || !errors.Is(mgr.Close(), want) || !errors.Is(mgr.Close(), want) {
		t.Fatal("background failure was lost during or after shutdown")
	}
}

func TestManagerTaskRegistrationRacesWithConcurrentClose(t *testing.T) {
	cfg := DefaultConfig()
	cfg.WorkingDir = t.TempDir()
	var calls atomic.Int32
	mgr := NewManager(cfg, manualClientFunc(func(context.Context, []schema.Message, []tools.Tool) (*schema.LLMResponse, error) {
		calls.Add(1)
		return &schema.LLMResponse{Content: "done"}, nil
	}))
	t.Cleanup(func() { mgr.Close() })
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); mgr.QueueSummary([]schema.Message{{Role: "user", Content: "remember"}}) }()
		go func() { defer wg.Done(); mgr.Close() }()
	}
	wg.Wait()
	before := calls.Load()
	mgr.QueueSummary([]schema.Message{{Role: "user", Content: "after close"}})
	if calls.Load() != before {
		t.Fatal("a task started after shutdown")
	}
}

func TestSummaryQueueSnapshotsSerializesAndBoundsWork(t *testing.T) {
	cfg := DefaultConfig()
	cfg.WorkingDir = t.TempDir()
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var payloads []string
	mgr := NewManager(cfg, manualClientFunc(func(_ context.Context, msgs []schema.Message, _ []tools.Tool) (*schema.LLMResponse, error) {
		payloads = append(payloads, msgs[1].Content.(string))
		if len(payloads) == 1 {
			close(started)
			<-release
		}
		return &schema.LLMResponse{Content: "done"}, nil
	}))
	defer func() { once.Do(func() { close(release) }); mgr.Close() }()
	if err := mgr.QueueSummary([]schema.Message{{Role: "user", Content: "first payload"}}); err != nil {
		t.Fatal(err)
	}
	awaitMemoryTask(t, started)
	msg := []schema.Message{{Role: "user", Content: []interface{}{map[string]interface{}{"type": "text", "text": "original snapshot"}}}}
	if err := mgr.QueueSummary(msg); err != nil {
		t.Fatal(err)
	}
	msg[0].Content.([]interface{})[0].(map[string]interface{})["text"] = "mutated snapshot"
	for i := 1; i < maxQueuedSummaries; i++ {
		if err := mgr.QueueSummary([]schema.Message{{Role: "user", Content: "later payload"}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := mgr.QueueSummary([]schema.Message{{Role: "user", Content: "overflow"}}); err == nil {
		t.Fatal("unbounded queue")
	}
	once.Do(func() { close(release) })
	mgr.tasks.Wait()
	if len(payloads) != maxQueuedSummaries+1 || !strings.Contains(payloads[0], "first payload") || !strings.Contains(payloads[1], "original snapshot") || strings.Contains(payloads[1], "mutated snapshot") {
		t.Fatalf("snapshot or FIFO failure: %v", payloads)
	}
	mgr.Close()
	if err := mgr.QueueSummary([]schema.Message{{Role: "user", Content: "closed"}}); err == nil {
		t.Fatal("closed queue accepted work")
	}
}
