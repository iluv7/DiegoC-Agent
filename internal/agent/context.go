package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"diegoc-agent/internal/memory"
	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

type ContextUsage struct {
	System, ToolDefinitions, Summary, Conversation, ToolResults int
	Total, Limit, AutoCompactThreshold                          int
}

// ContextUsage is a read-only estimate of the next request's current contents.
// It never calls the model, compacts history, or uses cumulative billing tokens.
func (a *Agent) ContextUsage() ContextUsage {
	usage := ContextUsage{Limit: a.contextLimit, AutoCompactThreshold: a.TokenLimit}
	if usage.Limit <= 0 {
		usage.Limit = a.TokenLimit
	}
	if a.memoryManager != nil {
		usage.Limit, usage.AutoCompactThreshold, _ = a.memoryManager.ContextSettings()
	}
	counter := memory.NewRuleTokenCounter()
	for _, msg := range a.goalRequestMessages() {
		tokens := counter.CountMessages([]schema.Message{msg})
		switch {
		case memory.IsSummaryMessage(msg):
			usage.Summary += tokens
		case msg.Role == "system":
			usage.System += tokens
		case msg.Role == "tool":
			usage.ToolResults += tokens
		default:
			usage.Conversation += tokens
		}
	}
	for _, tool := range a.Tools {
		data, _ := json.Marshal(tools.ToAnthropicSchema(tool))
		usage.ToolDefinitions += counter.CountText(string(data))
	}
	usage.Total = usage.System + usage.ToolDefinitions + usage.Summary + usage.Conversation + usage.ToolResults
	return usage
}

type CompactionStats struct {
	Before, After ContextUsage
}

// Compact must be called while this foreground agent is idle. It only changes
// conversation state; file backups and rewind snapshots remain available.
func (a *Agent) Compact(ctx context.Context, instructions string) (stats CompactionStats, err error) {
	stats.Before = a.ContextUsage()
	stats.After = stats.Before
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	var system, conversation []schema.Message
	previous := a.compressedSummary
	if a.memoryManager != nil {
		previous = a.memoryManager.CompressedSummary()
	}
	for _, msg := range cloneMessages(a.Messages) {
		switch {
		case memory.IsSummaryMessage(msg):
			if previous == "" {
				text, _ := msg.Content.(string)
				previous = text
			}
		case msg.Role == "system":
			system = append(system, msg)
		default:
			conversation = append(conversation, msg)
		}
	}
	if len(conversation) == 0 {
		return stats, fmt.Errorf("no new conversation to compact")
	}
	result, err := memory.CompactConversation(ctx, a.LLM, conversation, previous, instructions, stats.Before.Limit, a.compactLanguage)
	a.APITotalTokens += result.TotalTokens
	if err != nil {
		return stats, err
	}
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	compacted := append(system, memory.SummaryMessage(result.Summary))
	// Compare with the same estimator; reject larger summaries without changing
	// messages, permission state, pending-tool repair state, or memory summary.
	if memory.EstimateTokens(compacted) >= memory.EstimateTokens(a.Messages) {
		return stats, fmt.Errorf("summary would not reduce context; conversation was kept")
	}
	a.Messages = compacted
	a.compressedSummary = result.Summary
	if a.memoryManager != nil {
		a.memoryManager.SetCompressedSummary(result.Summary)
	}
	a.needsToolRepair = false
	a.skipNextTokenCheck = true
	stats.After = a.ContextUsage()
	return stats, nil
}
