package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"diegoc-agent/internal/permission"
	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

type fileVersion struct {
	Data   []byte
	Mode   os.FileMode
	Exists bool
}
type checkpoint struct {
	ID       int
	Prompt   string
	Messages []schema.Message
	Summary  string
	Mode     permission.Mode
	Before   map[string]fileVersion
	After    map[string]fileVersion
}

// CheckpointInfo describes a user turn without exposing its saved state.
type CheckpointInfo struct {
	ID     int
	Prompt string
	Files  int
}

func cloneMessages(messages []schema.Message) []schema.Message {
	data, err := json.Marshal(messages)
	if err != nil {
		panic(err)
	}
	var cloned []schema.Message
	if err := json.Unmarshal(data, &cloned); err != nil {
		panic(err)
	}
	return cloned
}

func (a *Agent) makeCheckpoint(prompt string) {
	a.checkpointSequence++
	summary := a.compressedSummary
	if a.memoryManager != nil {
		summary = a.memoryManager.CompressedSummary()
	}
	a.checkpoints = append(a.checkpoints, checkpoint{ID: a.checkpointSequence, Prompt: prompt, Messages: cloneMessages(a.Messages), Summary: summary, Mode: a.PermissionCtx.Mode, Before: map[string]fileVersion{}, After: map[string]fileVersion{}})
	if len(a.checkpoints) > 100 {
		a.checkpoints = a.checkpoints[len(a.checkpoints)-100:]
	}
}

func (a *Agent) Checkpoints() []CheckpointInfo {
	out := make([]CheckpointInfo, 0, len(a.checkpoints))
	for _, cp := range a.checkpoints {
		out = append(out, CheckpointInfo{cp.ID, cp.Prompt, len(cp.Before)})
	}
	return out
}

func readVersion(path string) (fileVersion, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return fileVersion{}, nil
	}
	if err != nil {
		return fileVersion{}, err
	}
	if !info.Mode().IsRegular() {
		return fileVersion{}, fmt.Errorf("cannot checkpoint non-regular file: %s", path)
	}
	data, err := os.ReadFile(path)
	return fileVersion{data, info.Mode().Perm(), true}, err
}

func (a *Agent) trackBefore(tool tools.Tool, args map[string]interface{}) (string, error) {
	if len(a.checkpoints) == 0 {
		return "", nil
	}
	var workspace string
	switch t := tool.(type) {
	case *tools.WriteTool:
		workspace = t.WorkspaceDir
	case *tools.EditTool:
		workspace = t.WorkspaceDir
	default:
		return "", nil
	}
	path, _ := args["path"].(string)
	if path == "" {
		return "", nil
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(workspace, path)
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	// Resolve parent aliases so the same file is tracked only once; reject symlink files.
	parent, err := resolveParent(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	path = filepath.Join(parent, filepath.Base(path))
	version, err := readVersion(path)
	if err != nil {
		return "", err
	}
	cp := &a.checkpoints[len(a.checkpoints)-1]
	if _, ok := cp.Before[path]; !ok {
		cp.Before[path] = version
	}
	return path, nil
}

func resolveParent(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return "", err
	}
	resolved, err = resolveParent(parent)
	return filepath.Join(resolved, filepath.Base(path)), err
}

func equalVersion(a, b fileVersion) bool {
	return a.Exists == b.Exists && a.Mode == b.Mode && bytes.Equal(a.Data, b.Data)
}
func writeVersion(path string, v fileVersion) error {
	if !v.Exists {
		err := os.Remove(path)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(path, v.Data, v.Mode); err != nil {
		return err
	}
	return os.Chmod(path, v.Mode)
}

// Rewind restores the state immediately before the selected user turn.
// It must be called while the agent is idle. Modes: conversation, files, both.
func (a *Agent) Rewind(id int, mode string) (string, error) {
	if mode != "conversation" && mode != "files" && mode != "both" {
		return "", fmt.Errorf("mode must be conversation, files, or both")
	}
	idx := -1
	for i, cp := range a.checkpoints {
		if cp.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return "", fmt.Errorf("checkpoint %d not found", id)
	}
	cp := a.checkpoints[idx]
	if mode != "conversation" {
		targets := map[string]fileVersion{}
		current := map[string]fileVersion{}
		for _, entry := range a.checkpoints[idx:] {
			for path, v := range entry.Before {
				if _, ok := targets[path]; !ok {
					targets[path] = v
				}
			}
			for path, v := range entry.After {
				current[path] = v
			}
		}
		paths := make([]string, 0, len(targets))
		for path := range targets {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		for _, path := range paths {
			parent, err := resolveParent(filepath.Dir(path))
			if err != nil {
				return "", err
			}
			if parent != filepath.Dir(path) {
				return "", fmt.Errorf("file parent changed to a symlink: %s", path)
			}
			actual, err := readVersion(path)
			if err != nil {
				return "", err
			}
			expected, ok := current[path]
			if !ok || !equalVersion(actual, expected) {
				return "", fmt.Errorf("file changed outside tracked edits; refusing to overwrite: %s", path)
			}
		}
		var restored []string
		for _, path := range paths {
			if err := writeVersion(path, targets[path]); err != nil {
				rollbackErr := writeVersion(path, current[path])
				for i := len(restored) - 1; i >= 0; i-- {
					if e := writeVersion(restored[i], current[restored[i]]); e != nil {
						rollbackErr = e
					}
				}
				return "", fmt.Errorf("restore %s: %w (rollback: %v)", path, err, rollbackErr)
			}
			restored = append(restored, path)
		}
		for i := idx; i < len(a.checkpoints); i++ {
			a.checkpoints[i].Before = map[string]fileVersion{}
			a.checkpoints[i].After = map[string]fileVersion{}
		}
	}
	if mode != "files" {
		a.Messages = cloneMessages(cp.Messages)
		a.compressedSummary = cp.Summary
		if a.memoryManager != nil {
			a.memoryManager.SetCompressedSummary(cp.Summary)
		}
		a.PermissionCtx.Mode = cp.Mode
		a.skipNextTokenCheck = false
		a.toolCallStates = make(map[string]permission.ToolCallState)
		a.checkpoints = a.checkpoints[:idx]
	}
	return cp.Prompt, nil
}

// ClearConversation also discards rewind points and compacted context.
func (a *Agent) ClearConversation() {
	a.Messages = []schema.Message{{Role: "system", Content: a.SystemPrompt}}
	a.checkpoints = nil
	a.compressedSummary = ""
	a.skipNextTokenCheck = false
	if a.memoryManager != nil {
		a.memoryManager.SetCompressedSummary("")
	}
}
