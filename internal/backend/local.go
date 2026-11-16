package backend

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// LocalBackend runs commands and file operations on the host.
//
// It embeds *Base for the shell-derived filesystem helpers but overrides
// nearly all of them with native os.* calls, which are cheaper and do not
// depend on GNU find/stat.
type LocalBackend struct {
	*Base
}

var (
	_ Backend           = (*LocalBackend)(nil)
	_ BackgroundStarter = (*LocalBackend)(nil)
)

// NewLocal returns a host backend rooted at workdir. An empty workdir means
// the host process's working directory.
func NewLocal(workdir string) *LocalBackend {
	b := &LocalBackend{}
	b.Base = NewBase(workdir, NativePaths, b.exec)
	return b
}

// Kind identifies the host environment.
func (b *LocalBackend) Kind() string { return "local" }

// OS reports the host family using the os.name convention.
func (b *LocalBackend) OS() string {
	if runtime.GOOS == "windows" {
		return "windows"
	}
	return "posix"
}

// Shell returns the native shell invocation: PowerShell on Windows, bash
// on POSIX hosts when available, sh otherwise.
func (b *LocalBackend) Shell() []string {
	if b.OS() == "windows" {
		return []string{"powershell.exe", "-NoProfile", "-Command"}
	}
	if _, err := exec.LookPath("bash"); err == nil {
		return []string{"bash", "-c"}
	}
	return []string{"sh", "-c"}
}

// HostPaths is true: the local backend's files are host files.
func (b *LocalBackend) HostPaths() bool { return true }

// ── primitives ─────────────────────────────────────────────────────────

func (b *LocalBackend) exec(ctx context.Context, command []string, opts ExecOptions) (*ExecResult, error) {
	if len(command) == 0 {
		return nil, errors.New("backend: empty command")
	}
	runCtx := ctx
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(runCtx, command[0], command[1:]...)
	// Put the command in its own process group and terminate the whole
	// group on timeout. Killing only the direct child leaves grandchildren
	// holding the output pipes, so Wait would block until they exit.
	configureProcessGroup(cmd)
	cmd.Cancel = func() error { return terminateProcessGroup(cmd) }
	cwd := opts.Cwd
	if cwd == "" {
		cwd = b.workdir
	}
	if cwd != "" {
		cmd.Dir = cwd
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	res := &ExecResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if runCtx.Err() != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			res.ExitCode = -1
			res.TimedOut = true
			return res, nil
		}
		return nil, runCtx.Err()
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
			return res, nil
		}
		return nil, err
	}
	return res, nil
}

// Exec runs an argv list directly, without an intervening shell.
func (b *LocalBackend) Exec(ctx context.Context, command []string, opts ExecOptions) (*ExecResult, error) {
	return b.exec(ctx, command, opts)
}

// ReadFile reads path from the host filesystem.
func (b *LocalBackend) ReadFile(_ context.Context, path string) ([]byte, error) {
	return os.ReadFile(path)
}

// WriteFile writes data to path, creating parent directories.
func (b *LocalBackend) WriteFile(_ context.Context, path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// ── native filesystem overrides ────────────────────────────────────────

// MkdirAll creates path and any missing parents.
func (b *LocalBackend) MkdirAll(_ context.Context, path string) error {
	return os.MkdirAll(path, 0o755)
}

// FileExists reports whether path exists.
func (b *LocalBackend) FileExists(_ context.Context, path string) (bool, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// IsDir reports whether path is a directory.
func (b *LocalBackend) IsDir(_ context.Context, path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return info.IsDir(), nil
}

// ListDir lists entries with os.ReadDir or, recursively, filepath.WalkDir.
func (b *LocalBackend) ListDir(_ context.Context, path string, recursive bool) ([]string, error) {
	if recursive {
		var out []string
		err := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				out = append(out, p)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		return out, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.Name())
	}
	return out, nil
}

// Stat returns one path's type, size and mtime, or nil when it is missing.
func (b *LocalBackend) Stat(_ context.Context, path string) (*DirEntry, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	entry := &DirEntry{Name: filepath.Base(path), IsDir: info.IsDir()}
	if !info.IsDir() {
		size := info.Size()
		entry.Size = &size
	}
	modTime := info.ModTime()
	entry.ModTime = &modTime
	return entry, nil
}

// StatMtime returns the file's modification time.
func (b *LocalBackend) StatMtime(_ context.Context, path string) (modTime time.Time, ok bool, err error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, err
	}
	return info.ModTime(), true, nil
}

// DeletePath removes path recursively; a missing path is a no-op.
func (b *LocalBackend) DeletePath(_ context.Context, path string) error {
	return os.RemoveAll(path)
}

// ExpandUser expands ~ using the host's home directory.
func (b *LocalBackend) ExpandUser(_ context.Context, path string) (string, error) {
	if path == "" || path[0] != '~' {
		return path, nil
	}
	if len(path) > 1 && path[1] != '/' && path[1] != '\\' {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path, nil
	}
	return home + path[1:], nil
}

// ── background execution ───────────────────────────────────────────────

// StartBackground runs command detached from any turn context. The caller
// owns the returned process and must Kill it or wait for Done.
func (b *LocalBackend) StartBackground(command []string, cwd string) (BackgroundProcess, error) {
	if len(command) == 0 {
		return nil, errors.New("backend: empty command")
	}
	cmd := exec.Command(command[0], command[1:]...)
	configureProcessGroup(cmd)
	if cwd == "" {
		cwd = b.workdir
	}
	if cwd != "" {
		cmd.Dir = cwd
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	proc := &localBackgroundProcess{cmd: cmd, done: make(chan struct{})}
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			proc.appendLine(scanner.Text())
		}
		_ = cmd.Wait()
		close(proc.done)
	}()
	return proc, nil
}

type localBackgroundProcess struct {
	cmd  *exec.Cmd
	done chan struct{}
	mu   sync.Mutex
	out  strings.Builder
}

func (p *localBackgroundProcess) appendLine(line string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.out.WriteString(line)
	p.out.WriteString("\n")
}

// Done is closed when the process exits.
func (p *localBackgroundProcess) Done() <-chan struct{} { return p.done }

// Output returns everything captured so far.
func (p *localBackgroundProcess) Output() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.out.String()
}

// Kill terminates the process group. Killing an exited process is not an
// error.
func (p *localBackgroundProcess) Kill() error {
	select {
	case <-p.done:
		return nil
	default:
	}
	err := terminateProcessGroup(p.cmd)
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}
