package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"diegoc-agent/internal/permission"
)

func questionArgs() map[string]interface{} {
	return map[string]interface{}{
		"question": "使用哪种存储？",
		"options":  []QuestionOption{{Label: "SQLite", Description: "单机部署"}, {Label: "PostgreSQL", Description: "多实例部署"}},
	}
}

func TestAskUserReturnsActualAnswer(t *testing.T) {
	for _, answer := range []UserAnswer{{"SQLite", "option"}, {"先用内存，稍后接 Redis", "custom"}} {
		t.Run(answer.Source, func(t *testing.T) {
			ctx := WithUserQuestionHandler(context.Background(), func(_ context.Context, q UserQuestion) (UserAnswer, error) {
				if q.Question != "使用哪种存储？" || q.Options[1].Description != "多实例部署" {
					t.Fatalf("lost question details: %+v", q)
				}
				return answer, nil
			})
			result, err := (&AskUserTool{}).Execute(ctx, questionArgs())
			if err != nil || !result.Success {
				t.Fatalf("execute: %+v %v", result, err)
			}
			var decoded struct {
				Question string `json:"question"`
				UserAnswer
			}
			if err := json.Unmarshal([]byte(result.Content), &decoded); err != nil || decoded.UserAnswer != answer || decoded.Question != "使用哪种存储？" {
				t.Fatalf("invalid result: %+v %v", decoded, err)
			}
		})
	}
}

func TestAskUserRejectsInvalidModelArgumentsBeforePrompting(t *testing.T) {
	cases := map[string]map[string]interface{}{
		"missing":          {},
		"blank":            {"question": "  ", "options": []QuestionOption{{Label: "A"}, {Label: "B"}}},
		"too few":          {"question": "Choose?", "options": []QuestionOption{{Label: "A"}}},
		"too many":         {"question": "Choose?", "options": []QuestionOption{{Label: "A"}, {Label: "B"}, {Label: "C"}, {Label: "D"}, {Label: "E"}}},
		"duplicate":        {"question": "Choose?", "options": []QuestionOption{{Label: "A"}, {Label: " a "}}},
		"empty label":      {"question": "Choose?", "options": []QuestionOption{{Label: " "}, {Label: "B"}}},
		"wrong type":       {"question": 123, "options": "A,B"},
		"long question":    {"question": strings.Repeat("中", 501), "options": []QuestionOption{{Label: "A"}, {Label: "B"}}},
		"long description": {"question": "Choose?", "options": []QuestionOption{{Label: "A", Description: strings.Repeat("中", 301)}, {Label: "B"}}},
		"injected answer":  {"question": "Choose?", "options": []QuestionOption{{Label: "A"}, {Label: "B"}}, "answer": "A"},
	}
	ctx := WithUserQuestionHandler(context.Background(), func(context.Context, UserQuestion) (UserAnswer, error) {
		t.Fatal("invalid model input reached the terminal")
		return UserAnswer{}, nil
	})
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			result, err := (&AskUserTool{}).Execute(ctx, args)
			if err != nil || result.Success || result.Error == "" {
				t.Fatalf("invalid input accepted: %+v %v", result, err)
			}
		})
	}
}

func TestAskUserMissingHandlerCancellationAndInvalidAnswers(t *testing.T) {
	tool := &AskUserTool{}
	result, err := tool.Execute(context.Background(), questionArgs())
	if err != nil || result.Success || !strings.Contains(result.Error, "foreground") {
		t.Fatalf("missing handler: %+v %v", result, err)
	}
	for _, answer := range []UserAnswer{{}, {"unknown", "option"}, {"SQLite", "automatic"}} {
		ctx := WithUserQuestionHandler(context.Background(), func(context.Context, UserQuestion) (UserAnswer, error) { return answer, nil })
		result, err := tool.Execute(ctx, questionArgs())
		if err != nil || result.Success {
			t.Fatalf("invalid answer accepted: %+v %v", result, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	ctx = WithUserQuestionHandler(ctx, func(context.Context, UserQuestion) (UserAnswer, error) {
		cancel()
		return UserAnswer{"SQLite", "option"}, nil
	})
	if _, err := tool.Execute(ctx, questionArgs()); !errors.Is(err, context.Canceled) {
		t.Fatalf("answer accepted after cancellation: %v", err)
	}
	if _, err := tool.Execute(ctx, questionArgs()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled run prompted: %v", err)
	}
	ctx = WithUserQuestionHandler(context.Background(), func(context.Context, UserQuestion) (UserAnswer, error) { return UserAnswer{}, context.Canceled })
	if _, err := tool.Execute(ctx, questionArgs()); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
}

func TestAskUserCannotBeAutoAnsweredByPermissions(t *testing.T) {
	for _, mode := range []permission.Mode{permission.ModeDefault, permission.ModeBypass, permission.ModeExplore} {
		perm := permission.NewContext()
		perm.Mode = mode
		if decision := (&AskUserTool{}).CheckPermissions(nil, &perm); decision.Behavior != permission.BehaviorALLOW {
			t.Fatalf("question requires a redundant approval in %s: %+v", mode, decision)
		}
	}
	perm := permission.NewContext()
	perm.Mode = permission.ModeDontAsk
	if decision := (&AskUserTool{}).CheckPermissions(nil, &perm); decision.Behavior != permission.BehaviorDENY {
		t.Fatal("unattended execution can prompt")
	}
}
