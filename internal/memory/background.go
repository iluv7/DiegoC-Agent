package memory

import (
	"errors"
	"fmt"

	"diegoc-agent/internal/schema"
)

// startSummary registers before starting the goroutine, so Close cannot miss
// an accepted task. Calls racing with shutdown do not start new work.
func (m *Manager) startSummary(messages []schema.Message) {
	m.taskMu.Lock()
	defer m.taskMu.Unlock()
	if m.closed {
		return
	}
	m.tasks.Add(1)
	go func() {
		defer m.tasks.Done()
		err := m.summarizer.Summarize(m.taskCtx, messages)
		if err == nil || (m.taskCtx.Err() != nil && errors.Is(err, m.taskCtx.Err())) {
			return
		}
		m.taskMu.Lock()
		m.taskErr = fmt.Errorf("background memory summary: %w", err)
		m.taskMu.Unlock()
	}()
}

// BackgroundError returns the most recent background summary failure. Normal
// cancellation during shutdown is not a failure. Close also returns this error.
func (m *Manager) BackgroundError() error {
	m.taskMu.Lock()
	defer m.taskMu.Unlock()
	return m.taskErr
}
