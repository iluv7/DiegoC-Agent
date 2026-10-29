package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"diegoc-agent/internal/agent"
	"diegoc-agent/internal/config"
	"diegoc-agent/internal/llm"
	"diegoc-agent/internal/logger"
	"diegoc-agent/internal/loop"
	"diegoc-agent/internal/memory"
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
	cReset, cBold, cDim, cCyan, cGreen, cYellow, cBlue, cMagenta, cRed  string
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
	workspaceDir, err = filepath.Abs(expandHome(workspaceDir))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Workspace error: %v\n", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(workspaceDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Workspace error: %v\n", err)
		os.Exit(1)
	}

	toolList, skillLoader := buildTools(cfg, workspaceDir)
	var skillsMetadata string
	if skillLoader != nil {
		skillsMetadata = skillLoader.GetSkillsMetadataPrompt()
	}
	systemPrompt, instructionsPath, err := buildSystemPrompt(systemPrompt, workspaceDir, skillsMetadata)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Project instructions error: %v\n", err)
		os.Exit(1)
	}
	if instructionsPath != "" {
		fmt.Printf("Loaded project instructions: %s\n", selectorText(instructionsPath, 10000))
	}

	// Phase 11-13: Memory pipeline (optional, opt-in via config)
	var memManager *memory.Manager
	if cfg.Memory.Enabled {
		memCfg := memory.Config{
			WorkingDir:      cfg.Memory.WorkingDir,
			MaxInputLength:  cfg.Memory.MaxInputLength,
			CompactRatio:    cfg.Memory.CompactRatio,
			CompactReserve:  cfg.Memory.CompactReserve,
			ToolResultKeepN: cfg.Memory.ToolResultKeepN,
			Language:        cfg.Memory.Language,
			RetentionDays:   cfg.Memory.RetentionDays,
		}
		memManager = memory.NewManager(memCfg, client)
		if memManager != nil {
			// Phase 9-10: FileStore (ChromaDB + FTS5) + FileWatcher + memory_search tool.
			if fs := buildMemoryFileStore(cfg, memCfg.WorkingDir); fs != nil {
				memManager.SetFileStore(fs)
				if searchTool := memManager.SearchTool(); searchTool != nil {
					toolList = append(toolList, searchTool)
				}
				_ = memManager.Start(context.Background())
			}
		}
	}

	ag := agent.New(client, systemPrompt, cfg.Agent.MaxSteps, cfg.Agent.TokenLimit, toolList)
	goalClient, err := llm.NewClient(cfg.LLM.APIKey, cfg.LLM.APIBase, cfg.Goal.Model, provider, retry)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Goal evaluator client error: %v\n", err)
		os.Exit(1)
	}
	ag.ConfigureGoals(goalClient, cfg.Goal.Model, cfg.Goal.MaxChecks)
	agentLogger := logger.New()
	ag.Logger = agentLogger

	if memManager != nil {
		ag.SetMemoryManager(memManager)
		defer memManager.Close()
	}

	runInteractive(ag, workspaceDir, agentLogger)
}

