package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"diegoc-agent/internal/permission"
	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

func TestLoopFingerprint(t *testing.T) {
	a := executorTestCall("a", "read", map[string]interface{}{"path": "x", "offset": 1})
	b := executorTestCall("b", "write", map[string]interface{}{"content": "first"})
	key, err := toolCallFingerprint([]schema.ToolCall{a, b})
	if err != nil {
		t.Fatal(err)
	}
	a.ID = "new-id"
	a.Function.Arguments = map[string]interface{}{"arguments": `{"offset":1,"path":"x"}`}
	reordered, _ := toolCallFingerprint([]schema.ToolCall{b, a})
	if key != reordered {
		t.Fatal("IDs, argument encoding or order changed fingerprint")
	}
	for _, calls := range [][]schema.ToolCall{
		{a}, {a, b, b},
		{executorTestCall("a", "read", map[string]interface{}{"path": "x", "offset": 2}), b},
		{a, executorTestCall("b", "write", map[string]interface{}{"content": "second"})},
	} {
		changed, _ := toolCallFingerprint(calls)
		if changed == key {
			t.Fatal("different operations collapsed")
		}
	}
	_, err = toolCallFingerprint([]schema.ToolCall{executorTestCall("bad", "read", map[string]interface{}{"bad": make(chan int)})})
	if err == nil {
		t.Fatal("invalid arguments silently fingerprinted")
	}
}

func TestLoopGuardIntegration(t *testing.T) {
	executions, requests, warnings := 0, 0, 0
	tool := &executorTestTool{name: "read", decision: permission.Decision{Behavior: permission.BehaviorALLOW}, readOnly: true,
		run: func(context.Context, map[string]interface{}) (*tools.ToolResult, error) {
			executions++
			return &tools.ToolResult{Success: true, Content: "same"}, nil
		}}
	client := forkClientFunc(func(_ context.Context, msgs []schema.Message, _ []tools.Tool) (*schema.LLMResponse, error) {
		requests++
		last := msgs[len(msgs)-1]
		if text, ok := last.Content.(string); ok && strings.HasPrefix(text, "Loop warning:") {
			warnings++
			if msgs[len(msgs)-2].Role != "tool" {
				t.Fatal("warning broke tool pairing")
			}
		}
		return &schema.LLMResponse{ToolCalls: []schema.ToolCall{executorTestCall(fmt.Sprint(requests), "read", nil)}}, nil
	})
	a := New(client, "system", 20, 100000, []tools.Tool{tool})
	a.SetMiddlewareFactories(func() any {
		m, err := NewLoopDetectionMiddleware(LoopDetectionConfig{})
		if err != nil {
			panic(err)
		}
		return m
	})
	a.AddUserMessage("go")
	for run := 0; run < 2; run++ {
		_, err := a.RunWithHITL(context.Background(), nil, nil)
		var loopErr *LoopDetectedError
		if !errors.As(err, &loopErr) || loopErr.Count != 5 {
			t.Fatalf("expected loop stop: %v", err)
		}
		if executions != (run+1)*4 || requests != (run+1)*5 || warnings != run+1 {
			t.Fatalf("executions=%d requests=%d warnings=%d", executions, requests, warnings)
		}
		if a.Messages[len(a.Messages)-1].Role != "tool" {
			t.Fatal("blocked batch left unmatched tool calls")
		}
		for _, msg := range a.Messages {
			if s, ok := msg.Content.(string); ok && strings.HasPrefix(s, "Loop warning:") {
				t.Fatal("transient warning persisted")
			}
		}
	}
	child := a.cloneSession(backgroundTaskClone, nil, "task")
	if child.middlewares[0] == a.middlewares[0] {
		t.Fatal("fork shares detector")
	}
}

func TestLoopWarningRetryAndSlidingWindow(t *testing.T) {
	m, err := NewLoopDetectionMiddleware(LoopDetectionConfig{WarnThreshold: 1, HardLimit: 3, WindowSize: 3})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	req := ModelRequest{Messages: []schema.Message{{Role: "tool", Content: "result"}}}
	call := func(name string) ModelHandler {
		return func(context.Context, ModelRequest) (*schema.LLMResponse, error) {
			return &schema.LLMResponse{ToolCalls: []schema.ToolCall{executorTestCall("id", name, nil)}}, nil
		}
	}
	if _, err = m.OnModelCall(ctx, nil, req, call("a")); err != nil {
		t.Fatal(err)
	}
	failed := errors.New("temporary")
	_, err = m.OnModelCall(ctx, nil, req, func(_ context.Context, r ModelRequest) (*schema.LLMResponse, error) {
		if len(r.Messages) != 2 {
			t.Fatal("warning missing")
		}
		return nil, failed
	})
	if !errors.Is(err, failed) || m.pending == "" || len(req.Messages) != 1 {
		t.Fatal("retry lost warning or mutated request")
	}
	for _, name := range []string{"b", "c", "d", "a"} {
		if _, err = m.OnModelCall(ctx, nil, req, call(name)); err != nil {
			t.Fatal(err)
		}
	}
	if len(m.history) != 3 || len(m.warned) != 3 || m.pending == "" {
		t.Fatal("window eviction did not rearm warning")
	}
	// Lifecycle cleanup applies to failed replies as well.
	_, _ = m.OnReply(ctx, nil, func(context.Context) (string, error) { return "", failed })
	if len(m.history) != 0 || m.pending != "" {
		t.Fatal("stale run state")
	}
	for _, cfg := range []LoopDetectionConfig{{WarnThreshold: -1}, {WarnThreshold: 5, HardLimit: 3}, {HardLimit: 30, WindowSize: 20}} {
		if _, err := NewLoopDetectionMiddleware(cfg); err == nil {
			t.Fatal("accepted invalid thresholds")
		}
	}
}

func TestLoopStopPausesGoal(t *testing.T) {
	a := New(forkClientFunc(func(context.Context, []schema.Message, []tools.Tool) (*schema.LLMResponse, error) {
		return &schema.LLMResponse{ToolCalls: []schema.ToolCall{executorTestCall("id", "missing", nil)}}, nil
	}), "system", 20, 100000, nil)
	a.SetMiddlewareFactories(func() any { m, _ := NewLoopDetectionMiddleware(LoopDetectionConfig{}); return m })
	if err := a.SetGoal("finish task"); err != nil {
		t.Fatal(err)
	}
	_, err := a.RunWithHITL(context.Background(), nil, nil)
	var loopErr *LoopDetectedError
	if !errors.As(err, &loopErr) || a.GoalStatus().Status != "paused" {
		t.Fatalf("%v %+v", err, a.GoalStatus())
	}
}
