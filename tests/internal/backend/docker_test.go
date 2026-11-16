package backend

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"
)

func TestDockerBackendExecBuildsCLIArgs(t *testing.T) {
	var gotArgs []string
	b := NewDocker(DockerOptions{
		Container: "box",
		Workdir:   "/work",
		Runner: func(
			_ context.Context,
			args []string,
			_ []byte,
		) ([]byte, []byte, int, error) {
			gotArgs = append([]string(nil), args...)
			return []byte("ok"), []byte("warn"), 3, nil
		},
	})

	res, err := b.Exec(context.Background(), []string{"echo", "hi"}, ExecOptions{})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	want := []string{"exec", "-i", "-w", "/work", "box", "echo", "hi"}
	if strings.Join(gotArgs, " ") != strings.Join(want, " ") {
		t.Fatalf("args: %v", gotArgs)
	}
	if res.ExitCode != 3 || string(res.Stdout) != "ok" || string(res.Stderr) != "warn" {
		t.Fatalf("result: %+v", res)
	}
}

func TestDockerBackendExecTimeout(t *testing.T) {
	b := NewDocker(DockerOptions{
		Container: "box",
		Workdir:   "/work",
		Runner: func(
			ctx context.Context,
			_ []string,
			_ []byte,
		) ([]byte, []byte, int, error) {
			<-ctx.Done()
			return nil, nil, -1, ctx.Err()
		},
	})
	res, err := b.Exec(
		context.Background(),
		[]string{"sleep", "10"},
		ExecOptions{Timeout: 50 * time.Millisecond},
	)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !res.TimedOut || res.ExitCode != -1 {
		t.Fatalf("timeout not reported: %+v", res)
	}
}

func TestDockerBackendReadFileMapsMissingToNotExist(t *testing.T) {
	b := NewDocker(DockerOptions{
		Container: "box",
		Runner: func(
			_ context.Context,
			args []string,
			_ []byte,
		) ([]byte, []byte, int, error) {
			if len(args) != 4 || args[0] != "exec" || args[2] != "cat" {
				t.Fatalf("unexpected read args: %v", args)
			}
			return nil, []byte("cat: /work/x: No such file or directory"), 1, nil
		},
	})
	if _, err := b.ReadFile(context.Background(), "/work/x"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing file should be ErrNotExist: %v", err)
	}
}

func TestDockerBackendWriteFileStreamsStdin(t *testing.T) {
	var gotStdin []byte
	var gotArgs []string
	b := NewDocker(DockerOptions{
		Container: "box",
		Runner: func(
			_ context.Context,
			args []string,
			stdin []byte,
		) ([]byte, []byte, int, error) {
			gotArgs = append([]string(nil), args...)
			gotStdin = append([]byte(nil), stdin...)
			return nil, nil, 0, nil
		},
	})
	if err := b.WriteFile(context.Background(), "/work/a.txt", []byte("payload")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if string(gotStdin) != "payload" {
		t.Fatalf("stdin: %q", gotStdin)
	}
	joined := strings.Join(gotArgs, " ")
	if !strings.Contains(joined, "exec -i box sh -c") || gotArgs[len(gotArgs)-1] != "/work/a.txt" {
		t.Fatalf("args: %v", gotArgs)
	}
}

func TestDockerBackendRequiresContainer(t *testing.T) {
	b := NewDocker(DockerOptions{})
	if _, err := b.Exec(context.Background(), []string{"true"}, ExecOptions{}); err == nil {
		t.Fatal("missing container accepted")
	}
	if _, err := b.ReadFile(context.Background(), "/x"); err == nil {
		t.Fatal("missing container accepted")
	}
	if err := b.WriteFile(context.Background(), "/x", nil); err == nil {
		t.Fatal("missing container accepted")
	}
}
