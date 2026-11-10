package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"

	"diegoc-agent/internal/schema"
)

// LoopDetectionConfig counts identical tool-call batches in recent model turns.
// Zero values use defaults. The window must contain the hard limit.
type LoopDetectionConfig struct {
	WarnThreshold int
	HardLimit     int
	WindowSize    int
}

// LoopDetectedError stops execution before the repeated batch is recorded or run.
// Returning an error also prevents goal continuations from bypassing the limit.
type LoopDetectedError struct{ Count, Limit int }

func (e *LoopDetectedError) Error() string {
	return fmt.Sprintf("tool-call loop detected: identical batch repeated %d times (limit %d); stopped before executing tools", e.Count, e.Limit)
}

// LoopDetectionMiddleware adapts deer-flow's identical-call-set guard:
// https://github.com/bytedance/deer-flow/blob/main/backend/packages/harness/deerflow/agents/middlewares/loop_detection_middleware.py
// Each session must get its own instance through a MiddlewareFactory. Like Agent,
// an instance is used by one reply at a time. Auxiliary model calls are excluded.
type LoopDetectionMiddleware struct {
	config  LoopDetectionConfig
	history [][32]byte
	warned  map[[32]byte]bool
	pending string
}

func NewLoopDetectionMiddleware(cfg LoopDetectionConfig) (*LoopDetectionMiddleware, error) {
	if cfg.WarnThreshold == 0 {
		cfg.WarnThreshold = 3
	}
	if cfg.HardLimit == 0 {
		cfg.HardLimit = 5
	}
	if cfg.WindowSize == 0 {
		cfg.WindowSize = 20
	}
	if cfg.WarnThreshold < 1 || cfg.HardLimit <= cfg.WarnThreshold || cfg.WindowSize < cfg.HardLimit {
		return nil, fmt.Errorf("loop detection requires 0 < warn threshold < hard limit <= window size")
	}
	return &LoopDetectionMiddleware{config: cfg}, nil
}

func (m *LoopDetectionMiddleware) reset() {
	m.history = nil
	m.warned = make(map[[32]byte]bool)
	m.pending = ""
}

// OnReply keeps the budget across hidden goal continuations, but resets it for
// the next user reply, even after failure or cancellation.
func (m *LoopDetectionMiddleware) OnReply(ctx context.Context, _ *Agent, next ReplyHandler) (string, error) {
	m.reset()
	defer m.reset()
	return next(ctx)
}

func (m *LoopDetectionMiddleware) OnModelCall(ctx context.Context, _ *Agent, req ModelRequest, next ModelHandler) (*schema.LLMResponse, error) {
	if m.pending != "" {
		// Copy the slice so transient warnings never enter persisted history.
		// Tool results already precede this message, preserving provider pairing.
		req.Messages = append(append([]schema.Message(nil), req.Messages...), schema.Message{
			Role: "user", Content: m.pending,
		})
	}
	resp, err := next(ctx, req)
	if err != nil || resp == nil {
		// Keep the warning queued when an outer middleware retries the call.
		return resp, err
	}
	m.pending = ""
	if len(resp.ToolCalls) == 0 {
		return resp, nil
	}
	key, err := toolCallFingerprint(resp.ToolCalls)
	if err != nil {
		return nil, fmt.Errorf("fingerprint tool calls: %w", err)
	}
	m.history = append(m.history, key)
	if len(m.history) > m.config.WindowSize {
		m.history = m.history[1:]
	}
	counts := make(map[[32]byte]int)
	for _, previous := range m.history {
		counts[previous]++
	}
	for previous := range m.warned {
		if counts[previous] == 0 {
			delete(m.warned, previous)
		}
	}
	count := counts[key]
	if count >= m.config.HardLimit {
		return nil, &LoopDetectedError{Count: count, Limit: m.config.HardLimit}
	}
	if count >= m.config.WarnThreshold && !m.warned[key] {
		if m.warned == nil {
			m.warned = make(map[[32]byte]bool)
		}
		m.warned[key] = true
		m.pending = fmt.Sprintf("Loop warning: the same tool-call batch has occurred %d times in recent turns. Stop repeating these calls. Use a different approach or finish with the results available and explain any blocker.", count)
	}
	return resp, nil
}

// IDs and batch ordering do not change the operation. Full arguments do:
// paging offsets and changed write contents must remain distinct. JSON encoding
// sorts map keys; the executor's provider argument normalization is reused.
func toolCallFingerprint(calls []schema.ToolCall) ([32]byte, error) {
	keys := make([]string, 0, len(calls))
	for _, call := range calls {
		args := toolArgsFromRaw(call.Function.Arguments)
		if args == nil {
			args = map[string]interface{}{}
		}
		data, err := json.Marshal(schema.FunctionCall{Name: call.Function.Name, Arguments: args})
		if err != nil {
			return [32]byte{}, err
		}
		keys = append(keys, string(data))
	}
	sort.Strings(keys)
	data, err := json.Marshal(keys)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(data), nil
}
