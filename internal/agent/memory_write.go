package agent

import (
	"context"
	"fmt"
	"reflect"

	"diegoc-agent/internal/memory"
	"diegoc-agent/internal/schema"
)

// MemoryWriteMiddleware submits user inputs and final assistant responses after
// successful replies and before compaction. Background work belongs to Manager,
// not the foreground reply context. Forks resolve their own manager and therefore
// do not inherit authority to write the parent session's memory.
type MemoryWriteMiddleware struct {
	manager           *memory.Manager
	conversationStart int              // first checkpoint ID distinguishes cleared conversations
	covered           []schema.Message // accepted prefix of the current filtered history
	replacement       []schema.Message // alternative prefix if a prepared compaction commits
}

func (m *MemoryWriteMiddleware) OnReply(ctx context.Context, a *Agent, next ReplyHandler) (string, error) {
	out, err := next(ctx)
	if err != nil {
		return out, err
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	return out, m.submit(a, a.Messages)
}

func (m *MemoryWriteMiddleware) OnContextCompaction(ctx context.Context, a *Agent, event ContextCompactionEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.submit(a, event.Before); err != nil {
		return err
	}
	// The replacement contains only previously submitted turns and framework
	// summaries. Remember its prefix without assuming all observers will accept it.
	m.replacement = memoryTurns(event.After)
	return nil
}

func (m *MemoryWriteMiddleware) submit(a *Agent, messages []schema.Message) error {
	start := 0
	if len(a.checkpoints) > 0 {
		start = a.checkpoints[0].ID
	}
	if m.manager != a.memoryManager || m.conversationStart != start {
		m.conversationStart = start
		m.manager = a.memoryManager
		m.covered = nil
		m.replacement = nil
	}
	if m.manager == nil {
		return nil
	}
	turns := memoryTurns(messages)
	commonPrefix := func(covered []schema.Message) int {
		i := 0
		for i < len(covered) && i < len(turns) && reflect.DeepEqual(covered[i], turns[i]) {
			i++
		}
		return i
	}
	// Keep both prefixes until the next submission: another observer may reject
	// the prepared replacement after this writer has already accepted the work.
	common := max(commonPrefix(m.covered), commonPrefix(m.replacement))
	// Compare by position, not a content set: identical user turns at different
	// positions are distinct. Rewinds or edits restart from the first divergence.
	if err := m.manager.QueueSummary(turns[common:]); err != nil {
		return fmt.Errorf("submit memory: %w", err)
	}
	m.covered = turns
	m.replacement = nil
	return nil
}

func memoryTurns(messages []schema.Message) []schema.Message {
	var turns []schema.Message
	for _, msg := range messages {
		if memory.IsSummaryMessage(msg) {
			continue
		}
		if msg.Role != "user" && !(msg.Role == "assistant" && len(msg.ToolCalls) == 0) {
			continue
		}
		if contentString(msg) == "" {
			continue
		}
		msg.Thinking = ""
		turns = append(turns, msg)
	}
	return cloneMessages(turns)
}
