package main

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"diegoc-agent/internal/agent"
	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
	"github.com/peterh/liner"
)

type promptTestClient struct{}

func (promptTestClient) Generate(ctx context.Context, _ []schema.Message, _ []tools.Tool) (*schema.LLMResponse, error) {
	select {
	case <-time.After(300 * time.Millisecond):
		return &schema.LLMResponse{Content: "background report"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func TestForkPromptTerminalHelper(t *testing.T) {
	if os.Getenv("DIEGOC_FORK_PROMPT_TEST") != "1" {
		t.Skip("PTY helper")
	}
	m := agent.NewForkManager(context.Background())
	defer m.Close()
	parent := agent.New(promptTestClient{}, "system", 10, 1000, nil)
	parent.EnableForks(m)
	if _, err := m.Fork(parent, "background task"); err != nil {
		t.Fatal(err)
	}
	state := liner.NewLiner()
	defer state.Close()
	line, err := promptWithForkEvents(state, m, []string{"old message"})
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("INPUT=%q\n", line)
}
func TestTerminalReportStripsEscapeSequences(t *testing.T) {
	if got := terminalReport("a\x1b\x07\r\nb\t"); got != "a\nb\t" {
		t.Fatalf("unsafe report %q", got)
	}
}
