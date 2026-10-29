package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"diegoc-agent/internal/agent"
	"diegoc-agent/internal/loop"
)

var slashCommands = []string{"/fork", "/sessions", "/switch", "/session", "/tasks", "/send", "/cancel", "/approve", "/help", "/rewind", "/diff", "/loop", "/goal", "/clear", "/history", "/stats", "/context", "/compact", "/plan", "/execute", "/log", "/exit", "/quit", "/q"}

// linePrompter is implemented by liner.State and allows command confirmations
// to be exercised without owning a terminal.
type linePrompter interface {
	Prompt(string) (string, error)
}

type commandResult struct {
	prompt string // nonempty input to run through the Agent
	exit   bool
}

type commandDispatcher struct {
	sessions     *agent.ConversationSessions
	forks        *agent.ForkManager
	loops        *loop.Scheduler
	workspaceDir string
	state        linePrompter
}

// dispatch preserves command precedence and returns only runnable input or exit.
// Scheduled prompts bypass dispatch in the interactive runner.
func (d *commandDispatcher) dispatch(line string) commandResult {
	current := d.sessions.Current()
	ag, sessionStart, agentLogger := current.Agent, current.CreatedAt, current.Agent.Logger
	sessions, forks, loops := d.sessions, d.forks, d.loops
	workspaceDir, state := d.workspaceDir, d.state
	if prompt, handled := handleGoalCommand(ag, line); handled {
		if prompt == "" {
			return commandResult{}
		}
		line = prompt
	}
	if handleLoopCommand(loops, current.ID, workspaceDir, line) {
		return commandResult{}
	}
	if handleSessionCommand(sessions, line, selectRewindOption) {
		return commandResult{}
	}
	if handleBackgroundCommand(forks, line) {
		return commandResult{}
	}
	if handleContextCommand(ag, line) {
		return commandResult{}
	}
	if handleDiffCommand(ag, workspaceDir, line) {
		return commandResult{}
	}

	if strings.HasPrefix(line, "/") {
		cmd := strings.ToLower(line)
		if cmd == "/plan" {
			ag.SetPlanMode(true)
			fmt.Print("\nPlan Mode enabled: project edits and state-changing commands are blocked. Describe the task; the agent will save its plan to .agent_plan.md. Use /execute to review and approve it.\n\n")
			return commandResult{}
		}
		if cmd == "/execute" {
			planPath := filepath.Join(workspaceDir, ".agent_plan.md")
			plan, err := os.ReadFile(planPath)
			if err != nil {
				fmt.Printf("\nNo plan found at %s. Enter Plan Mode with /plan and ask the agent to prepare one.\n\n", planPath)
				return commandResult{}
			}
			fmt.Printf("\nProposed plan (%s):\n\n%s\n", planPath, string(plan))
			answer, err := state.Prompt("Approve this plan and begin execution? [y/N] ")
			if err != nil || (strings.ToLower(strings.TrimSpace(answer)) != "y" && strings.ToLower(strings.TrimSpace(answer)) != "yes") {
				fmt.Print("Plan not approved. Remain in Plan Mode; revise it or approve later with /execute.\n\n")
				return commandResult{}
			}
			ag.SetPlanMode(false)
			fmt.Print("Plan approved. Starting execution...\n\n")
			line = "The plan in .agent_plan.md is approved. Execute it now, verify the result, and report what changed."
		} else if cmd == "/exit" || cmd == "/quit" || cmd == "/q" {
			fmt.Printf("\n%s👋 Goodbye! Thanks for using %s%s%s\n\n", cBrightYellow, cBold, appName, cReset)
			printStats(ag, sessionStart)
			return commandResult{exit: true}
		}
		if cmd == "/rewind" || strings.HasPrefix(cmd, "/rewind ") {
			handleRewind(ag, state, line)
			return commandResult{}
		}
		if cmd == "/help" {
			printHelp()
			return commandResult{}
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
			return commandResult{}
		}
		if cmd == "/history" {
			fmt.Printf("\n%sCurrent session message count: %s%s%d%s\n\n", cBrightCyan, cReset, cBold, len(ag.Messages), cReset)
			return commandResult{}
		}
		if cmd == "/stats" {
			printStats(ag, sessionStart)
			return commandResult{}
		}
		if cmd == "/log" || strings.HasPrefix(cmd, "/log ") {
			parts := strings.SplitN(line, " ", 2)
			if len(parts) == 1 {
				showLogDirectory(agentLogger)
			} else {
				readLogFile(agentLogger.LogDir(), strings.Trim(strings.TrimSpace(parts[1]), "\"'"))
			}
			return commandResult{}
		}
		if cmd != "/execute" {
			fmt.Printf("%s❌ Unknown command: %s%s (type %s/help%s)\n\n", cRed, line, cReset, cBrightGreen, cReset)
			return commandResult{}
		}
	}
	if line == "exit" || line == "quit" || line == "q" {
		fmt.Printf("\n%s👋 Goodbye! Thanks for using %s%s%s\n\n", cBrightYellow, cBold, appName, cReset)
		printStats(ag, sessionStart)
		return commandResult{exit: true}
	}
	return commandResult{prompt: line}
}
