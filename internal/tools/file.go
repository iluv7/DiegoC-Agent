package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// truncateByTokens keeps head and tail of text to stay under roughly maxTokens (char/4 approx).
func truncateByTokens(text string, maxTokens int) string {
	const charsPerToken = 4
	maxChars := maxTokens * charsPerToken
	if len(text) <= maxChars {
		return text
	}
	half := (maxChars / 2) - 50
	head := text[:half]
	if i := strings.LastIndex(head, "\n"); i > 0 {
		head = head[:i]
	}
	tail := text[len(text)-half:]
	if i := strings.Index(tail, "\n"); i > 0 {
		tail = tail[i+1:]
	}
	return head + "\n\n... [Content truncated] ...\n\n" + tail
}

// ReadTool reads file contents with line numbers.
type ReadTool struct {
	WorkspaceDir string
}

func (t *ReadTool) Name() string { return "read_file" }

func (t *ReadTool) Description() string {
	return "Read file contents from the filesystem. Output always includes line numbers in format 'LINE_NUMBER|LINE_CONTENT' (1-indexed). Supports reading partial content by specifying line offset and limit for large files."
}

func (t *ReadTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path":   map[string]interface{}{"type": "string", "description": "Absolute or relative path to the file"},
			"offset": map[string]interface{}{"type": "integer", "description": "Starting line number (1-indexed). Use for large files to read from specific line"},
			"limit":  map[string]interface{}{"type": "integer", "description": "Number of lines to read. Use with offset for large files to read in chunks"},
		},
		"required": []interface{}{"path"},
	}
}

func (t *ReadTool) Execute(ctx context.Context, args map[string]interface{}) (*ToolResult, error) {
	path, _ := args["path"].(string)
	if path == "" {
		return &ToolResult{Success: false, Error: "path is required"}, nil
	}
	fullPath := path
	if !filepath.IsAbs(path) {
		fullPath = filepath.Join(t.WorkspaceDir, path)
	}
	fullPath = filepath.Clean(fullPath)
	data, err := os.ReadFile(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &ToolResult{Success: false, Error: "File not found: " + path}, nil
		}
		return &ToolResult{Success: false, Error: err.Error()}, nil
	}
	lines := strings.Split(string(data), "\n")
	offset := 0
	if v, ok := args["offset"]; ok {
		if n, ok := toInt(v); ok && n >= 1 {
			offset = n - 1
		}
	}
	limit := len(lines)
	if v, ok := args["limit"]; ok {
		if n, ok := toInt(v); ok && n > 0 {
			limit = n
		}
	}
	if offset < 0 {
		offset = 0
	}
	end := offset + limit
	if end > len(lines) {
		end = len(lines)
	}
	selected := lines[offset:end]
	var out strings.Builder
	for i, line := range selected {
		lineNum := offset + i + 1
		clean := strings.TrimRight(line, "\r\n")
		out.WriteString(fmt.Sprintf("%6d|", lineNum))
		out.WriteString(clean)
		out.WriteString("\n")
	}
	content := out.String()
	content = truncateByTokens(content, 32000)
	return &ToolResult{Success: true, Content: content}, nil
}

func toInt(v interface{}) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case float64:
		return int(x), true
	default:
		return 0, false
	}
}

// WriteTool writes content to a file.
type WriteTool struct {
	WorkspaceDir string
}

func (t *WriteTool) Name() string { return "write_file" }

func (t *WriteTool) Description() string {
	return "Write content to a file. Will overwrite existing files completely. For existing files, you should read the file first using read_file. Prefer editing existing files over creating new ones unless explicitly needed."
}

func (t *WriteTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path":    map[string]interface{}{"type": "string", "description": "Absolute or relative path to the file"},
			"content": map[string]interface{}{"type": "string", "description": "Complete content to write (will replace existing content)"},
		},
		"required": []interface{}{"path", "content"},
	}
}

func (t *WriteTool) Execute(ctx context.Context, args map[string]interface{}) (*ToolResult, error) {
	path, _ := args["path"].(string)
	content, _ := args["content"].(string)
	if path == "" {
		return &ToolResult{Success: false, Error: "path is required"}, nil
	}
	fullPath := path
	if !filepath.IsAbs(path) {
		fullPath = filepath.Join(t.WorkspaceDir, path)
	}
	fullPath = filepath.Clean(fullPath)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		return &ToolResult{Success: false, Error: err.Error()}, nil
	}
	if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
		return &ToolResult{Success: false, Error: err.Error()}, nil
	}
	return &ToolResult{Success: true, Content: "Successfully wrote to " + fullPath}, nil
}

// EditTool edits a file by exact string replacement.
type EditTool struct {
	WorkspaceDir string
}

func (t *EditTool) Name() string { return "edit_file" }

func (t *EditTool) Description() string {
	return "Perform exact string replacement in a file. The old_str must match exactly and appear uniquely in the file, otherwise the operation will fail. You must read the file first before editing. Preserve exact indentation from the source."
}

func (t *EditTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path":    map[string]interface{}{"type": "string", "description": "Absolute or relative path to the file"},
			"old_str": map[string]interface{}{"type": "string", "description": "Exact string to find and replace (must be unique in file)"},
			"new_str": map[string]interface{}{"type": "string", "description": "Replacement string (use for refactoring, renaming, etc.)"},
		},
		"required": []interface{}{"path", "old_str", "new_str"},
	}
}

func (t *EditTool) Execute(ctx context.Context, args map[string]interface{}) (*ToolResult, error) {
	path, _ := args["path"].(string)
	oldStr, _ := args["old_str"].(string)
	newStr, _ := args["new_str"].(string)
	if path == "" || oldStr == "" {
		return &ToolResult{Success: false, Error: "path and old_str are required"}, nil
	}
	fullPath := path
	if !filepath.IsAbs(path) {
		fullPath = filepath.Join(t.WorkspaceDir, path)
	}
	fullPath = filepath.Clean(fullPath)
	data, err := os.ReadFile(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &ToolResult{Success: false, Error: "File not found: " + path}, nil
		}
		return &ToolResult{Success: false, Error: err.Error()}, nil
	}
	content := string(data)
	if !strings.Contains(content, oldStr) {
		return &ToolResult{Success: false, Error: "Text not found in file: " + oldStr}, nil
	}
	newContent := strings.Replace(content, oldStr, newStr, 1)
	if err := os.WriteFile(fullPath, []byte(newContent), 0644); err != nil {
		return &ToolResult{Success: false, Error: err.Error()}, nil
	}
	return &ToolResult{Success: true, Content: "Successfully edited " + fullPath}, nil
}
