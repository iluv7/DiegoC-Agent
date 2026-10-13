package main

import (
	"fmt"
	"strings"

	"diegoc-agent/internal/agent"
	"diegoc-agent/internal/logger"
)

func handleSessionCommand(sessions *agent.ConversationSessions, line string, selectSession func(string, []string, int) (int, error)) bool {
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return false
	}
	switch strings.ToLower(parts[0]) {
	case "/fork":
		if len(parts) != 1 {
			fmt.Println("Usage: /fork — create and enter a new session with the current context")
			return true
		}
		parent := sessions.Current()
		entry, err := sessions.Fork()
		if err != nil {
			fmt.Println(err)
			return true
		}
		if parent.Agent.Logger != nil {
			entry.Agent.Logger = logger.NewInDirectory(parent.Agent.Logger.LogDir())
		} else {
			entry.Agent.Logger = logger.New()
		}
		fmt.Printf("Created and switched to [%s] from [%s]. Type /sessions to switch, or /switch %s to return.\n", entry.ID, entry.ParentID, entry.ParentID)
	case "/sessions", "/switch", "/session":
		if len(parts) > 2 {
			fmt.Println("Usage: /sessions or /switch session-ID")
			return true
		}
		id := ""
		if len(parts) == 2 {
			id = parts[1]
		} else {
			entries := sessions.List()
			options := make([]string, len(entries))
			initial := 0
			for i, entry := range entries {
				parent := ""
				if entry.ParentID != "" {
					parent = " (from " + entry.ParentID + ")"
				}
				options[i] = fmt.Sprintf("%s%s — %d messages", entry.ID, parent, len(entry.Agent.Messages))
				if entry == sessions.Current() {
					initial = i
					options[i] += " [current]"
				}
			}
			selected, err := selectSession("Switch session · current: "+sessions.Current().ID, options, initial)
			if err != nil {
				for _, option := range options {
					fmt.Println(option)
				}
				fmt.Println("Use /switch session-ID to switch.")
				return true
			}
			if selected < 0 {
				fmt.Println("Session switch cancelled.")
				return true
			}
			if selected >= len(entries) {
				fmt.Println("Invalid session selection.")
				return true
			}
			id = entries[selected].ID
		}
		if err := sessions.Switch(id); err != nil {
			fmt.Println(err)
			return true
		}
		entry := sessions.Current()
		fmt.Printf("Switched to [%s].\n", entry.ID)
		// Replay this session's own conversation, including its inherited prefix.
		for _, msg := range entry.ConversationPreview() {
			fmt.Printf("%s › %s\n", msg.Role, terminalReport(fmt.Sprint(msg.Content)))
		}
	default:
		return false
	}
	return true
}
