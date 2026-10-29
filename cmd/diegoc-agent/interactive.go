package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"diegoc-agent/internal/agent"
	"diegoc-agent/internal/llm"
	"diegoc-agent/internal/logger"
	"diegoc-agent/internal/loop"
	"diegoc-agent/internal/permission"
	"diegoc-agent/internal/tools"

	"github.com/peterh/liner"
)

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

	commands := commandDispatcher{sessions: sessions, forks: forks, loops: loops, workspaceDir: workspaceDir, state: state}
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
			result := commands.dispatch(line)
			if result.exit {
				return
			}
			if result.prompt == "" {
				continue
			}
			line = result.prompt
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
