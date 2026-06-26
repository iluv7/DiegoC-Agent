package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"bufio"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"diegoc-agent/internal/agent"
	"diegoc-agent/internal/config"
	"diegoc-agent/internal/logger"
	"diegoc-agent/internal/llm"
	"diegoc-agent/internal/permission"
	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"

	"github.com/peterh/liner"
	"golang.org/x/term"
)

const version = "0.1.0"
const appName = "DiegoC Agent"

// ANSI colors and styles for terminal UI (empty when stdout is not a TTY)
var (
	cReset, cBold, cDim, cCyan, cGreen, cYellow, cBlue, cMagenta, cRed string
	cBrightCyan, cBrightBlue, cBrightGreen, cBrightYellow, cBrightWhite string
)

func init() {
	if term.IsTerminal(int(os.Stdout.Fd())) {
		cReset, cBold, cDim = "\033[0m", "\033[1m", "\033[2m"
		cCyan, cGreen, cYellow, cBlue, cMagenta, cRed = "\033[36m", "\033[32m", "\033[33m", "\033[34m", "\033[35m", "\033[31m"
		cBrightCyan = "\033[96m"
		cBrightBlue = "\033[94m"
		cBrightGreen = "\033[92m"
		cBrightYellow = "\033[93m"
		cBrightWhite = "\033[97m"
	}
}

func main() {
	workspace := flag.String("workspace", "", "Workspace directory (default: current directory)")
	showVersion := flag.Bool("version", false, "Show version")
	flag.Parse()

	// Ensure MCP connections are cleaned up on exit
	defer tools.CleanupMCPConnections()

	if *showVersion {
		fmt.Printf("%s %s\n", appName, version)
		os.Exit(0)
	}
	if flag.NArg() >= 1 && flag.Arg(0) == "log" {
		var filename string
		if flag.NArg() >= 2 {
			filename = flag.Arg(1)
		}
		runLogSubcommand(filename)
		os.Exit(0)
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Configuration error: %v\n", err)
		if os.IsNotExist(err) || strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "valid API key") {
			printConfigHelp()
		}
		os.Exit(1)
	}

	provider := schema.ProviderAnthropic
	if cfg.LLM.Provider == "openai" {
		provider = schema.ProviderOpenAI
	}

	retry := llm.RetryConfig{
		Enabled:         cfg.LLM.Retry.Enabled,
		MaxRetries:      cfg.LLM.Retry.MaxRetries,
		InitialDelay:    time.Duration(cfg.LLM.Retry.InitialDelay * float64(time.Second)),
		MaxDelay:        time.Duration(cfg.LLM.Retry.MaxDelay * float64(time.Second)),
		ExponentialBase: cfg.LLM.Retry.ExponentialBase,
	}

	client, err := llm.NewClient(cfg.LLM.APIKey, cfg.LLM.APIBase, cfg.LLM.Model, provider, retry)
	if err != nil {
		fmt.Fprintf(os.Stderr, "LLM client error: %v\n", err)
		os.Exit(1)
	}

	// Default fallback aligned with Mini-Agent: when no system_prompt file is found
	systemPrompt := "You are DiegoC Agent, an intelligent assistant powered by MiniMax that can help users complete various tasks."
	if p := config.FindConfigFile(cfg.Agent.SystemPromptPath); p != "" {
		if b, e := os.ReadFile(p); e == nil {
			systemPrompt = string(b)
		}
	}

	workspaceDir := cfg.Agent.WorkspaceDir
	if *workspace != "" {
		workspaceDir = *workspace
	}
	workspaceDir, _ = filepath.Abs(expandHome(workspaceDir))
	_ = os.MkdirAll(workspaceDir, 0755)

	toolList, skillLoader := buildTools(cfg, workspaceDir)
	if skillLoader != nil {
		meta := skillLoader.GetSkillsMetadataPrompt()
		systemPrompt = strings.ReplaceAll(systemPrompt, "{SKILLS_METADATA}", meta)
	} else {
		systemPrompt = strings.ReplaceAll(systemPrompt, "{SKILLS_METADATA}", "")
	}
	workspaceInfo := "\n\n## Current Workspace\nYou are working in: `" + workspaceDir + "`\n"
	systemPrompt += workspaceInfo
	ag := agent.New(client, systemPrompt, cfg.Agent.MaxSteps, cfg.Agent.TokenLimit, toolList)
		agentLogger := logger.New()
	ag.Logger = agentLogger

	runInteractive(ag, workspaceDir, agentLogger)
}

