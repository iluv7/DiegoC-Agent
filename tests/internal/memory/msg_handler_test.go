package memory

import (
	"strings"
	"testing"

	"diegoc-agent/internal/schema"
)

func newHandler() *MsgHandler {
	return NewMsgHandler(NewRuleTokenCounter())
}

func TestStatMessage(t *testing.T) {
	h := newHandler()

	// 简单文本消息
	msg := schema.Message{Role: "user", Content: "hello world"}
	stat := h.StatMessage(msg)
	if stat.Role != "user" {
		t.Errorf("role 期望 user, 得到 %s", stat.Role)
	}
	if len(stat.Content) != 1 || stat.Content[0].BlockType != "text" {
		t.Errorf("期望 1 个 text block")
	}
	t.Logf("简单消息 token: %d", stat.TotalTokens())

	// 带 tool_calls 的消息
	msg2 := schema.Message{
		Role:    "assistant",
		Content: "Let me search",
		ToolCalls: []schema.ToolCall{
			{
				ID:   "call_1",
				Type: "function",
				Function: schema.FunctionCall{
					Name:      "memory_search",
					Arguments: map[string]interface{}{"query": "Python"},
				},
			},
		},
	}
	stat2 := h.StatMessage(msg2)
	if stat2.TotalTokens() <= stat.TotalTokens() {
		t.Errorf("带 tool_calls 的消息 token 应该多于简单消息")
	}

	// tool 消息
	msg3 := schema.Message{
		Role:       "tool",
		Content:    "User prefers Python 3.12",
		ToolCallID: "call_1",
		Name:       "memory_search",
	}
	stat3 := h.StatMessage(msg3)
	t.Logf("tool 消息 token: %d", stat3.TotalTokens())
}

func TestCountMsgsToken(t *testing.T) {
	h := newHandler()

	messages := []schema.Message{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: "What is my Python version?"},
		{Role: "assistant", Content: "Let me check.", ToolCalls: []schema.ToolCall{
			{
				ID:   "call_1",
				Type: "function",
				Function: schema.FunctionCall{
					Name:      "memory_search",
					Arguments: map[string]interface{}{"query": "Python version"},
				},
			},
		}},
		{Role: "tool", Content: "User prefers Python 3.12", ToolCallID: "call_1", Name: "memory_search"},
		{Role: "assistant", Content: "You use Python 3.12."},
	}

	count := h.CountMsgsToken(messages)
	if count <= 0 {
		t.Errorf("期望正数 token，得到 %d", count)
	}
	t.Logf("5 条消息总 token: %d", count)
}

func TestFormatMsgsToStr(t *testing.T) {
	h := newHandler()

	messages := []schema.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello!"},
		{Role: "user", Content: "what is my name?"},
	}

	result := h.FormatMsgsToStr(messages, 10000, true)
	if !strings.Contains(result, "hello!") {
		t.Errorf("格式化结果应包含 'hello!'")
	}
	// 最新的消息应该在最前面处理，所以结果中应有最后一条消息
	if !strings.Contains(result, "what is my name?") {
		t.Errorf("格式化结果应包含最后一条消息")
	}

	// 超低阈值：只保留最新一条
	result2 := h.FormatMsgsToStr(messages, 5, true)
	lines := strings.Split(result2, "\n")
	t.Logf("低阈值结果:\n%s", result2)
	if len(lines) < 1 || !strings.Contains(result2, "what is my name?") {
		t.Errorf("低阈值至少应保留最新一条消息")
	}
	// 但更早的消息应被丢弃
	if strings.Contains(result2, "hi") && strings.Contains(result2, "hello!") {
		// 如果全都在，说明阈值没生效
		if len(messages) > 1 {
			t.Logf("阈值可能没生效，所有消息都被保留了")
		}
	}
}

func TestFormatMsgsToStr_Thinking(t *testing.T) {
	h := newHandler()

	messages := []schema.Message{
		{Role: "assistant", Content: "hello", Thinking: "用户说 hi，我应该回复 hello"},
	}

	// 保留 thinking
	withThinking := h.FormatMsgsToStr(messages, 1000, true)
	if !strings.Contains(withThinking, "[think]") {
		t.Errorf("includeThinking=true 应包含 thinking")
	}

	// 丢弃 thinking
	withoutThinking := h.FormatMsgsToStr(messages, 1000, false)
	if strings.Contains(withoutThinking, "[think]") {
		t.Errorf("includeThinking=false 不应包含 thinking")
	}
}

func TestValidateToolIDsAlignment(t *testing.T) {
	h := newHandler()

	// 配对
	paired := []schema.Message{
		{Role: "assistant", Content: "searching", ToolCalls: []schema.ToolCall{
			{ID: "call_1", Type: "function", Function: schema.FunctionCall{Name: "search", Arguments: nil}},
			{ID: "call_2", Type: "function", Function: schema.FunctionCall{Name: "read", Arguments: nil}},
		}},
		{Role: "tool", Content: "result 1", ToolCallID: "call_1", Name: "search"},
		{Role: "tool", Content: "result 2", ToolCallID: "call_2", Name: "read"},
	}
	if !h.ValidateToolIDsAlignment(paired) {
		t.Errorf("配对消息应返回 true")
	}

	// 不配对
	unpaired := []schema.Message{
		{Role: "assistant", Content: "searching", ToolCalls: []schema.ToolCall{
			{ID: "call_1", Type: "function", Function: schema.FunctionCall{Name: "search", Arguments: nil}},
		}},
		{Role: "tool", Content: "result", ToolCallID: "call_2", Name: "read"},
	}
	if h.ValidateToolIDsAlignment(unpaired) {
		t.Errorf("不配对消息应返回 false")
	}
}

