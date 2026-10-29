package main

import (
	"context"
	"fmt"
	"os"
	"testing"

	"diegoc-agent/internal/agent"
	"diegoc-agent/internal/logger"
	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

func TestSessionCommandsCreateAndSwitchWithoutTaskPrompt(t *testing.T) {
	root := agent.New(nil, "system", 5, 1000, nil)
	root.Logger = logger.NewInDirectory(t.TempDir())
	root.AddUserMessage("context")
	sessions := agent.NewConversationSessions(root)
	defer sessions.Close()
	noSelector := func(string, []string, int) (int, error) {
		t.Fatal("/fork must not prompt for task or selection")
		return -1, nil
	}
	if !handleSessionCommand(sessions, "/fork", noSelector) || sessions.Current().ID != "session-1" {
		t.Fatal("bare fork did not enter a new session")
	}
	child := sessions.Current()
	child.Agent.AddUserMessage("child message")
	handleSessionCommand(sessions, "/switch main", noSelector)
	if sessions.Current().Agent != root {
		t.Fatal("switch to main failed")
	}
	root.AddUserMessage("main message")
	handleSessionCommand(sessions, "/sessions", func(_ string, options []string, initial int) (int, error) {
		if len(options) != 2 || initial != 0 {
			t.Fatal("selector has wrong sessions/current position")
		}
		return 1, nil
	})
	if sessions.Current() != child || child.Agent.Messages[len(child.Agent.Messages)-1].Content != "child message" {
		t.Fatal("switch lost child state")
	}
	handleSessionCommand(sessions, "/fork task", noSelector)
	if len(sessions.List()) != 2 {
		t.Fatal("task arguments unexpectedly created a conversation")
	}
	handleSessionCommand(sessions, "/sessions", func(string, []string, int) (int, error) { return -1, nil })
	if sessions.Current() != child {
		t.Fatal("cancelled selection changed current session")
	}
}

type conversationTestClient struct{}

func (conversationTestClient) Generate(_ context.Context, messages []schema.Message, _ []tools.Tool) (*schema.LLMResponse, error) {
	return &schema.LLMResponse{Content: fmt.Sprintf("REPLY:%s (context=%d)", messages[len(messages)-1].Content, len(messages))}, nil
}
func TestInteractiveSessionsTerminalHelper(t *testing.T) {
	if os.Getenv("DIEGOC_SESSIONS_TEST") != "1" {
		t.Skip("PTY helper")
	}
	root := agent.New(conversationTestClient{}, "system", 5, 100000, nil)
	root.Logger = logger.NewInDirectory(t.TempDir())
	runInteractive(root, t.TempDir(), root.Logger)
}
