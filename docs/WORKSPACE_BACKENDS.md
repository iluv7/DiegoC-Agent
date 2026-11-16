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
- `DockerBackend`：通过 `docker exec` / `cat` / 标准输入写文件；路径语义固定
  为 POSIX。`HostPaths()` 表示容器工作区是否是宿主机路径的 bind mount。
- `K8sBackend`：通过 `kubectl exec` / `cat` / 标准输入读写 pod 内文件；
  `kubectl exec` 没有 `-w`，需要 cwd 时用 `sh -c 'cd "$1" && exec "$@"'` 包一层。

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

`LocalWorkspace` 在宿主机目录上绑定 `LocalBackend`；`DockerWorkspace`
负责容器的 inspect / run / start / rm，并把工作区以**同路径 bind mount**
挂进容器，因此：

- 系统提示里的工作区路径在容器内外一致；
- `AGENTS.md`、`.agent_plan.md`、笔记文件等仍然直接可见；
- 未来接入真正隔离的远端沙箱时，只需把 `HostPaths` 设为 `false`。

`K8sWorkspace` 负责 pod 的 get / apply / delete 和 Running 轮询：

- 默认挂 `emptyDir`，设置 `persistent_volume_claim` 时改挂已有 PVC；
- pod 已 Running 时直接复用，Failed/Succeeded 时删除重建；
- `HostPaths()` 为 false，文件只在 pod 内，系统提示会使用 pod 的 workdir
  而不是宿主机目录，rewind/diff 会自动跳过。

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
  backend: "local"          # local | docker | k8s
  docker:
    container: "diegoc-agent-workspace"
    image: "debian:bookworm-slim"
    remove_on_close: false  # 默认保留容器，下次运行直接复用
  k8s:
    namespace: "default"
    pod: "diegoc-agent-workspace"
    container: "agent"
    image: "debian:bookworm-slim"
    workdir: "/workspace"
    persistent_volume_claim: ""   # 设置后挂已有 PVC，否则用 emptyDir
    image_pull_policy: "IfNotPresent"
    remove_on_close: false        # 默认保留 pod，下次运行直接复用
    start_timeout_seconds: 120
```

- `backend: local`：与之前行为一致，直接在宿主机执行。
- `backend: docker`：首次运行创建容器并在其中执行命令；工作区目录以相同
  绝对路径 mount 进容器。`remove_on_close: true` 时退出会 `docker rm -f`。
- `backend: k8s`：需要本机已配置好 `kubectl`（或用 `kubeconfig` / `context`
  指定）。首次运行 `kubectl apply` 创建 pod，之后复用；`remove_on_close: true`
  时退出会 `kubectl delete pod`。
- Docker 后端目前不支持 Windows 宿主（同路径 bind mount 不成立）。

## 已知边界

- `docker` 后端使用 `sh -c`（镜像保证有 `/bin/sh`）；依赖 bash 扩展语法的
  命令请显式调用 `bash -c`，或把镜像换成带 bash 的基础镜像。
- `k8s` 后端的文件只存在于 pod 内：宿主机上的 `AGENTS.md`、`.agent_plan.md`
  不会被加载/同步，`/diff`、rewind 等宿主侧 UI 功能不适用；需要持久化时
  挂 PVC。
- `kubectl exec` 超时只能终止本地 kubectl 进程，远端命令不保证被杀死。
- 后台命令、rewind 快照依赖 backend 能力：后台需要
  `BackgroundStarter`，快照需要 `HostPaths() == true`，否则 rewind 会跳过。
- 云沙箱（E2B 等）尚未实现；按上面的 3 个原语补齐即可，工作区层无需改动。