func buildTools(cfg *config.Config, workspaceDir string) ([]tools.Tool, *tools.SkillLoader) {
	var list []tools.Tool
	var skillLoader *tools.SkillLoader

	if cfg.Tools.EnableFileTools {
		list = append(list,
			&tools.ReadTool{WorkspaceDir: workspaceDir},
			&tools.WriteTool{WorkspaceDir: workspaceDir},
			&tools.EditTool{WorkspaceDir: workspaceDir},
		)
	}
	if cfg.Tools.EnableBash {
		list = append(list,
			&tools.BashTool{WorkspaceDir: workspaceDir},
			&tools.BashOutputTool{},
			&tools.BashKillTool{},
		)
	}
	if cfg.Tools.EnableNote {
		memoryFile := filepath.Join(workspaceDir, ".agent_memory.json")
		list = append(list,
			&tools.RecordNoteTool{MemoryFile: memoryFile},
			&tools.RecallNoteTool{MemoryFile: memoryFile},
		)
	}
	if cfg.Tools.EnableSkills {
		skillsDir := resolveSkillsDir(cfg.Tools.SkillsDir)
		loader := tools.NewSkillLoader(skillsDir)
		skills := loader.DiscoverSkills()
		if len(skills) > 0 {
			list = append(list, &tools.GetSkillTool{Loader: loader})
			skillLoader = loader
		}
	}
	if cfg.Tools.EnableMCP {
		mcpPath := config.FindConfigFile(cfg.Tools.MCPConfigPath)
		if mcpPath != "" {
			mcpTools, err := tools.LoadMCPTools(mcpPath, tools.MCPTimeoutConfig{
				ConnectTimeout: cfg.Tools.MCP.ConnectTimeout,
				ExecuteTimeout: cfg.Tools.MCP.ExecuteTimeout,
				SSEReadTimeout: cfg.Tools.MCP.SSEReadTimeout,
			})
			if err == nil && len(mcpTools) > 0 {
				list = append(list, mcpTools...)
			}
		}
	}
	return list, skillLoader
}

func resolveSkillsDir(configured string) string {
	abs := filepath.Clean(configured)
	if filepath.IsAbs(abs) {
		return abs
	}
	cwd, _ := os.Getwd()
	p := filepath.Join(cwd, abs)
	if info, err := os.Stat(p); err == nil && info.IsDir() {
		return p
	}
	return p
}

func expandHome(p string) string {
	if len(p) >= 2 && p[:2] == "~/" {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}

func printConfigHelp() {
	fmt.Println()
	fmt.Println("Configuration search order:")
	fmt.Println("  1) ./config/config.yaml (development)")
	fmt.Println("  2) ~/.diegoc-agent/config/config.yaml (user)")
	fmt.Println("  3) <executable dir>/config/config.yaml")
	fmt.Println()
	fmt.Println("Quick setup:")
	fmt.Println("  mkdir -p ~/.diegoc-agent/config")
	fmt.Println("  cp config/config-example.yaml ~/.diegoc-agent/config/config.yaml")
	fmt.Println("  # Edit ~/.diegoc-agent/config/config.yaml and add your API key")
	fmt.Println()
}

func runLogSubcommand(filename string) {
	l := logger.New()
	logDir := l.LogDir()
	fmt.Printf("\n%s📁 Log directory: %s%s\n", cBrightCyan, logDir, cReset)
	info, err := os.Stat(logDir)
	if err != nil || !info.IsDir() {
		fmt.Printf("%sLog directory does not exist or is not a directory.%s\n", cRed, cReset)
		return
	}
	entries, err := os.ReadDir(logDir)
	if err != nil {
		fmt.Printf("%sError reading log dir: %v%s\n", cRed, err, cReset)
		return
	}
	var logFiles []os.DirEntry
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".log") {
			logFiles = append(logFiles, e)
		}
	}
	sort.Slice(logFiles, func(i, j int) bool {
		ii, _ := logFiles[i].Info()
		jj, _ := logFiles[j].Info()
		return ii.ModTime().After(jj.ModTime())
	})
	if filename == "" {
		fmt.Printf("%s%s\n", cDim, strings.Repeat("─", 60)+cReset)
		fmt.Printf("%sAvailable log files (newest first):%s\n", cBold+cBrightYellow, cReset)
		max := 10
		if len(logFiles) < max {
			max = len(logFiles)
		}
		for i := 0; i < max; i++ {
			inf, _ := logFiles[i].Info()
			fmt.Printf("  %s%2d.%s %s  %s(modified: %s)%s\n", cGreen, i+1, cReset, cBrightWhite+logFiles[i].Name()+cReset, cDim, inf.ModTime().Format("2006-01-02 15:04:05"), cReset)
		}
		if len(logFiles) > 10 {
			fmt.Printf("  %s... and %d more%s\n", cDim, len(logFiles)-10, cReset)
		}
		fmt.Printf("%s%s%s\n", cDim, strings.Repeat("─", 60), cReset)
		return
	}
	path := filepath.Join(logDir, filename)
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Printf("%sLog file not found or unreadable: %s%s\n", cRed, path, cReset)
		return
	}
	fmt.Printf("\n%s📄 Reading: %s%s\n", cBrightCyan, path, cReset)
	fmt.Printf("%s%s\n", cDim, strings.Repeat("─", 80)+cReset)
	fmt.Println(string(data))
	fmt.Printf("%s%s%s\n", cDim, strings.Repeat("─", 80), cReset)
}

