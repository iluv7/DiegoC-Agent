package memory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"diegoc-agent/internal/schema"
)

// MemoryMark 压缩标记。每条消息要么还在上下文里（MarkNone），
// 要么已经被压缩/持久化（MarkCompressed）。
type MemoryMark int

const (
	MarkNone       MemoryMark = iota // 正常在上下文中的消息
	MarkCompressed                   // 已压缩，可过滤 / 移除
)

// String 返回标记的可读名称。
func (m MemoryMark) String() string {
	switch m {
	case MarkNone:
		return "none"
	case MarkCompressed:
		return "compressed"
	default:
		return "unknown"
	}
}

// MarkedMessage 包装 schema.Message，附加压缩标记。
type MarkedMessage struct {
	Msg  schema.Message `json:"msg"`
	Mark MemoryMark     `json:"mark"`
}

// InMemoryMemory 管理会话的消息列表、压缩标记、摘要和 dialog 持久化。
//
// 对应 ReMe 的 InMemoryMemory（reme/memory/in_memory/in_memory_memory.py:32）。
type InMemoryMemory struct {
	content           []MarkedMessage
	compressedSummary string
	longTermMemory    string
	dialogDir         string
	counter           TokenCounter
}

// NewInMemoryMemory 创建会话内存管理器。
// dialogDir 是 dialog/ 目录路径（如 ".reme/dialog"），按 YYYY-MM-DD.jsonl 拆分。
func NewInMemoryMemory(dialogDir string, counter TokenCounter) *InMemoryMemory {
	return &InMemoryMemory{
		dialogDir: dialogDir,
		counter:   counter,
	}
}

// ---- 消息添加 ----

// AddMessage 追加一条消息到列表末尾，标记为 MarkNone。
func (m *InMemoryMemory) AddMessage(msg schema.Message) {
	m.content = append(m.content, MarkedMessage{Msg: msg, Mark: MarkNone})
}

// AddMessages 批量追加消息，全部标记为 MarkNone。
func (m *InMemoryMemory) AddMessages(msgs []schema.Message) {
	for _, msg := range msgs {
		m.content = append(m.content, MarkedMessage{Msg: msg, Mark: MarkNone})
	}
}

// ---- 摘要 / 记忆 ----

// SetCompressedSummary 设置压缩摘要（由 Compactor 产出）。
func (m *InMemoryMemory) SetCompressedSummary(summary string) {
	m.compressedSummary = summary
}

// SetLongTermMemory 设置长期记忆内容（从 memory/ 文件加载）。
func (m *InMemoryMemory) SetLongTermMemory(mem string) {
	m.longTermMemory = mem
}

// CompressedSummary 返回当前压缩摘要。
func (m *InMemoryMemory) CompressedSummary() string {
	return m.compressedSummary
}

// LongTermMemory 返回当前长期记忆。
func (m *InMemoryMemory) LongTermMemory() string {
	return m.longTermMemory
}

// ---- GetMemory ----

// GetMemory 构造发送给 LLM 的上下文：
//  1. 过滤掉 MarkCompressed 的消息
//  2. 如果有 longTermMemory 或 compressedSummary，在开头前置 # Memories / # Summary 块
//
// LLM 看到的最终格式：
//
//	# Memories
//	{longTermMemory}
//
//	# Summary of previous conversation
//	{compressedSummary}
//
//	{未压缩的消息}
func (m *InMemoryMemory) GetMemory() []schema.Message {
	result := make([]schema.Message, 0, len(m.content)+2)

	hasMemory := m.longTermMemory != ""
	hasSummary := m.compressedSummary != ""

	// 前置记忆块（作为 system 消息）
	if hasMemory || hasSummary {
		var block string
		if hasMemory {
			block += "# Memories\n" + m.longTermMemory
		}
		if hasSummary {
			if block != "" {
				block += "\n\n"
			}
			block += "# Summary of previous conversation\n" + m.compressedSummary
		}
		result = append(result, schema.Message{Role: "system", Content: block})
	}

	// 未压缩的消息
	for _, mm := range m.content {
		if mm.Mark != MarkCompressed {
			result = append(result, mm.Msg)
		}
	}

	return result
}

// ---- Token 估算 ----

// EstimateTokens 估算当前上下文相比模型上限的用量。
// 返回 used（已用）、free（剩余）token 数。
func (m *InMemoryMemory) EstimateTokens(maxInputLength int) (used int, free int) {
	msgs := m.GetMemory()
	used = m.counter.CountMessages(msgs)
	free = maxInputLength - used
	if free < 0 {
		free = 0
	}
	return used, free
}

// ---- 标记压缩 + dialog 持久化 ----

