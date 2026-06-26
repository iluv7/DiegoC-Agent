# Human-in-the-Loop (HITL) 技术方案

> 目标：在 DiegoC-Agent 中实现与 AgentScope 完全一致的 HITL 机制。
> 核心思路：通过工具权限系统实现，Agent 不需要知道"有人在等我确认"——它照常调工具，权限引擎告诉它"这个要问用户"，Agent 把自己挂起，等用户确认后恢复。

## 一、当前架构分析

### 现状

```
Agent.Run(ctx)
  │
  ├─ summarizeIfNeeded        ← 上下文压缩
  ├─ LLM.Generate(...)         ← 推理
  ├─ append assistant msg      ← 记录回复
  ├─ if no tool calls → return
  │
  └─ for each tool call:
       ├─ tool.Execute(ctx, args)   ← 直接执行
       ├─ append tool result        ← 记录结果
       └─ loop back to LLM.Generate
```

**问题**：工具直接执行，没有权限检查、没有暂停机制。Agent 一旦 `Run()` 就跑到结束。

### 现有 Tool 接口

```go
type Tool interface {
    Name() string
    Description() string
    Parameters() map[string]interface{}
    Execute(ctx context.Context, args map[string]interface{}) (*ToolResult, error)
}
```

**缺失**：没有 CheckPermissions，没有 is_concurrency_safe / is_read_only / is_external_tool 等元数据。

---

## 二、AgentScope HITL 模型回顾

```
Agent._reply_impl → _execute_tool_call
  │
  ├─ 1. 解析输入，验证 JSON schema
  ├─ 2. PermissionEngine.check_permission(tool, parsed_input)
  │     → ALLOW: 继续执行
  │     → ASK:   yield RequireUserConfirmEvent, return（挂起到DB）
  │     → DENY:  返回错误结果
  │
  └─ 3. self._acting(tool_call) → 真正执行
```

核心原理：
- **yield + return** = 暂停 + 退出，状态（tool_call.state = ASKING）写 DB
- **恢复** = 新的函数调用，读 DB 里的 ASKING 状态，用户确认后改为 ALLOWED，继续执行
- 权限判断由 `PermissionEngine` 统一处理，规则匹配 + 工具自身的 `check_permissions` 结合
- 用户确认时可以选"始终允许"，规则写入 PermissionContext

---

## 三、Go 实现方案

Go 没有 Python 的 async generator，用 **channel + goroutine** 实现等价的暂停/恢复：

### 3.1 整体流程

```
第一次 Run():
  Agent.{RunWithHITL}(ctx, inputCh, eventCh)
    │
    ├─ LLM 返回 tool call
    ├─ PermissionEngine.CheckPermission(tool, args)
    │     → ASK:
    │       tool_call.State = ASKING
    │       eventCh ← HITLConfirmRequest   ← 发事件给调用者
    │       result := ← inputCh            ← 阻塞等待用户输入
    │       if result.Confirmed:
    │         tool_call.State = ALLOWED
    │         继续执行工具
    │       else:
    │         写入拒绝结果
    │     → ALLOW: 直接执行
    │     → DENY:  写入拒绝结果
    └─ 继续循环

中间 (调用者处理):
  req := ← eventCh                              ← 收到确认请求
  前端弹框 / 终端询问
  inputCh ← HITLConfirmResponse{Confirmed: true} ← 把结果传回去
```

Go 的 channel 天然就是 Python `yield` 的等价物——goroutine 在 channel 上阻塞，调用者继续运行，调用者把结果写回 channel，goroutine 被唤醒继续。

### 3.2 新增类型

