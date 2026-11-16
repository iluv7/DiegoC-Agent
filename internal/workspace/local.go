package workspace

import (
	"context"
	"os"

	"diegoc-agent/internal/backend"
)

const localInstructions = `## Execution Environment
File and shell tools operate directly on the host filesystem. Relative paths resolve against {workdir}.`

// LocalOptions configures a host-directory workspace.
type LocalOptions struct {
	// ID is a stable identifier; empty means "local".
	ID string
	// Workdir is the agent-visible root directory.
	Workdir string
	// Instructions overrides the default system-prompt fragment.
	Instructions string
	// CreateLayout also creates data/, skills/ and sessions/ during
	// initialization. It is opt-in because Workdir may be a real project
	// directory the user did not ask us to populate.
	CreateLayout bool
}

// LocalWorkspace runs everything in the host process.
type LocalWorkspace struct {
	*Base
	createLayout bool
}

var (
	_ Workspace       = (*LocalWorkspace)(nil)
	_ BackendProvider = (*LocalWorkspace)(nil)
)

// NewLocal returns a workspace backed by the host filesystem.
func NewLocal(opts LocalOptions) *LocalWorkspace {
	if opts.ID == "" {
		opts.ID = "local"
	}
	if opts.Instructions == "" {
		opts.Instructions = localInstructions
	}
	w := &LocalWorkspace{createLayout: opts.CreateLayout}
	w.Base = NewBase(
		opts.ID,
		opts.Workdir,
		backend.NativePaths,
		w,
		formatInstructions(opts.Instructions, opts.Workdir),
	)
	return w
}

// Provision creates the workdir and binds a LocalBackend.
func (w *LocalWorkspace) Provision(ctx context.Context) (backend.Backend, error) {
	if err := os.MkdirAll(w.Workdir(), 0o755); err != nil {
		return nil, err
	}
	be := backend.NewLocal(w.Workdir())
	if w.createLayout {
		if err := w.ensureLayoutWith(ctx, be); err != nil {
			return nil, err
		}
	}
	return be, nil
}

// Teardown is a no-op: host directories outlive the agent.
func (w *LocalWorkspace) Teardown(context.Context) error { return nil }