// MarkMessagesCompressed 把指定消息标记为 MarkCompressed，
// 写入 dialog/YYYY-MM-DD.jsonl，然后从 content 中移除。
//
// 传入的消息不需要是 content 里的同一个指针 — 内部按 (Role, Content, ToolCallID) 匹配。
func (m *InMemoryMemory) MarkMessagesCompressed(msgs []schema.Message) error {
	if len(msgs) == 0 {
		return nil
	}

	// 构建匹配集合
	toMark := make(map[string]bool, len(msgs))
	for _, msg := range msgs {
		key := msgFingerprint(msg)
		toMark[key] = true
	}

	// 收集要持久化的消息 + 标记
	today := time.Now().Format("2006-01-02")
	var toPersist []MarkedMessage
	newContent := make([]MarkedMessage, 0, len(m.content))

	for _, mm := range m.content {
		key := msgFingerprint(mm.Msg)
		if toMark[key] {
			mm.Mark = MarkCompressed
			toPersist = append(toPersist, mm)
			// 不放入 newContent → 移除
		} else {
			newContent = append(newContent, mm)
		}
	}

	m.content = newContent

	// 持久化到 dialog jsonl
	if len(toPersist) > 0 {
		return m.appendToDialog(today, toPersist)
	}
	return nil
}

// ClearContent 将所有消息标记为 MarkCompressed、持久化到 dialog、然后清空 content。
func (m *InMemoryMemory) ClearContent() error {
	if len(m.content) == 0 {
		return nil
	}

	today := time.Now().Format("2006-01-02")
	for i := range m.content {
		m.content[i].Mark = MarkCompressed
	}

	if err := m.appendToDialog(today, m.content); err != nil {
		return err
	}

	m.content = nil
	return nil
}

// AppendToDialog 直接向 dialog/YYYY-MM-DD.jsonl 追加消息（不修改 content）。
func (m *InMemoryMemory) AppendToDialog(msgs []schema.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	today := time.Now().Format("2006-01-02")
	entries := make([]MarkedMessage, len(msgs))
	for i, msg := range msgs {
		entries[i] = MarkedMessage{Msg: msg, Mark: MarkNone}
	}
	return m.appendToDialog(today, entries)
}

// ---- 内部方法 ----

// appendToDialog 把 marked messages 追加到 dialog/<date>.jsonl。
// 每条消息序列化为一行 JSON。
func (m *InMemoryMemory) appendToDialog(date string, entries []MarkedMessage) error {
	if len(entries) == 0 {
		return nil
	}
	if err := os.MkdirAll(m.dialogDir, 0755); err != nil {
		return fmt.Errorf("inmemory: create dialog dir: %w", err)
	}

	filePath := filepath.Join(m.dialogDir, date+".jsonl")
	f, err := os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("inmemory: open dialog file: %w", err)
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	now := time.Now().Format(time.RFC3339)
	for _, entry := range entries {
		record := struct {
			Timestamp string       `json:"timestamp"`
			Role      string       `json:"role"`
			Content   interface{}  `json:"content,omitempty"`
			Name      string       `json:"name,omitempty"`
			ToolCalls interface{}  `json:"tool_calls,omitempty"`
			Mark      string       `json:"mark"`
		}{
			Timestamp: now,
			Role:      entry.Msg.Role,
			Content:   entry.Msg.Content,
			Name:      entry.Msg.Name,
			ToolCalls: orNil(entry.Msg.ToolCalls),
			Mark:      entry.Mark.String(),
		}
		if err := enc.Encode(record); err != nil {
			return fmt.Errorf("inmemory: encode dialog entry: %w", err)
		}
	}
	return nil
}

// ---- 工具函数 ----

// msgFingerprint 生成消息的匹配键，用于 MarkMessagesCompressed 中去重匹配。
func msgFingerprint(msg schema.Message) string {
	return fmt.Sprintf("%s|%v|%s", msg.Role, msg.Content, msg.ToolCallID)
}

// orNil 返回 v 本身，如果 v 是空切片则返回 nil（JSON 省略空数组）。
func orNil(v []schema.ToolCall) interface{} {
	if len(v) == 0 {
		return nil
	}
	return v
}

// ---- 测试辅助 ----

// Content 返回底层 marked messages 列表（供测试 / 外部检查）。
func (m *InMemoryMemory) Content() []MarkedMessage {
	return m.content
}

// ContentLen 返回当前未压缩的消息条数。
func (m *InMemoryMemory) ContentLen() int {
	return len(m.content)
}

// LoadDialog 从 dialog/<date>.jsonl 读取当天对话历史并恢复到 content。
func (m *InMemoryMemory) LoadDialog(date string) ([]schema.Message, error) {
	filePath := filepath.Join(m.dialogDir, date+".jsonl")
	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("inmemory: read dialog: %w", err)
	}

	lines := splitLines(string(data))
	msgs := make([]schema.Message, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		var record struct {
			Role      string          `json:"role"`
			Content   interface{}     `json:"content,omitempty"`
			Name      string          `json:"name,omitempty"`
			ToolCalls []schema.ToolCall `json:"tool_calls,omitempty"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			continue // skip malformed lines
		}
		msgs = append(msgs, schema.Message{
			Role:     record.Role,
			Content:  record.Content,
			Name:     record.Name,
			// Note: ToolCalls loaded but without ToolCallID on individual calls
		})
	}
	return msgs, nil
}

// splitLines 按 \n 切分行，保留 Windows \r\n 兼容。
func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			line := s[start:i]
			// trim trailing \r
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			lines = append(lines, line)
			start = i + 1
		}
	}
	if start < len(s) {
		line := s[start:]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		lines = append(lines, line)
	}
	return lines
}