```go
// ============ 权限行为 ============

type PermissionBehavior string

const (
    BehaviorALLOW       PermissionBehavior = "allow"
    BehaviorDENY        PermissionBehavior = "deny"
    BehaviorASK         PermissionBehavior = "ask"
    BehaviorPASSTHROUGH PermissionBehavior = "passthrough"
)

// ============ 权限决策 ============

type PermissionDecision struct {
    Behavior       PermissionBehavior
    Message        string
    SuggestedRules []PermissionRule
}

// ============ 权限模式 ============

type PermissionMode string

const (
    ModeDEFAULT     PermissionMode = "default"      // 严格：未命中规则就 ASK
    ModeACCEPTEDITS PermissionMode = "accept_edits" // 工作目录内自动 ALLOW
    ModeEXPLORE     PermissionMode = "explore"      // 只读
    ModeBYPASS      PermissionMode = "bypass"       // 全部 ALLOW
    ModeDONTASK     PermissionMode = "dont_ask"     // ASK 转 DENY（无人值守）
)

// ============ 权限规则 ============

type PermissionRule struct {
    ToolName    string             // 目标工具名（如 "bash"、"write_file"）
    RuleContent string             // Bash: 子串匹配命令; File: glob 匹配路径
    Behavior    PermissionBehavior // ALLOW / DENY / ASK
    Source      string             // "userSettings" / "userConfirm"
}

// ============ 权限上下文 ============

type PermissionContext struct {
    Mode               PermissionMode
    WorkingDirectories map[string]string // path → source
    AllowRules         map[string][]PermissionRule // toolName → rules
    DenyRules          map[string][]PermissionRule
    AskRules           map[string][]PermissionRule
}

// ============ 工具调用状态 ============

type ToolCallState string

const (
    ToolCallPending   ToolCallState = "pending"
    ToolCallAsking    ToolCallState = "asking"    // 等待用户确认
    ToolCallAllowed   ToolCallState = "allowed"   // 用户已允许，等待执行
    ToolCallSubmitted ToolCallState = "submitted" // 已提交外部执行
    ToolCallFinished  ToolCallState = "finished"  // 执行完毕
)

// ============ HITL 事件 ============

type HITLConfirmRequest struct {
    ReplyID        string
    ToolCalls      []PendingToolCall
}

type PendingToolCall struct {
    ID             string
    Name           string
    Args           map[string]interface{}
    SuggestedRules []PermissionRule
}

type HITLConfirmResponse struct {
    RepliedID string
    Results   []ToolConfirmResult
}

type ToolConfirmResult struct {
    ToolCall  PendingToolCall
    Confirmed bool
    Rules     []PermissionRule // 用户选"始终允许"时携带
}
```

### 3.3 扩展 Tool 接口

```go
type Tool interface {
    Name() string
    Description() string
    Parameters() map[string]interface{}

    // === 新增 ===
    Execute(ctx context.Context, args map[string]interface{}) (*ToolResult, error)

    // CheckPermissions 由每个工具自己实现——判断这个具体调用是否安全。
    // 返回 ALLOW → 直接放行；ASK → 继续走规则匹配；DENY → 直接拒绝。
    CheckPermissions(args map[string]interface{}, ctx *PermissionContext) PermissionDecision

    // 工具元数据
    IsConcurrencySafe() bool  // 是否可以和其他工具并发执行
    IsReadOnly() bool         // 是否只读（EXPLORE 模式下自动 ALLOW）
    IsExternalTool() bool     // 是否外部执行（Agent 不自己跑）
}
```

### 3.4 权限引擎

```go
type PermissionEngine struct {
    context *PermissionContext
}

func NewPermissionEngine(ctx *PermissionContext) *PermissionEngine {
    return &PermissionEngine{context: ctx}
}

// CheckPermission 按优先级评估：
// 1. deny 规则（最高优先级）→ 命中则 DENY
// 2. ask 规则 → 命中则 ASK
// 3. allow 规则 → 命中则 ALLOW
// 4. 工具自身的 CheckPermissions → ALLOW/DENY/ASK/PASSTHROUGH
// 5. PASSTHROUGH → 按当前 mode 的基线策略决定
func (e *PermissionEngine) CheckPermission(
    tool Tool,
    args map[string]interface{},
) PermissionDecision {
    toolName := tool.Name()

    // 1. 检查 deny 规则
    if rules, ok := e.context.DenyRules[toolName]; ok {
        for _, rule := range rules {
            if matchRule(rule, toolName, args) {
                return PermissionDecision{Behavior: BehaviorDENY, Message: "denied by rule: " + rule.RuleContent}
            }
        }
    }

    // 2. 检查 ask 规则
    if rules, ok := e.context.AskRules[toolName]; ok {
        for _, rule := range rules {
            if matchRule(rule, toolName, args) {
                return PermissionDecision{Behavior: BehaviorASK, Message: "ask by rule: " + rule.RuleContent}
            }
        }
    }

    // 3. 检查 allow 规则
    if rules, ok := e.context.AllowRules[toolName]; ok {
        for _, rule := range rules {
            if matchRule(rule, toolName, args) {
                return PermissionDecision{Behavior: BehaviorALLOW, Message: "allowed by rule: " + rule.RuleContent}
            }
        }
    }

    // 4. 工具自身的 CheckPermissions
    decision := tool.CheckPermissions(args, e.context)

    // 5. PASSTHROUGH → 按模式基线
    if decision.Behavior == BehaviorPASSTHROUGH {
        return e.modeFallback(tool, args)
    }

    return decision
}

func matchRule(rule PermissionRule, toolName string, args map[string]interface{}) bool {
    // Bash 工具：rule_content 子串匹配命令
    // File 工具 (read_file/write_file/edit_file)：rule_content glob 匹配路径
    // 其他工具：精确匹配
    switch toolName {
    case "bash":
        if cmd, ok := args["command"].(string); ok {
            return strings.Contains(cmd, rule.RuleContent)
        }
    case "read_file", "write_file", "edit_file":
        if path, ok := args["path"].(string); ok {
            return globMatch(rule.RuleContent, path)
        }
    }
    return false
}
```

