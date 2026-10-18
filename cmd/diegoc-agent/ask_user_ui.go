package main

import (
	"context"
	"fmt"
	"strings"

	"diegoc-agent/internal/tools"
	"github.com/mattn/go-runewidth"
)

type userQuestionRequest struct {
	Question tools.UserQuestion
	Reply    chan userQuestionResponse
}
type userQuestionResponse struct {
	Answer tools.UserAnswer
	Err    error
}

// Only the main terminal loop renders questions. Each exchange belongs to one
// run and has its own buffered reply so cancellation cannot block the UI.
func requestUserAnswer(ctx context.Context, requests chan<- userQuestionRequest, question tools.UserQuestion) (tools.UserAnswer, error) {
	if err := ctx.Err(); err != nil {
		return tools.UserAnswer{}, err
	}
	req := userQuestionRequest{Question: question, Reply: make(chan userQuestionResponse, 1)}
	select {
	case requests <- req:
	case <-ctx.Done():
		return tools.UserAnswer{}, ctx.Err()
	}
	select {
	case response := <-req.Reply:
		return response.Answer, response.Err
	case <-ctx.Done():
		return tools.UserAnswer{}, ctx.Err()
	}
}

func selectUserQuestion(question tools.UserQuestion) (int, error) {
	labels := make([]string, 0, len(question.Options)+1)
	details := make([]string, 0, len(question.Options)+1)
	for _, option := range question.Options {
		labels = append(labels, option.Label)
		details = append(details, option.Description)
	}
	labels = append(labels, "其他 / 自由输入")
	details = append(details, "输入自己的答案，Esc 取消当前执行。")
	return selectTerminalOption(question.Question, labels, 0, details)
}

func collectUserAnswer(question tools.UserQuestion, choose func(tools.UserQuestion) (int, error), prompt func(string) (string, error)) (tools.UserAnswer, error) {
	selected, err := choose(question)
	if err != nil {
		return tools.UserAnswer{}, err
	}
	if selected < 0 {
		return tools.UserAnswer{}, context.Canceled
	}
	if selected > len(question.Options) {
		return tools.UserAnswer{}, fmt.Errorf("invalid question selection")
	}
	fmt.Printf("\nAgent asks: %s\n", terminalReport(question.Question))
	if selected < len(question.Options) {
		answer := question.Options[selected].Label
		fmt.Printf("Your answer: %s\n", terminalReport(answer))
		return tools.UserAnswer{Answer: answer, Source: "option"}, nil
	}
	for {
		answer, err := prompt("Your answer › ")
		if err != nil {
			return tools.UserAnswer{}, err
		}
		answer = strings.TrimSpace(answer)
		if answer != "" {
			return tools.UserAnswer{Answer: answer, Source: "custom"}, nil
		}
		fmt.Println("请输入答案，或按 Esc 取消当前执行。")
	}
}

func questionHeader(question, description string, width, maxLines int) []string {
	text := question
	if description != "" {
		text += "  —  " + description
	}
	// Strip terminal controls before wrapping; account for Chinese display widths.
	text = selectorText(text, 10000)
	lines := strings.Split(runewidth.Wrap(text, width), "\n")
	if len(lines) > maxLines {
		lines = lines[:maxLines]
		lines[len(lines)-1] = runewidth.Truncate(lines[len(lines)-1]+"…", width, "…")
	}
	return lines
}
