package memory

import "diegoc-agent/internal/schema"

// InjectedMemoryMessage marks recalled data, distinct from real user turns and
// conversation summaries. Keep it at user authority, never in the system prompt.
func InjectedMemoryMessage(content string) schema.Message {
	return schema.Message{Role: "user", Name: "memory_context", Content: content}
}

func IsInjectedMemory(msg schema.Message) bool {
	return msg.Role == "user" && msg.Name == "memory_context"
}
