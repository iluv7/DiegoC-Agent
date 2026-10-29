// Command test runs the tests stored separately under tests/ in their original
// Go packages, so they retain access to package-private functions and state.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if err := runTests(args); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintln(os.Stderr, "test runner:", err)
		return 1
	}
	return 0
}

func runTests(args []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	root, err := findRoot(cwd)
	if err != nil {
		return err
	}
	replacements, err := testOverlay(root)
	if err != nil {
		return err
	}
	data, err := json.Marshal(struct {
		Replace map[string]string
	}{Replace: replacements})
	if err != nil {
		return err
	}
	tempDir, err := os.MkdirTemp("", "diegoc-test-overlay-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempDir)
	overlayPath := filepath.Join(tempDir, "overlay.json")
	if err := os.WriteFile(overlayPath, data, 0600); err != nil {
		return err
	}
	if len(args) == 0 {
		args = []string{"./..."}
	}
	command := exec.Command("go", append([]string{"test", "-overlay", overlayPath}, args...)...)
	command.Dir = root
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	fmt.Fprintf(os.Stderr, "Running %d test files from tests/ via Go overlay\n", len(replacements))
	return command.Run()
}

func findRoot(dir string) (string, error) {
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			if info, err := os.Stat(filepath.Join(dir, "tests")); err == nil && info.IsDir() {
				return dir, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("cannot find repository root with go.mod and tests/")
		}
		dir = parent
	}
}

func testOverlay(root string) (map[string]string, error) {
	testsDir := filepath.Join(root, "tests")
	replacements := make(map[string]string)
	err := filepath.WalkDir(testsDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		relative, err := filepath.Rel(testsDir, path)
		if err != nil {
			return err
		}
		target := filepath.Join(root, relative)
		if info, err := os.Stat(filepath.Dir(target)); err != nil || !info.IsDir() {
			return fmt.Errorf("test %s has no matching source directory", relative)
		}
		if _, err := os.Lstat(target); !os.IsNotExist(err) {
			return fmt.Errorf("test %s would overwrite a source-tree file", relative)
		}
		replacements[target] = path
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(replacements) == 0 {
		return nil, fmt.Errorf("no *_test.go files found under tests/")
	}
	return replacements, nil
}