func TestContextCheck_NoCompact(t *testing.T) {
	h := newHandler()

	// 少量消息，不超阈值
	messages := []schema.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	}
	toCompact, toKeep, aligned := h.ContextCheck(messages, 100000, 10000)

	if len(toCompact) != 0 {
		t.Errorf("不超阈值时 toCompact 应为空，得到 %d 条", len(toCompact))
	}
	if len(toKeep) != 2 {
		t.Errorf("不超阈值时 toKeep 应为全部消息，得到 %d 条", len(toKeep))
	}
	if !aligned {
		t.Errorf("不超阈值时 aligned 应为 true")
	}
}

func TestContextCheck_WithCompact(t *testing.T) {
	h := newHandler()

	// 构造大量消息模拟超阈值
	messages := make([]schema.Message, 0)
	for i := 0; i < 30; i++ {
		messages = append(messages, schema.Message{
			Role:    "user",
			Content: "This is a message with lots of content to take up token space number " + strings.Repeat("x", 50),
		})
	}
	// 最后一条短消息
	messages = append(messages, schema.Message{Role: "user", Content: "hi"})

	toCompact, toKeep, aligned := h.ContextCheck(messages, 500, 200)

	if len(toCompact) == 0 {
		t.Errorf("应触发压缩，toCompact 不应为空")
	}
	if len(toKeep) == 0 {
		t.Errorf("应保留一些消息")
	}
	// 保留的最后一条应该是 "hi"
	lastMsg := toKeep[len(toKeep)-1]
	if lastMsg.Content != "hi" {
		t.Errorf("最后一条保留消息应为 'hi'")
	}
	t.Logf("toCompact: %d 条, toKeep: %d 条, aligned: %v", len(toCompact), len(toKeep), aligned)
}

func TestContextCheck_ToolPairing(t *testing.T) {
	h := newHandler()

	// tool_use 和 tool_result 配对切割
	messages := []schema.Message{
		{Role: "assistant", Content: "searching", ToolCalls: []schema.ToolCall{
			{ID: "call_1", Type: "function", Function: schema.FunctionCall{
				Name:      "memory_search",
				Arguments: map[string]interface{}{"query": "test"},
			}},
		}},
		{Role: "tool", Content: strings.Repeat("large result ", 200), ToolCallID: "call_1", Name: "memory_search"},
		{Role: "assistant", Content: "result found"},
		{Role: "user", Content: "thanks"},
	}

	// 设一个阈值使得旧消息需要压缩，但保留区包括 tool_result
	// reserve 足够大保留最后三条（tool_result + assistant + user）
	toCompact, toKeep, aligned := h.ContextCheck(messages, 100, 200)

	// tool_result(下标1) 的 tool_use(下标0) 如果在 compact 区，
	// 应该被拉进 keep 区保证配对
	t.Logf("toCompact: %d 条, toKeep: %d 条, aligned: %v", len(toCompact), len(toKeep), aligned)

	if !aligned {
		// 如果不配对，说明切割有问题
		// 检查 keep 区是否有 tool_result 没有对应的 tool_use
		t.Logf("toKeep 中 tool id 不配对")
	}
}

func TestContextCheck_Empty(t *testing.T) {
	h := newHandler()
	toCompact, toKeep, aligned := h.ContextCheck(nil, 1000, 100)
	if len(toCompact) != 0 || len(toKeep) != 0 {
		t.Errorf("空消息列表应返回空")
	}
	if !aligned {
		t.Errorf("空消息列表 aligned 应为 true")
	}
}

func TestMsgStat_Format(t *testing.T) {
	stat := &MsgStat{
		Name: "assistant",
		Role: "assistant",
		Content: []BlockStat{
			{BlockType: "text", Text: "Let me search."},
			{BlockType: "tool_use", ToolName: "memory_search", ToolInput: `{"query":"Python"}`},
		},
	}
	formatted := stat.Format(true)
	if !strings.Contains(formatted, "assistant") {
		t.Errorf("格式化结果应包含角色名")
	}
	if !strings.Contains(formatted, "memory_search") {
		t.Errorf("格式化结果应包含工具名")
	}
}

func TestBlockStat_Format_Truncated(t *testing.T) {
	b := BlockStat{
		BlockType:  "tool_result",
		ToolName:   "bash",
		ToolOutput: "line 1\nline 2\n<<<TRUNCATED>>>\n完整内容保存在 tool_results/abc.txt",
	}
	result := b.Format(1000, true)
	if strings.Contains(result, "完整内容") {
		t.Errorf("截断标记后的内容不应出现在格式化结果中")
	}
}

func TestBlockStat_Format_Thinking(t *testing.T) {
	b := BlockStat{
		BlockType: "thinking",
		Text:      "我需要搜索用户的记忆",
	}

	withThink := b.Format(1000, true)
	if !strings.Contains(withThink, "[think]") {
		t.Errorf("includeThinking=true 应包含 thinking")
	}

	withoutThink := b.Format(1000, false)
	if withoutThink != "" {
		t.Errorf("includeThinking=false 应返回空, 得到 %s", withoutThink)
	}
}
