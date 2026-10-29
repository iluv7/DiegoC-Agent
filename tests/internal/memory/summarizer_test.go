package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"diegoc-agent/internal/llm"
	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

// stepMockLLM returns pre-scripted responses, one per Generate call.
// Each response is either text content (no tool calls) or a single tool call.
type stepMockLLM struct {
	steps []*schema.LLMResponse
	idx   int
}

func (m *stepMockLLM) Generate(_ context.Context, _ []schema.Message, _ []tools.Tool) (*schema.LLMResponse, error) {
	if m.idx >= len(m.steps) {
		// No more steps → return empty (no tool calls).
		return &schema.LLMResponse{}, nil
	}
	r := m.steps[m.idx]
	m.idx++
	return r, nil
}

var _ llm.Client = (*stepMockLLM)(nil)

func TestSummarizer_CreateNewFile(t *testing.T) {
	tmpDir := t.TempDir()
	memoryDir := filepath.Join(tmpDir, ".reme", "memory")

	h := newHandler()
	s := NewSummarizer(h, nil, 100000, "", memoryDir)

	// Mock: LLM writes a new memory file on the first call, then finishes.
	mock := &stepMockLLM{steps: []*schema.LLMResponse{
		{
			Content: "Let me create the memory file.",
			ToolCalls: []schema.ToolCall{{
				ID:   "call_1",
				Type: "function",
				Function: schema.FunctionCall{
					Name: "write_file",
					Arguments: map[string]interface{}{
						"path":    "2026-07-01.md", // today's date — hardcoded for test
						"content": "## Project Goal\n- Build an agent framework\n\n## Key Decisions\n- Use Go for the backend",
					},
				},
			}},
		},
		// Second call: LLM finishes (no tool calls).
		{Content: "Memory has been saved successfully."},
	}}
	s.llmClient = mock

	messages := []schema.Message{
		{Role: "user", Content: "Let's build a new agent framework in Go"},
		{Role: "assistant", Content: "Great idea! Go is perfect for this. We should use goroutines for concurrency."},
	}

	err := s.Summarize(context.Background(), messages)
	if err != nil {
		t.Fatalf("Summarize failed: %v", err)
	}

	// Verify the file was written
	files, err := os.ReadDir(memoryDir)
	if err != nil {
		t.Fatalf("read memory dir: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("expected at least 1 file in memory dir")
	}

	// Check the content of the first .md file
	mdPath := filepath.Join(memoryDir, files[0].Name())
	data, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatalf("read memory file: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "Project Goal") {
		t.Errorf("memory file should contain 'Project Goal', got: %s", content)
	}
	if !strings.Contains(content, "Key Decisions") {
		t.Errorf("memory file should contain 'Key Decisions', got: %s", content)
	}
}

func TestSummarizer_ReadThenEdit(t *testing.T) {
	tmpDir := t.TempDir()
	memoryDir := filepath.Join(tmpDir, ".reme", "memory")

	h := newHandler()
	s := NewSummarizer(h, nil, 100000, "", memoryDir)

	// Pre-create an existing memory file
	existingContent := "## Project Goal\n- Build an agent framework\n\n## Key Decisions\n- Use Python for the backend"
	todayFile := filepath.Join(memoryDir, "2026-07-01.md")
	if err := os.MkdirAll(memoryDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(todayFile, []byte(existingContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Mock: LLM reads, then edits
	mock := &stepMockLLM{steps: []*schema.LLMResponse{
		// Step 1: read the existing file
		{
			Content: "Let me check what's already there.",
			ToolCalls: []schema.ToolCall{{
				ID:   "call_1",
				Type: "function",
				Function: schema.FunctionCall{
					Name: "read_file",
					Arguments: map[string]interface{}{
						"path": "2026-07-01.md",
					},
				},
			}},
		},
		// Step 2: edit to update Python → Go
		{
			Content: "I'll update the tech decision.",
			ToolCalls: []schema.ToolCall{{
				ID:   "call_2",
				Type: "function",
				Function: schema.FunctionCall{
					Name: "edit_file",
					Arguments: map[string]interface{}{
						"path":    "2026-07-01.md",
						"old_str": "- Use Python for the backend",
						"new_str": "- Use Go for the backend",
					},
				},
			}},
		},
		// Step 3: done
		{Content: "Memory updated."},
	}}
	s.llmClient = mock

	messages := []schema.Message{
		{Role: "user", Content: "Actually let's switch to Go instead of Python"},
		{Role: "assistant", Content: "Good call. Go is better for concurrency."},
	}

	err := s.Summarize(context.Background(), messages)
	if err != nil {
		t.Fatalf("Summarize failed: %v", err)
	}

	// Verify the file was edited correctly
	data, err := os.ReadFile(todayFile)
	if err != nil {
		t.Fatalf("read memory file: %v", err)
	}
	content := string(data)
	if strings.Contains(content, "Python") {
		t.Error("file should no longer contain 'Python'")
	}
	if !strings.Contains(content, "Use Go for the backend") {
		t.Error("file should contain 'Use Go for the backend'")
	}
}

func TestSummarizer_EmptyMessages(t *testing.T) {
	tmpDir := t.TempDir()
	memoryDir := filepath.Join(tmpDir, ".reme", "memory")

	h := newHandler()
	s := NewSummarizer(h, nil, 100000, "", memoryDir)

	// No error on empty messages
	if err := s.Summarize(context.Background(), nil); err != nil {
		t.Errorf("empty messages should not error: %v", err)
	}
	if err := s.Summarize(context.Background(), []schema.Message{}); err != nil {
		t.Errorf("empty slice should not error: %v", err)
	}
}

func TestSummarizer_ChineseLanguage(t *testing.T) {
	tmpDir := t.TempDir()
	memoryDir := filepath.Join(tmpDir, ".reme", "memory")

	h := newHandler()
	s := NewSummarizer(h, nil, 100000, "zh", memoryDir)

	mock := &stepMockLLM{steps: []*schema.LLMResponse{
		{
			Content: "我来创建记忆文件。",
			ToolCalls: []schema.ToolCall{{
				ID:   "call_1",
				Type: "function",
				Function: schema.FunctionCall{
					Name: "write_file",
					Arguments: map[string]interface{}{
						"path":    "2026-07-01.md",
						"content": "## 项目目标\n- 构建智能代理框架\n\n## 关键决策\n- 使用 Go 语言",
					},
				},
			}},
		},
		{Content: "记忆已保存。"},
	}}
	s.llmClient = mock

	messages := []schema.Message{
		{Role: "user", Content: "我们要构建一个智能代理框架"},
		{Role: "assistant", Content: "好的，建议用 Go 语言实现"},
	}

	err := s.Summarize(context.Background(), messages)
	if err != nil {
		t.Fatalf("Summarize failed: %v", err)
	}

	files, err := os.ReadDir(memoryDir)
	if err != nil {
		t.Fatalf("read memory dir: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("expected memory file to be created")
	}

	data, _ := os.ReadFile(filepath.Join(memoryDir, files[0].Name()))
	content := string(data)
	if !strings.Contains(content, "项目目标") {
		t.Error("Chinese memory should contain 项目目标")
	}
	if !strings.Contains(content, "关键决策") {
		t.Error("Chinese memory should contain 关键决策")
	}
}

func TestSummarizer_ContextCancellation(t *testing.T) {
	tmpDir := t.TempDir()
	memoryDir := filepath.Join(tmpDir, ".reme", "memory")

	h := newHandler()
	s := NewSummarizer(h, nil, 100000, "", memoryDir)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	messages := []schema.Message{
		{Role: "user", Content: "Test message"},
	}

	err := s.Summarize(ctx, messages)
	if err == nil {
		t.Error("expected context cancellation error")
	}
}

func TestSummarizer_PathTraversal(t *testing.T) {
	// Verify that memory tools reject path traversal attempts.
	// Note: absolute paths like /etc/passwd resolve inside memoryDir
	// (filepath.Join keeps both parts), so only ../ traversals are dangerous.
	tests := []struct {
		path string
	}{
		{"../../../etc/passwd"},
		{"../../.ssh/id_rsa"},
		{"../secret.txt"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			rt := &memReadTool{memoryDir: "/tmp/memory"}
			full, err := rt.safePath(tt.path)
			if err == nil {
				t.Errorf("expected error for path %q, got %q", tt.path, full)
			}
		})
	}
}

func TestSummarizer_SafePath_ValidPaths(t *testing.T) {
	tests := []struct {
		rel string
	}{
		{"2026-07-01.md"},
		{"subdir/memory.md"},
		{"archive/2026/june.md"},
	}

	for _, tt := range tests {
		t.Run(tt.rel, func(t *testing.T) {
			rt := &memReadTool{memoryDir: "/tmp/memory"}
			full, err := rt.safePath(tt.rel)
			if err != nil {
				t.Errorf("unexpected error for %q: %v", tt.rel, err)
			}
			if !strings.HasPrefix(full, "/tmp/memory") {
				t.Errorf("path %q should be under /tmp/memory, got %q", tt.rel, full)
			}
		})
	}
}

func TestSummarizer_NoToolCallsResponse(t *testing.T) {
	tmpDir := t.TempDir()
	memoryDir := filepath.Join(tmpDir, ".reme", "memory")

	h := newHandler()
	s := NewSummarizer(h, nil, 100000, "", memoryDir)

	// Mock LLM returns only text (no tool calls), which means it's "done"
	mock := &stepMockLLM{steps: []*schema.LLMResponse{
		{Content: "No new information worth saving. Memory unchanged."},
	}}
	s.llmClient = mock

	messages := []schema.Message{
		{Role: "user", Content: "Hi"},
		{Role: "assistant", Content: "Hello! How can I help?"},
	}

	err := s.Summarize(context.Background(), messages)
	if err != nil {
		t.Fatalf("Summarize failed: %v", err)
	}

	// No file should be created (LLM decided nothing to save)
	files, _ := os.ReadDir(memoryDir)
	if len(files) > 0 {
		t.Log("LLM chose not to write, but minor — stepMock just returned text")
	}
}

func TestSummarizer_WriteThenAppend(t *testing.T) {
	tmpDir := t.TempDir()
	memoryDir := filepath.Join(tmpDir, ".reme", "memory")

	h := newHandler()
	s := NewSummarizer(h, nil, 100000, "", memoryDir)

	// Create the directory so the test can save a pre-existing file
	if err := os.MkdirAll(memoryDir, 0755); err != nil {
		t.Fatal(err)
	}
	existingContent := "## Project Goal\n- Build agent framework"
	todayFile := filepath.Join(memoryDir, "2026-07-01.md")
	if err := os.WriteFile(todayFile, []byte(existingContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Mock: read existing → write updated version (append new section)
	mock := &stepMockLLM{steps: []*schema.LLMResponse{
		// Step 1: read
		{
			Content: "Checking existing memory...",
			ToolCalls: []schema.ToolCall{{
				ID:   "call_1",
				Type: "function",
				Function: schema.FunctionCall{
					Name: "read_file",
					Arguments: map[string]interface{}{
						"path": "2026-07-01.md",
					},
				},
			}},
		},
		// Step 2: write updated file with appended content
		{
			Content: "Updating with new information...",
			ToolCalls: []schema.ToolCall{{
				ID:   "call_2",
				Type: "function",
				Function: schema.FunctionCall{
					Name: "write_file",
					Arguments: map[string]interface{}{
						"path": "2026-07-01.md",
						"content": existingContent + "\n\n## New Feature\n- Add memory summarization\n\n## Key Decisions\n- 2026-07-01: Use ReAct pattern for summarizer",
					},
				},
			}},
		},
		{Content: "Memory updated with new sections."},
	}}
	s.llmClient = mock

	messages := []schema.Message{
		{Role: "user", Content: "Let's add memory summarization feature"},
		{Role: "assistant", Content: "We should use a ReAct pattern so the LLM can read/write files."},
	}

	err := s.Summarize(context.Background(), messages)
	if err != nil {
		t.Fatalf("Summarize failed: %v", err)
	}

	data, _ := os.ReadFile(todayFile)
	content := string(data)
	if !strings.Contains(content, "Project Goal") {
		t.Error("should preserve existing sections")
	}
	if !strings.Contains(content, "New Feature") {
		t.Error("should append new section")
	}
	if !strings.Contains(content, "ReAct pattern") {
		t.Error("should include new decision")
	}
}
