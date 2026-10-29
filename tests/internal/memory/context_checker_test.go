package memory

import (
	"strings"
	"testing"

	"diegoc-agent/internal/schema"
)

func TestContextChecker_NoCompact(t *testing.T) {
	checker := NewContextChecker(newHandler(), 100000, 10000)

	// 少量消息，不超阈值
	messages := []schema.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	}
	toCompact, toKeep, isValid := checker.Check(messages)

	if len(toCompact) != 0 {
		t.Errorf("不超阈值 toCompact 应为空，得到 %d 条", len(toCompact))
	}
	if len(toKeep) != 2 {
		t.Errorf("不超阈值 toKeep 应为全部消息，得到 %d 条", len(toKeep))
	}
	if !isValid {
		t.Error("不超阈值 isValid 应为 true")
	}
}

func TestContextChecker_WithCompact(t *testing.T) {
	checker := NewContextChecker(newHandler(), 500, 200)

	// 大量消息触发压缩
	messages := make([]schema.Message, 0, 31)
	for i := 0; i < 30; i++ {
		messages = append(messages, schema.Message{
			Role:    "user",
			Content: "This is a message with lots of content " + strings.Repeat("x", 50),
		})
	}
	messages = append(messages, schema.Message{Role: "user", Content: "last message"})

	toCompact, toKeep, isValid := checker.Check(messages)

	if len(toCompact) == 0 {
		t.Error("超阈值应触发压缩，toCompact 不应为空")
	}
	if len(toKeep) == 0 {
		t.Error("应保留一些消息")
	}
	if toKeep[len(toKeep)-1].Content != "last message" {
		t.Error("最后一条消息应保留")
	}
	t.Logf("toCompact: %d, toKeep: %d, isValid: %v", len(toCompact), len(toKeep), isValid)
}

func TestContextChecker_Empty(t *testing.T) {
	checker := NewContextChecker(newHandler(), 1000, 100)
	toCompact, toKeep, isValid := checker.Check(nil)

	if len(toCompact) != 0 || len(toKeep) != 0 {
		t.Error("空消息应返回空")
	}
	if !isValid {
		t.Error("空消息 isValid 应为 true")
	}
}

func TestContextChecker_ThresholdBoundary(t *testing.T) {
	// 刚好等于阈值，不触发
	msgs := []schema.Message{
		{Role: "user", Content: "hello"},
	}
	counter := NewRuleTokenCounter()
	tokens := counter.CountMessages(msgs)

	// 阈值设得刚好比 token 多
	checker := NewContextChecker(newHandler(), tokens+1, 100)
	_, toKeep, _ := checker.Check(msgs)
	if len(toKeep) != 1 {
		t.Error("小于阈值不应触发压缩")
	}

	// 阈值设得刚好等于或小于
	checker2 := NewContextChecker(newHandler(), tokens, 100)
	toCompact, _, _ := checker2.Check(msgs)
	if len(toCompact) != 0 {
		t.Error("等于阈值不应触发压缩(total_tokens < threshold 才触发)")
	}
}
