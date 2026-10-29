package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

func writeTracked(t *testing.T, a *Agent, dir, path, content string) {
	t.Helper()
	a.executeAndAppend(context.Background(), schema.ToolCall{ID: "write", Function: schema.FunctionCall{Name: "write_file"}}, &tools.WriteTool{WorkspaceDir: dir}, map[string]interface{}{"path": path, "content": content})
	last := a.Messages[len(a.Messages)-1]
	if text, _ := last.Content.(string); len(text) >= 6 && text[:6] == "Error:" {
		t.Fatal(text)
	}
}
func requireFile(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != want {
		t.Fatalf("file = %q, %v; want %q", data, err, want)
	}
}
func TestRewindMultipleTurns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "existing")
	if err := os.WriteFile(path, []byte("original"), 0750); err != nil {
		t.Fatal(err)
	}
	a := New(nil, "system", 10, 1000, nil)
	a.AddUserMessage("first")
	writeTracked(t, a, dir, "existing", "one")
	writeTracked(t, a, dir, "existing", "two")
	a.AddUserMessage("second")
	writeTracked(t, a, dir, "existing", "three")
	writeTracked(t, a, dir, "new", "new file")
	prompt, err := a.Rewind(1, "both")
	if err != nil || prompt != "first" {
		t.Fatalf("rewind: %q %v", prompt, err)
	}
	requireFile(t, path, "original")
	if _, err := os.Stat(filepath.Join(dir, "new")); !os.IsNotExist(err) {
		t.Fatal("new file was not removed")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0750 {
		t.Fatal("permissions not restored")
	}
	if len(a.Messages) != 1 || len(a.Checkpoints()) != 0 {
		t.Fatal("conversation not restored")
	}
}
func TestRewindFilesThenEarlier(t *testing.T) {
	dir := t.TempDir()
	a := New(nil, "system", 10, 1000, nil)
	a.AddUserMessage("first")
	writeTracked(t, a, dir, "file", "one")
	a.AddUserMessage("second")
	writeTracked(t, a, dir, "file", "two")
	count := len(a.Messages)
	if _, err := a.Rewind(2, "files"); err != nil {
		t.Fatal(err)
	}
	requireFile(t, filepath.Join(dir, "file"), "one")
	if len(a.Messages) != count {
		t.Fatal("files-only changed conversation")
	}
	if _, err := a.Rewind(1, "both"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "file")); !os.IsNotExist(err) {
		t.Fatal("file remains")
	}
}
func TestRewindRejectsExternalChangeBeforeAnyRestore(t *testing.T) {
	dir := t.TempDir()
	a := New(nil, "system", 10, 1000, nil)
	a.AddUserMessage("first")
	writeTracked(t, a, dir, "a", "agent")
	writeTracked(t, a, dir, "z", "agent")
	if err := os.WriteFile(filepath.Join(dir, "z"), []byte("manual"), 0644); err != nil {
		t.Fatal(err)
	}
	count := len(a.Messages)
	if _, err := a.Rewind(1, "both"); err == nil {
		t.Fatal("expected conflict")
	}
	requireFile(t, filepath.Join(dir, "a"), "agent")
	requireFile(t, filepath.Join(dir, "z"), "manual")
	if len(a.Messages) != count {
		t.Fatal("failed rewind changed context")
	}
}
func TestConversationSnapshotSurvivesCompaction(t *testing.T) {
	a := New(nil, "system", 10, 1000, nil)
	a.AddUserMessage("first")
	a.Messages = append(a.Messages, schema.Message{Role: "assistant", Content: "answer", ToolCalls: []schema.ToolCall{{ID: "call", Function: schema.FunctionCall{Arguments: map[string]interface{}{"value": "before"}}}}})
	a.compressedSummary = "old summary"
	a.AddUserMessage("second")
	a.Messages[2].ToolCalls[0].Function.Arguments["value"] = "mutated"
	a.Messages = []schema.Message{{Role: "system", Content: "compacted"}}
	a.compressedSummary = "future summary"
	if _, err := a.Rewind(2, "conversation"); err != nil {
		t.Fatal(err)
	}
	if len(a.Messages) != 3 || a.compressedSummary != "old summary" || a.Messages[2].ToolCalls[0].Function.Arguments["value"] != "before" {
		t.Fatal("snapshot was mutated or lost")
	}
	a.ClearConversation()
	if len(a.Checkpoints()) != 0 || a.compressedSummary != "" {
		t.Fatal("clear left rewind state")
	}
	if _, err := a.Rewind(1, "both"); err == nil {
		t.Fatal("cleared checkpoint accepted")
	}
}
func TestRewindRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "real")
	os.WriteFile(path, []byte("safe"), 0644)
	if err := os.Symlink(path, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	a := New(nil, "system", 10, 1000, nil)
	a.AddUserMessage("first")
	_, err := a.trackBefore(&tools.WriteTool{WorkspaceDir: dir}, map[string]interface{}{"path": "link"})
	if err == nil {
		t.Fatal("symlink accepted")
	}
	requireFile(t, path, "safe")
}
