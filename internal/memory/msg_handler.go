package memory

import (
	"encoding/json"
	"fmt"
	"strings"

	"diegoc-agent/internal/schema"
)

// BlockStat 单个 content block 的统计信息。
type BlockStat struct {
	BlockType  string // text / thinking / image / audio / video / tool_use / tool_result
	Text       string // block 的文本内容
	TokenCount int    // 该 block 的 token 数
	ToolName   string // tool_use / tool_result 的工具名
	ToolInput  string // tool_use 的 JSON 参数
	ToolOutput string // tool_result 的输出文本
	MediaURL   string // image/audio/video 的 URL
}

// MsgStat 单条消息的统计信息。
type MsgStat struct {
	Name      string      // 消息 name（如 agent 名称）
	Role      string      // user / assistant / system / tool
	Content   []BlockStat // 分解后的 block 列表
	Timestamp string      // 消息时间戳
	Metadata  map[string]interface{}
}

// TotalTokens 返回该消息所有 block 的总 token 数。
func (m *MsgStat) TotalTokens() int {
	total := 0
	for _, b := range m.Content {
		total += b.TokenCount
	}
	return total
}

// MsgHandler 消息处理工具类。所有组件都依赖它，是消息→token 的桥梁。
type MsgHandler struct {
	counter TokenCounter
}

// NewMsgHandler 创建 MsgHandler。
func NewMsgHandler(counter TokenCounter) *MsgHandler {
	return &MsgHandler{counter: counter}
}

// CountStrToken 计算纯文本字符串的 token 数。
func (h *MsgHandler) CountStrToken(text string) int {
	return h.counter.CountText(text)
}

// CountMsgsToken 批量计算消息列表的总 token 数。
func (h *MsgHandler) CountMsgsToken(messages []schema.Message) int {
	return h.counter.CountMessages(messages)
}

// StatMessage 解析单条消息，生成详细的 block 统计信息。
// 支持的 block 类型：text、thinking、image、audio、video、tool_use、tool_result。
func (h *MsgHandler) StatMessage(msg schema.Message) *MsgStat {
	stat := &MsgStat{
		Name:     msg.Name,
		Role:     msg.Role,
		Metadata: make(map[string]interface{}),
	}

	if msg.Name == "" {
		stat.Name = msg.Role
	}

	// 解析 content
	switch c := msg.Content.(type) {
	case string:
		stat.Content = append(stat.Content, BlockStat{
			BlockType:  "text",
			Text:       c,
			TokenCount: h.counter.CountText(c),
		})

	case []interface{}:
		for _, block := range c {
			b, ok := block.(map[string]interface{})
			if !ok {
				text := fmt.Sprint(block)
				stat.Content = append(stat.Content, BlockStat{
					BlockType:  "unknown",
					Text:       text,
					TokenCount: h.counter.CountText(text),
				})
				continue
			}

			blockType, _ := b["type"].(string)

			switch blockType {
			case "text":
				text, _ := b["text"].(string)
				stat.Content = append(stat.Content, BlockStat{
					BlockType:  "text",
					Text:       text,
					TokenCount: h.counter.CountText(text),
				})

			case "thinking":
				thinking, _ := b["thinking"].(string)
				stat.Content = append(stat.Content, BlockStat{
					BlockType:  "thinking",
					Text:       thinking,
					TokenCount: h.counter.CountText(thinking),
				})
			}
		}
	}

	// thinking 字段
	if msg.Thinking != "" {
		stat.Content = append(stat.Content, BlockStat{
			BlockType:  "thinking",
			Text:       msg.Thinking,
			TokenCount: h.counter.CountText(msg.Thinking),
		})
	}

	// tool_calls (tool_use block)
	for _, tc := range msg.ToolCalls {
		inputJSON, _ := json.Marshal(tc.Function.Arguments)
		inputStr := string(inputJSON)
		stat.Content = append(stat.Content, BlockStat{
			BlockType:  "tool_use",
			Text:       tc.Function.Name + inputStr,
			TokenCount: h.counter.CountText(tc.Function.Name) + h.counter.CountText(inputStr),
			ToolName:   tc.Function.Name,
			ToolInput:  inputStr,
		})
	}

	return stat
}

