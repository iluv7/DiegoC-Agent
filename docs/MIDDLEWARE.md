# Middleware 扩展接口

Agent 的 middleware 接入定义位于 `internal/agent/middleware.go`。本模块只提供扩展接口和调用链，不包含业务 middleware。

## 注册

在 Agent 创建之后、开始执行之前调用：

```go
ag.SetMiddlewareFactories(
    func() any { return yourMiddleware },
)
```

这里的 `yourMiddleware` 应在 factory 内创建；每次调用 factory 都应返回独立实例。有可变状态时不能复用同一对象。前台会话分叉和后台 Agent 任务会继承 factory，并分别创建新实例。factory 本身应支持并发调用。

`SetMiddlewareFactories` 替换全部注册；无参数调用清空注册。只应在 Agent 空闲时注册。一个实例可以实现任意多个 hook；未实现的 hook 自动跳过。

## Hook 边界

| 接口 | 范围 |
|---|---|
| `ReplyMiddleware` | 一次 `RunWithHITL`，包含完整 ReAct 和 goal 继续循环 |
| `ReasoningMiddleware` | 每步上下文准备、记忆处理和模型推理；返回响应后由循环统一记账、记录响应和追加消息 |
| `ModelCallMiddleware` | 主推理的逻辑模型请求，包含 provider 内部重试；可以通过 `ModelRequest` 替换客户端、消息和工具列表 |
| `ActingMiddleware` | 已获准工具的原始执行；备份、文件快照、结果记录及消息写入由外层执行器负责 |
| `SystemPromptMiddleware` | 请求中每条 system 消息的字符串变换，不写回会话历史 |

除系统提示词变换外，hook 均为洋葱调用链。注册 A、B 时依次执行 A 前置、B 前置、原始操作、B 后置、A 后置。调用 `next(ctx, ...)` 继续执行，也可以直接返回以短路。提示词按注册顺序依次变换，错误立即向上传播。

主推理调用路径为：

```text
Reply → ReAct loop
          → Reasoning → 上下文准备 → SystemPrompt → ModelCall → Client.Generate
          → 响应记账与写入
          → 权限确认 → Rewind 写前备份 → Acting → Tool.Execute
                                               → 写后快照、日志与工具消息
```

## 扩展约定

- 所有 hook 必须传播和遵守 `context.Context` 的取消信号；后置清理使用 `defer`。
- middleware 是可信的进程内扩展，可以访问 Agent。不要在同一 Agent 上并发执行或递归启动回复。
- 正常执行只调用一次 `next`。重复调用工具 handler 会重复产生副作用；框架不自动重试工具。
- `ActingMiddleware` 仅提供调用 ID 和工具名，使用闭包传递获准参数，避免变更审批和备份已经确定的操作。通过结果返回值加工工具输出。
- 模型和推理 hook 成功返回时必须提供非 nil 响应；工具 hook 成功返回时必须提供非 nil 结果。框架会把空返回转成错误。
- 工具 hook 的错误沿用工具错误处理，作为 tool 消息交给下一轮模型；推理和回复错误终止本次执行。
- 压缩、记忆内部模型调用和 goal 评估不会经过 `ModelCallMiddleware`。本版本不提供权限决策或压缩 hook。
- 系统提示词变换作用于实际请求；`/context` 对会话内容的估算不包含动态变换增加的内容。
- 零注册时保留原执行流程，无需修改现有构造函数或 CLI 配置。