var slashCommands = []string{"/help", "/clear", "/history", "/stats", "/log", "/exit", "/quit", "/q"}

func runInteractive(ag *agent.Agent, workspaceDir string, agentLogger *logger.AgentLogger) {
	sessionStart := time.Now()
	printBanner(workspaceDir)

	state := liner.NewLiner()
	defer state.Close()
	state.SetCtrlCAborts(true)
	if home, err := os.UserHomeDir(); err == nil {
		historyPath := filepath.Join(home, ".diegoc-agent", ".history")
		_ = os.MkdirAll(filepath.Dir(historyPath), 0755)
		if f, err := os.Open(historyPath); err == nil {
			state.ReadHistory(f)
			f.Close()
		}
		defer func() {
			if f, err := os.Create(filepath.Join(home, ".diegoc-agent", ".history")); err == nil {
				state.WriteHistory(f)
				f.Close()
			}
		}()
	}
	state.SetCompleter(func(line string) []string {
		var out []string
		for _, c := range slashCommands {
			if strings.HasPrefix(c, strings.ToLower(line)) {
				out = append(out, c)
			}
		}
		return out
	})

	for {
		// liner 不接受含 ANSI 转义的 prompt，会报 "invalid prompt"，故用纯文本
		line, err := state.Prompt("You › ")
		if err != nil {
			if err == liner.ErrPromptAborted {
				fmt.Printf("\n%s👋 Interrupt, exiting...%s\n\n", cBrightYellow, cReset)
				printStats(ag, sessionStart)
			} else {
				fmt.Fprintf(os.Stderr, "%sInput error: %v%s\n", cRed, err, cReset)
				fmt.Fprintf(os.Stderr, "Tip: run in a terminal with a TTY (e.g. system Terminal.app or iTerm), not from a non-interactive environment.\n")
			}
			break
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "/") {
			cmd := strings.ToLower(line)
			if cmd == "/exit" || cmd == "/quit" || cmd == "/q" {
				fmt.Printf("\n%s👋 Goodbye! Thanks for using %s%s%s\n\n", cBrightYellow, cBold, appName, cReset)
				printStats(ag, sessionStart)
				return
			}
			if cmd == "/help" {
				printHelp()
				continue
			}
			if cmd == "/clear" {
				oldCount := len(ag.Messages)
				if oldCount > 1 {
					ag.Messages = ag.Messages[:1]
					fmt.Printf("%s✅ Cleared %d messages, new session.%s\n\n", cGreen, oldCount-1, cReset)
				} else {
					fmt.Printf("%sHistory already empty.%s\n\n", cDim, cReset)
				}
				continue
			}
			if cmd == "/history" {
				fmt.Printf("\n%sCurrent session message count: %s%s%d%s\n\n", cBrightCyan, cReset, cBold, len(ag.Messages), cReset)
				continue
			}
			if cmd == "/stats" {
				printStats(ag, sessionStart)
				continue
			}
			if cmd == "/log" || strings.HasPrefix(cmd, "/log ") {
				parts := strings.SplitN(line, " ", 2)
				if len(parts) == 1 {
					showLogDirectory(agentLogger)
				} else {
					readLogFile(agentLogger.LogDir(), strings.Trim(strings.TrimSpace(parts[1]), "\"'"))
				}
				continue
			}
			fmt.Printf("%s❌ Unknown command: %s%s (type %s/help%s)\n\n", cRed, line, cReset, cBrightGreen, cReset)
			continue
		}
		if line == "exit" || line == "quit" || line == "q" {
			fmt.Printf("\n%s👋 Goodbye! Thanks for using %s%s%s\n\n", cBrightYellow, cBold, appName, cReset)
			printStats(ag, sessionStart)
			return
		}

		ag.AddUserMessage(line)
		fmt.Println("\nAgent › Thinking... (Esc to cancel)")
		ctx, cancel := context.WithCancel(context.Background())

		// HITL 通道
		hitlReqCh := make(chan permission.HITLConfirmRequest, 1)
		hitlRespCh := make(chan permission.HITLConfirmResponse, 1)

		// Esc 取消监听
		escDone := make(chan struct{})
		go func() {
			runWithEscCancel(ctx, cancel, escDone)
		}()

		// Agent 跑在另一个 goroutine
		type agentResult struct {
			out string
			err error
		}
		agentDone := make(chan agentResult, 1)
		go func() {
			out, err := ag.RunWithHITL(ctx, hitlRespCh, hitlReqCh)
			agentDone <- agentResult{out, err}
		}()

		// 主循环：处理 Agent 的 HITL 请求或等待完成
		var out string
		var runErr error
	agentLoop:
		for {
			select {
				case req := <-hitlReqCh:
					for _, tc := range req.ToolCalls {
						fmt.Printf("\n%s⚠️  Agent wants to run: %s%s%s%s\n", cBrightYellow, cBold, cBrightCyan, tc.Name, cReset)
						fmt.Printf("   Args: %v\n", tc.Args)

						fmt.Printf("   %s[y=yes / a=always allow / n=no]%s ", cDim, cReset)
						reader := bufio.NewReader(os.Stdin)
						text, _ := reader.ReadString('\n')
						answer := strings.TrimSpace(strings.ToLower(text))

						switch {
						case answer == "y" || answer == "yes":
							// 只这次允许
							hitlRespCh <- permission.HITLConfirmResponse{
								ReplyID: req.ReplyID,
								Results: []permission.ToolConfirmResult{{
									ToolCall:  tc,
									Confirmed: true,
								}},
							}
							fmt.Printf("   %s✅ Allowed (this time)%s\n", cGreen, cReset)

						case answer == "a" || answer == "always":
							// 永远允许 → 生成规则
							rules := makeAllowRule(tc)
							hitlRespCh <- permission.HITLConfirmResponse{
								ReplyID: req.ReplyID,
								Results: []permission.ToolConfirmResult{{
									ToolCall:  tc,
									Confirmed: true,
									Rules:     rules,
								}},
							}
							fmt.Printf("   %s✅ Allowed (always)%s\n", cGreen, cReset)
							if len(rules) > 0 {
								fmt.Printf("   %s📌 Rule added: %s %s → %s%s\n",
									cDim, rules[0].ToolName, rules[0].RuleContent, rules[0].Behavior, cReset)
							}

						default:
							// n 或任何其他 → 拒绝
							hitlRespCh <- permission.HITLConfirmResponse{
								ReplyID: req.ReplyID,
								Results: []permission.ToolConfirmResult{{
									ToolCall:  tc,
									Confirmed: false,
								}},
							}
							fmt.Printf("   %s❌ Denied%s\n", cRed, cReset)
						}
					}

			case result := <-agentDone:
				out = result.out
				runErr = result.err
				break agentLoop

			case <-ctx.Done():
				break agentLoop
			}
		}

		cancel()
		<-escDone

		if runErr != nil {
			if runErr == context.Canceled {
				fmt.Printf("\n%s⚠️  Cancelled by user (Esc).%s\n", cBrightYellow, cReset)
			} else {
				var retryErr *llm.RetryExhaustedError
				if errors.As(runErr, &retryErr) {
					fmt.Fprintf(os.Stderr, "%s❌ LLM call failed after %d retries\nLast error: %v%s\n", cRed, retryErr.Attempts, retryErr.LastErr, cReset)
				} else {
					fmt.Fprintf(os.Stderr, "%s❌ Error: %v%s\n", cRed, runErr, cReset)
				}
			}
		} else if out != "" {
			fmt.Printf("\n%s🤖 Agent%s %s›%s\n%s\n", cBold, cReset, cDim, cReset, formatMarkdownForTerminal(out))
		}
		if path := agentLogger.GetLogFilePath(); path != "" {
			fmt.Printf("\n%s📝 Log file: %s%s%s\n", cDim, cReset, path, cReset)
		}
		if line != "" {
			state.AppendHistory(line)
		}
	}
}

