# Fork session：创建独立会话并自由切换

`/fork` 复制当前 session 的上下文并立即进入新 session。它不要求任务，不追加隐藏指令，也不会在创建时调用模型。创建之后直接正常聊天。

```text
You [main] › 已经分析过登录功能
You [main] › /fork
Created and switched to [session-1] from [main].
You [session-1] › 这里讨论缓存
You [session-1] › /switch main
You [main] › 这里继续讨论权限
You [main] › /switch session-1
You [session-1] › 接着刚刚的缓存话题
```

## 切换方式

- `/sessions`、`/switch`：打开会话选择器，用滚轮、上下键、翻页键选择，回车切换；Esc 取消。
- `/switch main`：回到初始会话。
- `/switch session-1`、`/sessions session-1`、`/session session-1`：直接进入指定会话。
- 任何前台会话都可以再次 `/fork`。新会话从当前所在会话分叉，并记录来源 ID。

选择器包含 main 和所有前台分叉，标出当前项。切换后会显示该 session 的对话文本，提示符始终标识当前 ID。没有支持终端选择器的环境可直接使用带 ID 的命令。

会话执行当前回答时，先等待回答结束，或按 Esc 取消当前回合后切换。程序等待该回合真正停止后才允许操作消息历史，不会同时向同一个前台 session 追加消息。

## 状态边界

创建时深拷贝消息历史及嵌套工具参数，继承当时的摘要、渲染后的系统提示词、模型客户端及同样的工具定义。Agent 工具会重新绑定到新会话，权限配置也独立复制。

后续聊天不会自动同步。交互式分叉不会向来源 session 发送完成报告。切回来源会话时，继续使用其原有消息、摘要、权限和统计状态。每个 session 有独立日志文件；快速切换也不会因为同秒时间戳覆盖日志。

`/clear`、`/rewind` 处理当前 session 的对话状态。新分叉从创建之后建立自己的 rewind 检查点，不继承来源会话的文件回退记录。

不共享可变的 MemoryManager。创建时已经读进上下文的记忆和摘要会继承，磁盘记忆文件不会复制；后续仍可以通过相同工具访问原路径。分叉使用自己的基础上下文压缩状态，来源会话的自动长期记忆管线仍留在来源会话。

工作目录和磁盘文件共享。`/fork` 不创建 worktree；恢复文件仍会改变所有 session 看到的同一份磁盘文件。需要工作副本隔离时，在不同 worktree 中分别启动程序。

会话和会话 ID 仅保留在当前进程中，退出程序后不能恢复。当前没有跨进程会话存储。

## 前缀缓存

`/fork` 创建时没有 API 请求。之后新会话发出第一条消息时，前面的历史、系统提示词和工具定义保持一致，新消息放在末尾。首个分叉请求跳过上下文压缩，减少公共前缀变化。

没有复制客户端 KV cache，也没有新增 provider 缓存开关或 Anthropic `cache_control`。是否实际复用由服务端和现有客户端配置决定。

## 后台 Agent 任务

模型 `Agent` 工具的后台任务能力保留，和 `/fork` 的前台会话分叉分开：

```text
/tasks                    # 查看后台任务
/tasks fork-1             # 查看任务结果和停止后的消息历史
/send fork-1 继续检查缓存  # 给后台任务的会话追加任务
/approve fork-1 y          # 批准一次；a 保存建议规则（若有），n 拒绝
/cancel fork-1             # 取消后台任务
```

后台任务继承分配任务的那个 session，报告也只排入那个 session 的收件箱，在其下次推理时送达；终端通知带任务 ID。切换前台会话不会将报告注入其他会话。

最多同时执行 8 个后台任务，禁止后台任务递归生成任务。批准请求使用独立回复通道；退出程序时取消并等待所有后台任务停止。后台任务的规则不影响前台 session 的规则。

## 实现

- `internal/agent/sessions.go`：前台会话快照、列表、切换和嵌套分叉。
- `cmd/diegoc-agent/sessions_ui.go`：`/fork` 与切换选择器。
- `internal/agent/fork.go`：后台 Agent 任务及报告、权限路由。
- `cmd/diegoc-agent/fork_ui.go`：终端输入和后台通知。

交互式 fork session 最多 7 个（main 不计入，总计最多 8 个会话）。从 main 或已有 session 分叉均计入同一个上限；达到上限时不创建新会话，也不切换当前会话。
