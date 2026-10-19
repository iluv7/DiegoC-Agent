package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"diegoc-agent/internal/permission"
	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

const forkPlaceholder = "Fork started — processing in background"

// ForkInfo is a race-free view of an independently running conversation.
type ForkInfo struct {
	ID, Task, Status, Result, Error string
	Messages                        int
}
type ForkEvent struct{ SessionID, Status, Result, Error string }
type ForkPermission struct {
	SessionID string
	Request   permission.HITLConfirmRequest
	// Reply is private to this request, so two workers cannot consume each other's approval.
	Reply chan permission.HITLConfirmResponse
}

type forkSession struct {
	info   ForkInfo
	agent  *Agent
	parent *Agent
	cancel context.CancelFunc
}

// ForkManager owns worker lifetimes independently of a parent's individual turn.
// Agent mutation is confined to its runner. Only events/inboxes cross goroutines.
type ForkManager struct {
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.Mutex
	sessions    map[string]*forkSession
	sequence    int
	events      []ForkEvent
	permissions []*ForkPermission
	pending     map[string]*ForkPermission
	wg          sync.WaitGroup
	closed      bool
	toolMu      sync.Mutex
}

func NewForkManager(ctx context.Context) *ForkManager {
	ctx, cancel := context.WithCancel(ctx)
	return &ForkManager{ctx: ctx, cancel: cancel, sessions: map[string]*forkSession{}, pending: map[string]*ForkPermission{}}
}

