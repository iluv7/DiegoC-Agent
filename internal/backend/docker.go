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

// DockerRunner executes one `docker` CLI invocation. It is a seam for tests
// and for callers that need a non-default docker binary location.
type DockerRunner func(
	ctx context.Context,
	args []string,
	stdin []byte,
) (stdout []byte, stderr []byte, exitCode int, err error)

// RunDockerCLI runs the docker CLI from PATH.
func RunDockerCLI(ctx context.Context, args []string, stdin []byte) ([]byte, []byte, int, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
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
			// docker exec propagates the container command's exit code.
			return stdout.Bytes(), stderr.Bytes(), exitErr.ExitCode(), nil
		}
		return stdout.Bytes(), stderr.Bytes(), -1, err
	}
	return stdout.Bytes(), stderr.Bytes(), 0, nil
}

// DockerOptions configures a DockerBackend.
type DockerOptions struct {
	// Container is the running container to execute in.
	Container string
	// Workdir is the default working directory inside the container.
	Workdir string
	// HostPaths reports whether the container's workdir is a bind mount
	// of the host filesystem at the same path. When true, tools that
	// snapshot host files (checkpoint/rewind) remain correct.
	HostPaths bool
	// Runner overrides the docker CLI invocation; nil uses RunDockerCLI.
	Runner DockerRunner
}

// DockerBackend executes commands and file operations inside a container
// through the docker CLI. Path semantics are POSIX regardless of the host.
type DockerBackend struct {
	*Base
	container string
	runner    DockerRunner
	hostPaths bool
}

var _ Backend = (*DockerBackend)(nil)

// NewDocker returns a backend for an already-running container.
func NewDocker(opts DockerOptions) *DockerBackend {
	d := &DockerBackend{
		container: opts.Container,
		runner:    opts.Runner,
		hostPaths: opts.HostPaths,
	}
	if d.runner == nil {
		d.runner = RunDockerCLI
	}
	d.Base = NewBase(opts.Workdir, PosixPaths, d.exec)
	return d
}

// Kind identifies the container environment.
func (d *DockerBackend) Kind() string { return "docker" }

// Shell uses the POSIX shell, the one interpreter every supported image has.
func (d *DockerBackend) Shell() []string { return []string{"sh", "-c"} }

// HostPaths reports whether the container works on bind-mounted host files.
func (d *DockerBackend) HostPaths() bool { return d.hostPaths }

// ── primitives ─────────────────────────────────────────────────────────

func (d *DockerBackend) exec(ctx context.Context, command []string, opts ExecOptions) (*ExecResult, error) {
	return d.Exec(ctx, command, opts)
}

// Exec runs an argv list inside the container via docker exec.
func (d *DockerBackend) Exec(ctx context.Context, command []string, opts ExecOptions) (*ExecResult, error) {
	if d.container == "" {
		return nil, errors.New("backend: docker container is not configured")
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
		cwd = d.Workdir()
	}
	args := []string{"exec", "-i"}
	if cwd != "" {
		args = append(args, "-w", cwd)
	}
	args = append(args, d.container)
	args = append(args, command...)

	stdout, stderr, exitCode, err := d.runner(runCtx, args, nil)
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

// ReadFile reads a file through `docker exec cat`.
func (d *DockerBackend) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if d.container == "" {
		return nil, errors.New("backend: docker container is not configured")
	}
	stdout, stderr, exitCode, err := d.runner(
		ctx,
		[]string{"exec", d.container, "cat", path},
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

// WriteFile writes bytes through `docker exec -i sh -c ...`, which creates
// parent directories and streams the payload over stdin.
func (d *DockerBackend) WriteFile(ctx context.Context, path string, data []byte) error {
	if d.container == "" {
		return errors.New("backend: docker container is not configured")
	}
	const script = `mkdir -p "$(dirname "$1")" && cat > "$1"`
	_, stderr, exitCode, err := d.runner(
		ctx,
		[]string{"exec", "-i", d.container, "sh", "-c", script, "--", path},
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

// isMissingFile recognizes the message `cat` prints for a missing path.
func isMissingFile(stderr []byte) bool {
	return strings.Contains(strings.ToLower(string(stderr)), "no such file")
}
