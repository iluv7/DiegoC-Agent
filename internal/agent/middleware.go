package agent

import (
	"context"
	"fmt"

	"diegoc-agent/internal/llm"
	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

// ReplyHandler runs one complete reply, including any goal continuations.
type ReplyHandler func(context.Context) (string, error)

// ReasoningHandler prepares context and performs one reasoning step.
type ReasoningHandler func(context.Context) (*schema.LLMResponse, error)

// ModelRequest describes a main reasoning call. Auxiliary summary and goal
// evaluation calls deliberately do not pass through this hook.
type ModelRequest struct {
	Client   llm.Client
	Messages []schema.Message
	Tools    []tools.Tool
}

// ModelHandler performs a logical model call, including provider retries.
type ModelHandler func(context.Context, ModelRequest) (*schema.LLMResponse, error)

// ActingHandler executes an already approved tool with its approved arguments.
// Arguments cannot be replaced here: permission checks and rewind tracking
// belong to the executor outside this hook.
type ActingHandler func(context.Context) (*tools.ToolResult, error)

// ToolExecution identifies the approved operation without exposing mutable args.
type ToolExecution struct{ ID, Name string }

// ReplyMiddleware optionally wraps a complete reply.
type ReplyMiddleware interface {
	OnReply(context.Context, *Agent, ReplyHandler) (string, error)
}

// ReasoningMiddleware optionally wraps context preparation and model inference.
type ReasoningMiddleware interface {
	OnReasoning(context.Context, *Agent, ReasoningHandler) (*schema.LLMResponse, error)
}

// ModelCallMiddleware optionally transforms or intercepts main model requests.
type ModelCallMiddleware interface {
	OnModelCall(context.Context, *Agent, ModelRequest, ModelHandler) (*schema.LLMResponse, error)
}

// ActingMiddleware optionally wraps raw tool execution, after approval.
type ActingMiddleware interface {
	OnActing(context.Context, *Agent, ToolExecution, ActingHandler) (*tools.ToolResult, error)
}

// SystemPromptMiddleware transforms the outgoing prompt without persisting it.
type SystemPromptMiddleware interface {
	OnSystemPrompt(context.Context, *Agent, string) (string, error)
}

// MiddlewareFactory creates a fresh middleware instance for each session.
// The same factory is inherited by foreground and background forks. Register
// only while the agent is idle. Factories must support concurrent fork creation.
type MiddlewareFactory func() any

// SetMiddlewareFactories replaces the registered middleware chain. Each object
// may implement any subset of the hook interfaces. Registration order is outer
// to inner; prompt transformers run in registration order. Empty input disables
// middleware. Session state belongs in the factory-created instance; do not
// return shared mutable instances from a factory.
func (a *Agent) SetMiddlewareFactories(factories ...MiddlewareFactory) {
	a.middlewareFactories = append([]MiddlewareFactory(nil), factories...)
	a.middlewares = nil
	for _, factory := range factories {
		if factory == nil {
			continue
		}
		if middleware := factory(); middleware != nil {
			a.middlewares = append(a.middlewares, middleware)
		}
	}
}

func chainMiddleware[H any](middlewares []any, terminal H, wrap func(any, H) H) H {
	for i := len(middlewares) - 1; i >= 0; i-- {
		terminal = wrap(middlewares[i], terminal)
	}
	return terminal
}

func (a *Agent) reply(ctx context.Context, terminal ReplyHandler) (string, error) {
	handler := chainMiddleware(a.middlewares, terminal, func(m any, next ReplyHandler) ReplyHandler {
		if hook, ok := m.(ReplyMiddleware); ok {
			return func(ctx context.Context) (string, error) { return hook.OnReply(ctx, a, next) }
		}
		return next
	})
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return handler(ctx)
}

func (a *Agent) reasoning(ctx context.Context) (*schema.LLMResponse, error) {
	handler := chainMiddleware(a.middlewares, ReasoningHandler(a.reasoningImpl), func(m any, next ReasoningHandler) ReasoningHandler {
		if hook, ok := m.(ReasoningMiddleware); ok {
			return func(ctx context.Context) (*schema.LLMResponse, error) { return hook.OnReasoning(ctx, a, next) }
		}
		return next
	})
	resp, err := handler(ctx)
	if err == nil && resp == nil {
		err = fmt.Errorf("reasoning middleware returned a nil response")
	}
	return resp, err
}

func (a *Agent) callModel(ctx context.Context, req ModelRequest) (*schema.LLMResponse, error) {
	terminal := ModelHandler(func(ctx context.Context, req ModelRequest) (*schema.LLMResponse, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if req.Client == nil {
			return nil, fmt.Errorf("model request has no client")
		}
		return req.Client.Generate(ctx, req.Messages, req.Tools)
	})
	handler := chainMiddleware(a.middlewares, terminal, func(m any, next ModelHandler) ModelHandler {
		if hook, ok := m.(ModelCallMiddleware); ok {
			return func(ctx context.Context, req ModelRequest) (*schema.LLMResponse, error) {
				return hook.OnModelCall(ctx, a, req, next)
			}
		}
		return next
	})
	return handler(ctx, req)
}

func (a *Agent) act(ctx context.Context, call ToolExecution, terminal ActingHandler) (*tools.ToolResult, error) {
	handler := chainMiddleware(a.middlewares, terminal, func(m any, next ActingHandler) ActingHandler {
		if hook, ok := m.(ActingMiddleware); ok {
			return func(ctx context.Context) (*tools.ToolResult, error) { return hook.OnActing(ctx, a, call, next) }
		}
		return next
	})
	result, err := handler(ctx)
	if err == nil && result == nil {
		err = fmt.Errorf("acting middleware returned a nil result")
	}
	return result, err
}
