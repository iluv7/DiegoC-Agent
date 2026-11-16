// Package workspace owns the execution environment the agent works in:
// its lifecycle, its canonical directory layout, and the builtin tools bound
// to it.
//
// Workspace mirrors AgentScope's split between a backend (how commands and
// files are accessed) and a workspace (what state exists and when it is
// provisioned). Concrete workspaces only implement Provision and Teardown;
// Base owns initialization, idempotent close, reset and tool construction.
package workspace

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"diegoc-agent/internal/backend"
	"diegoc-agent/internal/tools"
)

// Canonical names inside a workspace. They are resolved against the
// backend's workdir, so they are valid in a container as well as on the host.
const (
	DataDirName     = "data"
	SkillsDirName   = "skills"
	SessionsDirName = "sessions"
	MCPFileName     = ".mcp"
	NotesFileName   = ".agent_memory.json"
	PlanFileName    = ".agent_plan.md"
)

// Layout is the resolved set of workspace paths for one backend.
type Layout struct {
	Workdir     string
	DataDir     string
	SkillsDir   string
	SessionsDir string
	MCPFile     string
}

// ToolOptions selects which builtin tools a workspace contributes.
type ToolOptions struct {
	FileTools bool
	Bash      bool
	Plan      bool
	Notes     bool
}

// Workspace is the execution environment an agent works in.
type Workspace interface {
	// ID is a stable identifier, used in logs and container naming.
	ID() string
	// Workdir is the agent-visible root; relative tool paths resolve here.
	Workdir() string
	// Backend returns the active backend, or an error before Initialize.
	Backend() (backend.Backend, error)
	// Layout returns the resolved canonical paths.
	Layout() Layout
	// Instructions is a system-prompt fragment describing the environment.
	Instructions() string

	// Initialize provisions the environment; it is idempotent.
	Initialize(ctx context.Context) error
	// Close releases the environment; it is idempotent and safe on a
	// workspace that was never initialized.
	Close(ctx context.Context) error
	// Reset wipes workspace state while keeping the environment alive.
	Reset(ctx context.Context) error

	// ListTools returns the builtin tools bound to this workspace.
	ListTools(opts ToolOptions) ([]tools.Tool, error)
}

// BackendProvider is the only part a concrete workspace has to implement.
type BackendProvider interface {
	// Provision creates or attaches the environment and returns a live
	// backend whose Workdir matches the workspace workdir.
	Provision(ctx context.Context) (backend.Backend, error)
	// Teardown releases the environment. It must be idempotent and should
	// swallow expected provider errors where closing cannot improve.
	Teardown(ctx context.Context) error
}

// Base implements the Workspace lifecycle around a BackendProvider.
type Base struct {
	id           string
	workdir      string
	ops          backend.PathOps
	instructions string
	provider     BackendProvider

	mu      sync.RWMutex
	backend backend.Backend
	alive   bool
}

// NewBase wires the shared lifecycle to a provider. workdir must be the
// path the provider's backend will use as its default working directory.
func NewBase(
	id string,
	workdir string,
	ops backend.PathOps,
	provider BackendProvider,
	instructions string,
) *Base {
	return &Base{
		id:           id,
		workdir:      workdir,
		ops:          ops,
		instructions: instructions,
		provider:     provider,
	}
}

// ID returns the workspace's stable identifier.
func (b *Base) ID() string { return b.id }

// Workdir returns the agent-visible root directory.
func (b *Base) Workdir() string { return b.workdir }

// Instructions returns the system-prompt fragment for this workspace.
func (b *Base) Instructions() string { return b.instructions }

// Layout resolves the canonical workspace paths with backend path semantics.
func (b *Base) Layout() Layout {
	return Layout{
		Workdir:     b.workdir,
		DataDir:     b.ops.Join(b.workdir, DataDirName),
		SkillsDir:   b.ops.Join(b.workdir, SkillsDirName),
		SessionsDir: b.ops.Join(b.workdir, SessionsDirName),
		MCPFile:     b.ops.Join(b.workdir, MCPFileName),
	}
}

