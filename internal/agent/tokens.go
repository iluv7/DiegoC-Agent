package agent

import (
	"encoding/json"
	"fmt"

	"diegoc-agent/internal/schema"
)

// estimateTokens returns approximate token count for message history (chars/2.5 fallback, like Python).
func estimateTokens(messages []schema.Message) int {
	const charsPerToken = 2.5
	var totalChars int
	for _, msg := range messages {
		switch c := msg.Content.(type) {
		case string:
			totalChars += len(c)
		default:
			totalChars += len(fmt.Sprint(c))
		}
		totalChars += len(msg.Thinking)
		if len(msg.ToolCalls) > 0 {
			b, _ := json.Marshal(msg.ToolCalls)
			totalChars += len(b)
		}
		totalChars += 4 // metadata overhead per message
	}
	return int(float64(totalChars) / charsPerToken)
}

// contentString returns message content as a single string for summary building.
func contentString(msg schema.Message) string {
	switch c := msg.Content.(type) {
	case string:
		return c
	default:
		return fmt.Sprint(c)
	}
}
