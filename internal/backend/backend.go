// Package backend abstracts the execution environment used by tools.
//
// A backend exposes exactly three primitives — Exec, ReadFile and WriteFile —
// whose mechanism genuinely differs between environments (host process,
// container, remote sandbox). Every other filesystem operation is
// derived from those primitives by Base, so a new backend only has to
// implement the three primitives plus a little environment metadata.
//
// The same shape as AgentScope's BackendBase: callers never branch on the
// provider; they ask the backend where its workdir is, how its paths are
// shaped, and let it resolve paths and run commands.
package backend

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// ExecOptions controls one Exec call.
type ExecOptions struct {
	// Cwd is the working directory for the command. Empty means the
	// backend's default workdir.
	Cwd string
	// Timeout bounds the call. Zero means no timeout. On timeout the
	// returned result carries ExitCode -1 and TimedOut true.
	Timeout time.Duration
}

// ExecResult is the outcome of running an argv list.
type ExecResult struct {
	// ExitCode is the process exit code. -1 conventionally reports an
	// internal failure (timeout, transport error) rather than a command's
	// own non-zero exit.
	ExitCode int
	Stdout   []byte
	Stderr   []byte
	// TimedOut reports that ExecOptions.Timeout elapsed and the process
	// was killed.
	TimedOut bool
}

// OK reports whether the command exited successfully.
func (r *ExecResult) OK() bool {
	return r != nil && r.ExitCode == 0 && !r.TimedOut
}

// DirEntry is one directory entry with the metadata a caller usually wants.
// Size and ModTime are nil when the provider could not determine them (a
// dangling symlink, a vanished entry), and Size is always nil for a
// directory.
type DirEntry struct {
	Name    string
	IsDir   bool
	Size    *int64
	ModTime *time.Time
}

// PathOps holds pure string path functions matching the backend environment.
// Use PosixPaths for remote Linux containers and NativePaths for the host.
type PathOps struct {
	Join  func(elem ...string) string
	Dir   func(path string) string
	Base  func(path string) string
	IsAbs func(path string) bool
	Clean func(path string) string
}

// Backend is the full surface consumed by tools.
//
// Only Exec, ReadFile and WriteFile are implemented per environment; the
// remaining filesystem methods come from embedding *Base. A backend with a
// cheaper native implementation (LocalBackend) overrides them.
type Backend interface {
	// Kind names the environment, e.g. "local" or "remote".
	Kind() string
	// OS is the execution environment's family, "posix" or "windows".
	// It describes where commands run, not the host process.
	OS() string
	// Shell returns the argv prefix that evaluates a command line, e.g.
	// ["bash", "-c"] or ["powershell.exe", "-NoProfile", "-Command"].
	Shell() []string
	// HostPaths reports whether files written through the backend are
	// visible on the host filesystem (true for the local backend and for
	// bind-mounted container workspaces, false for remote sandboxes).
	HostPaths() bool
	// Workdir is the default working directory inside the environment.
	Workdir() string

	// Primitives.
	Exec(ctx context.Context, command []string, opts ExecOptions) (*ExecResult, error)
	ReadFile(ctx context.Context, path string) ([]byte, error)
	WriteFile(ctx context.Context, path string, data []byte) error

	// MkdirAll creates a directory and any missing parents.
	MkdirAll(ctx context.Context, path string) error

	// Derived filesystem operations.
	FileExists(ctx context.Context, path string) (bool, error)
	IsDir(ctx context.Context, path string) (bool, error)
	ListDir(ctx context.Context, path string, recursive bool) ([]string, error)
	Stat(ctx context.Context, path string) (*DirEntry, error)
	StatMtime(ctx context.Context, path string) (time.Time, bool, error)
	DeletePath(ctx context.Context, path string) error
	ExpandUser(ctx context.Context, path string) (string, error)

	// Path helpers. These are pure string operations with the backend
	// environment's semantics; they never touch the host filesystem.
	JoinPath(elem ...string) string
	DirName(path string) string
	BaseName(path string) string
	IsAbs(path string) bool
	CleanPath(path string) string
	Abs(path, cwd string) string
}

// BackgroundProcess is a command started with BackgroundStarter.
type BackgroundProcess interface {
	// Done is closed when the process exits.
	Done() <-chan struct{}
	// Output returns everything captured so far, one line per entry.
	Output() string
	// Kill terminates the process.
	Kill() error
}

