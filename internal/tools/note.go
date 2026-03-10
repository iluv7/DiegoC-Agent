package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// RecordNoteTool records a session note to a JSON file.
type RecordNoteTool struct {
	MemoryFile string
	mu         sync.Mutex
}

func (t *RecordNoteTool) Name() string { return "record_note" }

func (t *RecordNoteTool) Description() string {
	return "Record important information as session notes for future reference. Use this to record key facts, user preferences, decisions, or context that should be recalled later in the agent execution chain. Each note is timestamped."
}

func (t *RecordNoteTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"content":  map[string]interface{}{"type": "string", "description": "The information to record as a note. Be concise but specific."},
			"category": map[string]interface{}{"type": "string", "description": "Optional category/tag for this note (e.g., 'user_preference', 'project_info', 'decision')"},
		},
		"required": []interface{}{"content"},
	}
}

func (t *RecordNoteTool) loadNotes() ([]map[string]interface{}, error) {
	data, err := os.ReadFile(t.MemoryFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var notes []map[string]interface{}
	if err := json.Unmarshal(data, &notes); err != nil {
		return nil, err
	}
	return notes, nil
}

func (t *RecordNoteTool) saveNotes(notes []map[string]interface{}) error {
	if err := os.MkdirAll(filepath.Dir(t.MemoryFile), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(notes, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(t.MemoryFile, data, 0644)
}

func (t *RecordNoteTool) Execute(ctx context.Context, args map[string]interface{}) (*ToolResult, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	content, _ := args["content"].(string)
	category := "general"
	if c, ok := args["category"].(string); ok && c != "" {
		category = c
	}
	if content == "" {
		return &ToolResult{Success: false, Error: "content is required"}, nil
	}
	notes, err := t.loadNotes()
	if err != nil {
		return &ToolResult{Success: false, Error: "Failed to load notes: " + err.Error()}, nil
	}
	if notes == nil {
		notes = []map[string]interface{}{}
	}
	notes = append(notes, map[string]interface{}{
		"timestamp": time.Now().Format(time.RFC3339),
		"category":  category,
		"content":   content,
	})
	if err := t.saveNotes(notes); err != nil {
		return &ToolResult{Success: false, Error: "Failed to record note: " + err.Error()}, nil
	}
	return &ToolResult{Success: true, Content: "Recorded note: " + content + " (category: " + category + ")"}, nil
}

// RecallNoteTool recalls session notes from the same JSON file.
type RecallNoteTool struct {
	MemoryFile string
}

func (t *RecallNoteTool) Name() string { return "recall_notes" }

func (t *RecallNoteTool) Description() string {
	return "Recall all previously recorded session notes. Use this to retrieve important information, context, or decisions from earlier in the session or previous agent execution chains."
}

func (t *RecallNoteTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"category": map[string]interface{}{"type": "string", "description": "Optional: filter notes by category"},
		},
	}
}

func (t *RecallNoteTool) Execute(ctx context.Context, args map[string]interface{}) (*ToolResult, error) {
	data, err := os.ReadFile(t.MemoryFile)
	if err != nil {
		if os.IsNotExist(err) {
			return &ToolResult{Success: true, Content: "No notes recorded yet."}, nil
		}
		return &ToolResult{Success: false, Error: err.Error()}, nil
	}
	var notes []map[string]interface{}
	if err := json.Unmarshal(data, &notes); err != nil {
		return &ToolResult{Success: false, Error: err.Error()}, nil
	}
	if len(notes) == 0 {
		return &ToolResult{Success: true, Content: "No notes recorded yet."}, nil
	}
	category, _ := args["category"].(string)
	if category != "" {
		filtered := notes[:0]
		for _, n := range notes {
			if c, _ := n["category"].(string); c == category {
				filtered = append(filtered, n)
			}
		}
		notes = filtered
		if len(notes) == 0 {
			return &ToolResult{Success: true, Content: "No notes found in category: " + category}, nil
		}
	}
	out := "Recorded Notes:\n"
	for i, n := range notes {
		ts, _ := n["timestamp"].(string)
		cat, _ := n["category"].(string)
		cont, _ := n["content"].(string)
		if ts == "" {
			ts = "unknown time"
		}
		if cat == "" {
			cat = "general"
		}
		out += fmt.Sprintf("%d. [%s] %s\n   (recorded at %s)\n", i+1, cat, cont, ts)
	}
	return &ToolResult{Success: true, Content: out}, nil
}