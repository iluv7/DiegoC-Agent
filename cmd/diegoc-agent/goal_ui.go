package main

import (
	"fmt"
	"strings"

	"diegoc-agent/internal/agent"
)

// A nonempty returned prompt starts/resumes work through the normal foreground
// runner. Status/cancel commands stay local and never invoke the main model.
func handleGoalCommand(ag *agent.Agent, line string) (string, bool) {
	parts := strings.Fields(line)
	if len(parts) == 0 || !strings.EqualFold(parts[0], "/goal") {
		return "", false
	}
	condition := strings.TrimSpace(line[len(parts[0]):])
	if condition == "" || strings.EqualFold(condition, "status") {
		info := ag.GoalStatus()
		if info.Status == "" {
			fmt.Println("No goal. Use /goal <completion condition> to start working toward one.")
			return "", true
		}
		fmt.Printf("Goal [%s]: %s\nStop checks: %d · reported API tokens: %d · evaluator: %s\n", info.Status, terminalReport(info.Condition), info.Checks, info.Tokens, selectorText(info.EvaluatorModel, 120))
		if info.Reason != "" {
			fmt.Printf("Last check: %s\n", terminalReport(info.Reason))
		}
		return "", true
	}
	switch strings.ToLower(parts[1]) {
	case "cancel":
		if len(parts) != 2 {
			fmt.Println("Usage: /goal cancel")
		} else if ag.CancelGoal() {
			fmt.Println("Goal cancelled.")
		} else {
			fmt.Println("No active or paused goal to cancel.")
		}
		return "", true
	case "resume":
		if len(parts) != 2 {
			fmt.Println("Usage: /goal resume")
			return "", true
		}
		info := ag.GoalStatus()
		if err := ag.ResumeGoal(); err != nil {
			fmt.Printf("Goal: %v\n", err)
			return "", true
		}
		fmt.Println("Goal resumed. Esc pauses execution and preserves the goal.")
		return "Resume work toward the user goal: " + info.Condition + "\nPrevious stop reason: " + info.Reason + "\nHonor all current instructions and tool permissions, and verify completion.", true
	}
	if err := ag.SetGoal(condition); err != nil {
		fmt.Printf("Goal: %v\n", err)
		return "", true
	}
	fmt.Println("Goal set. Working now; an independent evaluator checks before stopping. Esc pauses execution.")
	return "Work toward the following user goal and verify its completion:\n" + condition, true
}

func printGoalEvent(event agent.GoalEvent) {
	var output string
	switch event.Status {
	case "checking":
		output = fmt.Sprintf("\n[goal] Checking completion (%d)...\n", event.Checks)
	case "continuing":
		output = fmt.Sprintf("[goal] Not complete; continuing: %s\n", terminalReport(event.Reason))
	case "completed":
		output = fmt.Sprintf("[goal] Complete; goal cleared: %s\n", terminalReport(event.Reason))
	case "paused":
		output = fmt.Sprintf("[goal] Paused: %s\n", terminalReport(event.Reason))
	}
	// Esc polling puts the terminal in raw mode during a run.
	fmt.Print(strings.ReplaceAll(output, "\n", "\r\n"))
}