// FormatMsgsToStr 把消息列表格式化为单个字符串，从最新到最旧累加 token，
// 超过 threshold 就把更旧的消息丢弃。
// includeThinking 控制是否保留 thinking block 内容。
func (h *MsgHandler) FormatMsgsToStr(messages []schema.Message, threshold int, includeThinking bool) string {
	if len(messages) == 0 {
		return ""
	}

	// 从最新到最旧处理
	formattedParts := make([]string, 0, len(messages))
	totalTokens := 0

	for i := len(messages) - 1; i >= 0; i-- {
		stat := h.StatMessage(messages[i])
		formatted := stat.Format(includeThinking)
		contentTokens := h.counter.CountText(formatted)

		isLatest := i == len(messages)-1

		// 不是最后一条，加上就超过阈值就停
		if !isLatest && totalTokens+contentTokens > threshold {
			break
		}

		// 最新的那条消息即使自己就超阈值也保留
		formattedParts = append(formattedParts, formatted)
		totalTokens += contentTokens
	}

	// 反转回来（从旧到新）
	for i, j := 0, len(formattedParts)-1; i < j; i, j = i+1, j-1 {
		formattedParts[i], formattedParts[j] = formattedParts[j], formattedParts[i]
	}

	return strings.Join(formattedParts, "\n\n")
}

// ValidateToolIDsAlignment 校验消息列表中 tool_use 和 tool_result 的 ID 是否完全配对。
func (h *MsgHandler) ValidateToolIDsAlignment(messages []schema.Message) bool {
	toolUseIDs := make(map[string]bool)
	toolResultIDs := make(map[string]bool)

	for _, msg := range messages {
		for _, tc := range msg.ToolCalls {
			if tc.ID != "" {
				toolUseIDs[tc.ID] = true
			}
		}
		if msg.ToolCallID != "" {
			toolResultIDs[msg.ToolCallID] = true
		}
	}

	// 两个集合应该完全一致
	if len(toolUseIDs) != len(toolResultIDs) {
		return false
	}
	for id := range toolUseIDs {
		if !toolResultIDs[id] {
			return false
		}
	}
	return true
}