// Backend returns the live backend. Callers resolve it per operation rather
// than caching it, because reconnecting workspaces replace the backend.
func (b *Base) Backend() (backend.Backend, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.backend == nil {
		return nil, fmt.Errorf(
			"%s workspace %q is not initialized",
			b.id, b.workdir,
		)
	}
	return b.backend, nil
}

// Initialize provisions the environment. It is a no-op when already alive.
func (b *Base) Initialize(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.alive {
		return nil
	}
	if b.provider == nil {
		return errors.New("workspace: no backend provider configured")
	}
	be, err := b.provider.Provision(ctx)
	if err != nil {
		return fmt.Errorf("%s workspace %q: provision: %w", b.id, b.workdir, err)
	}
	b.backend = be
	b.alive = true
	return nil
}

// Close tears the environment down. It is a no-op when already closed and
// keeps the provider's error so callers can log it.
func (b *Base) Close(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.alive {
		b.backend = nil
		return nil
	}
	err := b.provider.Teardown(ctx)
	b.backend = nil
	b.alive = false
	if err != nil {
		return fmt.Errorf("%s workspace %q: teardown: %w", b.id, b.workdir, err)
	}
	return nil
}

// Reset deletes data/, skills/, sessions/ and .mcp inside the workspace but
// leaves the environment itself running.
func (b *Base) Reset(ctx context.Context) error {
	be, err := b.Backend()
	if err != nil {
		return err
	}
	layout := b.Layout()
	for _, path := range []string{
		layout.DataDir,
		layout.SkillsDir,
		layout.SessionsDir,
		layout.MCPFile,
	} {
		if err := be.DeletePath(ctx, path); err != nil {
			return fmt.Errorf("reset %s: %w", path, err)
		}
	}
	return nil
}

// EnsureLayout creates workdir, data/, skills/ and sessions/.
func (b *Base) EnsureLayout(ctx context.Context) error {
	be, err := b.Backend()
	if err != nil {
		return err
	}
	return b.ensureLayoutWith(ctx, be)
}

func (b *Base) ensureLayoutWith(ctx context.Context, be backend.Backend) error {
	layout := Layout{
		Workdir:     b.workdir,
		DataDir:     be.JoinPath(b.workdir, DataDirName),
		SkillsDir:   be.JoinPath(b.workdir, SkillsDirName),
		SessionsDir: be.JoinPath(b.workdir, SessionsDirName),
	}
	for _, dir := range []string{
		layout.Workdir,
		layout.DataDir,
		layout.SkillsDir,
		layout.SessionsDir,
	} {
		if err := be.MkdirAll(ctx, dir); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return nil
}

// ListTools builds the builtin tools bound to the active backend. Every tool
// resolves paths and I/O through that backend, so the same tool works on the
// host and inside a container.
func (b *Base) ListTools(opts ToolOptions) ([]tools.Tool, error) {
	be, err := b.Backend()
	if err != nil {
		return nil, err
	}
	workdir := be.Workdir()
	list := make([]tools.Tool, 0, 8)

	if opts.FileTools {
		list = append(list,
			&tools.ReadTool{WorkspaceDir: workdir, Backend: be},
			&tools.WriteTool{WorkspaceDir: workdir, Backend: be},
			&tools.EditTool{WorkspaceDir: workdir, Backend: be},
		)
	}
	if opts.Plan {
		list = append(list, &tools.WritePlanTool{WorkspaceDir: workdir, Backend: be})
	}
	if opts.Bash {
		list = append(list,
			&tools.BashTool{WorkspaceDir: workdir, Backend: be},
			&tools.BashOutputTool{},
			&tools.BashKillTool{},
		)
	}
	if opts.Notes {
		notesPath := be.JoinPath(workdir, NotesFileName)
		list = append(list,
			&tools.RecordNoteTool{MemoryFile: notesPath, Backend: be},
			&tools.RecallNoteTool{MemoryFile: notesPath, Backend: be},
		)
	}
	return list, nil
}

// formatInstructions substitutes the workdir into a prompt fragment.
func formatInstructions(template, workdir string) string {
	return strings.ReplaceAll(template, "{workdir}", workdir)
}
