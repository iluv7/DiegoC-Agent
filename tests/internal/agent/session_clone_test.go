package agent

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"

	"diegoc-agent/internal/logger"
	"diegoc-agent/internal/memory"
	"diegoc-agent/internal/permission"
	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

type cloneCountingClient struct{ calls atomic.Int32 }

func (c *cloneCountingClient) Generate(context.Context, []schema.Message, []tools.Tool) (*schema.LLMResponse, error) {
	c.calls.Add(1)
	return &schema.LLMResponse{Content: "answer"}, nil
}

func TestSessionClonePreservesInheritanceAndIsolation(t *testing.T) {
	for _, kind := range []sessionCloneKind{foregroundSessionClone, backgroundTaskClone} {
		name := "foreground"
		if kind == backgroundTaskClone {
			name = "background"
		}
		t.Run(name, func(t *testing.T) {
			client, evaluator := &cloneCountingClient{}, &cloneCountingClient{}
			read := &tools.ReadTool{WorkspaceDir: t.TempDir()}
			parent := New(client, "base instructions", 7, 6000, []tools.Tool{read})
			cfg := memory.DefaultConfig()
			cfg.WorkingDir, cfg.MaxInputLength, cfg.Language = t.TempDir(), 32000, "en"
			mgr := memory.NewManager(cfg, client)
			defer mgr.Close()
			mgr.SetCompressedSummary("manager summary")
			parent.SetMemoryManager(mgr)
			parent.compressedSummary = "cached summary"
			parent.ConfigureGoals(evaluator, "goal-model", 4)
			if err := parent.SetGoal("finish parent work"); err != nil {
				t.Fatal(err)
			}
			parent.SetPlanMode(true)
			parent.AddUserMessage("original task")
			parent.Messages = append(parent.Messages, schema.Message{Role: "assistant", ToolCalls: []schema.ToolCall{{ID: "pending", Function: schema.FunctionCall{Name: "read_file", Arguments: map[string]interface{}{"nested": map[string]interface{}{"value": "original"}}}}}})
			parent.forkBatch = cloneMessages(parent.Messages)
			parent.needsToolRepair = true
			parent.APITotalTokens = 123
			parent.forkReports = []ForkEvent{{SessionID: "previous", Result: "parent report"}}
			parent.Logger = logger.NewInDirectory(t.TempDir())
			defer parent.Logger.Close()
			parent.executor.replyID = "parent approval"
			parent.executor.states["pending"] = permission.ToolCallAsking
			parent.PermissionCtx.AddRule(permission.Rule{ToolName: "read_file", Behavior: permission.BehaviorALLOW, RuleContent: "original/**"})
			parent.PermissionCtx.WorkingDirectories["project"] = permission.WorkingDirectory{Path: "original", Source: "session"}
			parentForks, workerForks := NewForkManager(context.Background()), NewForkManager(context.Background())
			defer parentForks.Close()
			defer workerForks.Close()
			parent.EnableForks(parentForks)

			var child *Agent
			if kind == foregroundSessionClone {
				child = parent.cloneConversation()
			} else {
				child = parent.cloneSession(kind, workerForks, "worker task")
			}
			if client.calls.Load() != 0 || evaluator.calls.Load() != 0 || child.LLM != client || child.SystemPrompt != parent.SystemPrompt || child.MaxSteps != 7 || child.TokenLimit != 6000 {
				t.Fatal("cloning invoked a model or lost model/configuration inheritance")
			}
			if child.contextLimit != 32000 || child.compactLanguage != "en" || !child.skipNextTokenCheck || child.memoryManager != nil {
				t.Fatal("clone lost context settings or shared the mutable memory pipeline")
			}
			if child.goal != nil || child.lastGoal != nil || child.Logger != nil || child.APITotalTokens != 0 || len(child.checkpoints) != 0 || child.checkpointSequence != 0 || len(child.forkReports) != 0 || child.forkBatch != nil {
				t.Fatal("parent runtime state leaked into the new session")
			}
			if child.PlanMode() != parent.PlanMode() || !reflect.DeepEqual(child.PermissionCtx, parent.PermissionCtx) {
				t.Fatal("plan mode or copied permission state changed")
			}
			if child.executor == parent.executor || child.executor.agent != child || child.executor.permissions.Context != &child.PermissionCtx || child.executor.replyID != "" || len(child.executor.states) != 0 {
				t.Fatal("approval state or executor ownership leaked into the new session")
			}
			if kind == foregroundSessionClone {
				if child.isFork || !child.needsToolRepair || child.forks != parentForks || child.compressedSummary != "manager summary" || child.goalConfig.client != evaluator || child.goalConfig.model != "goal-model" || child.goalConfig.maxChecks != 4 {
					t.Fatal("foreground-specific inheritance changed")
				}
			} else if !child.isFork || child.needsToolRepair || child.forks != workerForks || child.compressedSummary != "cached summary" || child.goalConfig.client != nil || child.goalConfig.model != "" || child.goalConfig.maxChecks != 0 {
				t.Fatal("background-specific inheritance changed")
			}
			if child.Tools[0] != read || child.toolByName["read_file"] != read {
				t.Fatal("ordinary tool instances are no longer shared as before")
			}
			bound := child.toolByName["Agent"].(*ForkTool)
			if bound.owner != child || child.Tools[1] != bound || bound == parent.toolByName["Agent"] {
				t.Fatal("owner-bound tool was not rebound in both registries")
			}

			child.Messages[2].ToolCalls[0].Function.Arguments["nested"].(map[string]interface{})["value"] = "child only"
			child.PermissionCtx.AllowRules["read_file"][0].RuleContent = "child/**"
			child.PermissionCtx.WorkingDirectories["project"] = permission.WorkingDirectory{Path: "child"}
			child.Tools[0] = nil
			delete(child.toolByName, "read_file")
			child.executor.reset()
			for _, messages := range [][]schema.Message{parent.Messages, parent.forkBatch} {
				if messages[2].ToolCalls[0].Function.Arguments["nested"].(map[string]interface{})["value"] != "original" {
					t.Fatal("child mutation changed parent messages or the batch snapshot")
				}
			}
			if parent.PermissionCtx.AllowRules["read_file"][0].RuleContent != "original/**" || parent.PermissionCtx.WorkingDirectories["project"].Path != "original" || parent.Tools[0] != read || parent.toolByName["read_file"] != read || len(parent.executor.states) != 1 {
				t.Fatal("child mutation changed parent permissions, tools or approval state")
			}
			child.ClearConversation()
			if mgr.CompressedSummary() != "manager summary" || parent.compressedSummary != "cached summary" || parent.GoalStatus().Status != "active" || len(parent.checkpoints) != 1 {
				t.Fatal("clearing a clone changed its parent's memory, goal or checkpoints")
			}
		})
	}
}

