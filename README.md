# 本项目基于 [MiniMax-AI/Mini-Agent](https://github.com/MiniMax-AI/Mini-Agent) 二次开发，遵循 MIT 协议。

# DiegoC Agent 使用指南 (macOS)

> 从 GitHub 克隆本项目后，按照以下步骤配置和运行

---

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
├── cmd/diegoc-agent/    # 主程序入口
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
