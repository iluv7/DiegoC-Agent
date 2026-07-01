package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"diegoc-agent/internal/llm"
	"diegoc-agent/internal/permission"
	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

// Summarizer produces detailed long-term memory summaries and persists them
// to memory/YYYY-MM-DD.md files. Unlike Compactor whose output goes back into
// the context window, Summarizer writes to the filesystem for future retrieval.
//
// It uses a mini ReAct loop, giving the LLM read/write/edit tools scoped to
// the memory directory so it can autonomously decide whether to create new
// memory files, append to existing ones, or edit previous entries.
type Summarizer struct {
	handler   *MsgHandler
	llmClient llm.Client
	threshold int    // max tokens fed to LLM
	language  string // "zh" or "" (en)
	memoryDir string // e.g. ".reme/memory"
	maxSteps  int    // max ReAct steps
}

// NewSummarizer creates a long-term memory summarizer.
// memoryDir is the directory where memory/YYYY-MM-DD.md files live.
func NewSummarizer(handler *MsgHandler, llmClient llm.Client, threshold int, language string, memoryDir string) *Summarizer {
	if threshold <= 0 {
		threshold = 50000
	}
	return &Summarizer{
		handler:   handler,
		llmClient: llmClient,
		threshold: threshold,
		language:  language,
		memoryDir: memoryDir,
		maxSteps:  10,
	}
}

// Summarize formats the messages, then runs a mini ReAct loop so the LLM can
// read existing memory files, write new ones, or edit old entries.
// Returns an error only on I/O or LLM failures — tool execution errors are
// reported to the LLM so it can self-correct.
func (s *Summarizer) Summarize(ctx context.Context, messages []schema.Message) error {
	if len(messages) == 0 {
		return nil
	}

	// Format messages for the LLM (without thinking blocks — summaries don't need them).
	formatted := s.handler.FormatMsgsToStr(messages, s.threshold, false)
	if formatted == "" {
		return nil
	}

	// Ensure memory directory exists.
	if err := os.MkdirAll(s.memoryDir, 0755); err != nil {
		return fmt.Errorf("summarizer: create memory dir: %w", err)
	}

	today := time.Now().Format("2006-01-02")
	todayFile := filepath.Join(s.memoryDir, today+".md")

	// Build memory-scoped tool set.
	memTools := []tools.Tool{
		&memReadTool{memoryDir: s.memoryDir},
		&memWriteTool{memoryDir: s.memoryDir},
		&memEditTool{memoryDir: s.memoryDir},
	}
	toolByName := make(map[string]tools.Tool)
	for _, t := range memTools {
		toolByName[t.Name()] = t
	}

	// Start the mini ReAct conversation.
	chatMsgs := []schema.Message{
		{Role: "system", Content: s.systemPrompt(today)},
		{Role: "user", Content: s.userPrompt(formatted, todayFile)},
	}

	for step := 0; step < s.maxSteps; step++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		resp, err := s.llmClient.Generate(ctx, chatMsgs, memTools)
		if err != nil {
			return fmt.Errorf("summarizer: llm generate: %w", err)
		}

		chatMsgs = append(chatMsgs, schema.Message{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})

		// No tool calls → LLM is done.
		if len(resp.ToolCalls) == 0 {
			return nil
		}

		// Execute each tool call and feed results back.
		for _, tc := range resp.ToolCalls {
			args := normalizeArgs(tc.Function.Arguments)
			if args == nil {
				args = make(map[string]interface{})
			}

			tool := toolByName[tc.Function.Name]
			var result *tools.ToolResult
			if tool == nil {
				result = &tools.ToolResult{Success: false, Error: "Unknown tool: " + tc.Function.Name}
			} else {
				result, _ = tool.Execute(ctx, args)
			}

			content := result.Content
			if !result.Success {
				content = "Error: " + result.Error
			}

			chatMsgs = append(chatMsgs, schema.Message{
				Role:       "tool",
				Content:    content,
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
			})
		}
	}

	// Hit max steps — not an error, just a safety limit.
	return nil
}

// ---- prompt templates ----

