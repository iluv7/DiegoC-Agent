package permission

import "strings"

// Engine 是权限引擎，按优先级评估工具调用是否允许。
// 优先级：deny 规则 → ask 规则 → allow 规则 → 工具自身判断 → 模式基线。
type Engine struct {
	Context *Context
}

// NewEngine 创建权限引擎。
func NewEngine(ctx *Context) *Engine {
	if ctx == nil {
		c := NewContext()
		ctx = &c
	}
	return &Engine{Context: ctx}
}

// AddRule 向 Context 追加规则（通常在用户确认时调用）。
func (e *Engine) AddRule(r Rule) {
	e.Context.AddRule(r)
}

// CheckPermission 按五级优先级判断工具调用是否允许。
//
//  1. deny 规则（最高优先级）→ 命中则 DENY
//  2. ask 规则 → 命中则 ASK
//  3. allow 规则 → 命中则 ALLOW
//  4. tool.CheckPermissions(args, context) → 工具自己判断
//  5. PASSTHROUGH → 按当前 mode 的基线策略处理
func (e *Engine) CheckPermission(
	toolName string,
	toolCheck func() Decision,
	args map[string]interface{},
) Decision {
	// 1. 检查 deny 规则 —— 最高优先级
	if rules, ok := e.Context.DenyRules[toolName]; ok {
		for _, rule := range rules {
			if matchRule(rule, toolName, args) {
				return Decision{
					Behavior: BehaviorDENY,
					Message:  "操作被拒绝，命中拒绝规则: " + rule.RuleContent,
				}
			}
		}
	}

	// 2. 检查 ask 规则 —— 用户明确说"这个要问我"
	if rules, ok := e.Context.AskRules[toolName]; ok {
		for _, rule := range rules {
			if matchRule(rule, toolName, args) {
				return Decision{
					Behavior: BehaviorASK,
					Message:  "操作需要确认，命中询问规则: " + rule.RuleContent,
				}
			}
		}
	}

	// 3. 检查 allow 规则 —— 用户已允许过的
	if rules, ok := e.Context.AllowRules[toolName]; ok {
		for _, rule := range rules {
			if matchRule(rule, toolName, args) {
				return Decision{
					Behavior: BehaviorALLOW,
					Message:  "操作已允许，命中允许规则: " + rule.RuleContent,
				}
			}
		}
	}

	// 4. 工具自身的 CheckPermissions
	decision := toolCheck()

	// 5. 工具返回 PASSTHROUGH → 按模式基线
	if decision.Behavior == BehaviorPASSTHROUGH {
		return e.modeFallback(toolName, args)
	}

	return decision
}

// modeFallback 在工具返回 PASSTHROUGH 时，根据当前 mode 的基线策略决定。
func (e *Engine) modeFallback(toolName string, args map[string]interface{}) Decision {
	switch e.Context.Mode {
	case ModeBypass:
		return Decision{Behavior: BehaviorALLOW, Message: "BYPASS 模式：自动允许"}

	case ModeExplore:
		return Decision{Behavior: BehaviorDENY, Message: "EXPLORE 模式：拒绝非只读操作"}

	case ModeDontAsk:
		return Decision{Behavior: BehaviorDENY, Message: "DONT_ASK 模式：无人值守，拒绝需要确认的操作"}

	case ModeAcceptEdits:
		// 检查路径是否在工作目录内
		if path, ok := args["path"].(string); ok {
			if e.isInWorkingDirectory(path) {
				return Decision{Behavior: BehaviorALLOW, Message: "路径在工作目录内，自动允许"}
			}
		}
		if _, ok := args["command"].(string); ok {
			if e.isBashInWorkingDirectory(toolName, args) {
				return Decision{Behavior: BehaviorALLOW, Message: "命令目标在工作目录内，自动允许"}
			}
		}
		return Decision{Behavior: BehaviorASK, Message: "DEFAULT 模式：需要用户确认"}

	default: // ModeDefault
		return Decision{Behavior: BehaviorASK, Message: "DEFAULT 模式：需要用户确认"}
	}
}

// isInWorkingDirectory 判断路径是否在任一工作目录内。
func (e *Engine) isInWorkingDirectory(path string) bool {
	for _, wd := range e.Context.WorkingDirectories {
		if strings.HasPrefix(path, wd.Path) {
			return true
		}
	}
	return false
}

// isBashInWorkingDirectory 判断 bash 命令的目标路径是否在工作目录内。
func (e *Engine) isBashInWorkingDirectory(toolName string, args map[string]interface{}) bool {
	if toolName != "bash" {
		return false
	}
	// 简化判断：提取命令中的路径参数，检查是否在工作目录内
	// 完整实现需要 tree-sitter 解析，此处先做简单的路径前缀匹配
	cmd, _ := args["command"].(string)
	for _, wd := range e.Context.WorkingDirectories {
		if strings.Contains(cmd, wd.Path) {
			return true
		}
	}
	return false
}

// matchRule 判断规则是否匹配工具调用参数。
// Bash 工具：rule_content 子串匹配命令
// File 工具：rule_content glob 匹配路径
// 其他工具：精确匹配
func matchRule(rule Rule, toolName string, args map[string]interface{}) bool {
	switch toolName {
	case "bash":
		if cmd, ok := args["command"].(string); ok {
			return strings.Contains(cmd, rule.RuleContent)
		}
	case "read_file", "write_file", "edit_file":
		if path, ok := args["path"].(string); ok {
			return GlobMatch(rule.RuleContent, path)
		}
	default:
		// 其他工具：只要工具名匹配就算命中
		return true
	}
	return false
}
