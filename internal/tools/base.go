package tools

import (
	"context"
	"diegoc-agent/internal/permission"
)

// ToolResult 是工具执行的输出。
type ToolResult struct {
	Success bool   `json:"success"`           // 是否成功
	Content string `json:"content"`           // 输出内容
	Error   string `json:"error,omitempty"`   // 错误信息
}

// Tool 是所有工具必须实现的接口。
// 对齐 AgentScope 的 ToolBase，增加了 CheckPermissions 和元数据方法。
type Tool interface {
	// 基础信息
	Name() string
	Description() string
	Parameters() map[string]interface{}

	// 执行
	Execute(ctx context.Context, args map[string]interface{}) (*ToolResult, error)

	// CheckPermissions 由每个工具自己实现，判断这个具体调用是否安全。
	// 返回 ALLOW → 直接放行
	// 返回 ASK   → 需要用户确认
	// 返回 DENY  → 直接拒绝
	// 返回 PASSTHROUGH → 不表态，交给权限引擎按模式基线处理
	CheckPermissions(args map[string]interface{}, pCtx *permission.Context) permission.Decision

	// 工具元数据
	IsConcurrencySafe() bool // 是否可以和其他工具并发执行
	IsReadOnly() bool        // 是否只读（EXPLORE 模式下自动 ALLOW）
	IsExternalTool() bool    // 是否外部执行（Agent 不自己跑，委托给外部系统）
}

// ToAnthropicSchema 返回 Anthropic API 格式的工具 schema。
func ToAnthropicSchema(t Tool) map[string]interface{} {
	return map[string]interface{}{
		"name":         t.Name(),
		"description":  t.Description(),
		"input_schema": t.Parameters(),
	}
}

// ToOpenAISchema 返回 OpenAI API 格式的工具 schema。
func ToOpenAISchema(t Tool) map[string]interface{} {
	return map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{
			"name":        t.Name(),
			"description": t.Description(),
			"parameters":  t.Parameters(),
		},
	}
}
