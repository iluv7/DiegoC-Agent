package workspace

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeKube struct {
	mu          sync.Mutex
	exists      bool
	phase       string
	pendingGets int
	calls       []string
	applied     [][]byte
}

func (f *fakeKube) run(
	_ context.Context,
	args []string,
	stdin []byte,
) ([]byte, []byte, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join(args, " "))
	command := ""
	for _, arg := range args {
		switch arg {
		case "get", "apply", "delete":
			command = arg
		}
		if command != "" {
			break
		}
	}
	switch command {
	case "get":
		if !f.exists {
			return nil, []byte(`Error from server (NotFound): pods "test-pod" not found`), 1, nil
		}
		if f.phase == "Pending" {
			f.pendingGets++
			if f.pendingGets > 1 {
				f.phase = "Running"
			}
		}
		return []byte(f.phase), nil, 0, nil
	case "apply":
		f.applied = append(f.applied, append([]byte(nil), stdin...))
		f.exists = true
		f.phase = "Pending"
		f.pendingGets = 0
		return nil, nil, 0, nil
	case "delete":
		f.exists = false
		f.phase = ""
		return nil, nil, 0, nil
	}
	return nil, nil, 0, nil
}

func newTestK8s(fake *fakeKube, opts K8sOptions) *K8sWorkspace {
	opts.Namespace = "ns"
	opts.Pod = "test-pod"
	opts.Container = "agent"
	opts.Workdir = "/workspace"
	opts.Runner = fake.run
	opts.StartTimeout = 2 * time.Second
	ws := NewK8s(opts)
	ws.pollInterval = time.Millisecond
	return ws
}

func TestK8sWorkspaceCreatesPodAndBindsBackend(t *testing.T) {
	fake := &fakeKube{}
	ws := newTestK8s(fake, K8sOptions{})
	ctx := context.Background()

	if err := ws.Initialize(ctx); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	be, err := ws.Backend()
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	if be.Kind() != "k8s" || be.HostPaths() || be.Workdir() != "/workspace" {
		t.Fatalf("unexpected backend: %s %v %q", be.Kind(), be.HostPaths(), be.Workdir())
	}
	for _, want := range []string{"test-pod", "ns", "agent", "/workspace"} {
		if !strings.Contains(ws.Instructions(), want) {
			t.Fatalf("instructions missing %q: %q", want, ws.Instructions())
		}
	}

	if len(fake.applied) != 1 {
		t.Fatalf("pod applied %d times", len(fake.applied))
	}
	manifest := string(fake.applied[0])
	for _, want := range []string{
		"kind: Pod",
		"name: test-pod",
		"namespace: ns",
		"image: debian:bookworm-slim",
		"workingDir: /workspace",
		"emptyDir",
	} {
		if !strings.Contains(manifest, want) {
			t.Fatalf("manifest missing %q:\n%s", want, manifest)
		}
	}
	if !containsCall(fake.calls, "exec -n ns test-pod -c agent -- sh -c") ||
		!strings.Contains(strings.Join(fake.calls, "\n"), "mkdir -p /workspace/data") {
		t.Fatalf("layout not created in pod: %v", fake.calls)
	}

	list, err := ws.ListTools(ToolOptions{FileTools: true, Bash: true})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(list) == 0 {
		t.Fatal("no tools bound")
	}

	if err := ws.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
	if countCalls(fake.calls, "delete pod") != 0 {
		t.Fatalf("pod deleted despite keep-alive: %v", fake.calls)
	}
}

func TestK8sWorkspaceReusesRunningPod(t *testing.T) {
	fake := &fakeKube{exists: true, phase: "Running"}
	ws := newTestK8s(fake, K8sOptions{})
	if err := ws.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if len(fake.applied) != 0 {
		t.Fatalf("running pod was recreated: %v", fake.calls)
	}
}

func TestK8sWorkspaceReplacesTerminalPod(t *testing.T) {
	fake := &fakeKube{exists: true, phase: "Failed"}
	ws := newTestK8s(fake, K8sOptions{})
	if err := ws.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if !containsCall(fake.calls, "delete pod test-pod") {
		t.Fatalf("failed pod not deleted: %v", fake.calls)
	}
	if len(fake.applied) != 1 {
		t.Fatalf("replacement pod not applied: %v", fake.calls)
	}
}

func TestK8sWorkspacePVCRemoveOnClose(t *testing.T) {
	fake := &fakeKube{}
	ws := newTestK8s(fake, K8sOptions{
		PersistentVolumeClaim: "my-pvc",
		RemoveOnClose:         true,
	})
	ctx := context.Background()
	if err := ws.Initialize(ctx); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	manifest := string(fake.applied[0])
	if !strings.Contains(manifest, "persistentVolumeClaim") ||
		!strings.Contains(manifest, "claimName: my-pvc") {
		t.Fatalf("pvc not mounted:\n%s", manifest)
	}
	if err := ws.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !containsCall(fake.calls, "delete pod test-pod") {
		t.Fatalf("pod not deleted: %v", fake.calls)
	}
}

func TestK8sWorkspacePropagatesKubeconfigToCalls(t *testing.T) {
	fake := &fakeKube{}
	ws := NewK8s(K8sOptions{
		Namespace:    "ns",
		Pod:          "test-pod",
		Container:    "agent",
		Workdir:      "/workspace",
		Kubeconfig:   "/tmp/kubeconfig",
		Context:      "test-context",
		Runner:       fake.run,
		StartTimeout: 2 * time.Second,
	})
	ws.pollInterval = time.Millisecond
	if err := ws.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	prefix := "--kubeconfig /tmp/kubeconfig --context test-context"
	if !containsCall(fake.calls, prefix+" get pod") {
		t.Fatalf("workspace calls missing kubeconfig/context: %v", fake.calls)
	}
	if !containsCall(fake.calls, prefix+" exec") {
		t.Fatalf("backend calls missing kubeconfig/context: %v", fake.calls)
	}
}

func TestK8sWorkspaceRejectsInvalidNames(t *testing.T) {
	fake := &fakeKube{}
	ws := NewK8s(K8sOptions{
		Pod:    "Bad_Name",
		Runner: fake.run,
	})
	if err := ws.Initialize(context.Background()); err == nil {
		t.Fatal("invalid pod name accepted")
	}
	if len(fake.calls) != 0 {
		t.Fatalf("kubectl called before validation: %v", fake.calls)
	}
}
