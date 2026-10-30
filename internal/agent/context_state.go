package agent

import (
	"context"
	"encoding/json"

	"diegoc-agent/internal/memory"
	"diegoc-agent/internal/schema"
)

// conversationContext owns context settings and routes all summary access to
// one active store. With memory enabled, the Manager owns the summary; otherwise
// the conversation keeps it locally. It is only mutated by the Agent's runner.
type conversationContext struct {
	manager            *memory.Manager
	summary            string
	limit              int
	language           string
	skipNextTokenCheck bool
}

func (c *conversationContext) summaryText() string {
	if c.manager != nil {
		return c.manager.CompressedSummary()
	}
	return c.summary
}

func (c *conversationContext) setSummary(summary string) {
	if c.manager != nil {
		c.manager.SetCompressedSummary(summary)
		return
	}
	c.summary = summary
}

func (c *conversationContext) setManager(manager *memory.Manager) {
	// Retain the current effective summary when detaching. On attachment, the
	// Manager's existing summary is authoritative, including an empty summary.
	summary := c.summaryText()
	c.manager = manager
	if manager == nil {
		c.summary = summary
		return
	}
	c.summary = ""
	c.limit, _, c.language = manager.ContextSettings()
}

func (c *conversationContext) settings(tokenLimit int) (limit, threshold int, language string) {
	if c.manager != nil {
		return c.manager.ContextSettings()
	}
	limit = c.limit
	if limit <= 0 {
		limit = tokenLimit
	}
	return limit, tokenLimit, c.language
}

func (c *conversationContext) detached() conversationContext {
	return conversationContext{
		summary: c.summaryText(), limit: c.limit, language: c.language,
		skipNextTokenCheck: true, // Preserve the inherited API prefix on the first request.
	}
}

type contextSnapshot struct {
	messages []schema.Message
	summary  string
}

func (a *Agent) snapshotContext() contextSnapshot {
	return contextSnapshot{messages: cloneMessages(a.Messages), summary: a.contextState.summaryText()}
}

func (a *Agent) setContext(messages []schema.Message, summary string) {
	a.Messages = messages
	a.contextState.setSummary(summary)
}

func (a *Agent) restoreContext(snapshot contextSnapshot) {
	a.setContext(cloneMessages(snapshot.messages), snapshot.summary)
	a.contextState.skipNextTokenCheck = false
}

// inheritContext receives an already copied message list. It copies summary and
// settings without sharing the parent's mutable memory pipeline or skip state.
func (a *Agent) inheritContext(parent *Agent, messages []schema.Message) {
	a.contextState = parent.contextState.detached()
	a.Messages = messages
}

// prepareContext preserves the existing automatic compaction strategies. The
// memory pipeline commits its own summary, so no Agent-side mirror is needed.
func (a *Agent) prepareContext(ctx context.Context) error {
	if a.contextState.manager == nil {
		return a.summarizeIfNeeded(ctx)
	}
	systemPrompt := a.SystemPrompt
	if a.goalActive() && len(a.Messages) > 0 {
		systemPrompt = contentString(a.goalRequestMessages()[0])
	}
	messages, _, err := a.contextState.manager.PreReasoningHook(ctx, a.Messages, systemPrompt)
	if err != nil {
		return err
	}
	a.Messages = messages
	return nil
}

func cloneMessages(messages []schema.Message) []schema.Message {
	data, err := json.Marshal(messages)
	if err != nil {
		panic(err)
	}
	var cloned []schema.Message
	if err := json.Unmarshal(data, &cloned); err != nil {
		panic(err)
	}
	return cloned
}
