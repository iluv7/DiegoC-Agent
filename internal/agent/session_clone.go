package agent

import (
	"encoding/json"

	"diegoc-agent/internal/permission"
	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

type sessionCloneKind uint8

const (
	foregroundSessionClone sessionCloneKind = iota
	backgroundTaskClone
)

func (a *Agent) cloneConversation() *Agent {
	return a.cloneSession(foregroundSessionClone, a.forks, "")
}

// cloneSession is the allowlist of state inherited by new sessions. The client
// and ordinary tool instances remain shared, while message data, permission
// rules, tool registries and owner-bound Agent tools are copied or rebound.
//
// NewWithPermission gives both kinds fresh execution/approval state, accounting,
// checkpoints, inboxes, logger and goal state. Unlisted runtime fields retain
// constructor defaults, including PlanMode; the permission mode and rendered
// system message are inherited through their existing paths below.
func (a *Agent) cloneSession(kind sessionCloneKind, forks *ForkManager, task string) *Agent {
	child := NewWithPermission(a.LLM, a.SystemPrompt, a.MaxSteps, a.TokenLimit,
		append([]tools.Tool(nil), a.Tools...), copyPermission(a.PermissionCtx))
	child.compressedSummary = a.compressedSummary
	child.contextLimit = a.contextLimit
	child.compactLanguage = a.compactLanguage
	child.skipNextTokenCheck = true // Preserve the inherited API prefix on the first request.
	child.forks = forks

	switch kind {
	case foregroundSessionClone:
		// Foreground conversations keep the exact history and can themselves
		// fork. They inherit repair/evaluator settings, but no active goal.
		child.Messages = cloneMessages(a.Messages)
		if a.memoryManager != nil {
			child.compressedSummary = a.memoryManager.CompressedSummary()
		}
		child.needsToolRepair = a.needsToolRepair
		child.goalConfig = a.goalConfig
	case backgroundTaskClone:
		// Workers retain their existing cached-summary source and batch
		// snapshot, then pair pending tool calls before adding the task.
		history := a.Messages
		if a.forkBatch != nil {
			history = a.forkBatch
		}
		child.Messages = buildForkMessages(history, task)
		child.isFork = true
	default:
		panic("unknown session clone kind")
	}

	for i, tool := range child.Tools {
		if tool.Name() == "Agent" {
			bound := &ForkTool{owner: child}
			child.Tools[i] = bound
			child.toolByName[bound.Name()] = bound
		}
	}
	return child
}

func copyPermission(source permission.Context) permission.Context {
	data, _ := json.Marshal(source)
	target := permission.NewContext()
	_ = json.Unmarshal(data, &target)
	return target
}

// buildForkMessages leaves every historical message intact, pairs any pending
// tool calls with identical placeholders, and puts the unique task last.
func buildForkMessages(history []schema.Message, task string) []schema.Message {
	messages := cloneMessages(history)
	messages = completeToolResults(messages, forkPlaceholder)
	return append(messages, schema.Message{Role: "user", Content: "Complete this fork session's task using the inherited context. Report your result when finished.\n\n" + task})
}