### 3.5 Agent 循环改造

核心改动：`Run()` → `RunWithHITL()`。

```go
func (a *Agent) RunWithHITL(
    ctx context.Context,
    inputCh <-chan HITLConfirmResponse,   // 接收用户确认结果
    eventCh chan<- HITLConfirmRequest,    // 发送确认请求给调用者
) (string, error) {
    if a.permissionEngine == nil {
        a.permissionEngine = NewPermissionEngine(&a.PermissionContext)
    }

    for step := 0; step < a.MaxSteps; step++ {
        select {
        case <-ctx.Done():
            return "", ctx.Err()
        default:
        }

        a.summarizeIfNeeded(ctx)
        resp, _ := a.LLM.Generate(ctx, a.Messages, a.Tools)

        a.Messages = append(a.Messages, schema.Message{
            Role:      "assistant",
            Content:   resp.Content,
            Thinking:  resp.Thinking,
            ToolCalls: resp.ToolCalls,
        })

        if len(resp.ToolCalls) == 0 {
            return resp.Content, nil
        }

        // ============ 权限检查插入点 ============
        for _, tc := range resp.ToolCalls {
            args := toolArgsFromRaw(tc.Function.Arguments)
            tool := a.toolByName[tc.Function.Name]

            // 查找工具
            if tool == nil {
                a.appendToolError(tc, "Unknown tool: "+tc.Function.Name)
                continue
            }

            // 权限检查
            decision := a.permissionEngine.CheckPermission(tool, args)

            switch decision.Behavior {
            case BehaviorALLOW:
                // 直接执行
                a.executeAndAppend(ctx, tool, tc, args)

            case BehaviorASK, BehaviorPASSTHROUGH:
                // ===== 挂起，等用户确认 =====
                tc.State = ToolCallAsking

                // 发请求给调用者
                select {
                case eventCh <- HITLConfirmRequest{
                    ReplyID: a.ReplyID,
                    ToolCalls: []PendingToolCall{{
                        ID:   tc.ID,
                        Name: tc.Function.Name,
                        Args: args,
                        SuggestedRules: decision.SuggestedRules,
                    }},
                }:
                case <-ctx.Done():
                    return "", ctx.Err()
                }

                // 阻塞等待用户回复
                var response HITLConfirmResponse
                select {
                case response = <-inputCh:
                case <-ctx.Done():
                    return "", ctx.Err()
                }

                // 处理用户决定
                for _, cr := range response.Results {
                    if !cr.Confirmed {
                        a.appendToolError(tc, "denied by user")
                        tc.State = ToolCallFinished
                    } else {
                        tc.State = ToolCallAllowed
                        // 用户选了"始终允许" → 写入规则
                        for _, rule := range cr.Rules {
                            a.permissionEngine.AddRule(rule)
                        }
                        a.executeAndAppend(ctx, tool, tc, args)
                        tc.State = ToolCallFinished
                    }
                }

            case BehaviorDENY:
                a.appendToolError(tc, decision.Message)
                tc.State = ToolCallFinished
            }
        }
    }
    return "", nil
}
```

### 3.6 调用方代码（CLI / HTTP handler）

```go
func handleChat(agent *Agent, userMsg string) {
    inputCh := make(chan HITLConfirmResponse, 1)
    eventCh := make(chan HITLConfirmRequest, 1)

    go func() {
        result, err := agent.RunWithHITL(ctx, inputCh, eventCh)
        // 处理结果
    }()

    // 等待 HITL 事件
    for {
        select {
        case req := <-eventCh:
            // 收到确认请求 → 展示给用户
            fmt.Printf("Agent 要执行: %s(%v)，允许吗？[y/n] ",
                req.ToolCalls[0].Name, req.ToolCalls[0].Args)
            var answer string
            fmt.Scanln(&answer)

            // 把用户决定传回去
            inputCh <- HITLConfirmResponse{
                RepliedID: req.ReplyID,
                Results: []ToolConfirmResult{{
                    ToolCall:  req.ToolCalls[0],
                    Confirmed: answer == "y",
                }},
            }

        case <-ctx.Done():
            return
        }
    }
}
```

### 3.7 各工具 CheckPermissions 示意