// BackgroundStarter is an optional backend capability. BashTool uses it for
// run_in_background; backends without it reject background commands.
type BackgroundStarter interface {
	StartBackground(command []string, cwd string) (BackgroundProcess, error)
}

// ExecFunc is the primitive a concrete backend hands to Base.
type ExecFunc func(ctx context.Context, command []string, opts ExecOptions) (*ExecResult, error)

// ── base ───────────────────────────────────────────────────────────────

// Base implements every derived filesystem operation on top of exec, plus
// the path helpers. Concrete backends embed *Base and override Kind, OS,
// Shell, HostPaths and the three primitives.
type Base struct {
	workdir string
	ops     PathOps
	exec    ExecFunc
}

// NewBase wires the shared implementation to a primitive executor.
func NewBase(workdir string, ops PathOps, exec ExecFunc) *Base {
	return &Base{workdir: workdir, ops: ops, exec: exec}
}

// Kind is overridden by concrete backends.
func (b *Base) Kind() string { return "base" }

// OS defaults to posix, correct for container and remote Linux sandboxes.
func (b *Base) OS() string { return "posix" }

// Shell defaults to a POSIX shell.
func (b *Base) Shell() []string { return []string{"sh", "-c"} }

// HostPaths defaults to false: a backend does not know it is host-visible.
func (b *Base) HostPaths() bool { return false }

// Workdir returns the backend's default working directory.
func (b *Base) Workdir() string { return b.workdir }

// Exec, ReadFile and WriteFile are supplied by the concrete backend; the
// embedded Base deliberately does not define them so a missing primitive is
// a compile-time error rather than a runtime one.

// MkdirAll is the shell-based default.
func (b *Base) MkdirAll(ctx context.Context, path string) error {
	res, err := b.exec(ctx, []string{"mkdir", "-p", path}, ExecOptions{})
	if err != nil {
		return err
	}
	if !res.OK() {
		return fmt.Errorf("mkdir %s: %s", path, strings.TrimSpace(string(res.Stderr)))
	}
	return nil
}

// ── derived filesystem operations ──────────────────────────────────────

// FileExists reports whether path exists (file or directory).
func (b *Base) FileExists(ctx context.Context, path string) (bool, error) {
	res, err := b.exec(ctx, []string{"test", "-e", path}, ExecOptions{})
	if err != nil {
		return false, err
	}
	return res.OK(), nil
}

// IsDir reports whether path is an existing directory.
func (b *Base) IsDir(ctx context.Context, path string) (bool, error) {
	res, err := b.exec(ctx, []string{"test", "-d", path}, ExecOptions{})
	if err != nil {
		return false, err
	}
	return res.OK(), nil
}

// ListDir lists entries under path. Without recursive it returns the
// immediate children's base names; with recursive it returns file paths.
// Output is NUL-delimited so names containing spaces or newlines survive.
func (b *Base) ListDir(ctx context.Context, path string, recursive bool) ([]string, error) {
	command := []string{"find", path}
	if recursive {
		command = append(command, "-type", "f", "-print0")
	} else {
		command = append(command, "-mindepth", "1", "-maxdepth", "1", "-printf", "%f\\0")
	}
	res, err := b.exec(ctx, command, ExecOptions{})
	if err != nil {
		return nil, err
	}
	if !res.OK() {
		return nil, fmt.Errorf("list %s: %s", path, strings.TrimSpace(string(res.Stderr)))
	}
	parts := bytes.Split(res.Stdout, []byte{0})
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) == 0 {
			continue
		}
		out = append(out, string(part))
	}
	return out, nil
}

const findEntryFormat = "%Y\\t%s\\t%T@\\t%f\\0"

// Stat returns one path's type, size and mtime in one call, or nil when the
// path does not exist. Symlinks are followed, matching IsDir.
func (b *Base) Stat(ctx context.Context, path string) (*DirEntry, error) {
	entries, err := b.findEntries(ctx, []string{
		"find", path, "-maxdepth", "0", "-printf", findEntryFormat,
	}, true)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	return &entries[0], nil
}

// StatMtime returns the modification time of path, and false when it cannot
// be determined. GNU stat is tried first, BSD stat second.
func (b *Base) StatMtime(ctx context.Context, path string) (time.Time, bool, error) {
	quoted := shellQuote(path)
	script := fmt.Sprintf(
		"stat -c %%Y %s 2>/dev/null || stat -f %%m %s 2>/dev/null",
		quoted, quoted,
	)
	res, err := b.exec(ctx, []string{"sh", "-c", script}, ExecOptions{})
	if err != nil {
		return time.Time{}, false, err
	}
	if !res.OK() {
		return time.Time{}, false, nil
	}
	raw := strings.TrimSpace(string(res.Stdout))
	seconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return time.Time{}, false, nil
	}
	return time.Unix(seconds, 0), true, nil
}