// ContextCheck 检查上下文是否超阈值，如果超了就把消息切成两半。
// 从尾部向前累计 token，保留 reserve 个 token 的最近消息作为 toKeep。
// 保证 toKeep 中的 tool_use / tool_result 配对不被拆散。
//
// 返回：
//
//	toCompact — 需要压缩的旧消息
//	toKeep    — 保留在上下文中的最近消息
//	aligned   — toKeep 中的 tool id 是否配对
func (h *MsgHandler) ContextCheck(messages []schema.Message, threshold int, reserve int) (toCompact, toKeep []schema.Message, aligned bool) {
	if len(messages) == 0 {
		return nil, nil, true
	}

	// 计算每条消息的 token 统计 + 总 token
	type msgWithStat struct {
		msg  schema.Message
		stat *MsgStat
	}
	msgStats := make([]msgWithStat, len(messages))
	totalTokens := 0
	for i, msg := range messages {
		stat := h.StatMessage(msg)
		msgStats[i] = msgWithStat{msg: msg, stat: stat}
		totalTokens += stat.TotalTokens()
	}

	// 没超阈值，不需要切
	if totalTokens < threshold {
		return nil, messages, true
	}

	// 收集 tool_use id 和 tool_result id 所在的消息下标
	toolUseLocs := make(map[string]int)    // tool_use_id → msg index
	toolResultLocs := make(map[string]int) // tool_result_id → msg index

	for i, ms := range msgStats {
		// tool_use 来自 ToolCalls
		for _, tc := range ms.msg.ToolCalls {
			if tc.ID != "" {
				toolUseLocs[tc.ID] = i
			}
		}
		// tool_result 来自 ToolCallID 字段
		if ms.msg.ToolCallID != "" {
			toolResultLocs[ms.msg.ToolCallID] = i
		}
	}

	// 从尾部向前累加，保留 reserve 个 token
	keepSet := make(map[int]bool)
	accumulated := 0

	for i := len(msgStats) - 1; i >= 0; i-- {
		// 已经因为依赖被加入的跳过
		if keepSet[i] {
			continue
		}

		ms := msgStats[i]

		// 加上就超了，停
		if accumulated+ms.stat.TotalTokens() > reserve {
			break
		}

		// 检查这条消息是否有 tool_result，如果有，对应的 tool_use 也要拉进来
		extraTokens := 0
		deps := make(map[int]bool)

		// 从 ToolCallID 找到对应的 tool_use
		if ms.msg.ToolCallID != "" {
			if toolUseIdx, ok := toolUseLocs[ms.msg.ToolCallID]; ok {
				if toolUseIdx != i && !keepSet[toolUseIdx] {
					deps[toolUseIdx] = true
					extraTokens += msgStats[toolUseIdx].stat.TotalTokens()
				}
			}
		}

		// 加上依赖也超了，停
		if accumulated+ms.stat.TotalTokens()+extraTokens > reserve {
			break
		}

		keepSet[i] = true
		for idx := range deps {
			keepSet[idx] = true
		}
		accumulated += ms.stat.TotalTokens() + extraTokens
	}

	// 按 keepSet 构建两个列表（保持原始顺序）
	toCompact = make([]schema.Message, 0)
	toKeep = make([]schema.Message, 0)
	for i, ms := range msgStats {
		if keepSet[i] {
			toKeep = append(toKeep, ms.msg)
		} else {
			toCompact = append(toCompact, ms.msg)
		}
	}

	aligned = h.ValidateToolIDsAlignment(toKeep)

	return toCompact, toKeep, aligned
}

// Format 把 MsgStat 格式化为单行字符串表示。
// maxLength 控制每段文本的最大长度，includeThinking 控制是否保留 thinking。
func (m *MsgStat) Format(includeThinking bool) string {
	timeStr := ""
	if m.Timestamp != "" {
		timeStr = "[" + m.Timestamp + "] "
	}
	header := timeStr + m.Name + ":"

	blocks := make([]string, 0)
	for _, b := range m.Content {
		formatted := b.Format(1000, includeThinking)
		if formatted != "" {
			blocks = append(blocks, formatted)
		}
	}
	return header + "\n" + strings.Join(blocks, "\n")
}

// Format 把 BlockStat 格式化为单行字符串。
func (b *BlockStat) Format(maxLength int, includeThinking bool) string {
	switch b.BlockType {
	case "text":
		if b.Text == "" {
			return ""
		}
		return "[text]: " + truncate(b.Text, maxLength)

	case "thinking":
		if !includeThinking || b.Text == "" {
			return ""
		}
		return "[think]: " + truncate(b.Text, maxLength)

	case "image", "audio", "video":
		content := b.MediaURL
		if content == "" {
			content = ""
		}
		return "[" + b.BlockType + "]: " + content

	case "tool_use":
		content := b.ToolName + " params=" + truncate(b.ToolInput, maxLength)
		return "[tool_use]: " + content

	case "tool_result":
		if b.ToolOutput == "" {
			return ""
		}
		// 截断标记之后的部分不计入
		display := b.ToolOutput
		if idx := strings.Index(display, "<<<TRUNCATED>>>"); idx >= 0 {
			display = display[:idx]
		}
		content := b.ToolName + " output=" + truncate(display, maxLength)
		return "[tool_result]: " + content

	default:
		return ""
	}
}

// truncate 截断文本，替换换行为空格，超出长度加 ...。
func truncate(text string, maxLength int) string {
	text = strings.ReplaceAll(text, "\n", " ")
	if len([]rune(text)) <= maxLength {
		return text
	}
	runes := []rune(text)
	return string(runes[:maxLength]) + "..."
}
