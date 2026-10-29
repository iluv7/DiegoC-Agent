package memory

import (
	"context"
	"strings"
	"testing"

	"diegoc-agent/internal/llm"
	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

// mockLLM 模拟 LLM 客户端，返回预设内容。
type mockLLM struct {
	response string
	err      error
}

func (m *mockLLM) Generate(_ context.Context, _ []schema.Message, _ []tools.Tool) (*schema.LLMResponse, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &schema.LLMResponse{Content: m.response}, nil
}

var _ llm.Client = (*mockLLM)(nil)

func TestCompactor_InitialCompact(t *testing.T) {
	h := newHandler()
	c := NewCompactor(h, 100000, "", true)

	messages := []schema.Message{
		{Role: "user", Content: "Help me write a Python script"},
		{Role: "assistant", Content: "Sure, let me create a script for you.", ToolCalls: []schema.ToolCall{
			{ID: "call_1", Type: "function", Function: schema.FunctionCall{
				Name:      "write_file",
				Arguments: map[string]interface{}{"file_path": "script.py", "content": "print('hello')"},
			}},
		}},
		{Role: "tool", Content: "Wrote file", ToolCallID: "call_1", Name: "write_file"},
		{Role: "assistant", Content: "Done! The script is ready."},
	}

	// 模拟 LLM 返回有效摘要
	summary := "## Goal\nWrite a Python script\n\n## Progress\n### Done\n- [x] Created script.py\n\n## Key Decisions\n- Used write_file tool\n\n## Next Steps\n1. Test the script"
	mock := &mockLLM{response: summary}

	result := c.Compact(context.Background(), mock, messages, "")
	if result == "" {
		t.Fatal("Compact 应返回非空结果")
	}
	if !strings.Contains(result, "## Goal") {
		t.Error("摘要应包含 ## Goal")
	}
}

func TestCompactor_IncrementalUpdate(t *testing.T) {
	h := newHandler()
	c := NewCompactor(h, 100000, "", true)

	previous := "## Goal\nWrite a Python script\n\n## Progress\n### Done\n- [x] Created script.py\n\n## Next Steps\n1. Add tests"

	messages := []schema.Message{
		{Role: "user", Content: "Now add tests"},
		{Role: "assistant", Content: "Adding tests now."},
	}

	updatedSummary := "## Goal\nWrite a Python script\n\n## Progress\n### Done\n- [x] Created script.py\n- [x] Added tests\n\n## Key Decisions\n- Use pytest\n\n## Next Steps\n1. Run the tests"
	mock := &mockLLM{response: updatedSummary}

	result := c.Compact(context.Background(), mock, messages, previous)
	if result == "" {
		t.Fatal("增量更新应返回结果")
	}
	if !strings.Contains(result, "## Goal") {
		t.Error("增量摘要应保留 ## Goal")
	}
}

func TestCompactor_InvalidSummary(t *testing.T) {
	h := newHandler()
	c := NewCompactor(h, 100000, "", true)

	messages := []schema.Message{
		{Role: "user", Content: "hi"},
	}

	// 模拟 LLM 返回无效摘要（没有 ##）
	mock := &mockLLM{response: "Just some text without proper structure"}

	result := c.Compact(context.Background(), mock, messages, "")
	if result != "" {
		t.Error("无效摘要应返回空字符串")
	}
}

func TestCompactor_EmptyMessages(t *testing.T) {
	h := newHandler()
	c := NewCompactor(h, 100000, "", true)

	mock := &mockLLM{response: "## Goal\nNothing"}
	result := c.Compact(context.Background(), mock, nil, "")
	if result != "" {
		t.Error("空消息应返回空字符串")
	}
}

func TestCompactor_ExtraInstruction(t *testing.T) {
	h := newHandler()
	c := NewCompactor(h, 100000, "", true)
	c.SetExtraInstruction("Remove debug logs, keep requirements and decisions")

	messages := []schema.Message{
		{Role: "user", Content: "Debug this function"},
		{Role: "assistant", Content: "Here is the debug output: ..."},
	}

	summary := "## Goal\nDebug the function\n\n## Progress\n### Done\n- [x] Identified error\n\n## Key Decisions\n- Use debugger\n\n## Critical Context\n- Error in main.py line 42"
	mock := &mockLLM{response: summary}

	result := c.Compact(context.Background(), mock, messages, "")
	if result == "" {
		t.Fatal("带额外指令的 Compact 应返回结果")
	}
}

func TestCompactor_ChineseLanguage(t *testing.T) {
	h := newHandler()
	c := NewCompactor(h, 100000, "zh", true)

	messages := []schema.Message{
		{Role: "user", Content: "帮我写一个脚本"},
	}

	summary := "## 目标\n写一个脚本\n\n## 进展\n### 已完成\n- [x] 创建脚本\n\n## 关键决策\n- 使用 Python\n\n## 下一步\n1. 测试脚本"
	mock := &mockLLM{response: summary}

	result := c.Compact(context.Background(), mock, messages, "")
	if result == "" {
		t.Fatal("中文 Compact 应返回结果")
	}
}

func TestCompactor_NoThinkingBlock(t *testing.T) {
	h := newHandler()
	c := NewCompactor(h, 100000, "", false) // addThinkingBlock=false

	messages := []schema.Message{
		{Role: "assistant", Content: "hello", Thinking: "用户说了 hi"},
	}

	// 验证 thinking 被排除：格式化消息中不应包含 [think]
	formatted := h.FormatMsgsToStr(messages, 1000, false)
	if strings.Contains(formatted, "[think]") {
		t.Error("addThinkingBlock=false 时不应包含 thinking")
	}

	summary := "## Goal\nSay hello\n\n## Progress\n### Done\n- [x] Responded\n\n## Key Decisions\n- Greeted user\n\n## Next Steps\n1. Wait for input"
	mock := &mockLLM{response: summary}

	result := c.Compact(context.Background(), mock, messages, "")
	if result == "" {
		t.Fatal("应返回有效结果")
	}
}

func TestIsValidSummary(t *testing.T) {
	tests := []struct {
		content string
		valid   bool
	}{
		// 4 个字段 + 足够长度，通过
		{"## Goal\nWrite code for the project.\n\n## Progress\n### Done\n- [x] Finished.\n\n## Key Decisions\n- Use Go.\n\n## Next Steps\n1. Test everything.", true},
		// 只有 3 个字段，不够
		{"## Goal\nSomething\n\n## Progress\nDone\n\n## Next Steps\n1. Test it please!", false},
		// 只有 2 个有效字段，不够
		{"## Goal\nBrief.\n\n## Progress\nDone.\n\n### Other\nStuff", false},
		// 没有 ##
		{"Just plain text without any structure at all really and definitely not structured", false},
		// 空
		{"", false},
		// 6 个字段全有
		{"## Goal\nBuild the whole application.\n\n## Constraints & Preferences\n- Use Go.\n\n## Progress\n### Done\n- [x] Setup.\n\n## Key Decisions\n- SQLite.\n\n## Next Steps\n1. Deploy.\n\n## Critical Context\n- main.go", true},
	}
	for _, tt := range tests {
		got := isValidSummary(tt.content)
		if got != tt.valid {
			t.Errorf("isValidSummary(%q)=%v, 期望 %v", tt.content, got, tt.valid)
		}
	}
}
