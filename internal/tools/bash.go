package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"diegoc-agent/internal/backend"
	"diegoc-agent/internal/permission"
)

var (
	bashShells   = make(map[string]*backgroundShell)
	bashShellsMu sync.Mutex
)

type backgroundShell struct {
	ID      string
	Command string
	proc    backend.BackgroundProcess
}

func getBashShell(bashID string) *backgroundShell {
	bashShellsMu.Lock()
	defer bashShellsMu.Unlock()
	return bashShells[bashID]
}

func addBashShell(s *backgroundShell) {
	bashShellsMu.Lock()
	defer bashShellsMu.Unlock()
	bashShells[s.ID] = s
}

func removeBashShell(bashID string) {
	bashShellsMu.Lock()
	defer bashShellsMu.Unlock()
	delete(bashShells, bashID)
}

func shortID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:8]
}

func listBashIDs() []string {
	bashShellsMu.Lock()
	defer bashShellsMu.Unlock()
	ids := make([]string, 0, len(bashShells))
	for id := range bashShells {
		ids = append(ids, id)
	}
	return ids
}

// BashTool runs shell commands in workspace (foreground or background).
//
// Commands are not executed directly: they are handed to the workspace
// backend, which decides whether they run on the host or inside a container.
type BashTool struct {
	WorkspaceDir string
	Backend      backend.Backend
}

func (t *BashTool) resolveBackend() backend.Backend {
	return resolveBackend(t.Backend, t.WorkspaceDir)
}

func (t *BashTool) Name() string { return "bash" }

func (t *BashTool) Description() string {
	if t.resolveBackend().OS() == "windows" {
		return "Execute PowerShell commands in foreground or background. For terminal operations like git, npm, docker. Parameters: command (required), timeout (optional, default 120), run_in_background (optional). Use bash_output to monitor and bash_kill to terminate background commands."
	}
	return "Execute shell commands in foreground or background. For terminal operations like git, npm, docker. Parameters: command (required), timeout (optional, default 120), run_in_background (optional). Use bash_output to monitor and bash_kill to terminate background commands."
}

func (t *BashTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"command":           map[string]interface{}{"type": "string", "description": "The shell command to execute."},
			"timeout":           map[string]interface{}{"type": "integer", "description": "Timeout in seconds (default: 120, max: 600). Only for foreground commands.", "default": 120},
			"run_in_background": map[string]interface{}{"type": "boolean", "description": "Set true to run in background. Monitor with bash_output.", "default": false},
		},
		"required": []interface{}{"command"},
	}
}

func (t *BashTool) Execute(ctx context.Context, args map[string]interface{}) (*ToolResult, error) {
	command, _ := args["command"].(string)
	if command == "" {
		return &ToolResult{Success: false, Error: "command is required"}, nil
	}
	timeoutSec := 120
	if v, ok := args["timeout"]; ok {
		if n, ok := toInt(v); ok {
			if n > 600 {
				n = 600
			} else if n < 1 {
				n = 120
			}
			timeoutSec = n
		}
	}
	runInBackground := false
	if v, ok := args["run_in_background"]; ok {
		if b, ok := v.(bool); ok {
			runInBackground = b
		}
	}

	be := t.resolveBackend()
	argv := append(append([]string{}, be.Shell()...), command)

	if runInBackground {
		starter, ok := be.(backend.BackgroundStarter)
		if !ok {
			return &ToolResult{
				Success: false,
				Error:   fmt.Sprintf("backend %q does not support background commands", be.Kind()),
			}, nil
		}
		proc, err := starter.StartBackground(argv, be.Workdir())
		if err != nil {
			return &ToolResult{Success: false, Error: err.Error()}, nil
		}
		bashID := shortID()
		shell := &backgroundShell{ID: bashID, Command: command, proc: proc}
		addBashShell(shell)
		go func() {
			<-proc.Done()
			removeBashShell(bashID)
		}()
		msg := fmt.Sprintf("Command started in background. Use bash_output to monitor (bash_id='%s').\n\nCommand: %s\nBash ID: %s", bashID, command, bashID)
		return &ToolResult{Success: true, Content: msg}, nil
	}

	res, err := be.Exec(ctx, argv, backend.ExecOptions{
		Timeout: time.Duration(timeoutSec) * time.Second,
	})
	if err != nil {
		return &ToolResult{Success: false, Error: err.Error()}, nil
	}
	if res.TimedOut {
		return &ToolResult{Success: false, Error: fmt.Sprintf("Command timed out after %d seconds", timeoutSec)}, nil
	}
	output := string(res.Stdout) + string(res.Stderr)
	if !res.OK() {
		return &ToolResult{
			Success: false,
			Content: output,
			Error:   fmt.Sprintf("Command failed with exit code %d", res.ExitCode),
		}, nil
	}
	return &ToolResult{Success: true, Content: output}, nil
}

// BashOutputTool returns output from a background shell.
type BashOutputTool struct{}

func (t *BashOutputTool) Name() string { return "bash_output" }

func (t *BashOutputTool) Description() string {
	return "Retrieve output from a background bash shell. Takes bash_id (returned when starting a command with run_in_background=true)."
}

func (t *BashOutputTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"bash_id": map[string]interface{}{"type": "string", "description": "The ID of the background shell to retrieve output from."},
		},
		"required": []interface{}{"bash_id"},
	}
}