func (s *Summarizer) systemPrompt(today string) string {
	if s.language == "zh" {
		return fmt.Sprintf(
			"你是一个长期记忆管理助手。你的任务是将对话中的重要信息整理并写入记忆文件，以便未来检索。\n\n"+
				"## 当前日期\n%s\n\n"+
				"## 工作方式\n"+
				"1. 先使用 read_file 读取今日的记忆文件（memory/%s.md），了解已有内容\n"+
				"2. 如果文件不存在，使用 write_file 创建它\n"+
				"3. 如果文件已存在，分析新对话与已有记忆的关系：\n"+
				"   - 如果涉及已有主题的更新 → 使用 edit_file 精准替换对应段落\n"+
				"   - 如果涉及全新主题 → 在文件末尾追加新章节\n"+
				"   - 如果信息不再准确 → 删除或更新对应段落\n\n"+
				"## 记忆文件格式\n"+
				"每个主题使用 ## 二级标题，内容用要点（-）组织，保持简洁。示例：\n\n"+
				"## 项目目标\n"+
				"- 构建 DiegoC Agent 框架，支持多模型、多工具\n"+
				"- 目标部署环境：macOS / Linux 服务器\n\n"+
				"## 关键决策\n"+
				"- 2026-07-01: 选择 ChromaDB 作为向量存储后端\n"+
				"- 2026-07-01: 确定使用 memory/YYYY-MM-DD.md 按天拆分\n\n"+
				"## 注意事项\n"+
				"- 保留确切的文件路径、函数名称、配置项\n"+
				"- 每条重要信息标上日期\n"+
				"- 不要重复已存在的内容\n"+
				"- 输出只包含记忆文件内容本身，不要加额外解释",
			today, today)
	}
	return fmt.Sprintf(
		"You are a long-term memory management assistant. Your task is to organize "+
			"important information from conversations into memory files for future retrieval.\n\n"+
			"## Current Date\n%s\n\n"+
			"## How to Work\n"+
			"1. First use read_file to check today's memory file (memory/%s.md)\n"+
			"2. If the file doesn't exist, create it with write_file\n"+
			"3. If the file exists, analyze how new information relates to existing memory:\n"+
			"   - Updates to existing topics → use edit_file for precise in-place replacement\n"+
			"   - Entirely new topics → append a new section at the end of the file\n"+
			"   - Information no longer accurate → delete or update the relevant section\n\n"+
			"## Memory File Format\n"+
			"Use ## level-2 headings for each topic, bullet points (-) for details. Keep it concise. Example:\n\n"+
			"## Project Goals\n"+
			"- Build DiegoC Agent framework supporting multiple models and tools\n"+
			"- Target deployment: macOS / Linux servers\n\n"+
			"## Key Decisions\n"+
			"- 2026-07-01: Chose ChromaDB as vector storage backend\n"+
			"- 2026-07-01: Decided on memory/YYYY-MM-DD.md daily split\n\n"+
			"## Important Notes\n"+
			"- Preserve exact file paths, function names, and configuration values\n"+
			"- Date each important piece of information\n"+
			"- Don't duplicate content that already exists\n"+
			"- Output only the memory file content itself — no extra commentary",
		today, today)
}

func (s *Summarizer) userPrompt(formatted string, todayFile string) string {
	if s.language == "zh" {
		return fmt.Sprintf(
			"# 对话内容\n\n%s\n\n"+
				"# 任务\n"+
				"请将以上对话中的重要信息整理到长期记忆中。\n\n"+
				"目标文件: %s\n\n"+
				"先读取该文件了解已有内容（如存在），然后决定是创建、追加还是编辑。",
			formatted, todayFile)
	}
	return fmt.Sprintf(
		"# Conversation\n\n%s\n\n"+
			"# Task\n"+
			"Please organize the important information from the conversation above into long-term memory.\n\n"+
			"Target file: %s\n\n"+
			"Read the file first to understand existing content (if it exists), then decide whether to create, append, or edit.",
		formatted, todayFile)
}

// ---- arg normalisation (handles Anthropic string-encoded JSON args) ----

func normalizeArgs(raw map[string]interface{}) map[string]interface{} {
	if raw == nil {
		return nil
	}
	if s, ok := raw["arguments"].(string); ok {
		var m map[string]interface{}
		if json.Unmarshal([]byte(s), &m) == nil {
			return m
		}
	}
	return raw
}

// ---- memory-scoped file tools ----
// Each tool restricts all paths to be inside the Summarizer's memoryDir.

// memReadTool reads a file within the memory directory.
type memReadTool struct{ memoryDir string }

func (t *memReadTool) Name() string { return "read_file" }
func (t *memReadTool) Description() string {
	return "Read a file within the memory directory. Returns content with line numbers."
}
func (t *memReadTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "Path to the file, relative to the memory directory",
			},
		},
		"required": []interface{}{"path"},
	}
}

func (t *memReadTool) Execute(_ context.Context, args map[string]interface{}) (*tools.ToolResult, error) {
	path, _ := args["path"].(string)
	if path == "" {
		return &tools.ToolResult{Success: false, Error: "path is required"}, nil
	}
	fullPath, err := t.safePath(path)
	if err != nil {
		return &tools.ToolResult{Success: false, Error: err.Error()}, nil
	}
	data, err := os.ReadFile(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &tools.ToolResult{Success: false, Error: "File not found: " + path}, nil
		}
		return &tools.ToolResult{Success: false, Error: err.Error()}, nil
	}
	return &tools.ToolResult{Success: true, Content: string(data)}, nil
}

func (t *memReadTool) CheckPermissions(_ map[string]interface{}, _ *permission.Context) permission.Decision {
	return permission.Decision{Behavior: permission.BehaviorALLOW}
}
func (t *memReadTool) IsConcurrencySafe() bool { return true }
func (t *memReadTool) IsReadOnly() bool        { return true }
func (t *memReadTool) IsExternalTool() bool    { return false }

// memWriteTool writes content to a file within the memory directory.
type memWriteTool struct{ memoryDir string }

