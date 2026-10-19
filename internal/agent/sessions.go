package agent

import (
	"fmt"
	"strings"
	"time"

	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

// ConversationSession is a foreground conversation, not a background task.
// The CLI is its sole runner; switching never appends messages or invokes a model.
type ConversationSession struct {
	ID        string
	ParentID  string
	CreatedAt time.Time
	Agent     *Agent
}

// MaxForkSessions excludes the original main session and includes nested forks.
const MaxForkSessions = 7

type ConversationSessions struct {
	sessions []*ConversationSession
	active   int
}

func NewConversationSessions(root *Agent) *ConversationSessions {
	return &ConversationSessions{sessions: []*ConversationSession{{ID: "main", Agent: root, CreatedAt: time.Now()}}}
}
func (s *ConversationSessions) Current() *ConversationSession { return s.sessions[s.active] }
func (s *ConversationSessions) List() []*ConversationSession {
	return append([]*ConversationSession(nil), s.sessions...)
}

// Fork duplicates the current context without adding a task, synthetic message,
// completion report or API call. The newly created conversation becomes active.
func (s *ConversationSessions) Fork() (*ConversationSession, error) {
	if len(s.sessions)-1 >= MaxForkSessions {
		return nil, fmt.Errorf("最多创建 %d 个 fork session（不含 main），当前会话保持不变", MaxForkSessions)
	}
	parent := s.Current()
	child := parent.Agent.cloneConversation()
	entry := &ConversationSession{ID: fmt.Sprintf("session-%d", len(s.sessions)), ParentID: parent.ID, Agent: child, CreatedAt: time.Now()}
	s.sessions = append(s.sessions, entry)
	s.active = len(s.sessions) - 1
	return entry, nil
}
func (s *ConversationSessions) Switch(id string) error {
	for i, entry := range s.sessions {
		if entry.ID == strings.TrimSpace(id) {
			s.active = i
			return nil
		}
	}
	return fmt.Errorf("session %q not found; use /sessions to choose a session", id)
}

func (a *Agent) cloneConversation() *Agent {
	child := NewWithPermission(a.LLM, a.SystemPrompt, a.MaxSteps, a.TokenLimit, append([]tools.Tool(nil), a.Tools...), copyPermission(a.PermissionCtx))
	child.Messages = cloneMessages(a.Messages)
	child.compressedSummary = a.compressedSummary
	child.contextLimit = a.contextLimit
	child.compactLanguage = a.compactLanguage
	if a.memoryManager != nil {
		child.compressedSummary = a.memoryManager.CompressedSummary()
	}
	child.skipNextTokenCheck = true
	child.needsToolRepair = a.needsToolRepair
	child.forks = a.forks
	// A foreground clone can itself fork. It does not inherit worker-only limits,
	// a parent's pending reports, token billing counters, logger or mutable memory pipeline.
	for i, t := range child.Tools {
		if t.Name() == "Agent" {
			bound := &ForkTool{owner: child}
			child.Tools[i] = bound
			child.toolByName[bound.Name()] = bound
		}
	}
	return child
}

// ConversationPreview contains only user/assistant text for terminal replay.
func (s *ConversationSession) ConversationPreview() []schema.Message {
	var result []schema.Message
	for _, msg := range s.Agent.Messages {
		if (msg.Role == "user" || msg.Role == "assistant") && contentString(msg) != "" {
			result = append(result, schema.Message{Role: msg.Role, Content: contentString(msg)})
		}
	}
	return result
}

func (s *ConversationSessions) Close() {
	for _, entry := range s.sessions {
		if entry.Agent.Logger != nil {
			entry.Agent.Logger.Close()
		}
	}
}
