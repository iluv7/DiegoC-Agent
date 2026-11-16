package backend

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"
)

func TestK8sBackendExecBuildsArgsAndWrapsCwd(t *testing.T) {
	var gotArgs []string
	b := NewK8s(K8sOptions{
		Namespace:  "ns",
		Pod:        "pod",
		Container:  "agent",
		Workdir:    "/work",
		Kubeconfig: "/tmp/kubeconfig",
		Context:    "ctx",
		Runner: func(
			_ context.Context,
			args []string,
			_ []byte,
		) ([]byte, []byte, int, error) {
			gotArgs = append([]string(nil), args...)
			return []byte("ok"), nil, 0, nil
		},
	})

	res, err := b.Exec(context.Background(), []string{"echo", "hi"}, ExecOptions{})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	want := []string{
		"--kubeconfig", "/tmp/kubeconfig", "--context", "ctx",
		"exec", "-n", "ns", "pod", "-c", "agent", "--",
		"sh", "-c", `cd "$1" || exit 1; shift; exec "$@"`, "--", "/work",
		"echo", "hi",
	}
	if strings.Join(gotArgs, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args:\n got %v\nwant %v", gotArgs, want)
	}
	if res.ExitCode != 0 || string(res.Stdout) != "ok" {
		t.Fatalf("result: %+v", res)
	}
}

func TestK8sBackendExecWithoutWorkdirRunsDirectly(t *testing.T) {
	var gotArgs []string
	b := NewK8s(K8sOptions{
		Pod: "pod",
		Runner: func(
			_ context.Context,
			args []string,
			_ []byte,
		) ([]byte, []byte, int, error) {
			gotArgs = append([]string(nil), args...)
			return nil, nil, 0, nil
		},
	})
	if _, err := b.Exec(context.Background(), []string{"true"}, ExecOptions{}); err != nil {
		t.Fatalf("exec: %v", err)
	}
	want := []string{"exec", "-n", "default", "pod", "--", "true"}
	if strings.Join(gotArgs, " ") != strings.Join(want, " ") {
		t.Fatalf("args: %v", gotArgs)
	}
}

func TestK8sBackendExecTimeout(t *testing.T) {
	b := NewK8s(K8sOptions{
		Pod: "pod",
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

func TestK8sBackendReadFileMapsMissingToNotExist(t *testing.T) {
	b := NewK8s(K8sOptions{
		Pod: "pod",
		Runner: func(
			_ context.Context,
			args []string,
			_ []byte,
		) ([]byte, []byte, int, error) {
			if args[len(args)-2] != "cat" {
				t.Fatalf("unexpected read args: %v", args)
			}
			return nil, []byte("cat: /work/x: No such file or directory"), 1, nil
		},
	})
	if _, err := b.ReadFile(context.Background(), "/work/x"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing file should be ErrNotExist: %v", err)
	}
}

func TestK8sBackendWriteFileStreamsStdin(t *testing.T) {
	var gotArgs []string
	var gotStdin []byte
	b := NewK8s(K8sOptions{
		Pod: "pod",
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
	if gotArgs[1] != "-i" || gotArgs[len(gotArgs)-1] != "/work/a.txt" {
		t.Fatalf("args: %v", gotArgs)
	}
}

func TestK8sBackendRequiresPod(t *testing.T) {
	b := NewK8s(K8sOptions{})
	if _, err := b.Exec(context.Background(), []string{"true"}, ExecOptions{}); err == nil {
		t.Fatal("missing pod accepted")
	}
	if _, err := b.ReadFile(context.Background(), "/x"); err == nil {
		t.Fatal("missing pod accepted")
	}
	if err := b.WriteFile(context.Background(), "/x", nil); err == nil {
		t.Fatal("missing pod accepted")
	}
}
