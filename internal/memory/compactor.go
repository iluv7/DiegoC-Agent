package memory

import (
	"context"
	"strings"

	"diegoc-agent/internal/llm"
	"diegoc-agent/internal/schema"
)

// Compactor 对话压缩器。调用 LLM 将消息列表压缩为结构化摘要，支持增量更新。
//
// 对应 ReMe 的 Compactor (reme/memory/file_based/components/compactor.py:29)。
type Compactor struct {
	handler          *MsgHandler
	threshold        int    // 传入 LLM 的消息 token 上限
	language         string // "zh" 中文，空字符串英文
	addThinkingBlock bool   // 是否保留 thinking block
	extraInstruction string // 额外指令
}

// NewCompactor 创建对话压缩器。
func NewCompactor(handler *MsgHandler, threshold int, language string, addThinkingBlock bool) *Compactor {
	return &Compactor{
		handler:          handler,
		threshold:        threshold,
		language:         language,
		addThinkingBlock: addThinkingBlock,
	}
}

// SetExtraInstruction 设置额外指令（如 "Remove debug logs, keep requirements"）。
func (c *Compactor) SetExtraInstruction(instruction string) {
	c.extraInstruction = instruction
}

// Compact 压缩消息列表为结构化摘要。
// previousSummary 非空时走增量更新路径，将新对话合并到已有摘要中。
func (c *Compactor) Compact(ctx context.Context, llmClient llm.Client, messages []schema.Message, previousSummary string) string {
	if len(messages) == 0 {
		return ""
	}

	// 格式化消息为字符串（从最新到最旧，超阈值截断）
	formatted := c.handler.FormatMsgsToStr(messages, c.threshold, c.addThinkingBlock)
	if formatted == "" {
		return ""
	}

	// 构建 prompt
	var userMessage string
	if previousSummary != "" {
		userMessage = "# conversation\n" + formatted + "\n\n# previous-summary\n" + previousSummary + "\n\n" + c.updateUserMessage()
	} else {
		userMessage = "# conversation\n" + formatted + "\n\n" + c.initialUserMessage()
	}

	if c.extraInstruction != "" {
		userMessage += "\n\n# extra-instruction\n" + c.extraInstruction
	}

	msgs := []schema.Message{
		{Role: "system", Content: c.systemPrompt()},
		{Role: "user", Content: userMessage},
	}

	resp, err := llmClient.Generate(ctx, msgs, nil)
	if err != nil {
		return ""
	}

	summary := resp.Content
	if !isValidSummary(summary) {
		return ""
	}

	return summary
}

// 六个必需的顶级字段（## 开头），中英文均支持。
var requiredFields = []string{
	"## Goal", "## 目标",
	"## Constraints", "## 约束",
	"## Progress", "## 进展",
	"## Key Decisions", "## 关键决策",
	"## Next Steps", "## 下一步",
	"## Critical Context", "## 关键上下文",
}

// minRequiredFields 最少需要出现的字段数（允许部分字段合并为 "(none)"）。
const minRequiredFields = 4

// isValidSummary 校验摘要。至少 minRequiredFields 个顶级字段出现，
// 且非空内容不少于 50 字符。
func isValidSummary(content string) bool {
	trimmed := strings.TrimSpace(content)
	if len(trimmed) < 50 {
		return false
	}

	matched := 0
	for _, field := range requiredFields {
		if strings.Contains(trimmed, field) {
			matched++
		}
	}
	return matched >= minRequiredFields
}

// Prompt templates — 翻译自 compactor.yaml。

func (c *Compactor) systemPrompt() string {
	if c.language == "zh" {
		return "你是一个上下文压缩助手。你的角色是创建对话的结构化摘要，" +
			"这些摘要可以在未来会话中用于恢复上下文。专注于保留关键信息，同时减少 token 数量。"
	}
	return "You are a context compaction assistant. Your role is to create structured summaries of conversations " +
		"that can be used to restore context in future sessions. Focus on preserving critical information while reducing token count."
}

