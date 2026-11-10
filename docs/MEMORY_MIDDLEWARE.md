# 记忆与上下文中间件

## 上下文压缩

`ContextCompactionMiddleware.OnReasoning` 在组装模型请求之前准备上下文。
`Agent.New` 默认注册它，CLI 同时注册它与重复调用检测中间件。

中间件根据当前 Agent 选择现有实现：配置 MemoryManager 时使用记忆管线，
否则使用原来的按轮次摘要。摘要、工具结果裁剪及 token 阈值行为保持不变。
手动 `/compact` 继续复用底层压缩服务，不会经过正常推理中间件链。

`SetMiddlewareFactories` 会替换整个中间件链。自定义注册时需要显式包含
`func() any { return &agent.ContextCompactionMiddleware{} }`；空链禁用自动压缩。
Factory 不捕获父 Agent 的 MemoryManager，fork 仍使用自身会话状态和已有的
摘要继承规则，不会压缩或修改父会话。

## 记忆写入

`MemoryWriteMiddleware` 默认与压缩中间件一起注册，仅在当前 Agent 配置了
MemoryManager 时生效。它在回复成功后提交新增用户输入和最终助手回答，
过滤系统提示、工具调用/结果、thinking 和框架摘要。

自动压缩与手动 `/compact` 共用 `ContextCompactionObserver` 提交边界：先生成
可用摘要，再提交原始对话，最后替换上下文。提交失败时保留原上下文。
`MemoryManager.CompactContext` 只准备上下文，不再隐式触发记忆提取。

写入中间件保存当前上下文的已提交前缀，避免每轮重放整段历史；相同文本在
不同轮次仍作为不同输入处理。压缩后重新对齐前缀，失败/取消的回复不额外提交。
这不是跨进程的 exactly-once 保证；rewind 不会删除已经写入的长期记忆。

MemoryManager 的 `QueueSummary` 对输入做独立快照，使用有上限的 FIFO 队列
串行执行后台提取，避免同时改写同一记忆文件。任务不依赖前台回复的 context，
后台错误通过 `BackgroundError` / `Close` 返回。关闭仍会取消并等待工作线程，
尚未完成的任务可能被取消；队列不持久化到磁盘。

fork 的 factory 创建独立中间件实例，且子会话不继承父 MemoryManager，因此
不会写入父会话记忆。自定义注册时应同时包含压缩与写入中间件；仅启用压缩
不会再产生后台写入。记忆注入仍留待后续单独实现。