```go
// Bash: 只读命令自动 ALLOW，其他 PASSTHROUGH（交给引擎处理）
func (t *BashTool) CheckPermissions(args map[string]interface{}, ctx *PermissionContext) PermissionDecision {
    cmd, _ := args["command"].(string)

    // EXPLORE 模式下检测注入和危险命令
    if ctx.Mode == ModeEXPLORE {
        if isDangerousCommand(cmd) {
            return PermissionDecision{Behavior: BehaviorDENY, Message: "dangerous command in EXPLORE mode"}
        }
        if isReadOnlyCommand(cmd) {
            return PermissionDecision{Behavior: BehaviorALLOW}
        }
        return PermissionDecision{Behavior: BehaviorDENY, Message: "non-read-only command in EXPLORE mode"}
    }

    // 其他模式：自己先判断这是不是只读命令，只读就 ALLOW
    if isReadOnlyCommand(cmd) {
        return PermissionDecision{Behavior: BehaviorALLOW}
    }

    // 不是只读 → PASSTHROUGH，让引擎继续匹配规则
    return PermissionDecision{Behavior: BehaviorPASSTHROUGH}
}

// ReadFile: 永远 PASSTHROUGH（纯读，引擎根据模式处理）
func (t *ReadTool) CheckPermissions(args map[string]interface{}, ctx *PermissionContext) PermissionDecision {
    return PermissionDecision{Behavior: BehaviorPASSTHROUGH}
}

// WriteFile: 检测危险路径
func (t *WriteTool) CheckPermissions(args map[string]interface{}, ctx *PermissionContext) PermissionDecision {
    path, _ := args["path"].(string)

    if isDangerousPath(path) {
        return PermissionDecision{
            Behavior: BehaviorASK,
            Message:  fmt.Sprintf("Writing to %s may be dangerous", path),
            SuggestedRules: []PermissionRule{{
                ToolName:    "write_file",
                RuleContent: filepath.Dir(path) + "/**",
                Behavior:    BehaviorALLOW,
                Source:      "suggestion",
            }},
        }
    }

    return PermissionDecision{Behavior: BehaviorPASSTHROUGH}
}
```

---

## 四、后续：Toolkit 重构

当前 tools 是平铺的 `[]Tool` 列表，后续按 AgentScope 的 Toolkit 模式重构：

```
type ToolGroup struct {
    Name        string
    Description string
    Tools       []Tool
    MCPs        []MCPClient
    Skills      []Skill
}

type Toolkit struct {
    BasicGroup *ToolGroup          // 始终激活
    Groups     map[string]*ToolGroup // 可动态激活/停用

    activatedGroups []string
    permissionCtx   *PermissionContext
}
```

本次 HITL 不改 Toolkit 结构，只扩 Tool 接口和 Agent 循环。Toolkit 重构为独立迭代。

---

## 五、实施步骤

| 阶段 | 内容 | 涉及文件 |
|------|------|---------|
| **Phase 1** | 新增类型定义 | `internal/permission/` — types.go, rule.go, engine.go, context.go |
| **Phase 2** | 扩展 Tool 接口 | `internal/tools/base.go` — 加 CheckPermissions、元数据方法 |
| **Phase 3** | Agent 循环改造 | `internal/agent/agent.go` — 新增 RunWithHITL |
| **Phase 4** | 各工具实现 CheckPermissions | `internal/tools/bash.go`, `file.go`, `note.go`, `skill.go`, `mcp.go` |
| **Phase 5** | CLI 集成 | `cmd/` — 终端交互确认 |
| **Phase 6** | 测试 | `internal/agent/agent_hitl_test.go` |

---

## 六、与 AgentScope 的对应关系

| AgentScope (Python) | DiegoC-Agent (Go) |
|---------------------|-------------------|
| `PermissionBehavior` | `PermissionBehavior` |
| `PermissionMode` | `PermissionMode` |
| `PermissionRule` | `PermissionRule` |
| `PermissionContext` | `PermissionContext` |
| `PermissionEngine.check_permission()` | `PermissionEngine.CheckPermission()` |
| `ToolBase.check_permissions()` | `Tool.CheckPermissions()` |
| `ToolCallState` | `ToolCallState` |
| `RequireUserConfirmEvent` | `HITLConfirmRequest` (channel) |
| `UserConfirmResultEvent` | `HITLConfirmResponse` (channel) |
| `yield event; return` | `eventCh <- req; result := <-inputCh` |
| `_execute_tool_call` | `RunWithHITL` 内的权限检查块 |
| `_handle_incoming_event` | `RunWithHITL` 内 `<-inputCh` 后的处理块 |

唯一差异：AgentScope 用 yield/return + DB 持久化实现跨进程挂起/恢复；Go 版本先用 channel 实现进程内暂停（CLI 场景），后续加上 DB 持久化即可支持跨进程。
