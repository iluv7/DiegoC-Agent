package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"diegoc-agent/internal/agent"
)

func formatContextUsage(usage agent.ContextUsage) string {
	var out strings.Builder
	percentage := 0.0
	if usage.Limit > 0 {
		percentage = float64(usage.Total) * 100 / float64(usage.Limit)
	}
	free := usage.Limit - usage.Total
	if free < 0 {
		free = 0
	}
	fmt.Fprintf(&out, "Context usage (estimated, not cumulative API usage):\n")
	fmt.Fprintf(&out, "  Total: %d / %d tokens (%.1f%% of configured budget)\n", usage.Total, usage.Limit, percentage)
	fmt.Fprintf(&out, "  System prompt:    %d\n  Tool definitions: %d\n  Summary:          %d\n  Conversation:     %d\n  Tool results:     %d\n", usage.System, usage.ToolDefinitions, usage.Summary, usage.Conversation, usage.ToolResults)
	fmt.Fprintf(&out, "  Remaining: %d tokens\n", free)
	fmt.Fprintf(&out, "  Auto-compact configured threshold: %d tokens (memory mode also reserves prompt/summary space)\n", usage.AutoCompactThreshold)
	fmt.Fprintln(&out, "  Estimates include tool schemas; model tokenization and API framing may differ.")
	fmt.Fprintln(&out, "  /compact [preservation instructions] summarizes the current conversation.")
	return out.String()
}

// runManualCompact owns Esc input while the model summarizes. The outer REPL
// remains the sole caller and cannot switch sessions until this operation ends.
func runManualCompact(ag *agent.Agent, instructions string) {
	fmt.Println("Compacting conversation... (Esc to cancel)")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	escDone := make(chan struct{})
	go runWithEscCancel(ctx, cancel, escDone)
	stats, err := ag.Compact(ctx, instructions)
	cancel()
	<-escDone
	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Println("Compaction cancelled; conversation unchanged.")
		} else {
			fmt.Printf("Compaction failed; conversation unchanged: %s\n", terminalReport(err.Error()))
		}
		return
	}
	fmt.Printf("Compacted context: %d → %d estimated tokens (saved %d).\n", stats.Before.Total, stats.After.Total, stats.Before.Total-stats.After.Total)
}

func handleContextCommand(ag *agent.Agent, line string) bool {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return false
	}
	switch strings.ToLower(fields[0]) {
	case "/context":
		if len(fields) != 1 {
			fmt.Println("Usage: /context")
		} else {
			fmt.Print(formatContextUsage(ag.ContextUsage()))
		}
		return true
	case "/compact":
		runManualCompact(ag, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), fields[0])))
		return true
	}
	return false
}
