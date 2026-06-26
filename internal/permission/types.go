// Package permission 定义了 HITL（Human-in-the-Loop）权限系统。
// 对齐 AgentScope 的 permission 模块：PermissionMode、PermissionRule、
// PermissionContext、PermissionEngine 和 ToolCallState 生命周期。
package permission

import "path/filepath"

// Behavior 是权限检查的结果行为。
type Behavior string

const (
	BehaviorALLOW       Behavior = "allow"       // 直接允许
	BehaviorDENY        Behavior = "deny"        // 直接拒绝
	BehaviorASK         Behavior = "ask"         // 需要用户确认
	BehaviorPASSTHROUGH Behavior = "passthrough" // 不表态，交给引擎继续处理
)

// Mode 是权限的基线策略。
type Mode string

const (
	ModeDefault     Mode = "default"      // 严格模式：未命中规则就 ASK
	ModeAcceptEdits Mode = "accept_edits" // 工作目录内自动 ALLOW 修改操作
	ModeExplore     Mode = "explore"      // 只读模式：拒绝一切修改操作
	ModeBypass      Mode = "bypass"       // 完全信任：跳过所有检查
	ModeDontAsk     Mode = "dont_ask"     // 无人值守：所有 ASK 转为 DENY
)

// ToolCallState 追踪单个工具调用的生命周期。
type ToolCallState string

const (
	ToolCallPending   ToolCallState = "pending"   // 等待检查
	ToolCallAsking    ToolCallState = "asking"    // 等待用户确认
	ToolCallAllowed   ToolCallState = "allowed"   // 用户已允许，等待执行
	ToolCallSubmitted ToolCallState = "submitted" // 已提交外部执行
	ToolCallFinished  ToolCallState = "finished"  // 执行完毕
)

// Decision 是权限引擎检查后的决策。
type Decision struct {
	Behavior       Behavior // 决定行为
	Message        string   // 决策说明
	SuggestedRules []Rule   // 建议用户选择的规则（"始终允许 src/**"）
}

// Rule 是用户配置或系统建议的权限规则。
type Rule struct {
	ToolName    string   `json:"tool_name"`    // 目标工具名（如 "bash"、"write_file"）
	RuleContent string   `json:"rule_content"` // Bash: 子串匹配命令; File: glob 匹配路径
	Behavior    Behavior `json:"behavior"`     // ALLOW / DENY / ASK
	Source      string   `json:"source"`       // "userSettings" / "userConfirm" / "suggestion"
}

// WorkingDirectory 是权限范围内允许操作的额外目录。
type WorkingDirectory struct {
	Path   string `json:"path"`   // 绝对路径
	Source string `json:"source"` // 来源："userSettings" / "session"
}

// Context 保存当前会话的权限配置。
type Context struct {
	Mode               Mode                        `json:"mode"`                // 权限模式
	WorkingDirectories map[string]WorkingDirectory `json:"working_directories"`  // 工作目录
	AllowRules         map[string][]Rule           `json:"allow_rules"`          // toolName → 允许规则列表
	DenyRules          map[string][]Rule           `json:"deny_rules"`           // toolName → 拒绝规则列表
	AskRules           map[string][]Rule           `json:"ask_rules"`            // toolName → 询问规则列表
}

// NewContext 创建一个默认的权限上下文（DEFAULT 模式，空规则）。
func NewContext() Context {
	return Context{
		Mode:               ModeDefault,
		WorkingDirectories: make(map[string]WorkingDirectory),
		AllowRules:         make(map[string][]Rule),
		DenyRules:          make(map[string][]Rule),
		AskRules:           make(map[string][]Rule),
	}
}

// AddRule 根据规则的 Behavior 将其追加到对应列表。
func (c *Context) AddRule(r Rule) {
	switch r.Behavior {
	case BehaviorALLOW:
		c.AllowRules[r.ToolName] = append(c.AllowRules[r.ToolName], r)
	case BehaviorDENY:
		c.DenyRules[r.ToolName] = append(c.DenyRules[r.ToolName], r)
	case BehaviorASK:
		c.AskRules[r.ToolName] = append(c.AskRules[r.ToolName], r)
	}
}

// GlobMatch 判断文件路径是否匹配 glob 模式。
// 支持 ** 递归匹配和 * 单段匹配。
func GlobMatch(pattern, path string) bool {
	matched, _ := filepath.Match(pattern, path)
	if matched {
		return true
	}
	// ** 简化支持："src/**" 匹配 "src/a/b/c.go"
	if len(pattern) > 2 && pattern[len(pattern)-2:] == "**" {
		prefix := pattern[:len(pattern)-2]
		if len(path) >= len(prefix) && path[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

// PendingToolCall 表示一个等待用户确认的工具调用。
type PendingToolCall struct {
	ID             string                 `json:"id"`              // LLM 返回的 tool_call_id
	Name           string                 `json:"name"`            // 工具名
	Args           map[string]interface{} `json:"args"`            // 工具参数
	SuggestedRules []Rule                 `json:"suggested_rules"` // 建议的"始终允许"规则
}

// HITLConfirmRequest 是 Agent 需要用户确认时发给调用者的事件。
type HITLConfirmRequest struct {
	ReplyID   string            `json:"reply_id"`   // 当前回复的 ID，用于关联请求和响应
	ToolCalls []PendingToolCall `json:"tool_calls"` // 需要确认的工具调用列表
}

// ToolConfirmResult 是用户对单个工具调用的确认结果。
type ToolConfirmResult struct {
	ToolCall  PendingToolCall `json:"tool_call"`  // 被确认的工具调用
	Confirmed bool            `json:"confirmed"`  // 用户是否同意
	Rules     []Rule          `json:"rules"`      // 用户选的"始终允许"规则
}

// HITLConfirmResponse 是调用者对 HITLConfirmRequest 的回复。
type HITLConfirmResponse struct {
	ReplyID string              `json:"reply_id"` // 必须与 Request 的 ReplyID 一致
	Results []ToolConfirmResult `json:"results"`  // 每个工具调用的确认结果
}