func (t *BashOutputTool) Execute(ctx context.Context, args map[string]interface{}) (*ToolResult, error) {
	bashID, _ := args["bash_id"].(string)
	if bashID == "" {
		return &ToolResult{Success: false, Error: "bash_id is required"}, nil
	}
	shell := getBashShell(bashID)
	if shell == nil {
		ids := listBashIDs()
		avail := "none"
		if len(ids) > 0 {
			avail = fmt.Sprintf("%v", ids)
		}
		return &ToolResult{Success: false, Error: fmt.Sprintf("Shell not found: %s. Available: %s", bashID, avail)}, nil
	}
	out := shell.proc.Output()
	if out == "" {
		out = "(no output yet)"
	}
	return &ToolResult{Success: true, Content: out}, nil
}

// BashKillTool terminates a background shell.
type BashKillTool struct{}

func (t *BashKillTool) Name() string { return "bash_kill" }

func (t *BashKillTool) Description() string {
	return "Terminate a background bash shell. Takes bash_id (returned when starting a command with run_in_background=true)."
}

func (t *BashKillTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"bash_id": map[string]interface{}{"type": "string", "description": "The ID of the background shell to terminate."},
		},
		"required": []interface{}{"bash_id"},
	}
}

// —— 权限 & 元数据 (HITL) ——

func (t *BashTool) CheckPermissions(args map[string]interface{}, pCtx *permission.Context) permission.Decision {
	cmd, _ := args["command"].(string)
	if cmd == "" {
		return permission.Decision{Behavior: permission.BehaviorDENY, Message: "命令不能为空"}
	}
	// EXPLORE 模式：只允许只读命令
	if pCtx.Mode == permission.ModeExplore {
		if isReadOnlyBash(cmd) {
			return permission.Decision{Behavior: permission.BehaviorALLOW}
		}
		return permission.Decision{Behavior: permission.BehaviorDENY, Message: "EXPLORE 模式：禁止非只读命令"}
	}
	// 危险命令 → 必须确认
	if isDangerousBash(cmd) {
		return permission.Decision{Behavior: permission.BehaviorASK, Message: "潜在危险命令: " + cmd}
	}
	// 只读命令 → 自动允许
	if isReadOnlyBash(cmd) {
		return permission.Decision{Behavior: permission.BehaviorALLOW}
	}
	// 普通修改命令 → 交给引擎继续
	return permission.Decision{Behavior: permission.BehaviorPASSTHROUGH}
}

func (t *BashTool) IsConcurrencySafe() bool { return false }
func (t *BashTool) IsReadOnly() bool        { return false }
func (t *BashTool) IsExternalTool() bool    { return false }

func (t *BashOutputTool) CheckPermissions(args map[string]interface{}, pCtx *permission.Context) permission.Decision {
	return permission.Decision{Behavior: permission.BehaviorALLOW} // 只读，永远允许
}
func (t *BashOutputTool) IsConcurrencySafe() bool { return true }
func (t *BashOutputTool) IsReadOnly() bool        { return true }
func (t *BashOutputTool) IsExternalTool() bool    { return false }

func (t *BashKillTool) CheckPermissions(args map[string]interface{}, pCtx *permission.Context) permission.Decision {
	// 终止后台进程需要确认
	return permission.Decision{Behavior: permission.BehaviorASK, Message: "终止后台命令需要确认"}
}
func (t *BashKillTool) IsConcurrencySafe() bool { return true }
func (t *BashKillTool) IsReadOnly() bool        { return false }
func (t *BashKillTool) IsExternalTool() bool    { return false }

// ——— 只读命令 & 危险命令检查 ———

var readOnlyCmds = []string{
	"ls", "cat", "head", "tail", "wc", "du", "df",
	"find", "grep", "awk", "sed -n", "sort", "uniq",
	"git status", "git log", "git diff", "git branch",
	"docker ps", "docker images", "docker logs",
	"echo", "date", "whoami", "pwd", "env", "which",
	"ps", "top", "htop", "free", "uptime", "uname",
}

var dangerousCmds = []string{
	"rm -rf /", "rm -rf ~", "rm -rf .", "rm -rf /*",
	"sudo rm", "dd if=", "mkfs.",
	"chmod 777 /", "chown -R /",
	"> /dev/sda", "> /dev/null",
	"fork bomb", ":(){",
}

func init() {
	// 导入 permission 包不需要额外初始化
	_ = readOnlyCmds
}

func isReadOnlyBash(cmd string) bool {
	for _, ro := range readOnlyCmds {
		if strings.Contains(cmd, ro) {
			return true
		}
	}
	return false
}

func isDangerousBash(cmd string) bool {
	for _, dc := range dangerousCmds {
		if strings.Contains(cmd, dc) {
			return true
		}
	}
	return false
}

func (t *BashKillTool) Execute(ctx context.Context, args map[string]interface{}) (*ToolResult, error) {
	bashID, _ := args["bash_id"].(string)
	if bashID == "" {
		return &ToolResult{Success: false, Error: "bash_id is required"}, nil
	}
	shell := getBashShell(bashID)
	if shell == nil {
		return &ToolResult{Success: false, Error: "Shell not found: " + bashID}, nil
	}
	if err := shell.proc.Kill(); err != nil {
		return &ToolResult{Success: false, Error: "Failed to terminate shell: " + err.Error()}, nil
	}
	removeBashShell(bashID)
	return &ToolResult{Success: true, Content: "Terminated shell " + bashID}, nil
}
