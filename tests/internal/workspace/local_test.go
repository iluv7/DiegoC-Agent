package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"diegoc-agent/internal/tools"
)

func TestLocalWorkspaceLifecycleAndTools(t *testing.T) {
	dir := t.TempDir()
	ws := NewLocal(LocalOptions{
		Workdir:      dir,
		CreateLayout: true,
	})
	ctx := context.Background()

	if _, err := ws.Backend(); err == nil {
		t.Fatal("backend available before Initialize")
	}
	if err := ws.Initialize(ctx); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if err := ws.Initialize(ctx); err != nil {
		t.Fatalf("initialize should be idempotent: %v", err)
	}

	layout := ws.Layout()
	if layout.Workdir != dir ||
		layout.DataDir != filepath.Join(dir, DataDirName) ||
		layout.SkillsDir != filepath.Join(dir, SkillsDirName) ||
		layout.SessionsDir != filepath.Join(dir, SessionsDirName) {
		t.Fatalf("unexpected layout: %+v", layout)
	}
	for _, path := range []string{layout.DataDir, layout.SkillsDir, layout.SessionsDir} {
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			t.Fatalf("layout dir %s missing: %v", path, err)
		}
	}
	if !strings.Contains(ws.Instructions(), dir) {
		t.Fatalf("instructions do not mention workdir: %q", ws.Instructions())
	}

	list, err := ws.ListTools(ToolOptions{
		FileTools: true,
		Bash:      true,
		Plan:      true,
		Notes:     true,
	})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	byName := make(map[string]tools.Tool, len(list))
	for _, tool := range list {
		byName[tool.Name()] = tool
	}
	for _, name := range []string{
		"read_file", "write_file", "edit_file", "write_plan",
		"bash", "bash_output", "bash_kill", "record_note", "recall_notes",
	} {
		if byName[name] == nil {
			t.Fatalf("missing tool %q", name)
		}
	}

	write, _ := byName["write_file"].Execute(ctx, map[string]interface{}{
		"path":    "sub/x.txt",
		"content": "hello world",
	})
	if !write.Success {
		t.Fatalf("write: %+v", write)
	}
	read, _ := byName["read_file"].Execute(ctx, map[string]interface{}{"path": "sub/x.txt"})
	if !read.Success || !strings.Contains(read.Content, "hello world") {
		t.Fatalf("read: %+v", read)
	}

	bash, _ := byName["bash"].Execute(ctx, map[string]interface{}{"command": "echo from-bash"})
	if !bash.Success || !strings.Contains(bash.Content, "from-bash") {
		t.Fatalf("bash: %+v", bash)
	}

	note, _ := byName["record_note"].Execute(ctx, map[string]interface{}{"content": "remember me"})
	if !note.Success {
		t.Fatalf("record note: %+v", note)
	}
	recall, _ := byName["recall_notes"].Execute(ctx, nil)
	if !recall.Success || !strings.Contains(recall.Content, "remember me") {
		t.Fatalf("recall: %+v", recall)
	}

	plan, _ := byName["write_plan"].Execute(ctx, map[string]interface{}{"content": "step one"})
	if !plan.Success {
		t.Fatalf("plan: %+v", plan)
	}
	if data, err := os.ReadFile(filepath.Join(dir, PlanFileName)); err != nil || !strings.Contains(string(data), "step one") {
		t.Fatalf("plan file: %q %v", data, err)
	}

	if err := ws.Reset(ctx); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if _, err := os.Stat(layout.DataDir); !os.IsNotExist(err) {
		t.Fatalf("reset left data dir: %v", err)
	}
	if err := ws.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := ws.Close(ctx); err != nil {
		t.Fatalf("close should be idempotent: %v", err)
	}
	if _, err := ws.Backend(); err == nil {
		t.Fatal("backend still available after close")
	}

	if err := ws.Initialize(ctx); err != nil {
		t.Fatalf("re-initialize: %v", err)
	}
	if _, err := ws.Backend(); err != nil {
		t.Fatalf("backend after re-initialize: %v", err)
	}
}

func TestLocalWorkspaceWithoutLayoutDoesNotCreateExtraDirs(t *testing.T) {
	dir := t.TempDir()
	ws := NewLocal(LocalOptions{Workdir: filepath.Join(dir, "fresh")})
	if err := ws.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if _, err := os.Stat(ws.Layout().DataDir); !os.IsNotExist(err) {
		t.Fatalf("layout created without opt-in: %v", err)
	}
}
