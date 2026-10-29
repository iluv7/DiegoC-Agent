package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"diegoc-agent/internal/schema"
)

func TestToolResultCompactor_SmallOutput(t *testing.T) {
	dir := t.TempDir()
	c := NewToolResultCompactor(dir, 3, DefaultOldMaxBytes, DefaultRecentMaxBytes, 0)

	msgs := []schema.Message{
		{Role: "user", Content: "hi"},
		{Role: "tool", Content: "small result", ToolCallID: "call_1", Name: "bash"},
	}
	result := c.Compact(msgs)
	if result[1].Content != "small result" {
		t.Errorf("小输出不应截断, 得到 %v", result[1].Content)
	}
}

func TestToolResultCompactor_LargeOutput(t *testing.T) {
	dir := t.TempDir()
	// 旧阈值 50, 近期阈值 100000, recentN=0
	// 加一条非 tool 消息在尾部，让 tool 消息落在旧窗口
	c := NewToolResultCompactor(dir, 3, 50, 100000, 0)

	largeContent := strings.Repeat("a", 5000)
	msgs := []schema.Message{
		{Role: "tool", Content: largeContent, ToolCallID: "call_1", Name: "bash"},
		{Role: "user", Content: "done"},
	}

	result := c.Compact(msgs)
	content, _ := result[0].Content.(string)

	if !strings.Contains(content, TruncationNoticeMarker) {
		t.Error("截断输出应包含 TRUNCATED 标记")
	}
	if len(content) > len(largeContent) {
		t.Errorf("截断后不应比原始长, 原始 %d, 结果 %d", len(largeContent), len(content))
	}
	files, _ := os.ReadDir(dir)
	txtCount := 0
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".txt") {
			txtCount++
		}
	}
	if txtCount != 1 {
		t.Errorf("应生成 1 个 txt 文件, 得到 %d", txtCount)
	}
}

func TestToolResultCompactor_RecentProtection(t *testing.T) {
	dir := t.TempDir()
	// 旧阈值 50, 近期阈值 500, recentN=2
	c := NewToolResultCompactor(dir, 3, 50, 500, 2)

	largeContent := strings.Repeat("b", 200) // 超 50 但不超 500

	msgs := []schema.Message{
		{Role: "tool", Content: strings.Repeat("old", 100), ToolCallID: "call_1", Name: "bash"}, // 旧, 超 50
		{Role: "user", Content: "hi"},
		{Role: "tool", Content: largeContent, ToolCallID: "call_2", Name: "bash"}, // 近期, 不超 500
		{Role: "tool", Content: largeContent, ToolCallID: "call_3", Name: "bash"}, // 近期
	}

	result := c.Compact(msgs)

	// 下标 0: 旧消息，应被严格截断
	c0, _ := result[0].Content.(string)
	if !strings.Contains(c0, TruncationNoticeMarker) {
		t.Error("旧消息应被截断")
	}
	// 下标 2,3: 近期消息，不应被截断
	c2, _ := result[2].Content.(string)
	if strings.Contains(c2, TruncationNoticeMarker) {
		t.Error("近期消息不应被截断")
	}
	c3, _ := result[3].Content.(string)
	if strings.Contains(c3, TruncationNoticeMarker) {
		t.Error("近期消息不应被截断")
	}
}

func TestToolResultCompactor_MdFileProtection(t *testing.T) {
	dir := t.TempDir()
	// 旧阈值 50, 近期阈值 10000 (足够大不触发截断), recentN=0
	// 但 .md 保护会将其提升到 recentMaxBytes
	c := NewToolResultCompactor(dir, 3, 50, 2000, 0)

	// 超 50 但不超 2000
	mdContent := strings.Repeat("md ", 300) // ~900 字节

	msgs := []schema.Message{
		{Role: "assistant", Content: "reading", ToolCalls: []schema.ToolCall{
			{ID: "call_md", Type: "function", Function: schema.FunctionCall{
				Name:      "read_file",
				Arguments: map[string]interface{}{"file_path": "MEMORY.md"},
			}},
		}},
		{Role: "tool", Content: mdContent, ToolCallID: "call_md", Name: "read_file"},
	}

	result := c.Compact(msgs)
	c1, _ := result[1].Content.(string)

	if strings.Contains(c1, TruncationNoticeMarker) {
		t.Error(".md 文件结果不应被 50 的旧阈值截断")
	}
}

func TestToolResultCompactor_Retruncate(t *testing.T) {
	dir := t.TempDir()
	c := NewToolResultCompactor(dir, 3, DefaultOldMaxBytes, DefaultRecentMaxBytes, 0)

	// 模拟已截断过，让它超过阈值
	padding := strings.Repeat("x", DefaultOldMaxBytes+100)
	alreadyTruncated := padding + "\n" + TruncationNoticeMarker + "\nsaved to tool_results/abc.txt"

	msgs := []schema.Message{
		{Role: "tool", Content: alreadyTruncated, ToolCallID: "call_1", Name: "bash"},
	}

	result := c.Compact(msgs)
	content, _ := result[0].Content.(string)

	if !strings.Contains(content, TruncationNoticeMarker) {
		t.Error("retruncate 应保留 TRUNCATED 标记")
	}
	files, _ := os.ReadDir(dir)
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".txt") {
			t.Error("retruncate 不应生成新 txt 文件")
		}
	}
}

func TestToolResultCompactor_NonToolMessages(t *testing.T) {
	dir := t.TempDir()
	c := NewToolResultCompactor(dir, 3, DefaultOldMaxBytes, DefaultRecentMaxBytes, 0)

	largeContent := strings.Repeat("x", DefaultOldMaxBytes+500)
	msgs := []schema.Message{
		{Role: "user", Content: largeContent},
		{Role: "assistant", Content: largeContent},
	}

	result := c.Compact(msgs)
	if result[0].Content != largeContent {
		t.Error("user 消息不应被截断")
	}
	if result[1].Content != largeContent {
		t.Error("assistant 消息不应被截断")
	}
}

func TestToolResultCompactor_CleanupExpired(t *testing.T) {
	dir := t.TempDir()

	// 创建 5 天前的旧文件
	oldFile := filepath.Join(dir, "old.txt")
	os.WriteFile(oldFile, []byte("old"), 0644)
	oldTime := time.Now().AddDate(0, 0, -5)
	os.Chtimes(oldFile, oldTime, oldTime)

	// 创建今天的新文件
	newFile := filepath.Join(dir, "new.txt")
	os.WriteFile(newFile, []byte("new"), 0644)

	c := NewToolResultCompactor(dir, 3, DefaultOldMaxBytes, DefaultRecentMaxBytes, 1)
	deleted := c.CleanupExpiredFiles()

	if deleted != 1 {
		t.Errorf("应删除 1 个过期文件, 得到 %d", deleted)
	}
	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Error("过期文件应被删除")
	}
	if _, err := os.Stat(newFile); os.IsNotExist(err) {
		t.Error("新文件不应被删除")
	}
}

func TestTruncateOutput(t *testing.T) {
	result := truncateOutput("line1\nline2\nline3\n", 1, 4, 10, "/tmp/test.txt")
	if !strings.Contains(result, TruncationNoticeMarker) {
		t.Error("应包含截断标记")
	}
	if !strings.Contains(result, "/tmp/test.txt") {
		t.Error("应包含文件路径")
	}
}
