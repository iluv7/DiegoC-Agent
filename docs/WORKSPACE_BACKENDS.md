# 执行后端与工作区抽象

本文说明 DiegoC-Agent 的执行层（`internal/backend`）与工作区层
（`internal/workspace`），以及如何新增一个执行环境。

设计的核心只有一句话：**只抽象真正随环境变化的原语，其余全部在基类
派生**。这与 AgentScope 的 `BackendBase` / `SandboxedWorkspaceBase`
是同一套思路。

## 分层

```
┌────────────────────────────────────────────────────────────┐
│ CLI / Agent                                                │
│   bootstrap.buildApplication → buildWorkspace → Initialize │
└───────────────────────────┬────────────────────────────────┘
                            │ workspace.Workspace
┌───────────────────────────▼────────────────────────────────┐
│ workspace 层                                                │
│   Base：生命周期（Initialize/Close/Reset）、目录布局、        │
│         Instructions、ListTools（把 backend 注入工具）        │
│   子类只实现：Provision / Teardown                          │
└───────────────────────────┬────────────────────────────────┘
                            │ backend.Backend
┌───────────────────────────▼────────────────────────────────┐
│ backend 层                                                  │
│   Backend：Exec / ReadFile / WriteFile（3 个原语）           │
│   Base：FileExists / IsDir / ListDir / Stat / StatMtime /   │
│         DeletePath / ExpandUser / MkdirAll / 路径函数        │
│   子类只实现：3 个原语 + Kind/OS/Shell/HostPaths/Workdir     │
└────────────────────────────────────────────────────────────┘
```

### 1. 执行层 `internal/backend`

`Backend` 接口里只有三个方法在不同环境下的实现机制真正不同：

```go
Exec(ctx, command []string, opts ExecOptions) (*ExecResult, error)
ReadFile(ctx, path string) ([]byte, error)
WriteFile(ctx, path string, data []byte) error
```

`command` 是 **argv 列表**，不经过 shell，因此调用方不需要转义；需要
管道/重定向时自己传 `["sh", "-c", line]`。`ExecResult` 用 `ExitCode == -1`
表示超时或传输失败，与命令自身的非零退出区分。

其余文件系统操作由 `backend.Base` 基于 `Exec` 派生，适用于任何 POSIX
环境：

| 方法 | 派生方式 |
|---|---|
| `FileExists` / `IsDir` | `test -e` / `test -d` |
| `ListDir` | `find ... -print0`（NUL 分隔，文件名含空格/换行安全） |
| `Stat` | `find ... -printf '%Y\t%s\t%T@\t%f\0'`，一次调用拿全元数据 |
| `StatMtime` | GNU `stat -c %Y`，失败回退 BSD `stat -f %m` |
| `DeletePath` | `rm -rf` |
| `ExpandUser` | `printenv HOME` |
| `MkdirAll` | `mkdir -p` |

路径函数由 `PathOps` 提供：`PosixPaths` 用于容器/远端，`NativePaths`
用于本机。`Abs(path, cwd)` 强制传入 cwd，避免宿主的工作目录泄漏到远端
路径里。`OS()`/`Shell()` 描述的是**执行环境**而不是宿主进程。

已实现的后端：

- `LocalBackend`：宿主机，原生 `os.*` 覆盖多数派生方法，支持后台进程。

新增一个后端只需要：

1. 实现 `Exec` / `ReadFile` / `WriteFile`；
2. 构造时调用 `NewBase(workdir, PathOps, yourExec)` 并嵌入 `*Base`；
3. 覆盖 `Kind` / `OS` / `Shell` / `HostPaths`（按需）。

性能敏感的环境可以覆盖 `FileExists`、`Stat`、`ListDir` 等，语义保持
不变即可。

### 2. 工作区层 `internal/workspace`

`Workspace` 负责环境本身的状态：

```go
Initialize(ctx)   // 幂等；调用 Provider.Provision 绑定 backend
Close(ctx)        // 幂等；调用 Provider.Teardown
Reset(ctx)        // 删除 data/ skills/ sessions/ .mcp，保留环境
ListTools(opts)   // 构造绑定到 backend 的内置工具
Layout()          // Workdir / DataDir / SkillsDir / SessionsDir / MCPFile
Instructions()    // 注入 system prompt 的环境说明
```

子类只需实现两个钩子：

```go
Provision(ctx) (backend.Backend, error)  // 创建/连接环境，返回 backend
Teardown(ctx) error                      // 释放环境
```

`LocalWorkspace` 在宿主机目录上绑定 `LocalBackend`。未来接入容器或远端
沙箱时，工作区子类负责环境的创建/连接，并把 backend 的工作目录暴露给
工具即可；若文件不再位于宿主机，应把 `HostPaths()` 设为 `false`，这样
rewind 等依赖宿主路径的功能会自动跳过。

工具通过 `workspace.ListTools` 获得 backend，例如：

```go
&tools.ReadTool{WorkspaceDir: workdir, Backend: be}
```

工具内部统一走 `be.Abs` / `be.ReadFile` / `be.WriteFile` / `be.Exec`，
不再直接使用 `os.*` 或 `os/exec`。没有注入 backend 的工具会回退到
`LocalBackend(WorkspaceDir)`，因此既有调用方式保持兼容。

`run_in_background` 是可选能力：只有实现 `backend.BackgroundStarter`
的后端支持（目前是 `LocalBackend`）；其他后端会返回明确错误。

## 配置

```yaml
workspace:
  backend: "local"   # 目前仅 local；容器/远端后端在其上扩展
```

- `backend: local`：与之前行为一致，直接在宿主机执行。

## 已知边界

- 后台命令、rewind 快照依赖 backend 能力：后台需要
  `BackgroundStarter`，快照需要 `HostPaths() == true`，否则 rewind 会跳过。
- 容器/远端后端尚未实现；按上面的 3 个原语补齐即可，工作区层无需改动。
