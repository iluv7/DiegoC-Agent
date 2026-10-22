package agent

import (
	"fmt"
	"os"
	"sort"
)

// FileSnapshot is an owned copy of a recorded file version, not live disk state.
type FileSnapshot struct {
	Data   []byte
	Mode   os.FileMode
	Exists bool
}

type FileChange struct {
	Path          string
	Before, After FileSnapshot
}

type TurnChanges struct {
	ID          int
	Prompt      string
	Files       []FileChange
	Unavailable []string
}

// FileChanges reads a foreground session's checkpoints while it is idle.
// id=0 selects the latest user turn, even if that turn changed no files.
// No filesystem reads or mutations occur; later external edits are excluded.
func (a *Agent) FileChanges(id int) (TurnChanges, error) {
	if id < 0 {
		return TurnChanges{}, fmt.Errorf("turn ID must be positive")
	}
	idx := len(a.checkpoints) - 1
	if id != 0 {
		idx = -1
		for i, cp := range a.checkpoints {
			if cp.ID == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			return TurnChanges{}, fmt.Errorf("checkpoint %d not found", id)
		}
	}
	if idx < 0 {
		return TurnChanges{}, nil
	}
	cp := a.checkpoints[idx]
	result := TurnChanges{ID: cp.ID, Prompt: cp.Prompt}
	paths := make([]string, 0, len(cp.Before))
	for path := range cp.Before {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	copyVersion := func(v fileVersion) FileSnapshot {
		return FileSnapshot{Data: append([]byte(nil), v.Data...), Mode: v.Mode, Exists: v.Exists}
	}
	for _, path := range paths {
		before := cp.Before[path]
		after, ok := cp.After[path]
		if !ok {
			result.Unavailable = append(result.Unavailable, path)
			continue
		}
		if equalVersion(before, after) {
			continue
		}
		result.Files = append(result.Files, FileChange{Path: path, Before: copyVersion(before), After: copyVersion(after)})
	}
	return result, nil
}
