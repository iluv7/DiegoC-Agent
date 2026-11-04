package agent

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

type testReplyHook struct {
	name  string
	trace *[]string
}

func (m *testReplyHook) OnReply(ctx context.Context, _ *Agent, next ReplyHandler) (string, error) {
	*m.trace = append(*m.trace, m.name+" before")
	out, err := next(ctx)
	*m.trace = append(*m.trace, m.name+" after")
	return out, err
}

type testModelHook struct{ calls int }

func (m *testModelHook) OnModelCall(_ context.Context, _ *Agent, req ModelRequest, _ ModelHandler) (*schema.LLMResponse, error) {
	m.calls++
	return &schema.LLMResponse{Content: req.Messages[0].Content.(string)}, nil
}

type testPromptHook struct{}

func (testPromptHook) OnSystemPrompt(_ context.Context, _ *Agent, p string) (string, error) {
	return p + " transformed", nil
}

type testActHook struct{ called bool }

func (m *testActHook) OnActing(_ context.Context, _ *Agent, _ ToolExecution, _ ActingHandler) (*tools.ToolResult, error) {
	m.called = true
	return nil, errors.New("blocked")
}

func TestMiddlewareOrderAndError(t *testing.T) {
	a := New(nil, "system", 3, 1000, nil)
	var trace []string
	a.SetMiddlewareFactories(func() any { return &testReplyHook{"a", &trace} }, func() any { return &testReplyHook{"b", &trace} })
	wantErr := errors.New("terminal")
	_, err := a.reply(context.Background(), func(context.Context) (string, error) { trace = append(trace, "terminal"); return "", wantErr })
	if !errors.Is(err, wantErr) || !reflect.DeepEqual(trace, []string{"a before", "b before", "terminal", "b after", "a after"}) {
		t.Fatalf("%v %v", trace, err)
	}
}
func TestMiddlewareIntegrationAndForkIsolation(t *testing.T) {
	a := New(nil, "system", 3, 1000, nil)
	a.SetMiddlewareFactories(func() any { return &testModelHook{} }, func() any { return testPromptHook{} })
	a.AddUserMessage("hello")
	out, err := a.RunWithHITL(context.Background(), nil, nil)
	if err != nil || out != "system transformed" || a.Messages[0].Content != "system" {
		t.Fatalf("%q %v %+v", out, err, a.Messages)
	}
	for _, kind := range []sessionCloneKind{foregroundSessionClone, backgroundTaskClone} {
		child := a.cloneSession(kind, nil, "task")
		if child.middlewares[0] == a.middlewares[0] || child.middlewares[0].(*testModelHook).calls != 0 {
			t.Fatal("middleware state shared across sessions")
		}
		if _, err := child.RunWithHITL(context.Background(), nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if a.middlewares[0].(*testModelHook).calls != 1 {
		t.Fatal("child changed parent middleware")
	}
}
func TestActingShortCircuitAndCancellation(t *testing.T) {
	a := New(nil, "system", 3, 1000, nil)
	hook := &testActHook{}
	a.SetMiddlewareFactories(func() any { return hook })
	_, err := a.act(context.Background(), ToolExecution{Name: "test"}, func(context.Context) (*tools.ToolResult, error) {
		t.Fatal("short circuit called terminal")
		return nil, nil
	})
	if err == nil || !hook.called {
		t.Fatal("acting hook not applied")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.RunWithHITL(ctx, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
