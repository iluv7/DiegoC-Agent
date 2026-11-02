package agent

import (
	"context"
	"fmt"
	"sync"

	"diegoc-agent/internal/llm"
	"diegoc-agent/internal/logger"
	"diegoc-agent/internal/memory"
	"diegoc-agent/internal/permission"
	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

// Agent runs the conversation loop with optional tools and HITL support.
type Agent struct {
	PlanMode        bool
	needsToolRepair bool
	forks           *ForkManager
	isFork          bool
	forkBatch       []schema.Message
	inboxMu         sync.Mutex
	forkReports     []ForkEvent

	checkpoints        []checkpoint
	checkpointSequence int
	LLM                llm.Client
	SystemPrompt       string
	Messages           []schema.Message
	MaxSteps           int
	TokenLimit         int
	Tools              []tools.Tool
	toolByName         map[string]tools.Tool
	APITotalTokens     int
	skipNextTokenCheck bool
	Logger             *logger.AgentLogger
	goal               *GoalInfo
	lastGoal           *GoalInfo
	goalConfig         goalConfig

	// Memory pipeline (Phase 11-12). When set, replaces the legacy summarizeIfNeeded.
	memoryManager     *memory.Manager
	compressedSummary string
	contextLimit      int
	compactLanguage   string

	PermissionCtx permission.Context
	executor      *toolExecutor
}

// SetPlanMode switches the same ReAct agent between read-only planning and execution.
// The permission context is restored to its prior mode when planning is approved.
func (a *Agent) SetPlanMode(enabled bool) {
	if enabled == a.PlanMode {
		return
	}
	if enabled {
		a.PermissionCtx.PrePlanMode = a.PermissionCtx.Mode
		a.PermissionCtx.Mode = permission.ModeExplore
	} else {
		mode := a.PermissionCtx.PrePlanMode
		if mode == "" || mode == permission.ModeExplore {
			mode = permission.ModeDefault
		}
		a.PermissionCtx.Mode = mode
	}
	a.PlanMode = enabled
	if len(a.Messages) > 0 {
		base := a.SystemPrompt
		if enabled {
			base += "\n\n## Plan Mode\nExplore and reason before editing. Do not modify project files or run commands that change state. Write the complete proposed implementation plan to `.agent_plan.md` using `write_plan` when ready. Then summarize the plan and tell the user to approve it with `/execute`. If the request changes materially, update the plan first."
		} else {
			base += "\n\n## Execute Mode\nFollow the approved plan in `.agent_plan.md`. Implement it, verify the changes, and adapt to new user instructions."
		}
		a.Messages[0].Content = base
	}
}

// New creates an Agent with system prompt and tools.
func New(client llm.Client, systemPrompt string, maxSteps int, tokenLimit int, toolList []tools.Tool) *Agent {
	return NewWithPermission(client, systemPrompt, maxSteps, tokenLimit, toolList, permission.NewContext())
}

// NewWithPermission creates an Agent with custom permission context.
func NewWithPermission(client llm.Client, systemPrompt string, maxSteps int, tokenLimit int, toolList []tools.Tool, permCtx permission.Context) *Agent {
	if maxSteps <= 0 {
		maxSteps = 50
	}
	if tokenLimit <= 0 {
		tokenLimit = 80000
	}
	byName := make(map[string]tools.Tool)
	for _, t := range toolList {
		byName[t.Name()] = t
	}
	msgs := []schema.Message{
		{Role: "system", Content: systemPrompt},
	}
	ag := &Agent{
		LLM:             client,
		SystemPrompt:    systemPrompt,
		Messages:        msgs,
		MaxSteps:        maxSteps,
		TokenLimit:      tokenLimit,
		contextLimit:    tokenLimit,
		compactLanguage: "zh",
		Tools:           toolList,
		toolByName:      byName,
		PermissionCtx:   permCtx,
	}
	ag.executor = newToolExecutor(ag)
	return ag
}

// AddUserMessage appends a user message.
func (a *Agent) AddUserMessage(content string) {
	if a.needsToolRepair {
		a.cleanupPendingToolCalls()
		a.needsToolRepair = false
	}
	a.deliverForkReports()
	a.makeCheckpoint(content)
	a.Messages = append(a.Messages, schema.Message{Role: "user", Content: content})
}

// SetMemoryManager configures the memory pipeline.
// When set, PreReasoningHook replaces the legacy summarizeIfNeeded in the agent loop.
func (a *Agent) SetMemoryManager(mgr *memory.Manager) {
	a.memoryManager = mgr
	if mgr != nil {
		a.contextLimit, _, a.compactLanguage = mgr.ContextSettings()
	}
}

// RunWithHITL executes the loop with HITL support.
// 当工具需要用户确认时，通过 eventCh 发送 HITLConfirmRequest，
// 然后阻塞等待 inputCh 收到 HITLConfirmResponse。
func (a *Agent) RunWithHITL(
	ctx context.Context,
	inputCh <-chan permission.HITLConfirmResponse,
	eventCh chan<- permission.HITLConfirmRequest,
) (string, error) {
	return a.runLoop(ctx, inputCh, eventCh)
}

// runLoop 是 Run 和 RunWithHITL 共享的内部循环。
// 当 hitlInputCh == nil 时表示无 HITL 支持，所有 ASK 转为 DENY。
func (a *Agent) runLoop(
	ctx context.Context,
	hitlInputCh <-chan permission.HITLConfirmResponse,
	hitlEventCh chan<- permission.HITLConfirmRequest,
) (output string, runErr error) {

	defer func() {
		a.forkBatch = nil
		if runErr != nil {
			a.needsToolRepair = true
			if a.goalActive() {
				a.PauseGoal(runErr.Error())
				a.emitGoalEvent(ctx, "paused", runErr.Error())
			}
		}
	}()
	if a.Logger != nil {
		a.Logger.StartNewRun()
	}
	toolNames := make([]string, 0, len(a.Tools))
	for _, t := range a.Tools {
		toolNames = append(toolNames, t.Name())
	}
	goalChecks := 0
	for step := 0; step < a.MaxSteps; step++ {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		a.deliverForkReports()
		if a.memoryManager != nil {
			// Phase 11-12: full memory pipeline
			systemPrompt := a.SystemPrompt
			if a.goalActive() && len(a.Messages) > 0 {
				systemPrompt = contentString(a.goalRequestMessages()[0])
			}
			keptMsgs, summary, err := a.memoryManager.PreReasoningHook(ctx, a.Messages, systemPrompt)
			if err != nil {
				return "", err
			}
			a.Messages = keptMsgs
			if summary != "" {
				a.compressedSummary = summary
			}
		} else {
			// Legacy fallback: simple per-round summarization
			if err := a.summarizeIfNeeded(ctx); err != nil {
				return "", err
			}
		}

		requestMessages := a.goalRequestMessages()
		if a.Logger != nil {
			a.Logger.LogRequest(requestMessages, toolNames)
		}
		resp, err := a.LLM.Generate(ctx, requestMessages, a.Tools)
		if err != nil {
			return "", err
		}

		if resp.Usage != nil {
			a.APITotalTokens += resp.Usage.TotalTokens
			if a.goalActive() {
				a.goal.Tokens += resp.Usage.TotalTokens
			}
		}
		if a.Logger != nil {
			a.Logger.LogResponse(resp.Content, resp.Thinking, resp.ToolCalls, resp.FinishReason)
		}
		a.Messages = append(a.Messages, schema.Message{
			Role:      "assistant",
			Content:   resp.Content,
			Thinking:  resp.Thinking,
			ToolCalls: resp.ToolCalls,
		})

		if len(resp.ToolCalls) == 0 {
			if a.goalActive() {
				goalChecks++
				satisfied, err := a.checkGoal(ctx, goalChecks)
				if err != nil || satisfied {
					return resp.Content, err
				}
				continue
			}
			return resp.Content, nil
		}

		if a.forks != nil {
			a.forkBatch = cloneMessages(a.Messages)
		}
		if err := a.executor.executeCalls(ctx, resp.ToolCalls, hitlInputCh, hitlEventCh); err != nil {
			return "", err
		}
		a.forkBatch = nil
	}
	if a.goalActive() {
		return "", &GoalPausedError{Reason: fmt.Sprintf("reached the Agent step limit (%d) before the goal was verified", a.MaxSteps)}
	}
	if a.isFork {
		return "", fmt.Errorf("fork reached maximum steps (%d) without a final response", a.MaxSteps)
	}
	return "", nil
}

// cleanupIncompleteMessages 移除最后一个 assistant 消息及之后的所有消息（取消时清理）。
func (a *Agent) cleanupIncompleteMessages() {
	for i := len(a.Messages) - 1; i >= 0; i-- {
		if a.Messages[i].Role == "assistant" {
			a.Messages = a.Messages[:i]
			return
		}
	}
	// 没找到 assistant 消息 → 回退到只剩 system prompt
	if len(a.Messages) > 1 {
		a.Messages = a.Messages[:1]
	}
}
