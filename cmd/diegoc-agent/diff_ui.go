package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"diegoc-agent/internal/agent"
)

const diffUsage = "Usage: /diff [positive-turn-ID|git]"

func readOnlyGit(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--no-pager", "-c", "color.ui=false", "-c", "core.fsmonitor=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	return cmd
}

// gitDiff never invokes a shell, pager, external diff driver or textconv.
// Exit 1 is normal for --no-index when differences were found.
func gitDiff(ctx context.Context, dir string, noIndex bool, args ...string) (string, error) {
	command := append([]string{"diff", "--no-color", "--no-ext-diff", "--no-textconv"}, args...)
	cmd := readOnlyGit(ctx, dir, command...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		var exit *exec.ExitError
		if noIndex && errors.As(err, &exit) && exit.ExitCode() == 1 {
			return string(output), nil
		}
		return "", fmt.Errorf("git diff: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return string(output), nil
}

func snapshotDiff(ctx context.Context, workspace string, turn agent.TurnChanges) (string, error) {
	if resolved, err := filepath.EvalSymlinks(workspace); err == nil {
		workspace = resolved
	}
	if turn.ID == 0 {
		return "No file checkpoints in the current session.\n", nil
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Turn %d: %s\nRecorded write_file/edit_file changes; later or external edits are excluded.\n", turn.ID, turn.Prompt)
	if len(turn.Files) == 0 {
		out.WriteString("No recorded file changes for this turn.\n")
	}
	for _, path := range turn.Unavailable {
		fmt.Fprintf(&out, "Final snapshot unavailable: %q\n", path)
	}
	if len(turn.Files) == 0 {
		return out.String(), nil
	}
	tmp, err := os.MkdirTemp("", "diegoc-diff-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	for _, change := range turn.Files {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		label := change.Path
		if rel, err := filepath.Rel(workspace, change.Path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			label = filepath.ToSlash(rel)
		}
		status := "modified"
		if !change.Before.Exists {
			status = "added"
		} else if !change.After.Exists {
			status = "deleted"
		}
		fmt.Fprintf(&out, "\nFile: %q (%s)\n", label, status)
		if change.Before.Exists && change.After.Exists && change.Before.Mode != change.After.Mode {
			fmt.Fprintf(&out, "Permissions: %04o → %04o\n", change.Before.Mode.Perm(), change.After.Mode.Perm())
		}
		if !utf8.Valid(change.Before.Data) || !utf8.Valid(change.After.Data) || bytes.IndexByte(change.Before.Data, 0) >= 0 || bytes.IndexByte(change.After.Data, 0) >= 0 {
			fmt.Fprintf(&out, "Binary file: %d → %d bytes\n", len(change.Before.Data), len(change.After.Data))
			continue
		}
		paths := []string{os.DevNull, os.DevNull}
		for i, version := range []agent.FileSnapshot{change.Before, change.After} {
			if !version.Exists {
				continue
			}
			name := []string{"before", "after"}[i]
			path := filepath.Join(tmp, name)
			if err := os.WriteFile(path, version.Data, 0600); err != nil {
				return "", err
			}
			if err := os.Chmod(path, 0600|version.Mode.Perm()&0111); err != nil {
				return "", err
			}
			paths[i] = name
		}
		patch, err := gitDiff(ctx, tmp, true, "--no-index", "--src-prefix=a/", "--dst-prefix=b/", "--", paths[0], paths[1])
		if err != nil {
			return "", err
		}
		// Rewrite only file headers, never hunk content that resembles a header.
		inHeader := true
		for _, line := range strings.SplitAfter(patch, "\n") {
			if strings.HasPrefix(line, "@@") {
				inHeader = false
			}
			if inHeader {
				switch {
				case strings.HasPrefix(line, "diff --git "):
					line = fmt.Sprintf("diff --git %q %q\n", "a/"+label, "b/"+label)
				case strings.HasPrefix(line, "--- ") && change.Before.Exists:
					line = fmt.Sprintf("--- %q\n", "a/"+label)
				case strings.HasPrefix(line, "+++ ") && change.After.Exists:
					line = fmt.Sprintf("+++ %q\n", "b/"+label)
				}
			}
			out.WriteString(line)
		}
	}
	return out.String(), nil
}

func workspaceGitDiff(ctx context.Context, workspace string) (string, error) {
	probe := readOnlyGit(ctx, workspace, "rev-parse", "--is-inside-work-tree")
	inside, err := probe.Output()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return "", fmt.Errorf("Git worktree unavailable for %q: %w", workspace, err)
	}
	if strings.TrimSpace(string(inside)) != "true" {
		return "", fmt.Errorf("%q is not a Git worktree", workspace)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Git changes under %q\n", workspace)
	for _, section := range []struct {
		title string
		args  []string
	}{{"Staged", []string{"--cached", "--relative", "--", "."}}, {"Unstaged", []string{"--relative", "--", "."}}} {
		patch, err := gitDiff(ctx, workspace, false, section.args...)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&out, "\n%s:\n", section.title)
		if patch == "" {
			out.WriteString("(none)\n")
		} else {
			out.WriteString(patch)
		}
	}
	cmd := readOnlyGit(ctx, workspace, "ls-files", "--others", "--exclude-standard", "-z", "--", ".")
	files, err := cmd.Output()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return "", fmt.Errorf("list untracked files: %w", err)
	}
	out.WriteString("\nUntracked files (paths only; contents are not included):\n")
	if len(files) == 0 {
		out.WriteString("(none)\n")
	} else {
		for _, name := range strings.Split(strings.TrimSuffix(string(files), "\x00"), "\x00") {
			fmt.Fprintf(&out, "  %q\n", name)
		}
	}
	return out.String(), nil
}

func handleDiffCommand(ag *agent.Agent, workspace, line string) bool {
	fields := strings.Fields(line)
	if len(fields) == 0 || strings.ToLower(fields[0]) != "/diff" {
		return false
	}
	id, gitView := 0, false
	if len(fields) > 2 {
		fmt.Println(diffUsage)
		return true
	}
	if len(fields) == 2 {
		gitView = strings.ToLower(fields[1]) == "git"
		if !gitView {
			var err error
			id, err = strconv.Atoi(fields[1])
			if err != nil || id <= 0 {
				fmt.Println(diffUsage)
				return true
			}
		}
	}
	fmt.Println("Collecting diff... (Esc to cancel)")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	escDone := make(chan struct{})
	go runWithEscCancel(ctx, cancel, escDone)
	var output string
	var err error
	if gitView {
		output, err = workspaceGitDiff(ctx, workspace)
	} else {
		var turn agent.TurnChanges
		turn, err = ag.FileChanges(id)
		if err == nil {
			output, err = snapshotDiff(ctx, workspace, turn)
		}
	}
	if err == nil {
		err = ctx.Err()
	}
	cancel()
	<-escDone
	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Println("Diff cancelled.")
		} else {
			fmt.Printf("Cannot show diff: %s\n", terminalReport(err.Error()))
		}
	} else {
		fmt.Print(terminalReport(output))
	}
	return true
}
