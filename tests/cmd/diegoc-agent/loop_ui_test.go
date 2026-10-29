package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"diegoc-agent/internal/agent"
	"diegoc-agent/internal/logger"
	"diegoc-agent/internal/loop"
	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

func TestLoopCommandsStayLocalAndPreservePrompt(t *testing.T) {
	s := loop.New(nil)
	workspace := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	if handleLoopCommand(s, "main", workspace, "/looping") {
		t.Fatal("matched unrelated command")
	}
	for _, cmd := range []string{"/loop 0m task", "/loop -1h task", "/loop list extra", "/loop cancel"} {
		if !handleLoopCommand(s, "main", workspace, cmd) || len(s.List("main")) != 0 {
			t.Fatalf("invalid command created task: %q", cmd)
		}
	}
	handleLoopCommand(s, "main", workspace, "/LOOP 61s 检查 CI\n保留  空格")
	handleLoopCommand(s, "main", workspace, "/loop check deployment")
	tasks := s.List("main")
	if len(tasks) != 2 || tasks[0].Interval != 2*time.Minute || tasks[0].Prompt != "检查 CI\n保留  空格" || tasks[1].Interval != loop.DefaultInterval {
		t.Fatalf("unexpected loops: %+v", tasks)
	}
	handleLoopCommand(s, "main", workspace, "/loop list")
	handleLoopCommand(s, "session-1", workspace, "/loop cancel "+tasks[0].ID)
	if len(s.List("main")) != 2 {
		t.Fatal("CLI cancellation crossed sessions")
	}
	handleLoopCommand(s, "main", workspace, "/loop cancel "+tasks[0].ID)
	if len(s.List("main")) != 1 {
		t.Fatal("task cancellation failed")
	}
	handleLoopCommand(s, "main", workspace, "/loop cancel all")
	if len(s.List("main")) != 0 {
		t.Fatal("cancel all failed")
	}
}

func TestLoopPromptPrecedenceAndValidation(t *testing.T) {
	workspace, home := t.TempDir(), t.TempDir()
	write := func(root, content string) string {
		t.Helper()
		path := filepath.Join(root, ".diegoc-agent", "loop.md")
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	if got, err := loadLoopPrompt(workspace, home); err != nil || got != defaultLoopPrompt {
		t.Fatalf("default: %q %v", got, err)
	}
	write(home, "USER DEFAULT")
	if got, err := loadLoopPrompt(workspace, home); err != nil || got != "USER DEFAULT" {
		t.Fatalf("user default: %q %v", got, err)
	}
	write(workspace, "项目默认任务")
	t.Setenv("HOME", home)
	s := loop.New(nil)
	handleLoopCommand(s, "main", workspace, "/loop")
	if tasks := s.List("main"); len(tasks) != 1 || tasks[0].Prompt != "项目默认任务" {
		t.Fatalf("bare loop: %+v", tasks)
	}
	for _, invalid := range []string{" ", string([]byte{0xff}), strings.Repeat("x", loop.MaxPromptBytes+1)} {
		write(workspace, invalid)
		if _, err := loadLoopPrompt(workspace, home); err == nil {
			t.Fatal("invalid prompt was accepted or fell back silently")
		}
	}
	// Explicit prompts must not read a broken default file.
	handleLoopCommand(s, "main", workspace, "/loop 5m explicit task")
	if len(s.List("main")) != 2 {
		t.Fatal("explicit prompt tried to read default")
	}
}

type loopTerminalClient struct{}

func (loopTerminalClient) Generate(ctx context.Context, messages []schema.Message, _ []tools.Tool) (*schema.LLMResponse, error) {
	last := messages[len(messages)-1]
	text := fmt.Sprint(last.Content)
	if strings.Contains(text, "WAIT_FOR_ESC") {
		fmt.Println("MOCK_LOOP_WAIT")
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if strings.Contains(text, "SELF_CANCEL") && last.Role == "user" {
		return &schema.LLMResponse{ToolCalls: []schema.ToolCall{{ID: "stop-loop", Type: "function", Function: schema.FunctionCall{Name: "loop_cancel", Arguments: map[string]interface{}{"id": "all"}}}}}, nil
	}
	return &schema.LLMResponse{Content: "MOCK_LOOP_REPLY:" + text}, nil
}

// A real terminal driver enables this helper to exercise scheduled wakeups,
// draft restoration, switching, tool-driven completion, and Esc cancellation.
func TestLoopTerminalHelper(t *testing.T) {
	if os.Getenv("DIEGOC_LOOP_TEST") != "1" {
		t.Skip("PTY helper")
	}
	start := time.Now()
	// One scheduled minute passes per two wall-clock seconds.
	s := loop.New(func() time.Time { return start.Add(time.Since(start) * 30) })
	a := agent.New(loopTerminalClient{}, "system", 5, 100000, tools.LoopTools())
	a.Logger = logger.NewInDirectory(t.TempDir())
	runInteractiveWithLoops(a, t.TempDir(), a.Logger, s)
	for _, task := range s.List("main") {
		fmt.Println("REMAINING_LOOP:", task.ID)
	}
}