func TestSessionCloneKeepsExactForegroundHistoryAndWorkerBatchProtocol(t *testing.T) {
	for _, withBatch := range []bool{false, true} {
		name := "live history"
		if withBatch {
			name = "batch snapshot"
		}
		t.Run(name, func(t *testing.T) {
			client := &cloneCountingClient{}
			parent := New(client, "system", 5, 10000, nil)
			parent.AddUserMessage("live request")
			parent.Messages = append(parent.Messages,
				schema.Message{Role: "assistant", Thinking: "preserved thinking", ToolCalls: []schema.ToolCall{forkCall("completed", "one"), forkCall("pending", "two")}},
				schema.Message{Role: "tool", ToolCallID: "completed", Content: "actual result"})
			if withBatch {
				parent.forkBatch = cloneMessages(parent.Messages)
				parent.forkBatch[1].Content = "batch request"
			}
			original := cloneMessages(parent.Messages)
			foreground := parent.cloneConversation()
			background := parent.cloneSession(backgroundTaskClone, nil, "inspect worker task")
			if !reflect.DeepEqual(foreground.Messages, original) || !reflect.DeepEqual(parent.Messages, original) || client.calls.Load() != 0 {
				t.Fatal("foreground cloning changed history, repaired calls or invoked a model")
			}
			wantRequest := "live request"
			if withBatch {
				wantRequest = "batch request"
			}
			messages := background.Messages
			if len(messages) != 6 || messages[1].Content != wantRequest || messages[2].Thinking != "preserved thinking" || messages[3].ToolCallID != "completed" || messages[3].Content != "actual result" || messages[4].ToolCallID != "pending" || messages[4].Content != forkPlaceholder || messages[5].Role != "user" || messages[5].Content != "Complete this fork session's task using the inherited context. Report your result when finished.\n\ninspect worker task" {
				t.Fatalf("worker history/task protocol changed: %+v", messages)
			}
			foreground.Messages[2].ToolCalls[0].Function.Arguments["prompt"] = "foreground only"
			if background.Messages[2].ToolCalls[0].Function.Arguments["prompt"] != "one" || parent.Messages[2].ToolCalls[0].Function.Arguments["prompt"] != "one" || (withBatch && parent.forkBatch[2].ToolCalls[0].Function.Arguments["prompt"] != "one") {
				t.Fatal("foreground and worker snapshots share mutable argument maps")
			}
		})
	}
}