func buildTools(cfg *config.Config, workspaceDir string) ([]tools.Tool, *tools.SkillLoader) {
	list := append([]tools.Tool{&tools.AskUserTool{}}, tools.LoopTools()...)
	var skillLoader *tools.SkillLoader

	if cfg.Tools.EnableFileTools {
		list = append(list,
			&tools.ReadTool{WorkspaceDir: workspaceDir},
			&tools.WriteTool{WorkspaceDir: workspaceDir},
			&tools.EditTool{WorkspaceDir: workspaceDir},
		)
	}
	list = append(list, &tools.WritePlanTool{WorkspaceDir: workspaceDir})
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

// buildMemoryFileStore constructs the Phase 9-10 memory index stack:
// embedding client + ChromaDB vector store + SQLite FTS5 keyword store.
// Returns nil if any component fails to initialize (memory pipeline
// still works without index/search).
func buildMemoryFileStore(cfg *config.Config, workingDir string) *memory.FileStore {
	embedder := memory.NewOpenAIEmbeddingClient(cfg.LLM.APIKey, cfg.LLM.APIBase, cfg.Memory.EmbeddingModel)
	chroma := memory.NewChromaClient(cfg.Memory.ChromaEndpoint)

	fileStoreDir := filepath.Join(workingDir, "file_store")
	if err := os.MkdirAll(fileStoreDir, 0755); err != nil {
		fmt.Printf("✗ Failed to create file_store dir: %v\n", err)
		return nil
	}
	kwStore, err := memory.NewFTS5Store(filepath.Join(fileStoreDir, "fts5_index.db"))
	if err != nil {
		fmt.Printf("✗ Failed to open FTS5 index: %v\n", err)
		return nil
	}

	return memory.NewFileStore(
		embedder,
		chroma,
		kwStore,
		memory.NewRuleTokenCounter(),
		"diego_memory",
		400, // chunkTokens
		80,  // overlap
	)
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

var slashCommands = []string{"/fork", "/sessions", "/switch", "/session", "/tasks", "/send", "/cancel", "/approve", "/help", "/rewind", "/diff", "/loop", "/goal", "/clear", "/history", "/stats", "/context", "/compact", "/plan", "/execute", "/log", "/exit", "/quit", "/q"}

func runInteractive(ag *agent.Agent, workspaceDir string, agentLogger *logger.AgentLogger) {
	runInteractiveWithLoops(ag, workspaceDir, agentLogger, loop.New(nil))
}

func runInteractiveWithLoops(ag *agent.Agent, workspaceDir string, agentLogger *logger.AgentLogger, loops *loop.Scheduler) {
	sessionStart := time.Now()
	forks := agent.NewForkManager(context.Background())
	defer forks.Close()
	ag.EnableForks(forks)
	sessions := agent.NewConversationSessions(ag)
	defer sessions.Close()
	histories := map[string][]string{}
	var inputHistory []string
	drafts := map[string]*inputDraft{}
	printBanner(workspaceDir)

	state := liner.NewLiner()
	defer state.Close()
	state.SetCtrlCAborts(true)
	if home, err := os.UserHomeDir(); err == nil {
		historyPath := filepath.Join(home, ".diegoc-agent", ".history")
		_ = os.MkdirAll(filepath.Dir(historyPath), 0755)
		if f, err := os.Open(historyPath); err == nil {
			state.ReadHistory(f)
			if data, err := os.ReadFile(historyPath); err == nil {
				inputHistory = strings.Split(strings.TrimSpace(string(data)), "\n")
			}
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

	histories["main"] = inputHistory
	for {
		current := sessions.Current()
		sessionStart = current.CreatedAt
		ag = current.Agent
		agentLogger = ag.Logger
		inputHistory = histories[current.ID]
		// liner 不接受含 ANSI 转义的 prompt，会报 "invalid prompt"，故用纯文本
		if drafts[current.ID] == nil {
			drafts[current.ID] = &inputDraft{}
		}
		line, err := promptSessionInputUntil(state, forks, inputHistory, "You ["+current.ID+"] › ", false, drafts[current.ID], func() bool { return loops.HasDue(current.ID) })
		var scheduled *loop.Task
		if errors.Is(err, errLoopDue) {
			task, ok := loops.TakeDue(current.ID)
			if !ok {
				continue
			}
			scheduled = &task
			line = fmt.Sprintf("[Scheduled loop %s, run %d]\n%s\n\nUse loop_cancel with id=%s if this recurring task is complete.", task.ID, task.Runs, task.Prompt, task.ID)
			err = nil
			fmt.Printf("\n[%s] Running in [%s]: %s\n", task.ID, current.ID, selectorText(task.Prompt, 160))
		}
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

		if scheduled == nil {
			inputHistory = append(inputHistory, line)
			histories[current.ID] = inputHistory
			state.AppendHistory(line)
			if prompt, handled := handleGoalCommand(ag, line); handled {
				if prompt == "" {
					continue
				}
				line = prompt
			}
			if handleLoopCommand(loops, current.ID, workspaceDir, line) {
				continue
			}
			if handleSessionCommand(sessions, line, selectRewindOption) {
				continue
			}
			if handleBackgroundCommand(forks, line) {
				continue
			}
			if handleContextCommand(ag, line) {
				continue
			}
			if handleDiffCommand(ag, workspaceDir, line) {
				continue
			}

			if strings.HasPrefix(line, "/") {
				cmd := strings.ToLower(line)
				if cmd == "/plan" {
					ag.SetPlanMode(true)
					fmt.Print("\nPlan Mode enabled: project edits and state-changing commands are blocked. Describe the task; the agent will save its plan to .agent_plan.md. Use /execute to review and approve it.\n\n")
					continue
				}
				if cmd == "/execute" {
					planPath := filepath.Join(workspaceDir, ".agent_plan.md")
					plan, err := os.ReadFile(planPath)
					if err != nil {
						fmt.Printf("\nNo plan found at %s. Enter Plan Mode with /plan and ask the agent to prepare one.\n\n", planPath)
						continue
					}
					fmt.Printf("\nProposed plan (%s):\n\n%s\n", planPath, string(plan))
					answer, err := state.Prompt("Approve this plan and begin execution? [y/N] ")
					if err != nil || (strings.ToLower(strings.TrimSpace(answer)) != "y" && strings.ToLower(strings.TrimSpace(answer)) != "yes") {
						fmt.Print("Plan not approved. Remain in Plan Mode; revise it or approve later with /execute.\n\n")
						continue
					}
					ag.SetPlanMode(false)
					fmt.Print("Plan approved. Starting execution...\n\n")
					line = "The plan in .agent_plan.md is approved. Execute it now, verify the result, and report what changed."
				} else if cmd == "/exit" || cmd == "/quit" || cmd == "/q" {
					fmt.Printf("\n%s👋 Goodbye! Thanks for using %s%s%s\n\n", cBrightYellow, cBold, appName, cReset)
					printStats(ag, sessionStart)
					return
				}
				if cmd == "/rewind" || strings.HasPrefix(cmd, "/rewind ") {
					handleRewind(ag, state, line)
					continue
				}
				if cmd == "/help" {
					printHelp()
					continue
				}
				if cmd == "/clear" {
					oldCount := len(ag.Messages)
					ag.ClearConversation()
					if count := loops.Cancel(current.ID, "all"); count > 0 {
						fmt.Printf("Cancelled %d loop(s) in [%s].\n", count, current.ID)
					}
					if oldCount > 1 {
						fmt.Printf("%s✅ Cleared %d messages and goal, new session.%s\n\n", cGreen, oldCount-1, cReset)
					} else {
						fmt.Printf("%sHistory already empty; goal cleared.%s\n\n", cDim, cReset)
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
				if cmd != "/execute" {
					fmt.Printf("%s❌ Unknown command: %s%s (type %s/help%s)\n\n", cRed, line, cReset, cBrightGreen, cReset)
					continue
				}
			}
			if line == "exit" || line == "quit" || line == "q" {
				fmt.Printf("\n%s👋 Goodbye! Thanks for using %s%s%s\n\n", cBrightYellow, cBold, appName, cReset)
				printStats(ag, sessionStart)
				return
			}
		}

		ag.AddUserMessage(line)
		fmt.Println("\nAgent › Thinking... (Esc to cancel)")
		ctx, cancel := context.WithCancel(context.Background())
		ctx = tools.WithLoopSession(ctx, loops, current.ID)
		goalEvents := make(chan agent.GoalEvent, 16)
		ctx = agent.WithGoalEvents(ctx, goalEvents)
		questionRequests := make(chan userQuestionRequest)
		ctx = tools.WithUserQuestionHandler(ctx, func(ctx context.Context, question tools.UserQuestion) (tools.UserAnswer, error) {
			return requestUserAnswer(ctx, questionRequests, question)
		})

		// HITL 通道
		hitlReqCh := make(chan permission.HITLConfirmRequest, 1)
		hitlRespCh := make(chan permission.HITLConfirmResponse, 1)

		// Only one terminal reader may be active; pause Esc polling for approvals.
		var escCancel context.CancelFunc
		var escDone chan struct{}
		startEsc := func() {
			var escCtx context.Context
			escCtx, escCancel = context.WithCancel(ctx)
			escDone = make(chan struct{})
			go runWithEscCancel(escCtx, cancel, escDone)
		}
		stopEsc := func() { escCancel(); <-escDone }
		startEsc()

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
			case event := <-goalEvents:
				printGoalEvent(event)
			case req := <-questionRequests:
				stopEsc()
				if err := ctx.Err(); err != nil {
					req.Reply <- userQuestionResponse{Err: err}
					startEsc()
					continue
				}
				answer, err := collectUserAnswer(req.Question, selectUserQuestion, func(prompt string) (string, error) {
					return promptSessionInput(state, forks, nil, prompt, true)
				})
				if err != nil {
					cancel()
				}
				req.Reply <- userQuestionResponse{Answer: answer, Err: err}
				startEsc()
			case req := <-hitlReqCh:
				stopEsc()
				response := permission.HITLConfirmResponse{ReplyID: req.ReplyID}
				for _, tc := range req.ToolCalls {
					fmt.Printf("\nAgent wants to run: %s\nArgs: %v\n", tc.Name, tc.Args)
					answer, err := state.Prompt("[y=yes / a=always allow / n=no] ")
					if err != nil {
						cancel()
						break
					}
					answer = strings.ToLower(strings.TrimSpace(answer))
					confirmed := answer == "y" || answer == "yes" || answer == "a" || answer == "always"
					var rules []permission.Rule
					if answer == "a" || answer == "always" {
						rules = makeAllowRule(tc)
					}
					response.Results = append(response.Results, permission.ToolConfirmResult{ToolCall: tc, Confirmed: confirmed, Rules: rules})
				}
				select {
				case hitlRespCh <- response:
				case <-ctx.Done():
				}
				startEsc()
			case <-time.After(100 * time.Millisecond):
				printForkEvents(forks)
				if pending := forks.Pending(); len(pending) > 0 {
					stopEsc()
					for _, p := range pending {
						answer, err := state.Prompt("[" + p.SessionID + "] approve? [y/a/N] ")
						if err != nil {
							cancel()
							break
						}
						answer = strings.ToLower(strings.TrimSpace(answer))
						if err := forks.Approve(p.SessionID, answer == "y" || answer == "a", answer == "a"); err != nil {
							fmt.Println(err)
						}
					}
					startEsc()
				}

			case result := <-agentDone:
				out = result.out
				runErr = result.err
				break agentLoop

			case <-ctx.Done():
				result := <-agentDone
				out = result.out
				runErr = result.err
				break agentLoop
			}
		}

		cancel()
		stopEsc()
		for len(goalEvents) > 0 {
			printGoalEvent(<-goalEvents)
		}
		if scheduled != nil {
			loops.Complete(current.ID, scheduled.ID)
			if runErr != nil {
				loops.Cancel(current.ID, scheduled.ID)
				fmt.Printf("[%s] Stopped after cancellation or error.\n", scheduled.ID)
			}
		}

		var goalPaused *agent.GoalPausedError
		if runErr != nil {
			if errors.Is(runErr, context.Canceled) {
				fmt.Printf("\n%s⚠️  Cancelled by user (Esc).%s\n", cBrightYellow, cReset)
			} else if errors.As(runErr, &goalPaused) {
				fmt.Printf("\n%s%v%s\n", cBrightYellow, runErr, cReset)
			} else {
				var retryErr *llm.RetryExhaustedError
				if errors.As(runErr, &retryErr) {
					fmt.Fprintf(os.Stderr, "%s❌ LLM call failed after %d retries\nLast error: %v%s\n", cRed, retryErr.Attempts, retryErr.LastErr, cReset)
				} else {
					fmt.Fprintf(os.Stderr, "%s❌ Error: %v%s\n", cRed, runErr, cReset)
				}
			}
		}
		if out != "" && (runErr == nil || goalPaused != nil) {
			fmt.Printf("\n%s🤖 Agent%s %s›%s\n%s\n", cBold, cReset, cDim, cReset, formatMarkdownForTerminal(out))
		}
		if agentLogger != nil {
			if path := agentLogger.GetLogFilePath(); path != "" {
				fmt.Printf("\n%s📝 Log file: %s%s%s\n", cDim, cReset, path, cReset)
			}
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
	fmt.Println("  /goal <condition> - Work until an independent stop check verifies completion")
	fmt.Println("  /goal [status] - Show this session's goal, checks and reported token usage")
	fmt.Println("  /goal cancel|resume - Discard or resume a paused goal; Esc pauses execution")
	fmt.Println("  /loop [interval] [prompt] - Repeat a prompt (default 10m; s/m/h/d, minimum 1m)")
	fmt.Println("  /loop list - List this session's loops and next run times")
	fmt.Println("  /loop cancel loop-ID|all - Stop loops in this session")
	fmt.Println("  /diff [turn-ID|git] - Show recorded turn changes or Git staged/unstaged changes")
	fmt.Println("  /context - Show estimated current context usage and configured budget")
	fmt.Println("  /compact [instructions] - Summarize conversation, optionally preserving specified details")
	fmt.Println("  /plan - Enter read-only Plan Mode")
	fmt.Println("  /execute - Review the saved plan and approve execution")
	fmt.Println("  /fork - Create and enter a new session with the current context")
	fmt.Println("  /sessions - Choose a session with wheel/arrow keys")
	fmt.Println("  /switch session-ID - Switch to main or another session")
	fmt.Println("  /tasks [fork-ID] - Inspect background Agent tasks")
	fmt.Println("  /send fork-ID <message> - Continue that fork's independent conversation")
	fmt.Println("  /cancel fork-ID - Cancel a running fork")
	fmt.Println("  /approve fork-ID y|a|n - Answer a fork's permission request")
	fmt.Println("  /rewind [ID] [both|conversation|files] - Select a message or restore a turn")
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
	for {
		if ctx.Err() != nil {
			return
		}
		b, ok, err := readTerminalByte(f, 100)
		if err != nil {
			return
		}
		if ok && b == 0x1b {
			fmt.Printf("\n%s⏹  Esc pressed, cancelling...%s\n", cBrightYellow, cReset)
			cancel()
			return
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

func handleRewind(ag *agent.Agent, state *liner.State, line string) {
	parts := strings.Fields(line)
	if len(parts) == 1 {
		checkpoints := ag.Checkpoints()
		if len(checkpoints) == 0 {
			fmt.Println("No messages to rewind.")
			return
		}
		options := make([]string, len(checkpoints))
		for i, cp := range checkpoints {
			options[i] = fmt.Sprintf("%d  %s", cp.ID, cp.Prompt)
		}
		selected, err := selectRewindOption("Rewind to before which message?", options, len(options)-1)
		if err != nil {
			fmt.Println(err)
			return
		}
		if selected < 0 {
			fmt.Println("Rewind cancelled")
			return
		}
		mode, err := selectRewindOption("Restore before: "+checkpoints[selected].Prompt, []string{"Conversation and files", "Conversation only", "Files only"}, 0)
		if err != nil {
			fmt.Println(err)
			return
		}
		if mode < 0 {
			fmt.Println("Rewind cancelled")
			return
		}
		modes := []string{"both", "conversation", "files"}
		applyRewind(ag, checkpoints[selected].ID, modes[mode])
		return
	}
	if len(parts) > 3 {
		fmt.Println("Usage: /rewind ID [both|conversation|files]")
		return
	}
	var id int
	if _, err := fmt.Sscan(parts[1], &id); err != nil || fmt.Sprint(id) != parts[1] {
		fmt.Println("Invalid checkpoint ID")
		return
	}
	mode := "both"
	if len(parts) == 3 {
		mode = parts[2]
	}
	if mode != "both" && mode != "conversation" && mode != "files" {
		fmt.Println("Mode must be both, conversation, or files")
		return
	}
	found := false
	for _, cp := range ag.Checkpoints() {
		if cp.ID == id {
			found = true
		}
	}
	if !found {
		fmt.Println("Checkpoint not found")
		return
	}
	answer, err := state.Prompt("Restore " + mode + " before this turn? [y/N] ")
	if err != nil || strings.ToLower(strings.TrimSpace(answer)) != "y" {
		fmt.Println("Rewind cancelled")
		return
	}
	applyRewind(ag, id, mode)
}

func applyRewind(ag *agent.Agent, id int, mode string) {
	prompt, err := ag.Rewind(id, mode)
	if err != nil {
		fmt.Printf("Rewind failed: %v\n", err)
		return
	}
	fmt.Println("Rewind complete.")
	if mode != "files" {
		fmt.Printf("Original prompt: %s\n", prompt)
	}
}