// DeletePath removes path (file or directory tree); a missing path is a
// silent no-op, like rm -rf.
func (b *Base) DeletePath(ctx context.Context, path string) error {
	res, err := b.exec(ctx, []string{"rm", "-rf", path}, ExecOptions{})
	if err != nil {
		return err
	}
	if !res.OK() {
		return fmt.Errorf("delete %s: %s", path, strings.TrimSpace(string(res.Stderr)))
	}
	return nil
}

// ExpandUser expands a leading ~ or ~/ using the backend environment's
// $HOME. The ~user form is returned unchanged.
func (b *Base) ExpandUser(ctx context.Context, path string) (string, error) {
	if path == "" || path[0] != '~' {
		return path, nil
	}
	if len(path) > 1 && path[1] != '/' && path[1] != '\\' {
		return path, nil
	}
	res, err := b.exec(ctx, []string{"printenv", "HOME"}, ExecOptions{})
	if err != nil {
		return path, err
	}
	home := strings.TrimSpace(string(res.Stdout))
	if home == "" {
		return path, nil
	}
	return home + path[1:], nil
}

func (b *Base) findEntries(ctx context.Context, command []string, skipUnresolvable bool) ([]DirEntry, error) {
	res, err := b.exec(ctx, command, ExecOptions{})
	if err != nil {
		return nil, err
	}
	// A non-zero exit means the path is missing or unreadable; callers
	// treat both as "no entry".
	if !res.OK() {
		return nil, nil
	}
	return parseFindEntries(res.Stdout, skipUnresolvable), nil
}

// parseFindEntries parses records produced with findEntryFormat.
func parseFindEntries(stdout []byte, skipUnresolvable bool) []DirEntry {
	entries := make([]DirEntry, 0, 8)
	for _, record := range bytes.Split(stdout, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		fields := strings.SplitN(string(record), "\t", 4)
		if len(fields) != 4 {
			continue
		}
		kind, rawSize, rawMtime, name := fields[0], fields[1], fields[2], fields[3]

		var entry DirEntry
		entry.Name = name
		entry.IsDir = kind == "d"
		if size, err := strconv.ParseInt(rawSize, 10, 64); err == nil && !entry.IsDir {
			entry.Size = &size
		}
		if seconds, err := strconv.ParseFloat(rawMtime, 64); err == nil {
			whole, frac := math.Modf(seconds)
			modTime := time.Unix(int64(whole), int64(frac*float64(time.Second)))
			entry.ModTime = &modTime
		}
		// %Y reports N/L/? when it cannot follow the link. The size and
		// mtime printed alongside describe the link itself, which would
		// read as a real file; os.Stat fails there instead.
		if kind == "N" || kind == "L" || kind == "?" {
			if skipUnresolvable {
				continue
			}
			entry.Size, entry.ModTime = nil, nil
		}
		entries = append(entries, entry)
	}
	return entries
}

// ── path helpers ───────────────────────────────────────────────────────

// JoinPath joins path components with the backend's separator.
func (b *Base) JoinPath(elem ...string) string { return b.ops.Join(elem...) }

// DirName returns the directory component of path.
func (b *Base) DirName(path string) string { return b.ops.Dir(path) }

// BaseName returns the final component of path.
func (b *Base) BaseName(path string) string { return b.ops.Base(path) }

// IsAbs reports whether path is absolute in the backend environment.
func (b *Base) IsAbs(path string) bool { return b.ops.IsAbs(path) }

// CleanPath normalizes path without touching the filesystem.
func (b *Base) CleanPath(path string) string { return b.ops.Clean(path) }

// Abs resolves path against an explicit cwd. It never consults the host
// process's working directory, which is meaningless for remote backends.
func (b *Base) Abs(path, cwd string) string {
	if b.ops.IsAbs(path) {
		return b.ops.Clean(path)
	}
	return b.ops.Clean(b.ops.Join(cwd, path))
}

// shellQuote wraps s in single quotes for POSIX shells.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// isMissingFile recognizes the message cat prints for a missing path.
func isMissingFile(stderr []byte) bool {
	return strings.Contains(strings.ToLower(string(stderr)), "no such file")
}
