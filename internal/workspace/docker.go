package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"

	"diegoc-agent/internal/backend"
)

const dockerInstructions = `## Execution Environment
File and shell tools execute inside Docker container {container}. The host workspace {workdir} is bind-mounted at the same path, so file paths are identical inside and outside the container.`

// DockerOptions configures a container-backed workspace.
type DockerOptions struct {
	// ID is a stable identifier; defaults to the container name.
	ID string
	// Container is the docker container name; created on first use and
	// reused on later runs.
	Container string
	// Image is used only when the container has to be created.
	Image string
	// Workdir is the host workspace path, mounted at the same path in the
	// container and used as the default working directory.
	Workdir string
	// RemoveOnClose deletes the container when the workspace closes. The
	// default keeps it so files and installed packages survive restarts.
	RemoveOnClose bool
	// Runner overrides the docker CLI invocation; nil uses the real CLI.
	Runner backend.DockerRunner
}

// DockerWorkspace runs tools inside a container while sharing the host
// workspace through a bind mount.
type DockerWorkspace struct {
	*Base
	container     string
	image         string
	removeOnClose bool
	runner        backend.DockerRunner
}

var (
	_ Workspace       = (*DockerWorkspace)(nil)
	_ BackendProvider = (*DockerWorkspace)(nil)
)

// NewDocker returns a container-backed workspace. The environment is not
// touched until Initialize.
func NewDocker(opts DockerOptions) *DockerWorkspace {
	if opts.Container == "" {
		opts.Container = "diegoc-agent-workspace"
	}
	if opts.Image == "" {
		opts.Image = "debian:bookworm-slim"
	}
	if opts.ID == "" {
		opts.ID = opts.Container
	}
	if opts.Runner == nil {
		opts.Runner = backend.RunDockerCLI
	}
	w := &DockerWorkspace{
		container:     opts.Container,
		image:         opts.Image,
		removeOnClose: opts.RemoveOnClose,
		runner:        opts.Runner,
	}
	instructions := strings.ReplaceAll(dockerInstructions, "{container}", opts.Container)
	w.Base = NewBase(
		opts.ID,
		opts.Workdir,
		backend.PosixPaths,
		w,
		formatInstructions(instructions, opts.Workdir),
	)
	return w
}

// Provision reuses, starts or creates the container, then binds a
// DockerBackend and installs the canonical layout.
func (w *DockerWorkspace) Provision(ctx context.Context) (backend.Backend, error) {
	if runtime.GOOS == "windows" {
		return nil, errors.New(
			"docker workspace is not supported on Windows yet: " +
				"the workspace relies on a same-path bind mount",
		)
	}
	if w.Workdir() == "" {
		return nil, errors.New("docker workspace: workdir is required")
	}
	if err := os.MkdirAll(w.Workdir(), 0o755); err != nil {
		return nil, err
	}

	running, exists, err := w.inspect(ctx)
	if err != nil {
		return nil, err
	}
	switch {
	case !exists:
		if err := w.run(ctx,
			"run", "-d",
			"--name", w.container,
			"-v", w.Workdir()+":"+w.Workdir(),
			"-w", w.Workdir(),
			w.image,
			"sleep", "infinity",
		); err != nil {
			return nil, err
		}
	case !running:
		if err := w.run(ctx, "start", w.container); err != nil {
			return nil, err
		}
	}

	be := backend.NewDocker(backend.DockerOptions{
		Container: w.container,
		Workdir:   w.Workdir(),
		HostPaths: true,
		Runner:    w.runner,
	})
	if err := w.ensureLayoutWith(ctx, be); err != nil {
		return nil, err
	}
	return be, nil
}

// Teardown optionally removes the container; by default it is left running
// so the next run reattaches to the same environment.
func (w *DockerWorkspace) Teardown(ctx context.Context) error {
	if !w.removeOnClose {
		return nil
	}
	_, stderr, exitCode, err := w.docker(ctx, []string{"rm", "-f", w.container}, nil)
	if err != nil {
		return err
	}
	if exitCode != 0 && !strings.Contains(string(stderr), "No such container") {
		return fmt.Errorf("docker rm: %s", strings.TrimSpace(string(stderr)))
	}
	return nil
}

// inspect reports whether the container exists and is running.
func (w *DockerWorkspace) inspect(ctx context.Context) (running, exists bool, err error) {
	stdout, _, exitCode, err := w.docker(
		ctx,
		[]string{"inspect", "-f", "{{.State.Running}}", w.container},
		nil,
	)
	if err != nil {
		return false, false, err
	}
	if exitCode != 0 {
		return false, false, nil
	}
	return strings.TrimSpace(string(stdout)) == "true", true, nil
}

func (w *DockerWorkspace) run(ctx context.Context, args ...string) error {
	_, stderr, exitCode, err := w.docker(ctx, args, nil)
	if err != nil {
		return err
	}
	if exitCode != 0 {
		return fmt.Errorf("docker %s: %s", args[0], strings.TrimSpace(string(stderr)))
	}
	return nil
}

func (w *DockerWorkspace) docker(ctx context.Context, args []string, stdin []byte) ([]byte, []byte, int, error) {
	return w.runner(ctx, args, stdin)
}
