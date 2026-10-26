package loop

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseInterval(t *testing.T) {
	for value, want := range map[string]time.Duration{"1s": time.Minute, "61s": 2 * time.Minute, "5m": 5 * time.Minute, "2H": 2 * time.Hour, "1d": 24 * time.Hour} {
		got, err := ParseInterval(value)
		if err != nil || got != want {
			t.Errorf("%s: got %s, %v; want %s", value, got, err, want)
		}
	}
	for _, value := range []string{"", "0m", "-1m", "1.5h", "5x", "7d", "8d", "999999999999999999999999m"} {
		if _, err := ParseInterval(value); err == nil {
			t.Errorf("accepted invalid interval %q", value)
		}
	}
}

func TestDueWorkIsSessionScopedAndMissedRunsCoalesce(t *testing.T) {
	now := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	s := New(func() time.Time { return now })
	first, _ := s.Create("main", "watch CI", 5*time.Minute)
	second, _ := s.Create("main", "watch deployment", 5*time.Minute)
	other, _ := s.Create("session-1", "other task", time.Minute)
	if s.HasDue("main") {
		t.Fatal("ran immediately instead of waiting one interval")
	}
	now = now.Add(16 * time.Minute) // busy for three intervals
	task, ok := s.TakeDue("main")
	if !ok || task.ID != first.ID || task.Runs != 1 {
		t.Fatalf("first pending run: %+v %v", task, ok)
	}
	// A running task cannot be taken a second time, and tie order is stable.
	if next, ok := s.TakeDue("main"); !ok || next.ID != second.ID {
		t.Fatalf("next pending run: %+v %v", next, ok)
	}
	if s.HasDue("main") {
		t.Fatal("running tasks were re-enqueued")
	}
	now = now.Add(11 * time.Minute) // first run itself takes multiple intervals
	s.Complete("main", first.ID)
	s.Complete("main", second.ID)
	if s.HasDue("main") || s.List("main")[0].NextRun.Minute() != 30 {
		t.Fatalf("missed runs were replayed: %+v", s.List("main"))
	}
	if task, ok := s.TakeDue("session-1"); !ok || task.ID != other.ID {
		t.Fatal("inactive session's work was lost or transferred")
	}
	if s.Cancel("main", other.ID) != 0 || len(s.List("session-1")) != 1 {
		t.Fatal("cancel crossed a session boundary")
	}
	if s.Cancel("main", "all") != 2 || s.HasDue("main") {
		t.Fatal("failed to cancel all tasks")
	}
}

func TestLimitsExpiryAndCancelledCompletion(t *testing.T) {
	now := time.Now()
	s := New(func() time.Time { return now })
	for i := 0; i < MaxTasks; i++ {
		if _, err := s.Create("main", fmt.Sprint(i), time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Create("main", "too many", time.Minute); err == nil {
		t.Fatal("task limit not enforced")
	}
	if _, err := s.Create("other", "separate limit", time.Minute); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	task, _ := s.TakeDue("main")
	s.Cancel("main", task.ID)
	s.Complete("main", task.ID)
	if len(s.List("main")) != MaxTasks-1 {
		t.Fatal("completion resurrected cancelled task")
	}
	now = now.Add(Lifetime)
	if s.HasDue("main") || len(s.List("other")) != 0 {
		t.Fatal("expired tasks still runnable")
	}
	if _, err := s.Create("main", "new after expiry", time.Minute); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		prompt string
		d      time.Duration
	}{{"", time.Minute}, {strings.Repeat("x", MaxPromptBytes+1), time.Minute}, {"task", time.Second}, {"task", 90 * time.Second}} {
		if _, err := s.Create("main", tc.prompt, tc.d); err == nil {
			t.Fatal("invalid task accepted")
		}
	}
}

func TestSchedulerConcurrentAccess(t *testing.T) {
	s := New(nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			session := fmt.Sprint(i)
			for j := 0; j < 10; j++ {
				task, err := s.Create(session, "task", time.Minute)
				if err != nil {
					t.Error(err)
				}
				s.List(session)
				s.HasDue(session)
				s.TakeDue(session)
				s.Complete(session, task.ID)
				s.Cancel(session, task.ID)
			}
		}(i)
	}
	wg.Wait()
}