func (c *Compactor) initialUserMessage() string {
	if c.language == "zh" {
		return `# 任务
根据上面的对话创建一个结构化摘要。

# 规则：
- 保持每个部分简洁
- 保留确切的文件路径、函数名称和错误消息

# 输出格式：

## 目标
[用户试图完成什么？如果会话涵盖不同任务，可以有多个项目。]

## 约束和偏好
- [任何用户提到的约束、偏好或要求]
- [或者如果没有提到则为"(none)"]

## 进展
### 已完成
- [x] [已完成的任务/更改]

### 进行中
- [ ] [当前工作]

### 阻塞
- [如果有任何阻碍进展的问题]

## 关键决策
- **[决策]**: [简短理由]

## 下一步
1. [接下来应该发生的事情的有序列表]

## 关键上下文
- [任何继续工作所需的数据、示例或参考资料]
- [或者如果不适用则为"(none)"]

请按照上面格式，输出结构化摘要。`
	}
	return `# Task
Create a structured summary from the conversation above.

# Rules:
- Keep each section concise
- Preserve exact file paths, function names, and error messages

# Output Format:

## Goal
[What is the user trying to accomplish? Can be multiple items if the session covers different tasks.]

## Constraints & Preferences
- [Any constraints, preferences, or requirements mentioned by user]
- [Or "(none)" if none were mentioned]

## Progress
### Done
- [x] [Completed tasks/changes]

### In Progress
- [ ] [Current work]

### Blocked
- [Issues preventing progress, if any]

## Key Decisions
- **[Decision]**: [Brief rationale]

## Next Steps
1. [Ordered list of what should happen next]

## Critical Context
- [Any data, examples, or references needed to continue]
- [Or "(none)" if not applicable]

Output the structured summary following the format above.`
}

func (c *Compactor) updateUserMessage() string {
	if c.language == "zh" {
		return `# 任务
使用新的对话内容来更新结构化摘要。

# 规则：
- 保留来自先前摘要的所有现有信息
- 从新消息中添加新的进展、决策和上下文
- 更新进度部分：当完成时将项目从"进行中"移到"已完成"
- 根据已完成的内容更新"下一步"
- 保留确切的文件路径、函数名称和错误消息
- 如果某些内容不再相关，您可以删除它

# 输出格式：

## 目标
[保留现有目标，如果任务扩展则添加新目标]

## 约束和偏好
- [保留现有内容，添加发现的新内容]

## 进展
### 已完成
- [x] [包含以前完成的项目和新完成的项目]

### 进行中
- [ ] [当前工作 - 根据进展更新]

### 阻塞
- [当前阻塞问题 - 如果解决则删除]

## 关键决策
- **[决策]**: [简短理由]（保留所有之前的内容，添加新的）

## 下一步
1. [根据当前状态更新]

## 关键上下文
- [保留重要上下文，如需要则添加新的]

请按照上面格式，输出结构化摘要。`
	}
	return `# Task
Update the structured summary with new conversation messages.

# Rules:
- PRESERVE all existing information from the previous summary
- ADD new progress, decisions, and context from the new messages
- UPDATE the Progress section: move items from "In Progress" to "Done" when completed
- UPDATE "Next Steps" based on what was accomplished
- PRESERVE exact file paths, function names, and error messages
- If something is no longer relevant, you may remove it

# Output Format:

## Goal
[Preserve existing goals, add new ones if the task expanded]

## Constraints & Preferences
- [Preserve existing, add new ones discovered]

## Progress
### Done
- [x] [Include previously done items AND newly completed items]

### In Progress
- [ ] [Current work - update based on progress]

### Blocked
- [Current blockers - remove if resolved]

## Key Decisions
- **[Decision]**: [Brief rationale] (preserve all previous, add new)

## Next Steps
1. [Update based on current state]

## Critical Context
- [Preserve important context, add new if needed]

Output the structured summary following the format above.`
}
