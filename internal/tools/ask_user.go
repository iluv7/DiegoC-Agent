package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"diegoc-agent/internal/permission"
)

type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type UserQuestion struct {
	Question string           `json:"question"`
	Options  []QuestionOption `json:"options"`
}

type UserAnswer struct {
	Answer string `json:"answer"`
	Source string `json:"source"` // "option" or "custom"
}

// Question handlers belong to one run's context, never to the shared tool.
// Background workers without a foreground handler fail instead of reading stdin.
type UserQuestionHandler func(context.Context, UserQuestion) (UserAnswer, error)
type userQuestionHandlerKey struct{}

func WithUserQuestionHandler(ctx context.Context, handler UserQuestionHandler) context.Context {
	return context.WithValue(ctx, userQuestionHandlerKey{}, handler)
}

// UserInteractionTool waits for human input without holding the shared file/tool lock.
type UserInteractionTool interface {
	RequiresUserInteraction() bool
}

type AskUserTool struct{}

func (t *AskUserTool) Name() string { return "ask_user" }
func (t *AskUserTool) Description() string {
	return "Ask the user one concrete question with 2-4 distinct options when a missing preference or requirement materially changes the work. The user can select an option or enter a custom answer; execution continues with their answer. Put a recommended option first and explain trade-offs in descriptions. Do not use for routine decisions or tool permission approval. Only available in foreground interactive runs; background agents should report the question to their parent instead."
}
func (t *AskUserTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"question": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 500, "description": "A self-contained question in the user's language."},
			"options": map[string]interface{}{
				"type": "array", "minItems": 2, "maxItems": 4,
				"description": "Distinct choices. Do not add an Other option; custom input is provided automatically.",
				"items": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"label":       map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 80},
						"description": map[string]interface{}{"type": "string", "maxLength": 300},
					},
					"required": []string{"label"}, "additionalProperties": false,
				},
			},
		},
		"required": []string{"question", "options"}, "additionalProperties": false,
	}
}

func parseUserQuestion(args map[string]interface{}) (UserQuestion, error) {
	var q UserQuestion
	data, err := json.Marshal(args)
	if err != nil {
		return q, fmt.Errorf("invalid question: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&q); err != nil {
		return q, fmt.Errorf("invalid question: %w", err)
	}
	q.Question = strings.TrimSpace(q.Question)
	if q.Question == "" || utf8.RuneCountInString(q.Question) > 500 {
		return q, fmt.Errorf("question must contain 1-500 characters")
	}
	if len(q.Options) < 2 || len(q.Options) > 4 {
		return q, fmt.Errorf("provide 2-4 distinct options")
	}
	seen := map[string]bool{}
	for i := range q.Options {
		option := &q.Options[i]
		option.Label = strings.TrimSpace(option.Label)
		option.Description = strings.TrimSpace(option.Description)
		key := strings.ToLower(option.Label)
		if key == "" || utf8.RuneCountInString(option.Label) > 80 || seen[key] {
			return q, fmt.Errorf("option labels must be distinct and contain 1-80 characters")
		}
		if utf8.RuneCountInString(option.Description) > 300 {
			return q, fmt.Errorf("option descriptions must contain at most 300 characters")
		}
		seen[key] = true
	}
	return q, nil
}

func (t *AskUserTool) Execute(ctx context.Context, args map[string]interface{}) (*ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	q, err := parseUserQuestion(args)
	if err != nil {
		return &ToolResult{Error: err.Error()}, nil
	}
	handler, _ := ctx.Value(userQuestionHandlerKey{}).(UserQuestionHandler)
	if handler == nil {
		return &ToolResult{Error: "ask_user requires a foreground interactive question handler. Report the question to the user or parent agent and wait for their answer; do not guess."}, nil
	}
	answer, err := handler(ctx, q)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	answer.Answer = strings.TrimSpace(answer.Answer)
	if answer.Answer == "" || (answer.Source != "option" && answer.Source != "custom") {
		return &ToolResult{Error: "question handler returned an invalid answer"}, nil
	}
	if answer.Source == "option" {
		matched := false
		for _, option := range q.Options {
			matched = matched || answer.Answer == option.Label
		}
		if !matched {
			return &ToolResult{Error: "question handler returned an unknown option"}, nil
		}
	}
	data, _ := json.Marshal(struct {
		Question string `json:"question"`
		UserAnswer
	}{q.Question, answer})
	return &ToolResult{Success: true, Content: string(data)}, nil
}

func (t *AskUserTool) CheckPermissions(_ map[string]interface{}, ctx *permission.Context) permission.Decision {
	if ctx.Mode == permission.ModeDontAsk {
		return permission.Decision{Behavior: permission.BehaviorDENY, Message: "ask_user is unavailable in unattended mode"}
	}
	return permission.Decision{Behavior: permission.BehaviorALLOW}
}
func (t *AskUserTool) RequiresUserInteraction() bool { return true }
func (t *AskUserTool) IsConcurrencySafe() bool       { return true }
func (t *AskUserTool) IsReadOnly() bool              { return true }
func (t *AskUserTool) IsExternalTool() bool          { return false }
