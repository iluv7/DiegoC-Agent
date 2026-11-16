package tools

import "diegoc-agent/internal/backend"

// resolveBackend returns the tool's backend, falling back to a host backend
// rooted at workdir. The fallback keeps tools usable when they are
// constructed directly (tests, memory tools) without an explicit workspace.
func resolveBackend(be backend.Backend, workdir string) backend.Backend {
	if be != nil {
		return be
	}
	return backend.NewLocal(workdir)
}
