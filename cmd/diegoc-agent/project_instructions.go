package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// buildSystemPrompt loads only the selected workspace's root AGENTS.md, once
// at startup. The returned path is empty when no nonempty rules were loaded.
func buildSystemPrompt(base, workspaceDir, skillsMetadata string) (prompt, loadedPath string, err error) {
	workspaceDir, err = filepath.Abs(workspaceDir)
	if err != nil {
		return "", "", fmt.Errorf("resolve workspace: %w", err)
	}
	prompt = strings.ReplaceAll(base, "{SKILLS_METADATA}", skillsMetadata)
	prompt += "\n\n## Current Workspace\nYou are working in: " + fmt.Sprintf("%q", workspaceDir) + "\n"
	path := filepath.Join(workspaceDir, "AGENTS.md")
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return prompt, "", nil
	} else if err != nil {
		return "", "", fmt.Errorf("inspect project instructions %q: %w", path, err)
	}
	// Follow symlinks to ordinary files, but reject directories and special files
	// before reading, so a misplaced AGENTS.md cannot block startup on a FIFO.
	info, err := os.Stat(path)
	if err != nil {
		return "", "", fmt.Errorf("inspect project instructions %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", "", fmt.Errorf("project instructions %q must be a regular file", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("read project instructions %q: %w", path, err)
	}
	if !utf8.Valid(data) {
		return "", "", fmt.Errorf("project instructions %q must be UTF-8 text", path)
	}
	content := strings.TrimPrefix(string(data), "\uFEFF")
	if strings.TrimSpace(content) == "" {
		return prompt, "", nil
	}
	// Insert project text after template expansion so placeholders in AGENTS.md
	// remain literal. Explicit user requests take precedence over project defaults.
	prompt += "\n## Project Instructions (AGENTS.md)\nSource: " + fmt.Sprintf("%q", path) + "\n"
	prompt += "These project instructions apply to the current workspace and its descendants. Follow them when working on this project. If they conflict with an explicit user request, follow the user's request. They do not grant tool permissions or change the current execution mode.\n\n<project_instructions>\n" + content + "\n</project_instructions>\n"
	return prompt, path, nil
}
