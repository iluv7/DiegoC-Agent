package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"strings"
)

// KubectlRunner executes one kubectl CLI invocation. It is a seam for tests
// and for callers that need a non-default kubectl binary location.
type KubectlRunner func(
	ctx context.Context,
	args []string,
	stdin []byte,
) (stdout []byte, stderr []byte, exitCode int, err error)

// RunKubectlCLI runs kubectl from PATH.
func RunKubectlCLI(ctx context.Context, args []string, stdin []byte) ([]byte, []byte, int, error) {
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// kubectl exec propagates the remote command's exit code.
			return stdout.Bytes(), stderr.Bytes(), exitErr.ExitCode(), nil
		}
		return stdout.Bytes(), stderr.Bytes(), -1, err
	}
	return stdout.Bytes(), stderr.Bytes(), 0, nil
}

// K8sOptions configures a K8sBackend.
type K8sOptions struct {
	// Namespace and Pod locate the running pod. Namespace defaults to
	// "default".
	Namespace string
	Pod       string
	// Container selects a container in multi-container pods; empty uses
	// kubectl's default (the pod's first container).
	Container string
	// Workdir is the default working directory inside the container.
	Workdir string
	// Kubeconfig and Context override kubectl's default kubeconfig and
	// current context.
	Kubeconfig string
	Context    string
	// Runner overrides the kubectl CLI invocation; nil uses RunKubectlCLI.
	Runner KubectlRunner
}

// K8sBackend executes commands and file operations inside a Kubernetes pod
// through the kubectl CLI. Path semantics are POSIX.
type K8sBackend struct {
	*Base
	namespace string
	pod       string
	container string
	prefix    []string
	runner    KubectlRunner
}

var _ Backend = (*K8sBackend)(nil)

// NewK8s returns a backend for an already-running pod.
func NewK8s(opts K8sOptions) *K8sBackend {
	namespace := opts.Namespace
	if namespace == "" {
		namespace = "default"
	}
	k := &K8sBackend{
		namespace: namespace,
		pod:       opts.Pod,
		container: opts.Container,
		runner:    opts.Runner,
	}
	if k.runner == nil {
		k.runner = RunKubectlCLI
	}
	if opts.Kubeconfig != "" {
		k.prefix = append(k.prefix, "--kubeconfig", opts.Kubeconfig)
	}
	if opts.Context != "" {
		k.prefix = append(k.prefix, "--context", opts.Context)
	}
	k.Base = NewBase(opts.Workdir, PosixPaths, k.exec)
	return k
}

// Kind identifies the pod environment.
func (k *K8sBackend) Kind() string { return "k8s" }

// ── primitives ─────────────────────────────────────────────────────────

func (k *K8sBackend) exec(ctx context.Context, command []string, opts ExecOptions) (*ExecResult, error) {
	return k.Exec(ctx, command, opts)
}

// Exec runs an argv list inside the pod via kubectl exec. When a working
// directory is set, the command is wrapped in a POSIX shell that chdirs
// first, because kubectl exec has no working-directory flag.
func (k *K8sBackend) Exec(ctx context.Context, command []string, opts ExecOptions) (*ExecResult, error) {
	if k.pod == "" {
		return nil, errors.New("backend: k8s pod is not configured")
	}
	if len(command) == 0 {
		return nil, errors.New("backend: empty command")
	}

	runCtx := ctx
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	cwd := opts.Cwd
	if cwd == "" {
		cwd = k.Workdir()
	}
	remote := command
	if cwd != "" {
		const script = `cd "$1" || exit 1; shift; exec "$@"`
		remote = append(
			[]string{"sh", "-c", script, "--", cwd},
			command...,
		)
	}

	stdout, stderr, exitCode, err := k.runner(runCtx, k.execArgs(false, remote...), nil)
	res := &ExecResult{ExitCode: exitCode, Stdout: stdout, Stderr: stderr}
	if err != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return &ExecResult{ExitCode: -1, TimedOut: true}, nil
		}
		return nil, err
	}
	if runCtx.Err() != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			res.ExitCode = -1
			res.TimedOut = true
			return res, nil
		}
		return nil, runCtx.Err()
	}
	return res, nil
}

// ReadFile reads a file through `kubectl exec -- cat`.
func (k *K8sBackend) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if k.pod == "" {
		return nil, errors.New("backend: k8s pod is not configured")
	}
	stdout, stderr, exitCode, err := k.runner(
		ctx,
		k.execArgs(false, "cat", path),
		nil,
	)
	if err != nil {
		return nil, err
	}
	if exitCode != 0 {
		if isMissingFile(stderr) {
			return nil, fmt.Errorf("%w: %s", fs.ErrNotExist, path)
		}
		return nil, fmt.Errorf("read %s: %s", path, strings.TrimSpace(string(stderr)))
	}
	return stdout, nil
}

// WriteFile writes bytes through `kubectl exec -i sh -c ...`, which creates
// parent directories and streams the payload over stdin.
func (k *K8sBackend) WriteFile(ctx context.Context, path string, data []byte) error {
	if k.pod == "" {
		return errors.New("backend: k8s pod is not configured")
	}
	const script = `mkdir -p "$(dirname "$1")" && cat > "$1"`
	_, stderr, exitCode, err := k.runner(
		ctx,
		k.execArgs(true, "sh", "-c", script, "--", path),
		data,
	)
	if err != nil {
		return err
	}
	if exitCode != 0 {
		return fmt.Errorf("write %s: %s", path, strings.TrimSpace(string(stderr)))
	}
	return nil
}

// execArgs assembles the kubectl invocation, including the global
// kubeconfig/context flags and the per-command namespace/container flags.
func (k *K8sBackend) execArgs(stdin bool, command ...string) []string {
	args := make([]string, 0, len(k.prefix)+8+len(command))
	args = append(args, k.prefix...)
	args = append(args, "exec")
	if stdin {
		args = append(args, "-i")
	}
	args = append(args, "-n", k.namespace, k.pod)
	if k.container != "" {
		args = append(args, "-c", k.container)
	}
	args = append(args, "--")
	args = append(args, command...)
	return args
}