func (t *memWriteTool) Name() string { return "write_file" }
func (t *memWriteTool) Description() string {
	return "Write content to a file within the memory directory. Overwrites existing files completely."
}
func (t *memWriteTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "Path to the file, relative to the memory directory",
			},
			"content": map[string]interface{}{
				"type":        "string",
				"description": "Complete content to write (replaces existing content)",
			},
		},
		"required": []interface{}{"path", "content"},
	}
}

func (t *memWriteTool) Execute(_ context.Context, args map[string]interface{}) (*tools.ToolResult, error) {
	path, _ := args["path"].(string)
	content, _ := args["content"].(string)
	if path == "" {
		return &tools.ToolResult{Success: false, Error: "path is required"}, nil
	}
	fullPath, err := t.safePath(path)
	if err != nil {
		return &tools.ToolResult{Success: false, Error: err.Error()}, nil
	}
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		return &tools.ToolResult{Success: false, Error: err.Error()}, nil
	}
	if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
		return &tools.ToolResult{Success: false, Error: err.Error()}, nil
	}
	return &tools.ToolResult{Success: true, Content: "Successfully wrote to " + path}, nil
}

func (t *memWriteTool) CheckPermissions(_ map[string]interface{}, _ *permission.Context) permission.Decision {
	return permission.Decision{Behavior: permission.BehaviorALLOW}
}
func (t *memWriteTool) IsConcurrencySafe() bool { return false }
func (t *memWriteTool) IsReadOnly() bool        { return false }
func (t *memWriteTool) IsExternalTool() bool    { return false }

// memEditTool edits a file within the memory directory by exact string replacement.
type memEditTool struct{ memoryDir string }

func (t *memEditTool) Name() string { return "edit_file" }
func (t *memEditTool) Description() string {
	return "Edit a file within the memory directory by exact string replacement. " +
		"The old_str must appear exactly once in the file."
}
func (t *memEditTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "Path to the file, relative to the memory directory",
			},
			"old_str": map[string]interface{}{
				"type":        "string",
				"description": "Exact string to find and replace (must be unique in file)",
			},
			"new_str": map[string]interface{}{
				"type":        "string",
				"description": "Replacement string",
			},
		},
		"required": []interface{}{"path", "old_str", "new_str"},
	}
}

func (t *memEditTool) Execute(_ context.Context, args map[string]interface{}) (*tools.ToolResult, error) {
	path, _ := args["path"].(string)
	oldStr, _ := args["old_str"].(string)
	newStr, _ := args["new_str"].(string)
	if path == "" || oldStr == "" {
		return &tools.ToolResult{Success: false, Error: "path and old_str are required"}, nil
	}
	fullPath, err := t.safePath(path)
	if err != nil {
		return &tools.ToolResult{Success: false, Error: err.Error()}, nil
	}
	data, err := os.ReadFile(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &tools.ToolResult{Success: false, Error: "File not found: " + path}, nil
		}
		return &tools.ToolResult{Success: false, Error: err.Error()}, nil
	}
	content := string(data)
	if !strings.Contains(content, oldStr) {
		return &tools.ToolResult{Success: false, Error: "Text not found in file. Make sure old_str matches exactly including whitespace."}, nil
	}
	if strings.Count(content, oldStr) > 1 {
		return &tools.ToolResult{Success: false, Error: "Text appears multiple times. Provide a larger string with more surrounding context to make it unique."}, nil
	}
	newContent := strings.Replace(content, oldStr, newStr, 1)
	if err := os.WriteFile(fullPath, []byte(newContent), 0644); err != nil {
		return &tools.ToolResult{Success: false, Error: err.Error()}, nil
	}
	return &tools.ToolResult{Success: true, Content: "Successfully edited " + path}, nil
}

func (t *memEditTool) CheckPermissions(_ map[string]interface{}, _ *permission.Context) permission.Decision {
	return permission.Decision{Behavior: permission.BehaviorALLOW}
}
func (t *memEditTool) IsConcurrencySafe() bool { return false }
func (t *memEditTool) IsReadOnly() bool        { return false }
func (t *memEditTool) IsExternalTool() bool    { return false }

// ---- path safety ----

// safePath resolves a memory-relative path and ensures it stays inside the
// memory directory (prevents path traversal).
func (t *memReadTool) safePath(rel string) (string, error) {
	return safeJoin(t.memoryDir, rel)
}
func (t *memWriteTool) safePath(rel string) (string, error) {
	return safeJoin(t.memoryDir, rel)
}
func (t *memEditTool) safePath(rel string) (string, error) {
	return safeJoin(t.memoryDir, rel)
}

func safeJoin(base, rel string) (string, error) {
	absBase, err := filepath.Abs(base)
	if err != nil {
		return "", fmt.Errorf("resolve base dir: %w", err)
	}
	joined := filepath.Join(absBase, rel)
	cleaned := filepath.Clean(joined)
	if !strings.HasPrefix(cleaned, absBase+string(filepath.Separator)) && cleaned != absBase {
		return "", fmt.Errorf("path escapes memory directory: %s", rel)
	}
	return cleaned, nil
}
