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
不会再产生后台写入。记忆注入由下面的独立中间件负责。

## 记忆注入

`MemoryInjectionMiddleware.OnReply` 参考 deer-flow 的
[DynamicContextMiddleware](https://github.com/bytedance/deer-flow/blob/main/backend/packages/harness/deerflow/agents/middlewares/dynamic_context_middleware.py)：
在会话首次回复前加载记忆，并冻结为会话快照，后续模型调用及用户轮次复用。
当前项目使用已有 `MemoryManager.SearchMemory` 混合检索，以最新用户输入为查询；
这与 deer-flow 调用记忆后端的 `get_context` 接口不同，不引入新的存储后端。

CLI 在 `memory.enabled: true` 时使用配置好的索引；`memory.injection_enabled`
默认 true，设为 false 可单独关闭注入，不影响写入与显式 `memory_search`。
没有索引或没有命中时不追加空消息。成功的空结果同样冻结，失败/超时则在后续
回复重试；整个回复取消时直接停止。查询超时默认 5 秒，依赖检索后端响应 context。

快照是带 `memory_context` 标记的 user 消息，插入前导 system 消息之后，保留
来源路径和行号，不改写系统提示，也不打断工具调用与结果的配对。框架会保留
它跨越自动压缩和 `/compact`，同时从摘要输入、长期记忆写入中排除它，避免
把召回内容再次提取为新的用户事实。

默认最多注入 5 条结果、1500 个估算 token，并限制在模型上下文预算的 10% 内；
截断包含消息开销、数据框和来源信息，保持 UTF-8 有效。`/context` 单独展示
Memory 分类。库调用者可设置中间件的 MaxResults、MaxTokens 和 Timeout。

fork 继承已有快照，不额外查询父会话存储。清空会话后会重新加载；rewind
恢复其检查点中的快照，若回到首次注入之前则在新用户轮次重新加载。
已有快照不会随后台记忆更新而实时变化，以保持会话前缀稳定。

`Agent.New` 默认注册注入、压缩、写入三个中间件。自定义链时将注入放在回复
处理的外层；若要从已有会话移除快照，可注册 `MemoryInjectionMiddleware{Disabled:true}`。
