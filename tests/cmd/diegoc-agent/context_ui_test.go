package main

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"diegoc-agent/internal/agent"
	"diegoc-agent/internal/logger"
	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

func TestContextOutputShowsBudgetBreakdownAndOverBudget(t *testing.T) {
	usage := agent.ContextUsage{System: 10, ToolDefinitions: 20, Summary: 30, Conversation: 40, ToolResults: 50, Total: 150, Limit: 100, AutoCompactThreshold: 70}
	text := formatContextUsage(usage)
	for _, want := range []string{"estimated", "not cumulative", "150 / 100", "150.0%", "System prompt:", "Tool definitions:", "Summary:", "Conversation:", "Tool results:", "Remaining: 0", "threshold: 70"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in context output: %s", want, text)
		}
	}
}

func TestContextCommandDoesNotAppendMessagesOrCallModel(t *testing.T) {
	a := agent.New(nil, "system", 5, 10000, nil)
	a.AddUserMessage("ongoing task")
	before := append([]schema.Message(nil), a.Messages...)
	if !handleContextCommand(a, "/CONTEXT") || !handleContextCommand(a, "/context unexpected") || handleContextCommand(a, "/contextual") {
		t.Fatal("incorrect command matching")
	}
	if !reflect.DeepEqual(before, a.Messages) {
		t.Fatal("/context entered model conversation")
	}
}

const terminalCompactSummary = "## Goal\nImplement storage.\n## Constraints\nKeep API_CONTRACT.\n## Progress\nImplementation is ready.\n## Key Decisions\nUse SQLite.\n## Next Steps\nRun tests.\n## Critical Context\nKeep src/storage.go."

type compactTerminalClient struct{}

func (compactTerminalClient) Generate(ctx context.Context, messages []schema.Message, _ []tools.Tool) (*schema.LLMResponse, error) {
	first, _ := messages[0].Content.(string)
	if strings.Contains(first, "上下文压缩助手") {
		text := messages[1].Content.(string)
		if strings.Contains(text, "WAIT_FOR_CANCEL") {
			fmt.Println("MOCK_COMPACT_WAIT")
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return &schema.LLMResponse{Content: terminalCompactSummary, Usage: &schema.TokenUsage{TotalTokens: 25}}, nil
	}
	return &schema.LLMResponse{Content: strings.Repeat("Implementation details. ", 300), Usage: &schema.TokenUsage{TotalTokens: 100}}, nil
}

func TestContextCompactTerminalHelper(t *testing.T) {
	if os.Getenv("DIEGOC_CONTEXT_TEST") != "1" {
		t.Skip("PTY helper")
	}
	a := agent.New(compactTerminalClient{}, "system", 5, 20000, nil)
	a.Logger = logger.NewInDirectory(t.TempDir())
	runInteractive(a, t.TempDir(), a.Logger)
}
