package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"diegoc-agent/internal/loop"
)

const defaultLoopPrompt = "Continue unfinished work already requested in this conversation. Check the current branch's pull request for review comments, failed CI, or merge conflicts if relevant. Work within the user's authorized scope and existing permissions. If nothing needs attention, report briefly; do not invent new tasks. Cancel this loop with loop_cancel once the requested work is complete."

func loadLoopPrompt(workspace, home string) (string, error) {
	paths := []string{filepath.Join(workspace, ".diegoc-agent", "loop.md")}
	if home != "" {
		paths = append(paths, filepath.Join(home, ".diegoc-agent", "loop.md"))
	}
	for _, path := range paths {
		f, err := os.Open(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("read loop prompt %s: %w", path, err)
		}
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			f.Close()
			return "", fmt.Errorf("loop prompt must be a readable regular file: %s", path)
		}
		data, err := io.ReadAll(io.LimitReader(f, loop.MaxPromptBytes+1))
		f.Close()
		if err != nil {
			return "", fmt.Errorf("read loop prompt %s: %w", path, err)
		}
		if !utf8.Valid(data) || len(data) > loop.MaxPromptBytes || strings.TrimSpace(string(data)) == "" {
			return "", fmt.Errorf("loop prompt must be nonempty UTF-8 text, at most %d bytes: %s", loop.MaxPromptBytes, path)
		}
		return strings.TrimSpace(string(data)), nil
	}
	return defaultLoopPrompt, nil
}

func handleLoopCommand(scheduler *loop.Scheduler, session, workspace, line string) bool {
	parts := strings.Fields(line)
	if len(parts) == 0 || !strings.EqualFold(parts[0], "/loop") {
		return false
	}
	args := strings.TrimSpace(line[len(parts[0]):])
	if len(parts) > 1 && strings.EqualFold(parts[1], "list") {
		if len(parts) != 2 {
			fmt.Println("Usage: /loop list")
			return true
		}
		tasks := scheduler.List(session)
		if len(tasks) == 0 {
			fmt.Printf("No loops in [%s].\n", session)
		}
		for _, task := range tasks {
			fmt.Printf("[%s] every %s · next %s · expires %s · runs %d\n  %s\n", task.ID, task.Interval, task.NextRun.Local().Format("2006-01-02 15:04:05"), task.ExpiresAt.Local().Format("2006-01-02 15:04:05"), task.Runs, selectorText(task.Prompt, 160))
		}
		return true
	}
	if len(parts) > 1 && strings.EqualFold(parts[1], "cancel") {
		if len(parts) != 3 {
			fmt.Println("Usage: /loop cancel loop-ID|all")
			return true
		}
		count := scheduler.Cancel(session, strings.ToLower(parts[2]))
		if count == 0 && !strings.EqualFold(parts[2], "all") {
			fmt.Printf("Loop %q not found in [%s].\n", selectorText(parts[2], 80), session)
		} else {
			fmt.Printf("Cancelled %d loop(s) in [%s].\n", count, session)
		}
		return true
	}
	interval := loop.DefaultInterval
	if len(parts) > 1 {
		token := parts[1]
		// A numeric first token is an interval, so malformed durations never
		// silently create a repeating task with the wrong cadence.
		if token[0] >= '0' && token[0] <= '9' || token[0] == '-' || token[0] == '+' {
			var err error
			interval, err = loop.ParseInterval(token)
			if err != nil {
				fmt.Printf("Loop: %v\nUsage: /loop [5m|2h|1d] [prompt], /loop list, /loop cancel loop-ID|all\n", err)
				return true
			}
			args = strings.TrimSpace(args[len(token):])
		}
	}
	if args == "" {
		home, _ := os.UserHomeDir()
		var err error
		args, err = loadLoopPrompt(workspace, home)
		if err != nil {
			fmt.Printf("Loop: %v\n", err)
			return true
		}
	}
	task, err := scheduler.Create(session, args, interval)
	if err != nil {
		fmt.Printf("Loop: %v\n", err)
		return true
	}
	fmt.Printf("Created [%s] in [%s]: every %s; first run %s; expires %s.\n  %s\nUse /loop cancel %s to stop. Runs only while this session is active and idle.\n", task.ID, session, task.Interval, task.NextRun.Local().Format("2006-01-02 15:04:05"), task.ExpiresAt.Local().Format("2006-01-02 15:04:05"), selectorText(task.Prompt, 160), task.ID)
	return true
}