// EnableForks must be called while the parent is idle. Children retain the same
// Agent schema but reject recursive spawning at execution time.
func (a *Agent) EnableForks(m *ForkManager) {
	a.forks = m
	for _, t := range a.Tools {
		if t.Name() == "Agent" {
			return
		}
	}
	t := &ForkTool{owner: a}
	a.Tools = append(a.Tools, t)
	a.toolByName[t.Name()] = t
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

// Fork is called by the parent runner (or the CLI while the parent is idle).
func (m *ForkManager) Fork(parent *Agent, task string) (string, error) {
	if parent.isFork {
		return "", errors.New("fork sessions cannot recursively fork")
	}
	if strings.TrimSpace(task) == "" {
		return "", errors.New("fork task is required")
	}
	history := parent.Messages
	if parent.forkBatch != nil {
		history = parent.forkBatch
	}
	// Stateful permission rules, tool bindings, messages and summaries are not shared.
	list := append([]tools.Tool(nil), parent.Tools...)
	child := NewWithPermission(parent.LLM, parent.SystemPrompt, parent.MaxSteps, parent.TokenLimit, list, copyPermission(parent.PermissionCtx))
	child.Messages = buildForkMessages(history, task)
	child.compressedSummary = parent.compressedSummary
	child.contextLimit = parent.contextLimit
	child.compactLanguage = parent.compactLanguage
	child.skipNextTokenCheck = true // Preserve the inherited API prefix on the first request.
	child.isFork = true
	child.forks = m
	for i, t := range child.Tools {
		if t.Name() == "Agent" {
			bound := &ForkTool{owner: child}
			child.Tools[i] = bound
			child.toolByName[bound.Name()] = bound
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.ctx.Err() != nil {
		return "", errors.New("fork manager is closed")
	}
	active := 0
	for _, s := range m.sessions {
		if s.cancel != nil {
			active++
		}
	}
	if active >= 8 {
		return "", errors.New("at most 8 fork sessions may run concurrently")
	}
	m.sequence++
	id := fmt.Sprintf("fork-%d", m.sequence)
	s := &forkSession{info: ForkInfo{ID: id, Task: task, Status: "running"}, agent: child, parent: parent}
	m.sessions[id] = s
	m.launchLocked(s)
	return id, nil
}

func (m *ForkManager) launchLocked(s *forkSession) {
	ctx, cancel := context.WithCancel(m.ctx)
	s.cancel = cancel
	s.info.Status = "running"
	s.info.Result = ""
	s.info.Error = ""
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer cancel()
		requests := make(chan permission.HITLConfirmRequest)
		responses := make(chan permission.HITLConfirmResponse)
		// The broker never reads stdin; approvals are handled by the parent terminal.
		brokerDone := make(chan struct{})
		go func() {
			defer close(brokerDone)
			for {
				select {
				case <-ctx.Done():
					return
				case req := <-requests:
					p := &ForkPermission{SessionID: s.info.ID, Request: req, Reply: make(chan permission.HITLConfirmResponse, 1)}
					m.mu.Lock()
					m.pending[s.info.ID] = p
					m.permissions = append(m.permissions, p)
					s.info.Status = "waiting_permission"
					m.mu.Unlock()
					select {
					case <-ctx.Done():
						return
					case reply := <-p.Reply:
						m.mu.Lock()
						delete(m.pending, s.info.ID)
						s.info.Status = "running"
						m.mu.Unlock()
						select {
						case responses <- reply:
						case <-ctx.Done():
							return
						}
					}
				}
			}
		}()
		out, err := s.agent.RunWithHITL(ctx, responses, requests)
		cancel()
		<-brokerDone
		status := "completed"
		errText := ""
		if err != nil {
			status = "failed"
			errText = err.Error()
		}
		if ctx.Err() != nil && errors.Is(err, context.Canceled) {
			status = "cancelled"
		}
		m.mu.Lock()
		s.info.Status = status
		s.info.Result = out
		s.info.Error = errText
		s.info.Messages = len(s.agent.Messages)
		s.cancel = nil
		delete(m.pending, s.info.ID)
		event := ForkEvent{SessionID: s.info.ID, Status: status, Result: out, Error: errText}
		m.events = append(m.events, event)
		// Queue a single report; never append to a running parent's Messages.
		s.parent.queueForkReport(event)
		m.mu.Unlock()
	}()
}

// Send continues a completed worker's own history; no new parent messages sync.
func (m *ForkManager) Send(id, task string) error {
	if strings.TrimSpace(task) == "" {
		return errors.New("message is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.ctx.Err() != nil {
		return errors.New("fork manager is closed")
	}
	s, ok := m.sessions[id]
	if !ok {
		return errors.New("fork session not found")
	}
	if s.cancel != nil {
		return errors.New("fork session is still running")
	}
	active := 0
	for _, entry := range m.sessions {
		if entry.cancel != nil {
			active++
		}
	}
	if active >= 8 {
		return errors.New("at most 8 fork sessions may run concurrently")
	}
	s.agent.cleanupPendingToolCalls()
	s.agent.needsToolRepair = false
	s.agent.AddUserMessage(task)
	s.info.Task = task
	m.launchLocked(s)
	return nil
}

func (m *ForkManager) Cancel(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return errors.New("fork session not found")
	}
	if s.cancel == nil {
		return errors.New("fork session is not running")
	}
	s.cancel()
	return nil
}
func (m *ForkManager) List() []ForkInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	list := make([]ForkInfo, 0, len(m.sessions))
	for _, s := range m.sessions {
		list = append(list, s.info)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	return list
}
func (m *ForkManager) Events() ([]ForkEvent, []*ForkPermission) {
	m.mu.Lock()
	defer m.mu.Unlock()
	events := m.events
	var perms []*ForkPermission
	for _, p := range m.permissions {
		if m.pending[p.SessionID] == p {
			perms = append(perms, p)
		}
	}
	m.events = nil
	m.permissions = nil
	return events, perms
}
func (m *ForkManager) Pending() []*ForkPermission {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*ForkPermission, 0, len(m.pending))
	for _, p := range m.pending {
		out = append(out, p)
	}
	return out
}
func (m *ForkManager) Approve(id string, allow, always bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pending[id]
	if !ok {
		return errors.New("no pending permission for that session")
	}
	response := permission.HITLConfirmResponse{ReplyID: p.Request.ReplyID}
	for _, tc := range p.Request.ToolCalls {
		var rules []permission.Rule
		if allow && always {
			rules = tc.SuggestedRules
		}
		response.Results = append(response.Results, permission.ToolConfirmResult{ToolCall: tc, Confirmed: allow, Rules: rules})
	}
	select {
	case p.Reply <- response:
		delete(m.pending, id)
		return nil
	default:
		return errors.New("permission already answered")
	}
}
func (m *ForkManager) Close() { m.mu.Lock(); m.closed = true; m.cancel(); m.mu.Unlock(); m.wg.Wait() }

func (a *Agent) queueForkReport(event ForkEvent) {
	a.inboxMu.Lock()
	defer a.inboxMu.Unlock()
	a.forkReports = append(a.forkReports, event)
}
func (a *Agent) deliverForkReports() {
	a.inboxMu.Lock()
	reports := a.forkReports
	a.forkReports = nil
	a.inboxMu.Unlock()
	for _, r := range reports {
		text := fmt.Sprintf("Fork session %s %s.\nResult:\n%s", r.SessionID, r.Status, r.Result)
		if r.Error != "" {
			text += "\nError: " + r.Error
		}
		a.Messages = append(a.Messages, schema.Message{Role: "user", Content: text})
	}
}

// Complete cancelled/error batches before accepting another user message.
func (a *Agent) cleanupPendingToolCalls() {
	a.Messages = completeToolResults(a.Messages, "Tool execution was interrupted before a result was returned.")
}

func completeToolResults(messages []schema.Message, placeholder string) []schema.Message {
	result := make([]schema.Message, 0, len(messages))
	for i := 0; i < len(messages); i++ {
		msg := messages[i]
		result = append(result, msg)
		if msg.Role != "assistant" || len(msg.ToolCalls) == 0 {
			continue
		}
		seen := map[string]bool{}
		for i+1 < len(messages) && messages[i+1].Role == "tool" {
			i++
			result = append(result, messages[i])
			seen[messages[i].ToolCallID] = true
		}
		for _, tc := range msg.ToolCalls {
			if !seen[tc.ID] {
				result = append(result, schema.Message{Role: "tool", Name: tc.Function.Name, ToolCallID: tc.ID, Content: placeholder})
			}
		}
	}
	return result
}

// ForkTool lives in agent to avoid a tools -> agent dependency cycle.
type ForkTool struct{ owner *Agent }

func (t *ForkTool) Name() string { return "Agent" }
func (t *ForkTool) Description() string {
	return "Start an independent background fork session with your current context, model, system prompt and tools. Provide a concrete task. Returns a session ID immediately; completion is reported later. Conversations are separate, but files and disk memory are shared. Fork sessions cannot recursively fork."
}
func (t *ForkTool) Parameters() map[string]interface{} {
	return map[string]interface{}{"type": "object", "properties": map[string]interface{}{"prompt": map[string]interface{}{"type": "string", "description": "Task for the fork session"}}, "required": []interface{}{"prompt"}}
}
func (t *ForkTool) Execute(ctx context.Context, args map[string]interface{}) (*tools.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	task, _ := args["prompt"].(string)
	if t.owner.forks == nil {
		return nil, errors.New("fork sessions are not enabled")
	}
	id, err := t.owner.forks.Fork(t.owner, task)
	if err != nil {
		return &tools.ToolResult{Error: err.Error()}, nil
	}
	return &tools.ToolResult{Success: true, Content: "Started background fork session " + id}, nil
}
func (t *ForkTool) CheckPermissions(_ map[string]interface{}, _ *permission.Context) permission.Decision {
	return permission.Decision{Behavior: permission.BehaviorALLOW}
}
func (t *ForkTool) IsConcurrencySafe() bool { return false }
func (t *ForkTool) IsReadOnly() bool        { return false }
func (t *ForkTool) IsExternalTool() bool    { return false }

// History returns a snapshot only while idle; running conversations cannot be read.
func (m *ForkManager) History(id string) ([]schema.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, errors.New("fork session not found")
	}
	if s.cancel != nil {
		return nil, errors.New("history is available after this session stops")
	}
	return cloneMessages(s.agent.Messages), nil
}
