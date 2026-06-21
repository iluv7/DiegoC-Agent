package memory

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"diegoc-agent/internal/schema"
)

// TokenCounter 估算文本和消息的 token 数量。
// 实现可以是纯估算（快速、零依赖），也可以调真实 tokenizer（精确、需要模型文件）。
type TokenCounter interface {
	// CountText 返回纯文本字符串的估算 token 数。
	CountText(text string) int

	// CountMessages 返回消息列表的估算 token 数。
	CountMessages(messages []schema.Message) int
}

// RuleTokenCounter 基于字符数估算 token，不加载任何 tokenizer 模型。
// 快速、可移植，用于上下文窗口管理决策。
//
// 除数 3.75 是经验值：英文约 4 字符/token，中文约 1.5-2 字符/token，
// 3.75 是一个折中估计。
type RuleTokenCounter struct {
	// Divisor 控制字节到 token 的换算比例，默认 3.75。
	Divisor float64
}

// NewRuleTokenCounter 创建默认配置的 RuleTokenCounter（除数 3.75）。
func NewRuleTokenCounter() *RuleTokenCounter {
	return &RuleTokenCounter{Divisor: 3.75}
}

// CountText 估算纯文本字符串的 token 数。
// 计算方式：len(utf8字节) / 3.75。
func (r *RuleTokenCounter) CountText(text string) int {
	if text == "" {
		return 0
	}
	return int(float64(len(text)) / r.Divisor)
}

// CountMessages 估算消息列表的总 token 数。
// 逐条处理每条消息的 role、content、thinking 和 tool_calls，
// 使用相同的字符估算方式。
func (r *RuleTokenCounter) CountMessages(messages []schema.Message) int {
	total := 0
	for _, msg := range messages {
		total += r.countMessage(msg)
	}
	return total
}

// countMessage 估算单条消息的 token 数，包含所有 block。
func (r *RuleTokenCounter) countMessage(msg schema.Message) int {
	n := 0

	// 每条消息的 role 开销（约 2 token）
	n += 2

	// Content — 可能是纯字符串，也可能是 content block 列表
	n += r.countContent(msg.Content)

	// thinking block
	if msg.Thinking != "" {
		n += r.CountText(msg.Thinking)
	}

	// tool_use block：工具名 + JSON 序列化后的参数
	if len(msg.ToolCalls) > 0 {
		for _, tc := range msg.ToolCalls {
			n += r.CountText(tc.Function.Name)
			if argsJSON, err := json.Marshal(tc.Function.Arguments); err == nil {
				n += r.CountText(string(argsJSON))
			}
		}
	}

	// tool_result 消息的 tool_call_id 开销
	if msg.ToolCallID != "" {
		n += r.CountText(msg.ToolCallID)
	}

	// 工具名开销
	if msg.Name != "" {
		n += r.CountText(msg.Name)
	}

	return n
}

// countContent 处理 Content，可能是 string 或 []interface{}（content block 列表）。
func (r *RuleTokenCounter) countContent(content interface{}) int {
	switch c := content.(type) {
	case string:
		return r.CountText(c)

	case []interface{}:
		return r.countContentBlocks(c)

	default:
		// 未知类型直接转字符串
		return r.CountText(fmt.Sprint(c))
	}
}

// countContentBlocks 遍历 content block 列表，按 block 类型分别计数。
// 支持的 block 类型：text、thinking、image、audio、video、tool_use、tool_result。
func (r *RuleTokenCounter) countContentBlocks(blocks []interface{}) int {
	n := 0
	for _, block := range blocks {
		b, ok := block.(map[string]interface{})
		if !ok {
			n += r.CountText(fmt.Sprint(block))
			continue
		}

		blockType, _ := b["type"].(string)

		switch blockType {
		case "text":
			text, _ := b["text"].(string)
			n += r.CountText(text)

		case "thinking":
			thinking, _ := b["thinking"].(string)
			n += r.CountText(thinking)

		case "image", "audio", "video":
			n += r.countMediaBlock(b)

		case "tool_use":
			n += r.countToolUseBlock(b)

		case "tool_result":
			n += r.countToolResultBlock(b)

		default:
			// 未知 block 类型直接转字符串
			n += r.CountText(fmt.Sprint(b))
		}
	}
	return n
}

