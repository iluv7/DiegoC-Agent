package agent

import (
	"context"
	"fmt"
	"strings"

	"diegoc-agent/internal/schema"
)

// summarizeIfNeeded checks token count and, if over limit, replaces execution segments with LLM-generated summaries.
func (a *Agent) summarizeIfNeeded(ctx context.Context) error {
	if a.skipNextTokenCheck {
		a.skipNextTokenCheck = false
		return nil
	}
	estimated := estimateTokens(a.Messages)
	if estimated <= a.TokenLimit && a.APITotalTokens <= a.TokenLimit {
		return nil
	}

	var userIndices []int
	for i, msg := range a.Messages {
		if msg.Role == "user" && i > 0 {
			userIndices = append(userIndices, i)
		}
	}
	if len(userIndices) == 0 {
		return nil
	}

	newMessages := make([]schema.Message, 0, len(userIndices)*2+1)
	newMessages = append(newMessages, a.Messages[0]) // system

	for i, userIdx := range userIndices {
		newMessages = append(newMessages, a.Messages[userIdx])

		nextUserIdx := len(a.Messages)
		if i+1 < len(userIndices) {
			nextUserIdx = userIndices[i+1]
		}
		execution := a.Messages[userIdx+1 : nextUserIdx]

		if len(execution) > 0 {
			summary, err := a.createSummary(ctx, execution, i+1)
			if err != nil {
				summary = buildSummaryFallback(execution, i+1)
			}
			if summary != "" {
				newMessages = append(newMessages, schema.Message{
					Role:    "user",
					Content: "[Assistant Execution Summary]\n\n" + summary,
				})
			}
		}
	}

	a.Messages = newMessages
	a.skipNextTokenCheck = true
	return nil
}

// createSummary calls the LLM to summarize one round of execution messages.
func (a *Agent) createSummary(ctx context.Context, execution []schema.Message, roundNum int) (string, error) {
	summaryContent := buildSummaryFallback(execution, roundNum)
	prompt := fmt.Sprintf(`Please provide a concise summary of the following Agent execution process:

%s

Requirements:
1. Focus on what tasks were completed and which tools were called
2. Keep key execution results and important findings
3. Be concise and clear, within 1000 words
4. Use English
5. Do not include "user" related content, only summarize the Agent's execution process`, summaryContent)

	msgs := []schema.Message{
		{Role: "system", Content: "You are an assistant skilled at summarizing Agent execution processes."},
		{Role: "user", Content: prompt},
	}
	resp, err := a.LLM.Generate(ctx, msgs, nil)
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

func buildSummaryFallback(execution []schema.Message, roundNum int) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Round %d execution process:\n\n", roundNum))
	for _, msg := range execution {
		if msg.Role == "assistant" {
			b.WriteString("Assistant: ")
			b.WriteString(contentString(msg))
			b.WriteString("\n")
			if len(msg.ToolCalls) > 0 {
				names := make([]string, len(msg.ToolCalls))
				for i, tc := range msg.ToolCalls {
					names[i] = tc.Function.Name
				}
				b.WriteString("  → Called tools: ")
				b.WriteString(strings.Join(names, ", "))
				b.WriteString("\n")
			}
		} else if msg.Role == "tool" {
			preview := contentString(msg)
			if len(preview) > 200 {
				preview = preview[:200] + "..."
			}
			b.WriteString("  ← Tool returned: ")
			b.WriteString(preview)
			b.WriteString("\n")
		}
	}
	return b.String()
}
