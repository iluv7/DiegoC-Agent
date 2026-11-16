package backend

import (
	"context"
	"testing"
	"time"
)

func TestBaseDerivesFilesystemOpsFromExec(t *testing.T) {
	var commands [][]string
	base := NewBase("/work", PosixPaths, func(
		_ context.Context,
		command []string,
		_ ExecOptions,
	) (*ExecResult, error) {
		commands = append(commands, command)
		return &ExecResult{
			Stdout: []byte(
				"f\t12\t1700000000.5\tfile.txt\x00" +
					"d\t4096\t1700000001\tdir\x00",
			),
		}, nil
	})
	ctx := context.Background()

	entry, err := base.Stat(ctx, "/work/file.txt")
	if err != nil || entry == nil {
		t.Fatalf("stat: %+v %v", entry, err)
	}
	if entry.Name != "file.txt" || entry.IsDir {
		t.Fatalf("unexpected entry: %+v", entry)
	}
	if entry.Size == nil || *entry.Size != 12 {
		t.Fatalf("unexpected size: %+v", entry.Size)
	}
	if entry.ModTime == nil || entry.ModTime.Unix() != 1700000000 {
		t.Fatalf("unexpected mtime: %+v", entry.ModTime)
	}

	exists, err := base.FileExists(ctx, "/work/file.txt")
	if err != nil || !exists {
		t.Fatalf("file exists: %v %v", exists, err)
	}
	if len(commands) != 2 || commands[0][0] != "find" || commands[1][0] != "test" {
		t.Fatalf("unexpected commands: %v", commands)
	}
}

func TestBaseStatSkipsUnresolvableSymlinks(t *testing.T) {
	if got := parseFindEntries([]byte("N\t0\t0\tbroken\x00"), true); len(got) != 0 {
		t.Fatalf("broken symlink should be skipped: %+v", got)
	}
	entries := parseFindEntries([]byte("N\t7\t1700000000\tbroken\x00"), false)
	if len(entries) != 1 || entries[0].Name != "broken" {
		t.Fatalf("unexpected entries: %+v", entries)
	}
	if entries[0].Size != nil || entries[0].ModTime != nil {
		t.Fatalf("unresolvable entry kept metadata: %+v", entries[0])
	}
}

func TestBasePathHelpersUseEnvironmentSemantics(t *testing.T) {
	base := NewBase("/work", PosixPaths, nil)
	if got := base.JoinPath("/a", "b", "c"); got != "/a/b/c" {
		t.Fatalf("join: %q", got)
	}
	if got := base.Abs("b", "/a"); got != "/a/b" {
		t.Fatalf("abs relative: %q", got)
	}
	if got := base.Abs("/b/../c", "/a"); got != "/c" {
		t.Fatalf("abs absolute: %q", got)
	}
	if !base.IsAbs("/a") || base.IsAbs("a") {
		t.Fatal("isabs")
	}
}

func TestExecResultOK(t *testing.T) {
	if !(&ExecResult{ExitCode: 0}).OK() {
		t.Fatal("zero exit should be ok")
	}
	if (&ExecResult{ExitCode: 1}).OK() || (&ExecResult{ExitCode: 0, TimedOut: true}).OK() {
		t.Fatal("failed or timed-out result reported ok")
	}
	var nilResult *ExecResult
	if nilResult.OK() {
		t.Fatal("nil result reported ok")
	}
}

func TestParseFindEntriesHandlesTabsInName(t *testing.T) {
	entries := parseFindEntries([]byte("f\t3\t1700000000\ta\tb.txt\x00"), false)
	if len(entries) != 1 || entries[0].Name != "a\tb.txt" {
		t.Fatalf("tab-containing name: %+v", entries)
	}
	if entries[0].ModTime == nil || *entries[0].ModTime != time.Unix(1700000000, 0) {
		t.Fatalf("mtime: %+v", entries[0].ModTime)
	}
}
