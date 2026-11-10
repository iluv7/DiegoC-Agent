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

本次先迁移自动压缩入口；记忆管线内的后台写入将在独立 PR 中拆出。