// countMediaBlock 估算 image/audio/video block 的 token 数。
// base64 数据：len(data) / 4（base64 每 4 字符编码 3 字节）。
// URL：直接计数字符串。
func (r *RuleTokenCounter) countMediaBlock(b map[string]interface{}) int {
	source, _ := b["source"].(map[string]interface{})
	if source == nil {
		return 10 // 最小开销
	}

	sourceType, _ := source["type"].(string)
	switch sourceType {
	case "base64":
		data, _ := source["data"].(string)
		if data != "" {
			return len(data) / 4
		}
		return 10

	default:
		url, _ := source["url"].(string)
		if url != "" {
			return r.CountText(url)
		}
		return 10
	}
}

// countToolUseBlock 估算 tool_use content block 的 token 数。
// 计数：工具名 + JSON 序列化的 input 参数。
func (r *RuleTokenCounter) countToolUseBlock(b map[string]interface{}) int {
	n := 4 // block 包装开销

	if name, ok := b["name"].(string); ok {
		n += r.CountText(name)
	}
	if input, ok := b["input"]; ok {
		inputJSON, err := json.Marshal(input)
		if err == nil {
			n += r.CountText(string(inputJSON))
		} else {
			n += r.CountText(fmt.Sprint(input))
		}
	}
	return n
}

// countToolResultBlock 估算 tool_result content block 的 token 数。
// 提取 output 中的文本部分（output 可能是 string 或 block 列表）。
func (r *RuleTokenCounter) countToolResultBlock(b map[string]interface{}) int {
	n := 4 // block 包装开销

	if name, ok := b["name"].(string); ok {
		n += r.CountText(name)
	}

	output := b["output"]
	switch o := output.(type) {
	case string:
		n += r.countToolResultText(o)
	case []interface{}:
		for _, part := range o {
			if p, ok := part.(map[string]interface{}); ok {
				if p["type"] == "text" {
					if text, ok := p["text"].(string); ok {
						n += r.countToolResultText(text)
					}
				}
			}
		}
	}
	return n
}

// countToolResultText 处理已截断的工具输出。
// 如果输出中包含截断标记 <<<TRUNCATED>>>，只计算标记之前的部分
// （截断部分已保存到磁盘文件，不在上下文中）。
func (r *RuleTokenCounter) countToolResultText(text string) int {
	if idx := truncationMarkerIndex(text); idx >= 0 {
		return r.CountText(text[:idx])
	}
	return r.CountText(text)
}

// truncationMarkerIndex 找到截断标记首次出现的位置，未找到返回 -1。
func truncationMarkerIndex(text string) int {
	marker := "<<<TRUNCATED>>>"
	for i := 0; i <= len(text)-len(marker); i++ {
		if text[i:i+len(marker)] == marker {
			return i
		}
	}
	return -1
}

// EstimateTokens 便捷函数，用默认 RuleTokenCounter（除数 3.75）估算消息列表 token。
// 用于迁移期间兼容现有的 agent.estimateTokens。
func EstimateTokens(messages []schema.Message) int {
	counter := NewRuleTokenCounter()
	return counter.CountMessages(messages)
}

// EstimateTokensWithDivisor 允许自定义除数，用于测试或适配特定模型 tokenizer。
func EstimateTokensWithDivisor(messages []schema.Message, divisor float64) int {
	counter := &RuleTokenCounter{Divisor: divisor}
	return counter.CountMessages(messages)
}

// CountTextLen 返回文本的字符数（rune 数）。
// 用于快速预检查，如判断工具输出大小是否需要截断。
func CountTextLen(text string) int {
	return utf8.RuneCountInString(text)
}
