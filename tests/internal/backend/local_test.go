package backend

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLocalBackendFilePrimitives(t *testing.T) {
	dir := t.TempDir()
	b := NewLocal(dir)
	ctx := context.Background()

	if b.Kind() != "local" || !b.HostPaths() || b.Workdir() != dir {
		t.Fatalf("unexpected identity: %s %v %q", b.Kind(), b.HostPaths(), b.Workdir())
	}

	path := b.JoinPath(dir, "nested", "file.txt")
	if err := b.WriteFile(ctx, path, []byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	data, err := b.ReadFile(ctx, path)
	if err != nil || string(data) != "hello" {
		t.Fatalf("read: %q %v", data, err)
	}
	exists, err := b.FileExists(ctx, path)
	if err != nil || !exists {
		t.Fatalf("exists: %v %v", exists, err)
	}
	if _, err := b.ReadFile(ctx, b.JoinPath(dir, "missing")); !os.IsNotExist(err) {
		t.Fatalf("missing read should be ErrNotExist: %v", err)
	}
}

func TestLocalBackendDerivedOperations(t *testing.T) {
	dir := t.TempDir()
	b := NewLocal(dir)
	ctx := context.Background()

	sub := b.JoinPath(dir, "sub")
	if err := b.MkdirAll(ctx, b.JoinPath(sub, "deeper")); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if isDir, err := b.IsDir(ctx, sub); err != nil || !isDir {
		t.Fatalf("is dir: %v %v", isDir, err)
	}
	if err := b.WriteFile(ctx, b.JoinPath(sub, "a.txt"), []byte("abc")); err != nil {
		t.Fatalf("write: %v", err)
	}

	names, err := b.ListDir(ctx, dir, false)
	if err != nil || len(names) != 1 || names[0] != "sub" {
		t.Fatalf("list: %v %v", names, err)
	}
	recursive, err := b.ListDir(ctx, dir, true)
	if err != nil || len(recursive) != 1 || !strings.HasSuffix(recursive[0], "a.txt") {
		t.Fatalf("recursive list: %v %v", recursive, err)
	}

	entry, err := b.Stat(ctx, b.JoinPath(sub, "a.txt"))
	if err != nil || entry == nil || entry.IsDir {
		t.Fatalf("stat: %+v %v", entry, err)
	}
	if entry.Size == nil || *entry.Size != 3 {
		t.Fatalf("size: %+v", entry.Size)
	}
	if entry.ModTime == nil {
		t.Fatal("missing mtime")
	}
	modTime, ok, err := b.StatMtime(ctx, b.JoinPath(sub, "a.txt"))
	if err != nil || !ok || modTime.IsZero() {
		t.Fatalf("stat mtime: %v %v %v", modTime, ok, err)
	}
	if missing, err := b.Stat(ctx, b.JoinPath(dir, "nope")); err != nil || missing != nil {
		t.Fatalf("missing stat: %+v %v", missing, err)
	}

	if err := b.DeletePath(ctx, sub); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if exists, err := b.FileExists(ctx, sub); err != nil || exists {
		t.Fatalf("delete left path: %v %v", exists, err)
	}
	if err := b.DeletePath(ctx, sub); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}

	expanded, err := b.ExpandUser(ctx, "~/x")
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if home, _ := os.UserHomeDir(); home != "" && expanded != filepath.Join(home, "x") {
		t.Fatalf("expand: %q", expanded)
	}
	if got, _ := b.ExpandUser(ctx, "~user/x"); got != "~user/x" {
		t.Fatalf("~user should be unchanged: %q", got)
	}
}

func TestLocalBackendExecExitAndTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell test")
	}
	b := NewLocal(t.TempDir())
	ctx := context.Background()

	res, err := b.Exec(ctx, []string{"sh", "-c", "echo out; echo err >&2; exit 7"}, ExecOptions{})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.ExitCode != 7 || strings.TrimSpace(string(res.Stdout)) != "out" || strings.TrimSpace(string(res.Stderr)) != "err" {
		t.Fatalf("unexpected result: %+v", res)
	}

	start := time.Now()
	res, err = b.Exec(ctx, []string{"sh", "-c", "sleep 2"}, ExecOptions{Timeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatalf("timeout exec: %v", err)
	}
	if !res.TimedOut || res.ExitCode != -1 {
		t.Fatalf("timeout not reported: %+v", res)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("timeout took too long: %v", elapsed)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := b.Exec(cancelled, []string{"sh", "-c", "echo nope"}, ExecOptions{}); err == nil {
		t.Fatal("cancelled context should fail")
	}
}

func TestLocalBackendBackgroundProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell test")
	}
	b := NewLocal(t.TempDir())
	proc, err := b.StartBackground([]string{"sh", "-c", "echo hello; sleep 0.05"}, "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case <-proc.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("background process did not finish")
	}
	if out := proc.Output(); !strings.Contains(out, "hello") {
		t.Fatalf("output: %q", out)
	}
	if err := proc.Kill(); err != nil {
		t.Fatalf("kill after exit: %v", err)
	}
}