func printBanner(workspaceDir string) {
	const boxW = 58
	title := appName + " – Multi-turn Interactive Session"
	sep := strings.Repeat("═", boxW)
	pad := boxW - 2 - len(title) // space before title + title + spaces to fill
	if pad < 0 {
		pad = 0
	}
	fmt.Printf("\n%s╔%s╗%s\n", cBold+cBrightCyan, sep, cReset)
	fmt.Printf("%s║%s %s%s%*s %s║%s\n", cBold+cBrightCyan, cReset, cBold, title, pad, "", cBold+cBrightCyan, cReset)
	fmt.Printf("%s╚%s╝%s\n\n", cBold+cBrightCyan, sep, cReset)
	fmt.Printf("%sWorkspace:%s %s\n", cDim, cReset, workspaceDir)
	fmt.Printf("%sType %s/help%s for commands, %s/exit%s to quit.%s\n\n", cDim, cBrightGreen, cDim, cBrightGreen, cDim, cReset)
}

// formatMarkdownForTerminal 只做 ** 粗体转 ANSI，不改动换行和列表
func formatMarkdownForTerminal(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 64)
	bold := false
	i := 0
	for i < len(s) {
		if i+1 < len(s) && s[i:i+2] == "**" {
			if bold {
				b.WriteString(cReset)
			} else {
				b.WriteString(cBold)
			}
			bold = !bold
			i += 2
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	if bold {
		b.WriteString(cReset)
	}
	return b.String()
}

func printHelp() {
	fmt.Printf(`
%sAvailable Commands:%s
  %s/help%s      - Show this help
  %s/clear%s     - Clear session history (keep system prompt)
  %s/history%s   - Show current session message count
  %s/stats%s     - Show session statistics
  %s/log%s       - Show log directory and recent files
  %s/log <file>%s - Read a specific log file
  %s/exit%s      - Exit (also: /quit, /q)

%sKeyboard:%s
  %sEsc%s        - Cancel current agent execution
  %sCtrl+C%s     - Exit

`, cBold+cBrightYellow, cReset,
		cBrightGreen, cReset, cBrightGreen, cReset, cBrightGreen, cReset, cBrightGreen, cReset, cBrightGreen, cReset, cBrightGreen, cReset, cBrightGreen, cReset,
		cBold+cBrightYellow, cReset, cBrightCyan, cReset, cBrightCyan, cReset)
}

func printStats(ag *agent.Agent, sessionStart time.Time) {
	d := time.Since(sessionStart)
	hours := int(d.Hours())
	mins := int(d.Minutes()) % 60
	secs := int(d.Seconds()) % 60
	var user, assistant, tool int
	for _, m := range ag.Messages {
		switch m.Role {
		case "user":
			user++
		case "assistant":
			assistant++
		case "tool":
			tool++
		}
	}
	fmt.Printf("%sSession statistics:%s\n", cBold+cBrightCyan, cReset)
	fmt.Printf("%s%s%s\n", cDim, strings.Repeat("─", 40), cReset)
	fmt.Printf("  Duration: %02d:%02d:%02d\n", hours, mins, secs)
	fmt.Printf("  Messages: %d (user: %s%d%s, assistant: %s%d%s, tool: %s%d%s)\n",
		len(ag.Messages), cBrightGreen, user, cReset, cBrightBlue, assistant, cReset, cYellow, tool, cReset)
	fmt.Printf("  Tools: %d\n", len(ag.Tools))
	if ag.APITotalTokens > 0 {
		fmt.Printf("  API tokens used: %s%d%s\n", cMagenta, ag.APITotalTokens, cReset)
	}
	fmt.Printf("%s%s%s\n\n", cDim, strings.Repeat("─", 40), cReset)
}

func showLogDirectory(l *logger.AgentLogger) {
	logDir := l.LogDir()
	fmt.Printf("\n%s📁 Log directory: %s%s\n\n", cBrightCyan, logDir, cReset)
	entries, err := os.ReadDir(logDir)
	if err != nil {
		fmt.Printf("%sLog directory does not exist or is not readable.%s\n\n", cRed, cReset)
		return
	}
	var logFiles []os.DirEntry
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".log") {
			logFiles = append(logFiles, e)
		}
	}
	sort.Slice(logFiles, func(i, j int) bool {
		ii, _ := logFiles[i].Info()
		jj, _ := logFiles[j].Info()
		return ii.ModTime().After(jj.ModTime())
	})
	fmt.Printf("%s%s\n", cDim, strings.Repeat("─", 60)+cReset)
	fmt.Printf("%sAvailable log files (newest first):%s\n", cBold+cBrightYellow, cReset)
	for i := 0; i < len(logFiles) && i < 10; i++ {
		inf, _ := logFiles[i].Info()
		fmt.Printf("  %s%2d.%s %s  %s(%s)%s\n", cGreen, i+1, cReset, cBrightWhite+logFiles[i].Name()+cReset, cDim, inf.ModTime().Format("2006-01-02 15:04:05"), cReset)
	}
	if len(logFiles) > 10 {
		fmt.Printf("  %s... and %d more%s\n", cDim, len(logFiles)-10, cReset)
	}
	fmt.Printf("%s%s%s\n\n", cDim, strings.Repeat("─", 60), cReset)
}

