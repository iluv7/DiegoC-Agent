package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"diegoc-agent/internal/agent"
	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
	"golang.org/x/sys/unix"
)

func writeProjectRules(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestProjectInstructionsUseOnlySelectedWorkspaceRoot(t *testing.T) {
	parent := t.TempDir()
	workspace := filepath.Join(parent, "selected")
	writeProjectRules(t, parent, "PARENT_RULE")
	writeProjectRules(t, filepath.Join(parent, "other"), "OTHER_WORKSPACE_RULE")
	writeProjectRules(t, filepath.Join(workspace, "nested"), "NESTED_RULE")
	content := "# 项目约定\n使用 go test ./...\nKeep {SKILLS_METADATA} literal here.\n"
	writeProjectRules(t, workspace, content)
	prompt, path, err := buildSystemPrompt("BASE_RULE\n{SKILLS_METADATA}", workspace, "SKILL_METADATA")
	if err != nil || path != filepath.Join(workspace, "AGENTS.md") {
		t.Fatalf("load rules: path=%q err=%v", path, err)
	}
	for _, want := range []string{"BASE_RULE", "SKILL_METADATA", content, path, "explicit user request", "do not grant tool permissions"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("missing %q in composed prompt", want)
		}
	}
	for _, unwanted := range []string{"PARENT_RULE", "OTHER_WORKSPACE_RULE", "NESTED_RULE"} {
		if strings.Contains(prompt, unwanted) {
			t.Fatalf("loaded rules outside the selected root: %s", unwanted)
		}
	}
	if strings.Count(prompt, content) != 1 || strings.Count(prompt, "{SKILLS_METADATA}") != 1 {
		t.Fatal("duplicated or template-expanded project content")
	}
}

func TestProjectInstructionsMissingEmptyAndUTF8(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		write, loaded bool
	}{
		{"missing", "", false, false},
		{"empty", "", true, false},
		{"whitespace", "\uFEFF \n\t", true, false},
		{"Chinese", "优先使用中文回答。", true, true},
		{"BOM", "\uFEFF优先使用中文回答。", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.write {
				writeProjectRules(t, dir, tc.content)
			}
			prompt, path, err := buildSystemPrompt("base {SKILLS_METADATA}", dir, "")
			if err != nil || (path != "") != tc.loaded {
				t.Fatalf("loaded=%q error=%v", path, err)
			}
			if strings.Contains(prompt, "{SKILLS_METADATA}") || strings.Contains(prompt, "\uFEFF") {
				t.Fatal("template or BOM survived unexpectedly")
			}
			if !tc.loaded && strings.Contains(prompt, "Project Instructions") {
				t.Fatal("empty instructions added a prompt block")
			}
			if tc.loaded && !strings.Contains(prompt, "优先使用中文回答。") {
				t.Fatal("Chinese instructions were lost")
			}
		})
	}
}

func TestProjectInstructionsRejectUnreadableOrNonTextEntries(t *testing.T) {
	for _, kind := range []string{"directory", "dangling symlink", "FIFO", "invalid UTF-8"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "AGENTS.md")
			var err error
			switch kind {
			case "directory":
				err = os.Mkdir(path, 0755)
			case "dangling symlink":
				err = os.Symlink(filepath.Join(dir, "missing.md"), path)
			case "FIFO":
				err = unix.Mkfifo(path, 0600)
			case "invalid UTF-8":
				err = os.WriteFile(path, []byte{0xff, 0xfe}, 0644)
			}
			if err != nil {
				t.Fatal(err)
			}
			prompt, loaded, err := buildSystemPrompt("base", dir, "")
			if err == nil || prompt != "" || loaded != "" || !strings.Contains(err.Error(), path) {
				t.Fatalf("invalid rules silently ignored: prompt=%q path=%q err=%v", prompt, loaded, err)
			}
		})
	}
}

func TestProjectInstructionsFollowFileSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "project-rules.md")
	if err := os.WriteFile(target, []byte("SHARED_PROJECT_RULE"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	prompt, path, err := buildSystemPrompt("base", dir, "")
	if err != nil || path != filepath.Join(dir, "AGENTS.md") || !strings.Contains(prompt, "SHARED_PROJECT_RULE") {
		t.Fatalf("file symlink: %s %v", path, err)
	}
}

type projectRuleClient struct{}

func (projectRuleClient) Generate(context.Context, []schema.Message, []tools.Tool) (*schema.LLMResponse, error) {
	return &schema.LLMResponse{Content: "## Goal\nMaintain the project.\n## Constraints\nFollow the API contract.\n## Progress\nImplementation complete.\n## Key Decisions\nUse Go.\n## Next Steps\nRun tests."}, nil
}

func TestProjectRulesRemainInSystemAcrossConversationOperations(t *testing.T) {
	dir := t.TempDir()
	rule := "STARTUP_PROJECT_RULE"
	writeProjectRules(t, dir, rule)
	prompt, _, err := buildSystemPrompt("base", dir, "")
	if err != nil {
		t.Fatal(err)
	}
	root := agent.New(projectRuleClient{}, prompt, 5, 100000, nil)
	check := func(a *agent.Agent) {
		t.Helper()
		text := a.Messages[0].Content.(string)
		if a.Messages[0].Role != "system" || strings.Count(text, rule) != 1 || strings.Contains(text, "CHANGED_ON_DISK") {
			t.Fatal("startup project rules were lost, duplicated, or reloaded")
		}
	}
	writeProjectRules(t, dir, "CHANGED_ON_DISK")
	root.AddUserMessage("first request")
	check(root)
	root.ClearConversation()
	check(root)
	root.SetPlanMode(true)
	check(root)
	sessions := agent.NewConversationSessions(root)
	defer sessions.Close()
	child, err := sessions.Fork()
	if err != nil {
		t.Fatal(err)
	}
	check(child.Agent)
	root.AddUserMessage(strings.Repeat("Preserve the API contract. ", 300))
	root.Messages = append(root.Messages, schema.Message{Role: "assistant", Content: strings.Repeat("Implementation notes. ", 300)})
	if _, err := root.Compact(context.Background(), "keep the API contract"); err != nil {
		t.Fatal(err)
	}
	check(root)
	if _, err := root.Rewind(2, "conversation"); err != nil {
		t.Fatal(err)
	}
	check(root)
	root.SetPlanMode(false)
	check(root)
}
