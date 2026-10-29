package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"diegoc-agent/internal/agent"
	"diegoc-agent/internal/config"
	"diegoc-agent/internal/logger"
	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
	"github.com/mattn/go-runewidth"
)

func sampleQuestion() tools.UserQuestion {
	return tools.UserQuestion{Question: "使用哪种存储？", Options: []tools.QuestionOption{{Label: "SQLite", Description: "单机部署"}, {Label: "PostgreSQL", Description: "多实例部署"}}}
}

func TestCollectUserAnswerSelectionCustomAndCancellation(t *testing.T) {
	q := sampleQuestion()
	noPrompt := func(string) (string, error) { t.Fatal("unexpected text prompt"); return "", nil }
	answer, err := collectUserAnswer(q, func(tools.UserQuestion) (int, error) { return 1, nil }, noPrompt)
	if err != nil || answer.Answer != "PostgreSQL" || answer.Source != "option" {
		t.Fatalf("selection: %+v %v", answer, err)
	}
	var prompts int
	answer, err = collectUserAnswer(q, func(tools.UserQuestion) (int, error) { return len(q.Options), nil }, func(string) (string, error) {
		prompts++
		if prompts == 1 {
			return "  ", nil
		}
		return "  Redis + SQLite  ", nil
	})
	if err != nil || prompts != 2 || answer.Answer != "Redis + SQLite" || answer.Source != "custom" {
		t.Fatalf("custom answer: %+v %v prompts=%d", answer, err, prompts)
	}
	if _, err := collectUserAnswer(q, func(tools.UserQuestion) (int, error) { return -1, nil }, noPrompt); !errors.Is(err, context.Canceled) {
		t.Fatalf("selector cancellation: %v", err)
	}
	if _, err := collectUserAnswer(q, func(tools.UserQuestion) (int, error) { return len(q.Options), nil }, func(string) (string, error) { return "", context.Canceled }); !errors.Is(err, context.Canceled) {
		t.Fatalf("custom input cancellation: %v", err)
	}
}

func TestQuestionExchangesDoNotReuseRepliesAfterCancellation(t *testing.T) {
	requests := make(chan userQuestionRequest)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstDone := make(chan error, 1)
	go func() { _, err := requestUserAnswer(ctx, requests, sampleQuestion()); firstDone <- err }()
	var first userQuestionRequest
	select {
	case first = <-requests:
	case <-time.After(time.Second):
		t.Fatal("question not sent")
	}
	cancel()
	select {
	case err := <-firstDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled question is still blocked")
	}
	// The terminal loop may finish a cancelled request later; this must not block
	// or be consumed by another foreground run.
	first.Reply <- userQuestionResponse{Answer: tools.UserAnswer{Answer: "stale", Source: "custom"}}
	secondDone := make(chan tools.UserAnswer, 1)
	secondCtx, secondCancel := context.WithTimeout(context.Background(), time.Second)
	defer secondCancel()
	go func() { answer, _ := requestUserAnswer(secondCtx, requests, sampleQuestion()); secondDone <- answer }()
	select {
	case second := <-requests:
		second.Reply <- userQuestionResponse{Answer: tools.UserAnswer{Answer: "fresh", Source: "custom"}}
	case <-secondCtx.Done():
		t.Fatal("second question not sent")
	}
	select {
	case answer := <-secondDone:
		if answer.Answer != "fresh" {
			t.Fatal("another run consumed the stale answer")
		}
	case <-secondCtx.Done():
		t.Fatal("second question is still blocked")
	}
	if _, err := requestUserAnswer(ctx, requests, sampleQuestion()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled request blocked before sending: %v", err)
	}
}

func TestQuestionHeaderWrapsChineseAndStripsControls(t *testing.T) {
	lines := questionHeader("选择数据库\x1b[2J\n", "用于多个实例并发读写", 18, 3)
	if len(lines) < 2 || len(lines) > 3 {
		t.Fatalf("header did not wrap: %#v", lines)
	}
	for _, line := range lines {
		if strings.ContainsAny(line, "\x1b\r\n") || runewidth.StringWidth(line) > 18 {
			t.Fatalf("unsafe or overflowing header: %q", line)
		}
	}
}

func TestAskUserRegisteredWithoutOptionalTools(t *testing.T) {
	list, _ := buildTools(&config.Config{}, t.TempDir())
	for _, tool := range list {
		if tool.Name() == "ask_user" {
			return
		}
	}
	t.Fatal("ask_user missing from default tools")
}

type questionTerminalClient struct{}

func (questionTerminalClient) Generate(_ context.Context, messages []schema.Message, _ []tools.Tool) (*schema.LLMResponse, error) {
	last := messages[len(messages)-1]
	if last.Role == "tool" {
		return &schema.LLMResponse{Content: "QUESTION_RESULT:" + fmt.Sprint(last.Content)}, nil
	}
	q := sampleQuestion()
	data, _ := json.Marshal(q)
	var args map[string]interface{}
	_ = json.Unmarshal(data, &args)
	return &schema.LLMResponse{ToolCalls: []schema.ToolCall{{ID: "ask", Type: "function", Function: schema.FunctionCall{Name: "ask_user", Arguments: args}}}}, nil
}

// Driven through a real PTY to verify input ownership, Esc and terminal restore.
func TestAskUserTerminalHelper(t *testing.T) {
	if os.Getenv("DIEGOC_ASK_USER_TEST") != "1" {
		t.Skip("PTY helper")
	}
	root := agent.New(questionTerminalClient{}, "system", 5, 100000, []tools.Tool{&tools.AskUserTool{}})
	root.Logger = logger.NewInDirectory(t.TempDir())
	runInteractive(root, t.TempDir(), root.Logger)
}
