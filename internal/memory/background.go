package memory

import (
	"encoding/json"
	"errors"
	"fmt"

	"diegoc-agent/internal/schema"
)

const maxQueuedSummaries = 64

// QueueSummary snapshots and accepts work independently of a foreground turn.
// A single worker drains in order so concurrent summaries cannot overwrite each
// other's memory files. A full queue or closed manager is reported to the caller.
func (m *Manager) QueueSummary(messages []schema.Message) error {
	if len(messages) == 0 {
		return nil
	}
	data, err := json.Marshal(messages)
	if err != nil {
		return fmt.Errorf("snapshot memory messages: %w", err)
	}
	var snapshot []schema.Message
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return err
	}
	m.taskMu.Lock()
	defer m.taskMu.Unlock()
	if m.closed {
		return fmt.Errorf("memory manager is closed")
	}
	if len(m.summaryQueue) >= maxQueuedSummaries {
		return fmt.Errorf("memory summary queue is full")
	}
	m.summaryQueue = append(m.summaryQueue, snapshot)
	if !m.summaryRunning {
		m.summaryRunning = true
		m.tasks.Add(1)
		go m.runSummaries()
	}
	return nil
}

func (m *Manager) runSummaries() {
	defer m.tasks.Done()
	for {
		m.taskMu.Lock()
		if len(m.summaryQueue) == 0 || m.closed {
			m.summaryQueue = nil
			m.summaryRunning = false
			m.taskMu.Unlock()
			return
		}
		messages := m.summaryQueue[0]
		m.summaryQueue[0] = nil
		m.summaryQueue = m.summaryQueue[1:]
		m.taskMu.Unlock()
		err := m.summarizer.Summarize(m.taskCtx, messages)
		if err != nil && !(m.taskCtx.Err() != nil && errors.Is(err, m.taskCtx.Err())) {
			m.taskMu.Lock()
			m.taskErr = fmt.Errorf("background memory summary: %w", err)
			m.taskMu.Unlock()
		}
	}
}

// BackgroundError returns the most recent background summary failure. Normal
// cancellation during shutdown is not a failure. Close also returns this error.
func (m *Manager) BackgroundError() error {
	m.taskMu.Lock()
	defer m.taskMu.Unlock()
	return m.taskErr
}
