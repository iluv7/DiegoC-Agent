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
