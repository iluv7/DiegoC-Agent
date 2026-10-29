# 本项目基于 [MiniMax-AI/Mini-Agent](https://github.com/MiniMax-AI/Mini-Agent) 二次开发，遵循 MIT 协议。

# DiegoC Agent 使用指南 (macOS)

> 从 GitHub 克隆本项目后，按照以下步骤配置和运行

---

## 环境要求

- **macOS** (本指南仅适用于 macOS)
- **Go 1.24.0** 或更高版本
- **Node.js** (如果需要使用 MCP 工具)
- **Git**

---

### 初始化 Claude Skills (一定要做 这会让Agent变得很强大)

本项目通过 **git submodule** 集成 [Anthropic 官方 skills 仓库](https://github.com/anthropics/skills)，克隆仓库后执行一次：

```bash
cd DiegoC-Agent
git submodule update --init --recursive
```

会将 Claude 的 skills 拉取到 `skills/` 目录。配置中 `skills_dir` 默认为 `./skills`，`enable_skills: true` 时 Agent 会扫描该目录下所有 `SKILL.md` 并注入元数据，可通过 `get_skill(skill_name)` 按需加载完整技能。


## 第一步：克隆项目

```bash
git clone https://github.com/your-repo/diegoc-agent.git
cd diegoc-agent
```

---

## 第二步：安装 Go

如果你还没有安装 Go，请使用 Homebrew 安装：

```bash
# 安装 Homebrew (如果没有)
/bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"

# 安装 Go
brew install go

# 验证安装
go version
```

---

## 第三步：配置 API Key

### 1. 复制配置示例文件

```bash
cp config/config-example.yaml config/config.yaml
```

### 2. 编辑配置文件

打开 `config/config.yaml`，填入你的 API Key：

```yaml
# ===== LLM Configuration =====

# 选择 LLM 提供商: anthropic / openai / minimax
provider: "anthropic"  # anthropic / openai / minimax

# Anthropic (Claude)
api_base: "https://api.anthropic.com"
model: "claude-sonnet-4-20250514"

# OpenAI
# api_base: "https://api.openai.com/v1"
# model: "gpt-4o"

# MiniMax
# 国内: api_base: "https://api.minimaxi.com"
# 海外: api_base: "https://api.minimax.io"
# model: "MiniMax-M2.5"

api_key: "你的API密钥"

# 配置好该项目下的config.yaml后 如果想在mac的任意一个目录下都可以使用该agent 还需要复制一份config.yaml到用户目录下
mkdir -p ~/.diegoc-agent/config
cp config/config.yaml ~/.diegoc-agent/config/config.yaml
```

> 💡 **API Key 获取**：根据选择的 provider 从对应平台申请 API Key

---

## 第四步：(可选) 配置 MCP 工具

如果需要使用 MCP 工具（如 Git、Brave Search 等），编辑 `config/mcp.json`：

```bash
vim config/mcp.json
```

示例配置：

```json
{
    "mcpServers": {
        "git": {
            "description": "Git - 读取和操作 Git 仓库",
            "type": "stdio",
            "command": "npx",
            "args": ["-y", "@modelcontextprotocol/server-git", "--repository", "."],
            "disabled": false
        }
    }
}
```

> ⚠️ MCP 工具需要安装 Node.js：
> ```bash
> brew install node
> ```

---

## 第五步：构建项目

```bash
# 使用 Makefile 构建
make build

# 或者直接使用 Go 构建
go build -o diegoc-agent ./cmd/diegoc-agent
```

构建完成后，会在当前目录生成 `diegoc-agent` 可执行文件。

---

## 第六步：运行

### 交互式模式（默认）

```bash
./diegoc-agent
```

进入交互式对话界面，直接输入你的问题即可。

### 单任务模式

```bash
./diegoc-agent --task "帮我写一个Hello World程序"
```

### 指定工作目录

```bash
./diegoc-agent --workspace /path/to/your/project
```

### 查看版本

```bash
./diegoc-agent --version
```

---

## 常用命令汇总

| 命令 | 说明 |
|------|------|
| `make build` | 构建项目 |
| `./diegoc-agent` | 启动交互式模式 |
| `./diegoc-agent --task "你的任务"` | 执行单次任务 |
| `./diegoc-agent --version` | 查看版本 |

---

## 配置文件说明

配置文件加载顺序（优先级从高到低）：

1. `./config/config.yaml` (项目目录)
2. `~/.diegoc-agent/config/config.yaml` (用户目录)
3. 可执行文件同目录下的 `config/config.yaml`

---

## 常见问题

### 1. 提示 "Configuration error: valid API key required"

请确保已在 `config/config.yaml` 中正确配置 `api_key`。

### 2. MCP 工具无法连接

- 确认已安装 Node.js：`node --version`
- 确认 `config/mcp.json` 中对应的工具 `disabled` 为 `false`

### 3. Go 版本过低

```bash
# 升级 Go
brew upgrade go
```

---

## 项目结构

```
diegoc-agent/
├── cmd/diegoc-agent/     # CLI
│   ├── main.go          # 参数、子命令与启动流程
│   ├── bootstrap.go     # 客户端、工具、记忆与 Agent 组装
│   ├── interactive.go   # 交互循环、定时任务与执行事件
│   ├── commands.go      # 本地命令分发与执行输入转换
│   ├── terminal_input.go # 终端取消与输入辅助
│   └── *_ui.go          # 终端展示及各功能的命令处理
├── config/              # 配置文件
│   ├── config-example.yaml
│   ├── mcp-example.json
│   └── system_prompt.md
├── internal/            # 内部包
├── skills/              # 技能模块
├── workspace/           # 工作目录
├── Makefile             # 构建脚本
└── diegoc-agent         # 编译后的可执行文件
```

---

## 卸载

```bash
# 删除项目目录
rm -rf diegoc-agent

# 如果已安装到 PATH
sudo rm /usr/local/bin/diegoc-agent
```

---

祝使用愉快！ 🚀

## 会话回退（Rewind）

交互模式下，每次提交用户消息会创建检查点，最多保留最近 100 个。

```text
/rewind                       # 打开消息选择器，滚轮或上下键选择回退位置
/rewind 3                     # 回到第 3 条输入之前，恢复对话和文件
/rewind 3 conversation        # 仅恢复对话
/rewind 3 files               # 仅恢复文件，保留当前对话
```

输入 `/rewind` 后，默认选中最近一条用户消息；使用鼠标滚轮或上下方向键切换，支持 PageUp/PageDown、Home/End。回车选择消息，再选择恢复对话和文件、仅对话或仅文件，回车执行；Esc 或 Ctrl+C 取消。带 ID 的命令仍需输入 `y` 确认。恢复对话后会显示原始输入，可修改后重新提交。
文件检查点记录内置 `write_file`、`edit_file` 修改前的内容与权限；恢复时会删除这些工具新建的文件。如果文件在工具操作之后又被手动或外部工具修改，回退会停止，避免覆盖这些改动。符号链接文件不接受检查点写入。

检查点仅在当前进程中有效，`/clear` 会清空检查点和会话摘要；恢复对话会丢弃所选检查点及之后的检查点。只恢复文件会消费相应文件备份，保留对话检查点。Shell、MCP、外部服务操作、创建的空目录及长期记忆写入不回退。鼠标滚轮需要终端支持鼠标事件上报；也可使用方向键。此版本未提供双击 Esc 入口和跨进程恢复。

实现参考本地 Claude-Code 的 `src/utils/fileHistory.ts`（用户回合检查点、写前备份、标记不存在的文件）和 `src/screens/REPL.tsx` 的 `rewindConversationTo`（恢复到用户输入之前）。本项目使用独立的 Go 实现，并在检查点保存对话与摘要副本，以支持上下文压缩后的回退。

## Fork session（会话分叉与切换）

直接输入 `/fork`，立即复制当前上下文并进入新 session，无需任务，也不会自动请求模型。

```text
You [main] › /fork
You [session-1] › 在这个分支继续讨论
You [session-1] › /switch main
You [main] › 继续原来的对话
You [main] › /switch session-1
```

- `/fork`：从当前 session 创建并进入新 session，支持再次分叉。
- `/sessions` 或 `/switch`：用滚轮、方向键选择会话，回车切换，Esc 取消。
- `/switch main`：回到原始主会话。
- `/switch session-1`：回到指定 session（`/session ID` 也可）。

提示符显示当前会话 ID。切换后会显示该会话自己的对话；直接输入普通消息即可继续聊天。后续消息、摘要、权限规则、日志和新建的 rewind 检查点各自独立，`/clear`、`/rewind` 只影响当前 session 的对话状态。代码文件和磁盘记忆仍共享；会话只在当前进程内保留。

模型的 `Agent` 工具仍可创建后台任务；使用 `/tasks [fork-ID]`、`/send`、`/approve`、`/cancel` 管理。这与交互式 `/fork` 会话分叉是两个独立入口。

详见 [Fork session](docs/FORK_SESSION.md)。

交互式 fork session 最多 7 个（main 不计入，总计最多 8 个会话）。从 main 或已有 session 分叉均计入同一个上限；达到上限时不创建新会话，也不切换当前会话。

## Plan-and-Execute 模式

- 输入 `/plan` 后描述任务，Agent 使用同一个对话循环探索代码并生成方案；普通文件写入和状态变更命令会被拒绝，方案保存在工作目录的 `.agent_plan.md`。
- 输入 `/execute` 查看方案。只有输入 `y`/`yes` 才会恢复进入 Plan Mode 前的权限并开始执行；拒绝审批时仍留在 Plan Mode，可继续修订方案。
- 新需求可以直接在 Plan Mode 输入，Agent 应更新方案；执行中也可以按 `Esc` 取消当前运行，再以 `/plan` 重拟方案。

---

## 结构化提问（ask_user）

前台 Agent 遇到会影响方案的缺失需求或偏好时，可以调用 `ask_user` 暂停执行，显示一个问题和 2–4 个选项。使用鼠标滚轮或上下方向键选择，回车确认；聚焦选项时会显示说明。也可以选择「其他 / 自由输入」填写自己的答案，提交后 Agent 会接着执行。

- 可直接要求：「实现前先用选项问我希望使用哪种存储方式」。此功能由模型调用工具触发，无需斜杠命令。
- Plan Mode 中也可提问；提问结果只记录选择，不授予文件或 Shell 操作权限。
- 在选择器或自由输入中按 Esc / Ctrl+C，会取消当前执行，保留会话以便继续。空白答案不会提交。
- `ask_user` 随前台 session 的当前运行绑定，切换会话后不会沿用之前的输入通道。问题和答案会作为工具调用与结果进入当前对话和日志。
- 后台 Agent、没有交互处理器的调用以及无人值守模式不支持提问，会明确返回错误或拒绝，不直接读取终端。

## 上下文占用与手动压缩

- `/context`：本地估算当前上下文，分别显示系统提示、工具定义、历史摘要、对话及工具结果占用，以及配置预算的使用比例。不请求模型、不修改对话。这里的预算来自配置，并非自动识别的模型窗口；估算包含工具 schema，可能与供应商实际 token 数不同。
- `/compact`：立即把当前对话整理成结构化摘要，再继续聊天；无需等自动压缩触发。可输入 `/compact 保留接口约定、已修改文件和未完成任务` 指定保留重点。长对话分段处理，压缩调用产生的 API token 仍累计到 `/stats`。
- 压缩期间按 Esc 取消。调用失败、摘要无效或未能减少占用时，保留原始对话。只有当前 session 的对话和摘要会更新；文件、权限、Rewind 检查点及其他 session 均保留。
- `/stats` 显示累计 API 消耗，`/context` 显示当前上下文占用。累计消耗增加本身不会触发压缩。未启用可选记忆管线时，这两个命令仍可使用。

## 项目规则自动加载

在工作区根目录放置 UTF-8 编码的 `AGENTS.md`，启动时会自动把内容加入系统提示，并显示加载路径。例如：

```markdown
# 项目约定
- 使用中文回答。
- Go 文件使用 gofmt 格式化。
- 修改后运行 go test ./...。
```

- 读取位置是 `--workspace` 指定的目录；未指定时使用配置里的 `workspace_dir`（默认 `./workspace`）。本版本只加载该目录下的 `AGENTS.md`，不扫描父目录或子目录。
- 规则随当前 Agent 的系统提示保留，清空对话、Plan Mode、会话分叉、压缩和 Rewind 后仍然有效；修改文件后需重启才能加载新内容。
- 用户的明确要求优先于项目约定；项目规则不授予工具权限或改变执行模式。
- 缺失、空白文件会跳过。已存在的规则文件无法读取、不是普通文件或不是 UTF-8 文本时，会报告路径并停止启动。

## 查看改动（/diff）

```text
/diff          # 当前 session 最近一轮用户输入产生的文件修改
/diff 3        # 查看第 3 个检查点所记录的修改，ID 与 /rewind 相同
/diff git      # 查看工作区内 Git 已暂存、未暂存的改动，以及未跟踪文件路径
```

- 轮次视图复用 Rewind 保存的 `write_file` / `edit_file` 写前、写后快照，将同一轮的多次写入合并为最终差异。后续轮次、手动编辑、Bash 和 MCP 操作不会混入该轮；最近一轮没有修改时会明确提示，不回退显示旧轮次。
- 文本使用 unified diff 显示增删行，并展示权限变化；二进制文件只显示大小。查看快照无需位于 Git 仓库中，但生成文本 diff 需要安装 Git。
- `/diff git` 要求工作区位于 Git 仓库内，只展示该工作区及其子目录。已暂存、未暂存的差异分开展示；未跟踪文件仅列路径，不展示内容，并遵循 Git 的忽略规则。
- 查看改动不请求模型，不修改工作文件、Git 暂存区或会话。生成期间可按 Esc 取消，最长等待 30 秒。
- 轮次快照仅在当前进程中保留，最多最近 100 个。`/clear` 清空快照；只回退文件会消费对应备份。新 fork session 不继承已有文件检查点，后续修改各自记录；上下文压缩不会清除文件快照。

## 周期任务（/loop）

在交互式终端中，让同一会话按固定间隔重复执行一段任务：

```text
/loop 5m 检查当前 PR 的 CI，失败时分析并修复，全部通过后停止循环
/loop 1h 检查部署状态，完成后通知我并停止循环
/loop 检查构建进度
/loop list
/loop cancel loop-1
/loop cancel all
```

- `/loop [interval] [prompt]` 直接创建任务，不请求模型。间隔使用正整数加 `s`、`m`、`h`、`d`，秒向上取整到整分钟，最短 1 分钟，最长小于 7 天。省略间隔固定为 **10 分钟**；第一轮在一个间隔之后执行。
- 省略任务时，依次读取工作区 `.diegoc-agent/loop.md`、用户目录 `~/.diegoc-agent/loop.md`；都不存在时，默认继续对话里已请求但未完成的工作并检查相关 PR。默认文件在创建任务时读取，支持最多 25,000 字节的非空 UTF-8 文本；修改后需要取消并重建任务。显式传入任务时不读取默认文件。
- `/loop list` 显示当前会话的任务 ID、间隔、下次执行时间、过期时间和已执行次数。`/loop cancel ID` 或 `/loop cancel all` 取消当前会话的任务。任务的日期时间按运行机器的本地时区显示。
- 到期任务通过当前 Agent 的对话循环执行，保留会话上下文、项目规则、Plan Mode 和工具权限。需要批准或提问时仍使用原有交互。可直接对 Agent 说「每 5 分钟检查一次 CI」；模型可调用 `loop_create`、`loop_list`、`loop_cancel` 管理任务，也可在目标完成后取消当前循环。
- Agent 正忙或用户正在输入时延后执行；长任务错过的多个周期合并为一次，下次执行时间落在完成后的原定周期上，不并发启动多个前台 Agent。定时任务运行后会恢复尚未提交的输入及光标位置；自动触发的任务不进入输入历史。
- 每个会话最多 50 个任务，创建 7 天后过期。切换会话时只执行当前会话的任务，其他会话的任务等待切回；新 fork session 不继承父会话的任务。后台 `Agent` 工作线程不能使用循环工具。
- 定时执行时按 **Esc** 会取消本轮及对应循环；本轮执行出错也会停止该循环。`/clear` 取消当前会话的所有循环；`/rewind` 只回退对话/文件，不恢复或取消任务。退出程序后所有任务清除。

当前版本实现固定间隔的会话内循环，未实现 Claude 最新版的动态间隔选择、cron 日历表达式、跨进程恢复或云端调度。需要保持终端和进程运行，不会在电脑休眠或程序退出后继续执行。循环中的内容作为模型任务处理，不执行 `/exit`、`/clear` 等终端斜杠命令。

## 目标执行（/goal）

设置一个可验证的完成条件，Agent 立即开始工作，每次准备结束时通过独立模型调用检查是否达成：

```text
/goal 实现登录接口，补齐边界情况，并运行相关测试直到全部通过
/goal
/goal status
/goal cancel
/goal resume
```

- `/goal <条件>` 设置或替换当前会话的目标并立即执行。主 Agent 返回最终回答时才触发停止检查，执行工具的过程中不检查。未完成时，把评估理由加入同一轮上下文并继续执行；完成时清除活动目标，显示检查结果和最终回答。
- `/goal` 或 `/goal status` 查看状态、累计检查次数、主执行与评估调用报告的 token 数及最近理由。查看状态和取消不调用模型。完成或取消后可以查看最近结果；新设目标会重新计数。
- 评估调用使用独立系统指令，不提供工具，只接收目标、压缩摘要和对话证据；不接收主模型的思考内容。为限制评估输入，保留最近约 48 KB 的对话证据，单条内容最多约 12 KB、摘要最多约 6 KB，截断会明确标记。评估器依据已有证据判断，不会自行读取文件或执行测试。
- 可在 `config/config.yaml` 中配置 `goal.model`，选择当前 API 服务支持的小模型；它沿用主模型的 `api_key`、`api_base` 和 `provider`。省略时使用主模型执行独立评估调用，不自动选择小模型。配置示例：

  ```yaml
  goal:
    model: ""       # 改为同一 API 服务支持的评估模型名称
    max_checks: 10  # 每次执行最多检查次数，允许 1–100
  ```

- `goal.max_checks` 默认 10，每次执行仍受现有 `max_steps` 限制，不因继续执行而重置步骤预算。达到任一限制、评估调用超过 60 秒、返回无效 JSON、发生错误或需要用户输入时，暂停并保留目标；`/goal resume` 开始一次新的受限执行，累计计数保留。
- **Esc** 在主执行或评估阶段均可取消本轮，目标保留为暂停状态。普通消息不会自动恢复暂停目标；`/goal cancel` 丢弃目标。继续执行沿用现有工具权限、审批和 Plan Mode，目标和评估理由都不授予额外权限。
- 目标按前台会话隔离。新 `/fork` 会话只继承评估配置，不继承目标；切回原会话可查看和恢复其目标。后台 `Agent` 任务不继承目标。`/clear` 清除当前会话的目标和最近结果；回退对话会暂停活动目标，只回退文件不改变目标。目标只在当前进程内保存。
- `/loop` 负责按时间触发任务，`/goal` 在单次运行准备结束时检查完成条件；两者可同时使用，`/goal` 完成不会取消已创建的 `/loop` 任务。
