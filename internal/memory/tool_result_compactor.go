package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"diegoc-agent/internal/schema"
)

// 截断标记，出现在输出中表示后续内容已保存到磁盘文件。
const TruncationNoticeMarker = "<<<TRUNCATED>>>"

// 默认阈值
const (
	DefaultRecentMaxBytes = 100 * 1024 // 近期消息截断阈值 100KB
	DefaultOldMaxBytes    = 3000       // 旧消息截断阈值 3KB
	DefaultRecentN        = 1          // 最少保留尾部 N 条 tool_result 为近期
	DefaultRetentionDays  = 3          // tool_result 文件保留天数
)

// ToolResultCompactor 工具输出压缩器。
// 双层截断：近期消息 100KB，旧消息 3KB。完整内容存入 tool_results/ 目录。
//
// 对应 ReMe 的 ToolResultCompactor
// (reme/memory/file_based/components/tool_result_compactor.py:18)。
type ToolResultCompactor struct {
	mu             sync.Mutex
	toolResultDir  string
	retentionDays  int
	oldMaxBytes    int
	recentMaxBytes int
	recentN        int
}

// NewToolResultCompactor 创建工具输出压缩器。
func NewToolResultCompactor(toolResultDir string, retentionDays, oldMaxBytes, recentMaxBytes, recentN int) *ToolResultCompactor {
	os.MkdirAll(toolResultDir, 0755)
	return &ToolResultCompactor{
		toolResultDir:  toolResultDir,
		retentionDays:  retentionDays,
		oldMaxBytes:    oldMaxBytes,
		recentMaxBytes: recentMaxBytes,
		recentN:        recentN,
	}
}

// Compact 处理消息列表，截断大工具输出。
// 近期消息（尾部连续 tool_result，至少 recentN 条）用宽松阈值，
// 旧消息用严格阈值。.md 文件的 read_file 结果始终用宽松阈值。
func (t *ToolResultCompactor) Compact(messages []schema.Message) []schema.Message {
	if len(messages) == 0 {
		return messages
	}

	// 计算尾部连续 tool_result 消息数
	trailingToolResults := 0
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "tool" {
			trailingToolResults++
		} else {
			break
		}
	}
	// 分割点：近期窗口 = max(尾部连续数, recentN)
	splitIndex := len(messages) - max(trailingToolResults, t.recentN)
	if splitIndex < 0 {
		splitIndex = 0
	}

	// 收集 md 文件 read_file 的 tool_use ID
	mdFileToolIDs := t.findMdFileReadToolIDs(messages)

	// 处理每条消息
	for i := range messages {
		msg := &messages[i]
		if msg.Role != "tool" {
			continue
		}

		isRecent := i >= splitIndex
		maxBytes := t.oldMaxBytes
		if isRecent {
			maxBytes = t.recentMaxBytes
		}

		// .md 文件保护
		if _, ok := mdFileToolIDs[msg.ToolCallID]; ok {
			maxBytes = t.recentMaxBytes
		}

		contentStr, ok := msg.Content.(string)
		if !ok || contentStr == "" {
			continue
		}

		msg.Content = t.truncate(contentStr, maxBytes)
	}

	return messages
}

// findMdFileReadToolIDs 找出所有读 .md 文件的 read_file 调用。
func (t *ToolResultCompactor) findMdFileReadToolIDs(messages []schema.Message) map[string]bool {
	ids := make(map[string]bool)
	for _, msg := range messages {
		for _, tc := range msg.ToolCalls {
			if tc.ID == "" {
				continue
			}
			name := strings.ToLower(tc.Function.Name)
			if name != "read_file" && name != "read" {
				continue
			}
			// 检查参数中是否包含 .md
			argsStr := fmt.Sprint(tc.Function.Arguments)
			if strings.Contains(strings.ToLower(argsStr), ".md") {
				ids[tc.ID] = true
			}
		}
	}
	return ids
}

// truncate 截断内容。如果已有截断标记，复用已有信息。
// 超阈值时生成 UUID 文件名保存完整内容，返回截断版 + 文件路径提示。
func (t *ToolResultCompactor) truncate(content string, maxBytes int) string {
	if content == "" {
		return content
	}

	// 已截断过：只调整截断长度，不重新存文件
	if strings.Contains(content, TruncationNoticeMarker) {
		return t.retruncate(content, maxBytes)
	}

	// 没超阈值，原样返回（留 100 字节缓冲）
	if len(content) <= maxBytes+100 {
		return content
	}

	// 超阈值，保存完整内容到文件
	t.mu.Lock()
	filename := filepath.Join(t.toolResultDir, newUUID()+".txt")
	err := os.WriteFile(filename, []byte(content), 0644)
	t.mu.Unlock()

	if err != nil {
		// 写文件失败，退化为纯截断
		return content[:maxBytes] + "..."
	}

	return truncateOutput(content, 1, strings.Count(content, "\n")+1, maxBytes, filename)
}

// retruncate 重新截断已有截断标记的内容，不生成新文件。
func (t *ToolResultCompactor) retruncate(content string, maxBytes int) string {
	// 截到标记处
	markerIdx := strings.Index(content, TruncationNoticeMarker)
	if markerIdx < 0 {
		return content
	}

	beforeMarker := content[:markerIdx]
	if len(beforeMarker) > maxBytes {
		beforeMarker = beforeMarker[:maxBytes]
	}
	return beforeMarker + "\n" + TruncationNoticeMarker + "\n[content previously saved to disk]"
}

// CleanupExpiredFiles 清理超过 retentionDays 的 tool_result 文件。
// 返回删除的文件数。
func (t *ToolResultCompactor) CleanupExpiredFiles() int {
	entries, err := os.ReadDir(t.toolResultDir)
	if err != nil {
		return 0
	}

	cutoff := time.Now().AddDate(0, 0, -t.retentionDays)
	deleted := 0

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".txt") {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		// 使用修改时间（跨平台兼容）
		if info.ModTime().Before(cutoff) {
			path := filepath.Join(t.toolResultDir, entry.Name())
			if os.Remove(path) == nil {
				deleted++
			}
		}
	}
	return deleted
}

// truncateOutput 截断文本输出，附加文件保存信息。
func truncateOutput(content string, startLine, totalLines, maxBytes int, filePath string) string {
	// 取前 maxBytes
	truncated := content
	if len(truncated) > maxBytes {
		truncated = truncated[:maxBytes]
	}

	notice := fmt.Sprintf(
		"\n%s\nThe output above was truncated.\n"+
			"The full content is saved to the file %s and contains %d lines in total.\n"+
			"This excerpt starts at line %d and covers the next %d bytes.\n"+
			"If the current content is not enough, call read_file with file_path=%s start_line=%d to read more.",
		TruncationNoticeMarker, filePath, totalLines, startLine, len(truncated), filePath, startLine+1,
	)

	return truncated + notice
}

// newUUID 生成简单 UUID（避免引入外部依赖）。
func newUUID() string {
	return fmt.Sprintf("%08x%08x%08x%08x",
		uint32(time.Now().UnixNano()>>32),
		uint32(time.Now().UnixNano()),
		uint32(os.Getpid()),
		uint32(time.Now().UnixNano()>>16),
	)
}