func readLogFile(logDir, filename string) {
	path := filepath.Join(logDir, filename)
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Printf("%s❌ Log file not found: %s%s\n\n", cRed, path, cReset)
		return
	}
	fmt.Printf("\n%s📄 Reading: %s%s\n", cBrightCyan, path, cReset)
	fmt.Printf("%s%s\n", cDim, strings.Repeat("─", 80)+cReset)
	fmt.Println(string(data))
	fmt.Printf("%s%s%s\n\n", cDim, strings.Repeat("─", 80), cReset)
}

func runWithEscCancel(ctx context.Context, cancel context.CancelFunc, done chan struct{}) {
	defer close(done)
	f, ok := stdinFile()
	if !ok {
		return
	}
	fd := int(f.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return
	}
	defer term.Restore(fd, oldState)
	ch := make(chan byte, 32)
	go func() {
		buf := make([]byte, 1)
		for {
			n, _ := f.Read(buf)
			if n > 0 {
				select {
				case ch <- buf[0]:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case b := <-ch:
			if b == 0x1b {
				fmt.Printf("\n%s⏹  Esc pressed, cancelling...%s\n", cBrightYellow, cReset)
				cancel()
				return
			}
		}
	}
}

func stdinFile() (*os.File, bool) {
	f := os.Stdin
	if f == nil {
		return nil, false
	}
	stat, err := f.Stat()
	if err != nil {
		return nil, false
	}
	if stat.Mode()&os.ModeCharDevice == 0 {
		return nil, false
	}
	return f, true
}


// makeAllowRule 根据工具调用生成"始终允许"规则。
func makeAllowRule(tc permission.PendingToolCall) []permission.Rule {
	switch tc.Name {
	case "bash":
		cmd, _ := tc.Args["command"].(string)
		if cmd == "" {
			return nil
		}
		return []permission.Rule{{
			ToolName:    "bash",
			RuleContent: cmd,
			Behavior:    permission.BehaviorALLOW,
			Source:      "userConfirm",
		}}
	case "write_file", "edit_file":
		path, _ := tc.Args["path"].(string)
		if path == "" {
			return nil
		}
		return []permission.Rule{{
			ToolName:    tc.Name,
			RuleContent: filepath.Dir(path) + "/**",
			Behavior:    permission.BehaviorALLOW,
			Source:      "userConfirm",
		}}
	case "read_file":
		path, _ := tc.Args["path"].(string)
		if path == "" {
			return nil
		}
		return []permission.Rule{{
			ToolName:    "read_file",
			RuleContent: filepath.Dir(path) + "/**",
			Behavior:    permission.BehaviorALLOW,
			Source:      "userConfirm",
		}}
	default:
		return []permission.Rule{{
			ToolName: tc.Name,
			Behavior: permission.BehaviorALLOW,
			Source:   "userConfirm",
		}}
	}
}
