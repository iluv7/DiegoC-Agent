package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"diegoc-agent/internal/memory"
	"diegoc-agent/internal/permission"
	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

type forkClientFunc func(context.Context, []schema.Message, []tools.Tool) (*schema.LLMResponse, error)

func (f forkClientFunc) Generate(ctx context.Context, msgs []schema.Message, ts []tools.Tool) (*schema.LLMResponse, error) {
	return f(ctx, msgs, ts)
}
func waitFork(t *testing.T, m *ForkManager, id, status string) ForkInfo {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, s := range m.List() {
			if s.ID == id && s.Status == status {
				return s
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s did not reach %s: %+v", id, status, m.List())
	return ForkInfo{}
}
func forkCall(id, task string) schema.ToolCall {
	return schema.ToolCall{ID: id, Type: "function", Function: schema.FunctionCall{Name: "Agent", Arguments: map[string]interface{}{"prompt": task}}}
}

func TestForkBatchPrefixesAndIndependentBindings(t *testing.T) {
	type captured struct {
		messages []schema.Message
		schemas  []map[string]interface{}
		tool     *ForkTool
	}
	requests := make(chan captured, 2)
	release := make(chan struct{})
	var rootCalls atomic.Int32
	client := forkClientFunc(func(ctx context.Context, messages []schema.Message, ts []tools.Tool) (*schema.LLMResponse, error) {
		last := messages[len(messages)-1]
		task, _ := last.Content.(string)
		if strings.HasPrefix(task, "Complete this fork") {
			schemas := []map[string]interface{}{}
			var bound *ForkTool
			for _, tool := range ts {
				schemas = append(schemas, tools.ToOpenAISchema(tool))
				if t, ok := tool.(*ForkTool); ok {
					bound = t
				}
			}
			requests <- captured{cloneMessages(messages), schemas, bound}
			select {
			case <-release:
				return &schema.LLMResponse{Content: "child result"}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if rootCalls.Add(1) == 1 {
			return &schema.LLMResponse{Content: "delegating", Thinking: "shared thinking", ToolCalls: []schema.ToolCall{forkCall("a", "check cache"), forkCall("b", "check permissions")}}, nil
		}
		return &schema.LLMResponse{Content: "parent remains free"}, nil
	})
	parent := New(client, "rendered system prompt", 10, 1000, nil)
	parent.AddUserMessage("login background")
	m := NewForkManager(context.Background())
	defer m.Close()
	parent.EnableForks(m)
	out, err := parent.RunWithHITL(context.Background(), nil, nil)
	if err != nil || out != "parent remains free" {
		t.Fatalf("parent: %s %v", out, err)
	}
	receive := func() captured {
		select {
		case req := <-requests:
			return req
		case <-time.After(2 * time.Second):
			t.Fatal("fork did not start")
			return captured{}
		}
	}
	a, b := receive(), receive()
	if !reflect.DeepEqual(a.messages[:len(a.messages)-1], b.messages[:len(b.messages)-1]) {
		t.Fatal("fork API prefixes differ")
	}
	if reflect.DeepEqual(a.messages[len(a.messages)-1], b.messages[len(b.messages)-1]) {
		t.Fatal("fork directives should differ")
	}
	prefix := a.messages[:len(a.messages)-1]
	if prefix[len(prefix)-1].Content != forkPlaceholder || prefix[len(prefix)-2].Content != forkPlaceholder {
		t.Fatal("missing placeholders for all tool uses")
	}
	if !reflect.DeepEqual(a.schemas, b.schemas) || a.tool.owner == parent || a.tool.owner == b.tool.owner {
		t.Fatal("tools were not rebound independently with identical schemas")
	}
	parentSchemas := []map[string]interface{}{}
	for _, tool := range parent.Tools {
		parentSchemas = append(parentSchemas, tools.ToOpenAISchema(tool))
	}
	if !reflect.DeepEqual(a.schemas, parentSchemas) {
		t.Fatal("child tool definitions differ from parent")
	}
	if reflect.ValueOf(a.tool.owner.LLM).Pointer() != reflect.ValueOf(parent.LLM).Pointer() {
		t.Fatal("model/config client not inherited")
	}
	// Mutating a worker's nested tool arguments cannot modify the sibling or parent.
	idx := len(prefix) - 3
	a.tool.owner.Messages[idx].ToolCalls[0].Function.Arguments["prompt"] = "mutated"
	if parent.Messages[2].ToolCalls[0].Function.Arguments["prompt"] != "check cache" {
		t.Fatal("fork mutated parent history")
	}
	if b.tool.owner.Messages[idx].ToolCalls[0].Function.Arguments["prompt"] != "check cache" {
		t.Fatal("fork mutated sibling history")
	}
	if _, err := m.Fork(a.tool.owner, "recursive"); err == nil {
		t.Fatal("recursive fork accepted")
	}
	close(release)
	waitFork(t, m, "fork-1", "completed")
	waitFork(t, m, "fork-2", "completed")
	if len(parent.Messages) != 6 {
		t.Fatal("background workers directly changed parent messages")
	}
	parent.deliverForkReports()
	if len(parent.Messages) != 8 {
		t.Fatal("completion reports not delivered")
	}
	parent.deliverForkReports()
	if len(parent.Messages) != 8 {
		t.Fatal("duplicate reports")
	}
}

func TestForkSnapshotFollowupAndPermissionIsolation(t *testing.T) {
	requests := make(chan []schema.Message, 2)
	client := forkClientFunc(func(_ context.Context, msgs []schema.Message, _ []tools.Tool) (*schema.LLMResponse, error) {
		requests <- cloneMessages(msgs)
		return &schema.LLMResponse{Content: "answer"}, nil
	})
	parent := New(client, "system", 10, 1, nil) // first fork must not compact the common prefix
	parent.AddUserMessage(strings.Repeat("context", 100))
	memCfg := memory.DefaultConfig()
	memCfg.WorkingDir = t.TempDir()
	mgr := memory.NewManager(memCfg, client)
	defer mgr.Close()
	mgr.SetCompressedSummary("inherited summary")
	parent.SetMemoryManager(mgr)
	parent.PermissionCtx.AddRule(permission.Rule{ToolName: "write_file", Behavior: permission.BehaviorALLOW, RuleContent: "first"})
	m := NewForkManager(context.Background())
	defer m.Close()
	parent.EnableForks(m)
	id, err := m.Fork(parent, "first task")
	if err != nil {
		t.Fatal(err)
	}
	waitFork(t, m, id, "completed")
	first := <-requests
	if first[1].Content != parent.Messages[1].Content {
		t.Fatal("initial prefix was compacted")
	}
	child := m.sessions[id].agent
	child.PermissionCtx.AllowRules["write_file"][0].RuleContent = "child only"
	child.PermissionCtx.AddRule(permission.Rule{ToolName: "bash", Behavior: permission.BehaviorALLOW})
	if parent.PermissionCtx.AllowRules["write_file"][0].RuleContent != "first" || len(parent.PermissionCtx.AllowRules["bash"]) != 0 {
		t.Fatal("permission rules are shared")
	}
	if child.contextState.manager != nil || child.Logger != nil || child.contextState.summaryText() != "inherited summary" {
		t.Fatal("stateful memory manager/logger was shared")
	}
	parent.AddUserMessage("parent's later message")
	child.TokenLimit = 100000
	if err := m.Send(id, "followup"); err != nil {
		t.Fatal(err)
	}
	waitFork(t, m, id, "completed")
	second := <-requests
	raw, _ := json.Marshal(second)
	if strings.Contains(string(raw), "parent's later message") {
		t.Fatal("parent followup leaked into worker")
	}
	if !strings.Contains(string(raw), "answer") || second[len(second)-1].Content != "followup" {
		t.Fatal("child followup did not continue its own history")
	}
	history, err := m.History(id)
	if err != nil {
		t.Fatal(err)
	}
	history[0].Content = "mutated view"
	if child.Messages[0].Content != "system" {
		t.Fatal("history view aliases the session")
	}
}

type forkAskTool struct{ executed atomic.Int32 }

func (t *forkAskTool) Name() string        { return "ask_tool" }
func (t *forkAskTool) Description() string { return "needs approval" }
func (t *forkAskTool) Parameters() map[string]interface{} {
	return map[string]interface{}{"type": "object"}
}
func (t *forkAskTool) Execute(context.Context, map[string]interface{}) (*tools.ToolResult, error) {
	t.executed.Add(1)
	return &tools.ToolResult{Success: true, Content: "executed"}, nil
}
func (t *forkAskTool) CheckPermissions(map[string]interface{}, *permission.Context) permission.Decision {
	return permission.Decision{Behavior: permission.BehaviorASK}
}
func (t *forkAskTool) IsConcurrencySafe() bool { return false }
func (t *forkAskTool) IsReadOnly() bool        { return false }
func (t *forkAskTool) IsExternalTool() bool    { return false }

func TestForkPermissionsRoutedBySession(t *testing.T) {
	tool := &forkAskTool{}
	client := forkClientFunc(func(_ context.Context, msgs []schema.Message, _ []tools.Tool) (*schema.LLMResponse, error) {
		if msgs[len(msgs)-1].Role == "tool" {
			return &schema.LLMResponse{Content: fmtContent(msgs[len(msgs)-1])}, nil
		}
		return &schema.LLMResponse{ToolCalls: []schema.ToolCall{{ID: "same-tool-id", Function: schema.FunctionCall{Name: tool.Name()}}}}, nil
	})
	parent := New(client, "system", 10, 10000, []tools.Tool{tool})
	m := NewForkManager(context.Background())
	defer m.Close()
	parent.EnableForks(m)
	a, _ := m.Fork(parent, "A")
	b, _ := m.Fork(parent, "B")
	waitFork(t, m, a, "waiting_permission")
	waitFork(t, m, b, "waiting_permission")
	if err := m.Approve(a, true, false); err != nil {
		t.Fatal(err)
	}
	if err := m.Approve(b, false, false); err != nil {
		t.Fatal(err)
	}
	one := waitFork(t, m, a, "completed")
	two := waitFork(t, m, b, "completed")
	if one.Result != "executed" || !strings.Contains(two.Result, "拒绝") || tool.executed.Load() != 1 {
		t.Fatalf("crossed approval responses: %+v %+v", one, two)
	}
	if err := m.Approve(a, true, false); err == nil {
		t.Fatal("stale approval accepted")
	}
}
func fmtContent(m schema.Message) string { s, _ := m.Content.(string); return s }

func TestForkCancellationLimitFailureAndClose(t *testing.T) {
	client := forkClientFunc(func(ctx context.Context, _ []schema.Message, _ []tools.Tool) (*schema.LLMResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	parent := New(client, "system", 5, 1000, nil)
	m := NewForkManager(context.Background())
	parent.EnableForks(m)
	for i := 0; i < 8; i++ {
		if _, err := m.Fork(parent, "task"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Fork(parent, "ninth"); err == nil {
		t.Fatal("concurrency bound ignored")
	}
	if _, err := m.History("fork-1"); err == nil {
		t.Fatal("running history read allowed")
	}
	if err := m.Send("fork-1", "busy"); err == nil {
		t.Fatal("concurrent message accepted")
	}
	if err := m.Cancel("fork-1"); err != nil {
		t.Fatal(err)
	}
	waitFork(t, m, "fork-1", "cancelled")
	m.Close()
	for _, s := range m.List() {
		if s.Status != "cancelled" {
			t.Fatal("shutdown left worker running")
		}
	}
	if _, err := m.Fork(parent, "closed"); err == nil {
		t.Fatal("spawn after close")
	}
	failure := New(forkClientFunc(func(context.Context, []schema.Message, []tools.Tool) (*schema.LLMResponse, error) {
		return nil, errors.New("provider failed")
	}), "system", 5, 1000, nil)
	n := NewForkManager(context.Background())
	defer n.Close()
	failure.EnableForks(n)
	id, _ := n.Fork(failure, "failure")
	s := waitFork(t, n, id, "failed")
	if s.Error != "provider failed" {
		t.Fatal("failure not reported")
	}
}

func TestForkCloseDuringPermissionAndRepair(t *testing.T) {
	tool := &forkAskTool{}
	client := forkClientFunc(func(context.Context, []schema.Message, []tools.Tool) (*schema.LLMResponse, error) {
		return &schema.LLMResponse{ToolCalls: []schema.ToolCall{{ID: "pending", Function: schema.FunctionCall{Name: tool.Name()}}}}, nil
	})
	parent := New(client, "system", 3, 1000, []tools.Tool{tool})
	m := NewForkManager(context.Background())
	parent.EnableForks(m)
	id, _ := m.Fork(parent, "task")
	waitFork(t, m, id, "waiting_permission")
	m.Close()
	if len(m.Pending()) != 0 {
		t.Fatal("stale permissions after shutdown")
	}
	history, _ := m.History(id)
	repaired := buildForkMessages(history, "new task")
	if repaired[len(repaired)-2].ToolCallID != "pending" {
		t.Fatal("cancelled tool batch was not paired")
	}
}

func TestForkStepLimitReportsFailure(t *testing.T) {
	tool := &forkAskTool{}
	client := forkClientFunc(func(context.Context, []schema.Message, []tools.Tool) (*schema.LLMResponse, error) {
		return &schema.LLMResponse{ToolCalls: []schema.ToolCall{{ID: "tool", Function: schema.FunctionCall{Name: tool.Name()}}}}, nil
	})
	parent := New(client, "system", 1, 1000, []tools.Tool{tool})
	m := NewForkManager(context.Background())
	defer m.Close()
	parent.EnableForks(m)
	id, _ := m.Fork(parent, "task")
	waitFork(t, m, id, "waiting_permission")
	if err := m.Approve(id, true, false); err != nil {
		t.Fatal(err)
	}
	info := waitFork(t, m, id, "failed")
	if !strings.Contains(info.Error, "maximum steps") {
		t.Fatal("step exhaustion was reported as success")
	}
}

func TestPermissionEngineUsesOwnSessionContext(t *testing.T) {
	parent := New(nil, "system", 1, 1000, nil)
	if parent.permissionEngine.Context != &parent.PermissionCtx {
		t.Fatal("engine uses a disconnected permission context")
	}
}

func TestToolPairRepairPreservesExistingResults(t *testing.T) {
	msgs := []schema.Message{{Role: "assistant", ToolCalls: []schema.ToolCall{forkCall("one", "A"), forkCall("two", "B")}}, {Role: "tool", ToolCallID: "one", Content: "actual result"}, {Role: "user", Content: "followup"}}
	repaired := completeToolResults(msgs, "interrupted")
	if len(repaired) != 4 || repaired[1].Content != "actual result" || repaired[2].ToolCallID != "two" || repaired[3].Content != "followup" {
		t.Fatal("invalid repaired history")
	}
	if !reflect.DeepEqual(repaired, completeToolResults(repaired, "interrupted")) {
		t.Fatal("repair is not idempotent")
	}
}
