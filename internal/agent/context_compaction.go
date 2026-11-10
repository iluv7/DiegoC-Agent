package agent

import (
	"context"

	"diegoc-agent/internal/schema"
)

// ContextCompactionMiddleware prepares bounded conversation history before model
// request assembly. It resolves services from the current agent so inherited
// factories never capture a parent session's memory manager or mutable history.
// Automatic compaction retains the existing memory-backed and legacy algorithms;
// manual Compact continues to use the shared memory compaction services directly.
type ContextCompactionMiddleware struct{}

func (*ContextCompactionMiddleware) OnReasoning(ctx context.Context, a *Agent, next ReasoningHandler) (*schema.LLMResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a.memoryManager != nil {
		// Phase 11-12: full memory pipeline
		systemPrompt := a.SystemPrompt
		if a.goalActive() && len(a.Messages) > 0 {
			systemPrompt = contentString(a.goalRequestMessages()[0])
		}
		keptMsgs, summary, err := a.memoryManager.PreReasoningHook(ctx, a.Messages, systemPrompt)
		if err != nil {
			return nil, err
		}
		a.Messages = keptMsgs
		if summary != "" {
			a.compressedSummary = summary
		}
	} else {
		// Legacy fallback: simple per-round summarization
		if err := a.summarizeIfNeeded(ctx); err != nil {
			return nil, err
		}
	}

	return next(ctx)
}
