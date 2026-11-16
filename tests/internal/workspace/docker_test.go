package workspace

import (
	"context"
	"runtime"
	"strings"
	"testing"
)

func TestDockerWorkspaceProvisionsReusesAndRemoves(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("docker workspace is not supported on Windows")
	}
	dir := t.TempDir()
	var calls []string
	created := false
	runner := func(
		_ context.Context,
		args []string,
		_ []byte,
	) ([]byte, []byte, int, error) {
		calls = append(calls, strings.Join(args, " "))
		switch args[0] {
		case "inspect":
			if !created {
				return nil, []byte("Error: No such object"), 1, nil
			}
			return []byte("true\n"), nil, 0, nil
		case "run":
			created = true
			return []byte("container-id\n"), nil, 0, nil
		}
		return nil, nil, 0, nil
	}

	ws := NewDocker(DockerOptions{
		Container:     "test-box",
		Image:         "debian:bookworm-slim",
		Workdir:       dir,
		RemoveOnClose: true,
		Runner:        runner,
	})
	ctx := context.Background()
	if err := ws.Initialize(ctx); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	be, err := ws.Backend()
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	if be.Kind() != "docker" || !be.HostPaths() || be.Workdir() != dir {
		t.Fatalf("unexpected backend: %s %v %q", be.Kind(), be.HostPaths(), be.Workdir())
	}
	if !containsCall(calls, "run -d --name test-box -v "+dir+":"+dir) {
		t.Fatalf("container not created with bind mount: %v", calls)
	}
	if !containsCall(calls, "exec -i -w "+dir+" test-box mkdir -p "+dir+"/data") {
		t.Fatalf("layout not created in container: %v", calls)
	}

	// A second Initialize is a no-op; the container is not recreated.
	before := len(calls)
	if err := ws.Initialize(ctx); err != nil {
		t.Fatalf("second initialize: %v", err)
	}
	if len(calls) != before {
		t.Fatalf("second initialize touched docker: %v", calls[before:])
	}

	if err := ws.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !containsCall(calls, "rm -f test-box") {
		t.Fatalf("container not removed: %v", calls)
	}

	// Re-initializing after close reattaches to the running container.
	if err := ws.Initialize(ctx); err != nil {
		t.Fatalf("re-initialize: %v", err)
	}
	if countCalls(calls, "run -d") != 1 {
		t.Fatalf("container recreated instead of reused: %v", calls)
	}
}

func TestDockerWorkspaceKeepAliveLeavesContainer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("docker workspace is not supported on Windows")
	}
	var calls []string
	runner := func(
		_ context.Context,
		args []string,
		_ []byte,
	) ([]byte, []byte, int, error) {
		calls = append(calls, strings.Join(args, " "))
		if args[0] == "inspect" {
			return []byte("true\n"), nil, 0, nil
		}
		return nil, nil, 0, nil
	}
	ws := NewDocker(DockerOptions{
		Container: "keep-box",
		Workdir:   t.TempDir(),
		Runner:    runner,
	})
	ctx := context.Background()
	if err := ws.Initialize(ctx); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if err := ws.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
	if countCalls(calls, "rm ") != 0 {
		t.Fatalf("container removed despite keep-alive: %v", calls)
	}
}

func containsCall(calls []string, prefix string) bool {
	for _, call := range calls {
		if strings.HasPrefix(call, prefix) {
			return true
		}
	}
	return false
}

func countCalls(calls []string, prefix string) int {
	count := 0
	for _, call := range calls {
		if strings.HasPrefix(call, prefix) {
			count++
		}
	}
	return count
}
