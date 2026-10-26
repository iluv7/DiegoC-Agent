// Package loop schedules recurring prompts. It never runs an Agent or owns a
// goroutine: the foreground runner takes due work only between user turns.
package loop

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	DefaultInterval = 10 * time.Minute
	Lifetime        = 7 * 24 * time.Hour
	MaxTasks        = 50 // per conversation session
	MaxPromptBytes  = 25000
)

var intervalPattern = regexp.MustCompile(`^([0-9]+)([smhd])$`)

// ParseInterval accepts positive integer s/m/h/d intervals, rounding seconds up
// to a whole minute. Durations longer than the task lifetime are rejected.
func ParseInterval(value string) (time.Duration, error) {
	parts := intervalPattern.FindStringSubmatch(strings.ToLower(value))
	if parts == nil {
		return 0, fmt.Errorf("invalid interval %q; use a positive integer with s, m, h or d (e.g. 5m)", value)
	}
	n, err := strconv.ParseUint(parts[1], 10, 64)
	unit := map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour}[parts[2]]
	if err != nil || n == 0 || n > uint64(Lifetime/unit) {
		return 0, fmt.Errorf("interval must be between 1 second and 7 days")
	}
	d := time.Duration(n) * unit
	d = ((d + time.Minute - 1) / time.Minute) * time.Minute
	if d >= Lifetime {
		return 0, fmt.Errorf("interval must be shorter than the 7-day expiry")
	}
	return d, nil
}

type Task struct {
	ID        string        `json:"id"`
	SessionID string        `json:"session_id"`
	Prompt    string        `json:"prompt"`
	Interval  time.Duration `json:"interval_ns"`
	NextRun   time.Time     `json:"next_run"`
	ExpiresAt time.Time     `json:"expires_at"`
	Runs      int           `json:"runs"`
	Running   bool          `json:"running"`
}

type Scheduler struct {
	mu       sync.Mutex
	now      func() time.Time
	sequence int
	tasks    []*Task
}

// New uses the supplied clock for scheduling; nil selects the wall clock.
func New(now func() time.Time) *Scheduler {
	if now == nil {
		now = time.Now
	}
	return &Scheduler{now: now}
}

func (s *Scheduler) expire(now time.Time) {
	kept := s.tasks[:0]
	for _, task := range s.tasks {
		if now.Before(task.ExpiresAt) || task.Running {
			kept = append(kept, task)
		}
	}
	s.tasks = kept
}

func (s *Scheduler) Create(session, prompt string, interval time.Duration) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prompt = strings.TrimSpace(prompt)
	if session == "" || prompt == "" || !utf8.ValidString(prompt) || len(prompt) > MaxPromptBytes {
		return Task{}, fmt.Errorf("session and a UTF-8 prompt are required; prompt limit is %d bytes", MaxPromptBytes)
	}
	if interval < time.Minute || interval >= Lifetime || interval%time.Minute != 0 {
		return Task{}, fmt.Errorf("interval must be a whole number of minutes, at least 1 minute and shorter than 7 days")
	}
	now := s.now()
	s.expire(now)
	count := 0
	for _, task := range s.tasks {
		if task.SessionID == session {
			count++
		}
	}
	if count >= MaxTasks {
		return Task{}, fmt.Errorf("session loop limit reached (%d); cancel a loop first", MaxTasks)
	}
	s.sequence++
	task := &Task{ID: fmt.Sprintf("loop-%d", s.sequence), SessionID: session, Prompt: prompt, Interval: interval, NextRun: now.Add(interval), ExpiresAt: now.Add(Lifetime)}
	s.tasks = append(s.tasks, task)
	return *task, nil
}

func (s *Scheduler) List(session string) []Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire(s.now())
	result := []Task{}
	for _, task := range s.tasks {
		if task.SessionID == session {
			result = append(result, *task)
		}
	}
	return result
}

// due returns the earliest due task, with creation order breaking ties.
func (s *Scheduler) due(session string, now time.Time) *Task {
	s.expire(now)
	var next *Task
	for _, task := range s.tasks {
		if task.SessionID == session && !task.Running && !now.Before(task.NextRun) && (next == nil || task.NextRun.Before(next.NextRun)) {
			next = task
		}
	}
	return next
}

func (s *Scheduler) HasDue(session string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.due(session, s.now()) != nil
}

func (s *Scheduler) TakeDue(session string) (Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task := s.due(session, s.now())
	if task == nil {
		return Task{}, false
	}
	task.Running = true
	task.Runs++
	return *task, true
}

// Complete coalesces missed runs instead of replaying them. Preserve the fixed
// cadence, but always leave the next run strictly after completion.
func (s *Scheduler) Complete(session, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for _, task := range s.tasks {
		if task.SessionID == session && task.ID == id && task.Running {
			task.Running = false
			if !now.Before(task.NextRun) {
				task.NextRun = task.NextRun.Add((now.Sub(task.NextRun)/task.Interval + 1) * task.Interval)
			}
		}
	}
	s.expire(now)
}

// Cancel only affects the caller's session. "all" clears that session's tasks.
func (s *Scheduler) Cancel(session, id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.tasks[:0]
	removed := 0
	for _, task := range s.tasks {
		if task.SessionID == session && (id == "all" || task.ID == id) {
			removed++
		} else {
			kept = append(kept, task)
		}
	}
	s.tasks = kept
	return removed
}
