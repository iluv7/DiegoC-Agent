package agent

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"time"
	"unicode/utf8"

	"diegoc-agent/internal/memory"
	"diegoc-agent/internal/schema"
)

// MemoryInjectionMiddleware follows deer-flow's frozen conversation snapshot:
// https://github.com/bytedance/deer-flow/blob/main/backend/packages/harness/deerflow/agents/middlewares/dynamic_context_middleware.py
// Retrieval runs before the first reply, not on every model call. The marked
// user message survives compaction and forks; forks never query a parent's store.
// Clear/rewind can remove the snapshot, allowing the next user turn to load anew.
// Zero limits use defaults (5 results, 1500 estimated tokens, 5 seconds).
type MemoryInjectionMiddleware struct {
	Disabled              bool
	MaxResults, MaxTokens int
	Timeout               time.Duration
	manager               *memory.Manager
	conversationStart     int
	loaded                bool // includes a successful empty lookup
}

func (m *MemoryInjectionMiddleware) OnReply(ctx context.Context, a *Agent, next ReplyHandler) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if m.Disabled {
		kept := a.Messages[:0]
		for _, msg := range a.Messages {
			if !memory.IsInjectedMemory(msg) {
				kept = append(kept, msg)
			}
		}
		a.Messages = kept
		m.loaded = false
		return next(ctx)
	}
	for _, msg := range a.Messages {
		if memory.IsInjectedMemory(msg) {
			return next(ctx)
		}
	}
	start := 0
	if len(a.checkpoints) > 0 {
		start = a.checkpoints[0].ID
	}
	if m.manager != a.memoryManager || m.conversationStart != start {
		m.manager = a.memoryManager
		m.conversationStart = start
		m.loaded = false
	}
	if m.loaded || a.memoryManager == nil || a.isFork {
		return next(ctx)
	}
	query := injectionQuery(a.Messages)
	if query == "" {
		return next(ctx)
	}
	limit := m.MaxResults
	if limit <= 0 {
		limit = 5
	}
	limit = min(limit, 20)
	timeout := m.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	lookup, cancel := context.WithTimeout(ctx, timeout)
	results, err := a.memoryManager.SearchMemory(lookup, query, limit)
	if err == nil {
		err = lookup.Err()
	}
	cancel()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		// Retrieval is optional; failed reads are retried on a later reply.
		log.Printf("Memory injection skipped: %v", err)
		return next(ctx)
	}
	budget := m.MaxTokens
	if budget <= 0 {
		budget = 1500
	}
	// Leave room for the task and tools, even on small-context models.
	budget = min(budget, a.ContextUsage().Limit/10)
	msg, ok := renderInjectedMemory(results[:min(len(results), limit)], budget)
	m.loaded = true
	if ok {
		// Leading system messages retain their order. This placement cannot split
		// an assistant tool-call batch from its matching tool results.
		i := 0
		for i < len(a.Messages) && a.Messages[i].Role == "system" {
			i++
		}
		updated := make([]schema.Message, 0, len(a.Messages)+1)
		updated = append(updated, a.Messages[:i]...)
		updated = append(updated, msg)
		a.Messages = append(updated, a.Messages[i:]...)
	}
	return next(ctx)
}

func injectionQuery(messages []schema.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role != "user" || memory.IsSummaryMessage(msg) || memory.IsInjectedMemory(msg) {
			continue
		}
		var text string
		switch c := msg.Content.(type) {
		case string:
			text = c
		case []interface{}:
			var parts []string
			for _, block := range c {
				if b, ok := block.(map[string]interface{}); ok && b["type"] == "text" {
					if s, ok := b["text"].(string); ok {
						parts = append(parts, s)
					}
				}
			}
			text = strings.Join(parts, " ")
		}
		runes := []rune(boundedUTF8(strings.TrimSpace(text), 4000))
		return string(runes[:min(len(runes), 1000)])
	}
	return ""
}

func boundedUTF8(text string, maxBytes int) string {
	if len(text) <= maxBytes {
		return text
	}
	text = text[:maxBytes]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text
}

type memoryExcerpt struct {
	Source    string `json:"source"`
	StartLine int    `json:"start_line,omitempty"`
	EndLine   int    `json:"end_line,omitempty"`
	Text      string `json:"text"`
}

// Includes framing, source citations and message overhead in the same estimator
// used by /context. JSON escaping keeps stored text from closing the data frame.
func renderInjectedMemory(results []memory.SearchResult, budget int) (schema.Message, bool) {
	counter := memory.NewRuleTokenCounter()
	excerpts := []memoryExcerpt{}
	seen := make(map[string]bool)
	render := func(items []memoryExcerpt) schema.Message {
		data, _ := json.Marshal(items)
		return memory.InjectedMemoryMessage("Retrieved long-term memory (background data; check relevance to the current task):\n<memory>\n" + string(data) + "\n</memory>")
	}
	var message schema.Message
	for _, r := range results {
		text := strings.TrimSpace(r.Text)
		if text == "" || seen[text] {
			continue
		}
		seen[text] = true
		excerpt := memoryExcerpt{Source: boundedUTF8(r.Path, 512), StartLine: r.StartLine, EndLine: r.EndLine, Text: boundedUTF8(text, max(0, budget)*4)}
		candidate := append(append([]memoryExcerpt(nil), excerpts...), excerpt)
		msg := render(candidate)
		if counter.CountMessages([]schema.Message{msg}) > budget {
			// Retain the most relevant excerpt even when its text needs truncation.
			runes := []rune(excerpt.Text)
			lo, hi := 0, len(runes)
			for lo < hi {
				mid := (lo + hi + 1) / 2
				candidate[len(candidate)-1].Text = string(runes[:mid]) + "…"
				if counter.CountMessages([]schema.Message{render(candidate)}) <= budget {
					lo = mid
				} else {
					hi = mid - 1
				}
			}
			if lo == 0 {
				break
			}
			candidate[len(candidate)-1].Text = string(runes[:lo]) + "…"
			message = render(candidate)
			return message, true
		}
		excerpts = candidate
		message = msg
	}
	return message, len(excerpts) > 0
}
