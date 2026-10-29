package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"diegoc-agent/internal/agent"

	"golang.org/x/term"
)

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
