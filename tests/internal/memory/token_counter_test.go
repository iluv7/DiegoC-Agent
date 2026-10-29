package memory

import (
	"testing"

	"diegoc-agent/internal/schema"
)

func TestRuleTokenCounter_CountText(t *testing.T) {
	c := NewRuleTokenCounter()

	tests := []struct {
		text     string
		minToken int
		maxToken int
		desc     string
	}{
		{"hello world", 2, 5, "简单英文"},
		{"", 0, 0, "空字符串"},
		{"Python", 1, 3, "单个单词"},
		{"你好世界", 1, 4, "中文文本"},
		{"hello world, this is a longer sentence for testing", 5, 15, "较长文本"},
	}

	for _, tt := range tests {
		got := c.CountText(tt.text)
		if got < tt.minToken || got > tt.maxToken {
			t.Errorf("%s: CountText(%q)=%d, 期望 %d-%d 之间",
				tt.desc, tt.text, got, tt.minToken, tt.maxToken)
		}
	}
}

func TestRuleTokenCounter_CountMessages(t *testing.T) {
	c := NewRuleTokenCounter()

	messages := []schema.Message{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: "What is my Python version preference?"},
		{Role: "assistant", Content: "Let me check your memory.", ToolCalls: []schema.ToolCall{
			{
				ID:   "call_1",
				Type: "function",
				Function: schema.FunctionCall{
					Name:      "memory_search",
					Arguments: map[string]interface{}{"query": "Python version"},
				},
			},
		}},
		{
			Role:       "tool",
			Content:    "User prefers Python 3.12",
			ToolCallID: "call_1",
			Name:       "memory_search",
		},
		{Role: "assistant", Content: "According to your memory, you use Python 3.12."},
	}

	count := c.CountMessages(messages)
	if count <= 0 {
		t.Errorf("期望正数 token，得到 %d", count)
	}
	t.Logf("5 条消息总 token: %d", count)

	// 长消息的 token 应该比短消息多
	short := []schema.Message{
		{Role: "user", Content: "hi"},
	}
	long := []schema.Message{
		{Role: "user", Content: "This is a very long message with lots of content that should produce significantly more tokens than a short greeting like hi"},
	}
	if c.CountMessages(short) >= c.CountMessages(long) {
		t.Errorf("长消息 token 应该多于短消息")
	}
}

func TestRuleTokenCounter_ContentBlocks(t *testing.T) {
	c := NewRuleTokenCounter()

	// 包含 content block 的消息（模拟 tool_use + tool_result）
	messages := []schema.Message{
		{
			Role: "assistant",
			Content: []interface{}{
				map[string]interface{}{"type": "text", "text": "Let me search."},
				map[string]interface{}{
					"type":  "tool_use",
					"name":  "memory_search",
					"input": map[string]interface{}{"query": "Python version"},
				},
			},
		},
		{
			Role: "tool",
			Content: []interface{}{
				map[string]interface{}{
					"type":   "tool_result",
					"name":   "memory_search",
					"output": "User prefers Python 3.12 for all projects.",
				},
			},
		},
	}

	count := c.CountMessages(messages)
	if count <= 0 {
		t.Errorf("content block 消息应返回正数 token，得到 %d", count)
	}
	t.Logf("content block 消息 token: %d", count)
}

func TestRuleTokenCounter_TruncatedToolResult(t *testing.T) {
	c := NewRuleTokenCounter()

	// 未被截断的输出
	full := []schema.Message{{
		Role: "tool",
		Content: []interface{}{
			map[string]interface{}{
				"type":   "tool_result",
				"name":   "bash",
				"output": "Short result",
			},
		},
	}}
	fullCount := c.CountMessages(full)

	// 已被截断的输出（含 <<<TRUNCATED>>> 标记）
	truncated := []schema.Message{{
		Role: "tool",
		Content: []interface{}{
			map[string]interface{}{
				"type":   "tool_result",
				"name":   "bash",
				"output": "Start of result\n<<<TRUNCATED>>>\n完整内容保存在 tool_results/abc.txt\n更多内容...",
			},
		},
	}}
	truncCount := c.CountMessages(truncated)

	// 截断后的 token 不应显著多于完整输出（只计算标记之前的部分）
	if truncCount >= fullCount+5 {
		t.Errorf("截断输出 token(%d) 不应显著大于完整输出 token(%d)", truncCount, fullCount)
	}
	t.Logf("完整: %d token, 截断: %d token", fullCount, truncCount)
}

func TestEstimateTokens(t *testing.T) {
	messages := []schema.Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi there!"},
	}
	count := EstimateTokens(messages)
	if count <= 0 {
		t.Errorf("EstimateTokens 返回 %d，期望 > 0", count)
	}
	t.Logf("EstimateTokens: %d", count)
}

func TestCountTextLen(t *testing.T) {
	if l := CountTextLen("hello"); l != 5 {
		t.Errorf("CountTextLen('hello')=%d, 期望 5", l)
	}
	if l := CountTextLen("你好"); l != 2 {
		t.Errorf("CountTextLen('你好')=%d, 期望 2", l)
	}
	if l := CountTextLen(""); l != 0 {
		t.Errorf("CountTextLen('')=%d, 期望 0", l)
	}
}
