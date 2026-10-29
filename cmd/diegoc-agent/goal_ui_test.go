package main

import (
	"context"
	"encoding/json"
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

func TestGoalCommandsStartWorkButStatusAndCancelStayLocal(t *testing.T) {
	a := agent.New(nil, "system", 5, 100000, nil)
	before := append([]schema.Message(nil), a.Messages...)
	if _, handled := handleGoalCommand(a, "/goals"); handled {
		t.Fatal("matched unrelated command")
	}
	for _, command := range []string{"/goal", "/goal status", "/goal resume", "/goal cancel unexpected"} {
		if prompt, handled := handleGoalCommand(a, command); !handled || prompt != "" {
			t.Fatalf("local command triggered a run: %q", command)
		}
	}
	if prompt, handled := handleGoalCommand(a, "/GOAL 实现功能\n并通过测试"); !handled || !strings.Contains(prompt, "实现功能\n并通过测试") || a.GoalStatus().Status != "active" {
		t.Fatal("setting a goal did not return a runnable prompt")
	}
	a.PauseGoal("waiting for input")
	if prompt, _ := handleGoalCommand(a, "/goal resume"); !strings.Contains(prompt, "waiting for input") || a.GoalStatus().Status != "active" {
		t.Fatal("resume lost previous reason")
	}
	handleGoalCommand(a, "/goal cancel")
	if a.GoalStatus().Status != "cancelled" || !reflect.DeepEqual(before, a.Messages) {
		t.Fatal("local commands entered the conversation")
	}
}

type goalTerminalMain struct{}

func (goalTerminalMain) Generate(ctx context.Context, messages []schema.Message, _ []tools.Tool) (*schema.LLMResponse, error) {
	last := messages[len(messages)-1]
	text := fmt.Sprint(last.Content)
	if strings.Contains(text, "WAIT_MAIN") {
		fmt.Println("MOCK_GOAL_MAIN_WAIT")
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if last.Role == "user" && strings.Contains(text, "NEEDS_APPROVAL") && !strings.Contains(text, "[Goal stop check:") {
		return &schema.LLMResponse{ToolCalls: []schema.ToolCall{{ID: "write-goal", Type: "function", Function: schema.FunctionCall{Name: "write_file", Arguments: map[string]interface{}{"path": "goal-result.txt", "content": "verified"}}}}}, nil
	}
	return &schema.LLMResponse{Content: "MOCK_GOAL_ANSWER", Usage: &schema.TokenUsage{TotalTokens: 10}}, nil
}

type goalTerminalJudge struct{ waited bool }

func (judge *goalTerminalJudge) Generate(ctx context.Context, messages []schema.Message, list []tools.Tool) (*schema.LLMResponse, error) {
	if len(list) > 0 {
		return nil, fmt.Errorf("judge inherited tools")
	}
	var payload struct {
		Goal string `json:"goal"`
	}
	json.Unmarshal([]byte(fmt.Sprint(messages[1].Content)), &payload)
	if strings.Contains(payload.Goal, "WAIT_JUDGE") && !judge.waited {
		judge.waited = true
		fmt.Println("MOCK_GOAL_JUDGE_WAIT")
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if strings.Contains(payload.Goal, "BLOCKED") {
		return &schema.LLMResponse{Content: `{"satisfied":false,"reason":"user input required","needs_user_input":true}`}, nil
	}
	if strings.Contains(payload.Goal, "INVALID_JSON") {
		return &schema.LLMResponse{Content: "not JSON"}, nil
	}
	if strings.Contains(payload.Goal, "LIMIT") || !strings.Contains(fmt.Sprint(messages[1].Content), "Goal stop check: not satisfied") {
		return &schema.LLMResponse{Content: `{"satisfied":false,"reason":"verify the remaining requirement","needs_user_input":false}`, Usage: &schema.TokenUsage{TotalTokens: 5}}, nil
	}
	return &schema.LLMResponse{Content: `{"satisfied":true,"reason":"verified from conversation evidence","needs_user_input":false}`, Usage: &schema.TokenUsage{TotalTokens: 5}}, nil
}

func TestGoalTerminalHelper(t *testing.T) {
	if os.Getenv("DIEGOC_GOAL_TEST") != "1" {
		t.Skip("PTY helper")
	}
	workspace := t.TempDir()
	a := agent.New(goalTerminalMain{}, "system", 20, 100000, []tools.Tool{&tools.WriteTool{WorkspaceDir: workspace}})
	a.ConfigureGoals(&goalTerminalJudge{}, "mock-small-judge", 3)
	a.Logger = logger.NewInDirectory(t.TempDir())
	runInteractive(a, workspace, a.Logger)
}
