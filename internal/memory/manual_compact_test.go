package memory

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

type manualClientFunc func(context.Context, []schema.Message, []tools.Tool) (*schema.LLMResponse, error)

func (f manualClientFunc) Generate(ctx context.Context, msgs []schema.Message, ts []tools.Tool) (*schema.LLMResponse, error) {
	return f(ctx, msgs, ts)
}

const manualSummary = "## Goal\nBuild a database service.\n## Constraints\nPreserve the API contract.\n## Progress\nStorage is ready.\n## Key Decisions\nUse SQLite.\n## Next Steps\nRun tests.\n## Critical Context\nKeep src/storage.go."

func TestManualCompactorProcessesLongUnicodeTranscriptWithoutSilentTruncation(t *testing.T) {
	var calls int
	var firstSeen, lastSeen bool
	client := manualClientFunc(func(_ context.Context, request []schema.Message, ts []tools.Tool) (*schema.LLMResponse, error) {
		calls++
		if len(ts) != 0 || len(request) != 2 || request[0].Role != "system" {
			t.Fatal("compaction must use a tool-free summary request")
		}
		text := request[1].Content.(string)
		if !utf8.ValidString(text) || !strings.Contains(text, "保留接口约定") {
			t.Fatal("fragment split UTF-8 or lost preservation instructions")
		}
		if calls > 1 && !strings.Contains(text, manualSummary) {
			t.Fatal("incremental compaction lost earlier summary")
		}
		if NewRuleTokenCounter().CountMessages(request)+1500/4 > 1500 {
			t.Fatal("request exceeded configured compaction budget")
		}
		firstSeen = firstSeen || strings.Contains(text, "FIRST-CONSTRAINT")
		lastSeen = lastSeen || strings.Contains(text, "LAST-CONSTRAINT")
		return &schema.LLMResponse{Content: manualSummary, Usage: &schema.TokenUsage{TotalTokens: 25}}, nil
	})
	result, err := CompactConversation(context.Background(), client, []schema.Message{{Role: "user", Content: "FIRST-CONSTRAINT\n" + strings.Repeat("重要的用户需求与上下文。", 900) + "\nLAST-CONSTRAINT"}}, "", "保留接口约定", 1500, "zh")
	if err != nil || calls < 2 || !firstSeen || !lastSeen || result.Summary != manualSummary || result.TotalTokens != calls*25 {
		t.Fatalf("compaction: calls=%d first=%v last=%v result=%+v err=%v", calls, firstSeen, lastSeen, result, err)
	}
}

func TestManualCompactorReportsErrorsAndSpentTokens(t *testing.T) {
	apiErr := errors.New("API unavailable")
	for name, response := range map[string]*schema.LLMResponse{
		"nil":              nil,
		"blank":            {},
		"invalid headings": {Content: strings.Repeat("unstructured", 20), Usage: &schema.TokenUsage{TotalTokens: 7}},
		"unexpected tool":  {Content: manualSummary, ToolCalls: []schema.ToolCall{{ID: "unexpected"}}},
	} {
		t.Run(name, func(t *testing.T) {
			client := manualClientFunc(func(context.Context, []schema.Message, []tools.Tool) (*schema.LLMResponse, error) {
				return response, nil
			})
			result, err := CompactConversation(context.Background(), client, []schema.Message{{Role: "user", Content: "hello"}}, "", "", 10000, "")
			if err == nil || result.Summary != "" {
				t.Fatal("invalid summary accepted")
			}
			if response != nil && response.Usage != nil && result.TotalTokens != response.Usage.TotalTokens {
				t.Fatal("discarded response's billed tokens were lost")
			}
		})
	}
	client := manualClientFunc(func(context.Context, []schema.Message, []tools.Tool) (*schema.LLMResponse, error) { return nil, apiErr })
	if _, err := CompactConversation(context.Background(), client, []schema.Message{{Role: "user", Content: "hello"}}, "", "", 10000, ""); !errors.Is(err, apiErr) {
		t.Fatalf("lost API error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CompactConversation(ctx, client, []schema.Message{{Role: "user", Content: "hello"}}, "", "", 10000, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
}

func TestAutoCompactionRetainsSystemAndUsesNonSystemSummary(t *testing.T) {
	for _, valid := range []bool{true, false} {
		cfg := DefaultConfig()
		cfg.WorkingDir = t.TempDir()
		cfg.MaxInputLength, cfg.CompactReserve = 600, 80
		client := manualClientFunc(func(context.Context, []schema.Message, []tools.Tool) (*schema.LLMResponse, error) {
			if valid {
				return &schema.LLMResponse{Content: manualSummary}, nil
			}
			return &schema.LLMResponse{Content: "invalid"}, nil
		})
		mgr := NewManager(cfg, client)
		messages := []schema.Message{{Role: "system", Content: strings.Repeat("system constraints ", 100)}, {Role: "user", Content: strings.Repeat("old conversation ", 300)}, {Role: "user", Content: "recent question"}}
		result, summary, err := mgr.PreReasoningHook(context.Background(), messages, messages[0].Content.(string))
		if err != nil {
			t.Fatal(err)
		}
		if valid {
			if result[0].Content != messages[0].Content || result[0].Role != "system" || len(result) < 2 || result[1].Role != "user" || !IsSummaryMessage(result[1]) || summary != manualSummary {
				t.Fatalf("lost system or incorrect summary role: %+v", result)
			}
		} else if !reflect.DeepEqual(result, messages) || summary != "" {
			t.Fatal("failed automatic compaction discarded conversation")
		}
		mgr.Close()
	}
}
