package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"diegoc-agent/internal/agent"
	"diegoc-agent/internal/logger"
	"diegoc-agent/internal/permission"
	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

func testGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("Git is required for diff rendering")
	}
	cmd := exec.Command("git", append([]string{"-c", "user.name=Diff Test", "-c", "user.email=diff@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func diffTestFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotDiffShowsHunksNewEmptyBinaryAndPermissions(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("Git is required")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "space name.txt")
	diffTestFile(t, path, "LIVE CONTENT MUST NOT CHANGE")
	version := func(data string, mode os.FileMode) agent.FileSnapshot {
		return agent.FileSnapshot{Data: []byte(data), Mode: mode, Exists: true}
	}
	turn := agent.TurnChanges{ID: 3, Prompt: "edit files", Files: []agent.FileChange{
		{Path: path, Before: version("-- a/before\nkeep\nold", 0644), After: version("after\nkeep\nnew", 0644)},
		{Path: filepath.Join(dir, "empty.txt"), After: version("", 0644)},
		{Path: filepath.Join(dir, "removed.txt"), Before: version("deleted\n", 0644)},
		{Path: filepath.Join(dir, "binary.dat"), Before: version("\x00a", 0644), After: version("\x00bc", 0644)},
		{Path: filepath.Join(dir, "permissions.txt"), Before: version("same\n", 0644), After: version("same\n", 0600)},
	}}
	out, err := snapshotDiff(context.Background(), dir, turn)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Turn 3", "space name.txt", "--- a/before\n", "-old", "+new", "\\ No newline at end of file", "empty.txt\" (added)", "new file mode", "removed.txt\" (deleted)", "-deleted", "Binary file: 2 → 3 bytes", "Permissions: 0644 → 0600"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in patch:\n%s", want, out)
		}
	}
	if strings.Contains(out, "diegoc-diff-") || strings.ContainsRune(out, '\x00') {
		t.Fatal("temporary paths or binary contents leaked into the view")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "LIVE CONTENT MUST NOT CHANGE" {
		t.Fatal("diff renderer modified the working file")
	}
}

func TestWorkspaceGitDiffSeparatesIndexAndWorkingTreeAndScopesToWorkspace(t *testing.T) {
	dir := t.TempDir()
	testGit(t, dir, "init", "-q")
	workspace := filepath.Join(dir, "sub")
	diffTestFile(t, filepath.Join(dir, "outside.txt"), "base outside\n")
	diffTestFile(t, filepath.Join(workspace, "tracked.txt"), "base\n")
	diffTestFile(t, filepath.Join(workspace, ".gitignore"), "ignored.txt\n")
	testGit(t, dir, "add", ".")
	testGit(t, dir, "commit", "-qm", "initial")
	diffTestFile(t, filepath.Join(dir, "outside.txt"), "OUTSIDE_WORKSPACE_CHANGE\n")
	diffTestFile(t, filepath.Join(workspace, "tracked.txt"), "staged\n")
	testGit(t, dir, "add", "sub/tracked.txt")
	diffTestFile(t, filepath.Join(workspace, "tracked.txt"), "working tree\n")
	diffTestFile(t, filepath.Join(workspace, "new \nfile.txt"), "UNTRACKED_CONTENT_NOT_FOR_PATCH\n")
	diffTestFile(t, filepath.Join(workspace, "ignored.txt"), "ignored\n")
	indexBefore := testGit(t, dir, "ls-files", "--stage")
	out, err := workspaceGitDiff(context.Background(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Staged:", "-base", "+staged", "Unstaged:", "-staged", "+working tree", "Untracked files (paths only", `new \nfile.txt`} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in Git view:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"OUTSIDE_WORKSPACE_CHANGE", "outside.txt", "UNTRACKED_CONTENT_NOT_FOR_PATCH", `  "ignored.txt"`} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("included unexpected content: %q", unwanted)
		}
	}
	if indexBefore != testGit(t, dir, "ls-files", "--stage") {
		t.Fatal("viewing Git diff changed the index")
	}
	data, _ := os.ReadFile(filepath.Join(workspace, "tracked.txt"))
	if string(data) != "working tree\n" {
		t.Fatal("viewing diff changed worktree contents")
	}
}

func TestWorkspaceGitDiffSupportsUnbornRepositoriesAndErrors(t *testing.T) {
	dir := t.TempDir()
	testGit(t, dir, "init", "-q")
	diffTestFile(t, filepath.Join(dir, "new.txt"), "first file\n")
	testGit(t, dir, "add", "new.txt")
	out, err := workspaceGitDiff(context.Background(), dir)
	if err != nil || !strings.Contains(out, "+first file") {
		t.Fatalf("unborn repository: %s %v", out, err)
	}
	if _, err := workspaceGitDiff(context.Background(), t.TempDir()); err == nil {
		t.Fatal("non-Git workspace was accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := workspaceGitDiff(ctx, dir); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation not propagated: %v", err)
	}
}

func TestDiffCommandDoesNotRequestModelOrAppendConversation(t *testing.T) {
	a := agent.New(nil, "system", 5, 100000, nil)
	dir := t.TempDir()
	for _, command := range []string{"/diff", "/DIFF", "/diff 999", "/diff 0", "/diff -1", "/diff nonsense", "/diff git extra", "/diff git"} {
		if !handleDiffCommand(a, dir, command) {
			t.Fatalf("command was not handled: %s", command)
		}
	}
	if handleDiffCommand(a, dir, "/different") || len(a.Messages) != 1 || len(a.Checkpoints()) != 0 {
		t.Fatal("diff matching or conversation state changed")
	}
}

type diffTerminalClient struct{ calls int }

func (c *diffTerminalClient) Generate(_ context.Context, messages []schema.Message, _ []tools.Tool) (*schema.LLMResponse, error) {
	c.calls++
	last := messages[len(messages)-1]
	if last.Role == "tool" {
		return &schema.LLMResponse{Content: "WRITE_DONE"}, nil
	}
	if last.Content == "noop" {
		return &schema.LLMResponse{Content: "NO_EDITS"}, nil
	}
	return &schema.LLMResponse{ToolCalls: []schema.ToolCall{{ID: "write", Type: "function", Function: schema.FunctionCall{Name: "write_file", Arguments: map[string]interface{}{"path": "file.txt", "content": fmt.Sprint(last.Content) + "\n"}}}}}, nil
}

func TestDiffTerminalHelper(t *testing.T) {
	if os.Getenv("DIEGOC_DIFF_TEST") != "1" {
		t.Skip("PTY helper")
	}
	dir := t.TempDir()
	diffTestFile(t, filepath.Join(dir, "file.txt"), "base\n")
	client := &diffTerminalClient{}
	a := agent.New(client, "system", 5, 100000, []tools.Tool{&tools.WriteTool{WorkspaceDir: dir}})
	a.PermissionCtx.Mode = permission.ModeBypass
	a.Logger = logger.NewInDirectory(t.TempDir())
	runInteractive(a, dir, a.Logger)
	fmt.Printf("MODEL_CALLS=%d\n", client.calls)
}
