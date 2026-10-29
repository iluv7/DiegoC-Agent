package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"diegoc-agent/internal/agent"
	"diegoc-agent/internal/logger"
	"diegoc-agent/internal/loop"
	"diegoc-agent/internal/schema"
)

type commandPrompt struct {
	answer string
	err    error
	calls  int
}

func (p *commandPrompt) Prompt(string) (string, error) {
	p.calls++
	return p.answer, p.err
}

func newTestDispatcher(t *testing.T) *commandDispatcher {
	t.Helper()
	a := agent.New(nil, "system", 5, 100000, nil)
	a.Logger = logger.NewInDirectory(t.TempDir())
	forks := agent.NewForkManager(context.Background())
	a.EnableForks(forks)
	sessions := agent.NewConversationSessions(a)
	t.Cleanup(forks.Close)
	t.Cleanup(sessions.Close)
	return &commandDispatcher{
		sessions: sessions, forks: forks, loops: loop.New(nil),
		workspaceDir: t.TempDir(), state: &commandPrompt{},
	}
}

func TestCommandDispatchKeepsLocalCommandsOutOfConversation(t *testing.T) {
	d := newTestDispatcher(t)
	a := d.sessions.Current().Agent
	before := append([]schema.Message(nil), a.Messages...)
	for _, command := range []string{
		"/help", "/history", "/stats", "/context", "/goal status",
		"/loop list", "/tasks", "/log", "/log 'missing.log'", "/unknown",
	} {
		if got := d.dispatch(command); got != (commandResult{}) {
			t.Fatalf("%q should stay local: %+v", command, got)
		}
	}
	if !reflect.DeepEqual(before, a.Messages) {
		t.Fatal("local commands changed conversation history")
	}
	for _, prompt := range []string{"实现功能\n保留  空格", "exit later", "EXIT"} {
		if got := d.dispatch(prompt); got.prompt != prompt || got.exit {
			t.Fatalf("ordinary input was changed: %+v", got)
		}
	}
	for _, command := range []string{"/exit", "/QUIT", "/q", "exit", "quit", "q"} {
		if got := d.dispatch(command); !got.exit || got.prompt != "" {
			t.Fatalf("%q did not exit locally: %+v", command, got)
		}
	}
}

func TestCommandDispatchExecuteRequiresSavedPlanAndApproval(t *testing.T) {
	for _, tc := range []struct {
		name, answer string
		savedPlan    bool
		promptErr    error
		approved     bool
	}{
		{name: "missing plan"},
		{name: "declined", savedPlan: true, answer: "n"},
		{name: "input cancelled", savedPlan: true, promptErr: errors.New("cancelled")},
		{name: "approved", savedPlan: true, answer: " YES ", approved: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newTestDispatcher(t)
			a := d.sessions.Current().Agent
			if got := d.dispatch("/PLAN"); got != (commandResult{}) || !a.PlanMode {
				t.Fatal("plan command did not enter planning locally")
			}
			if tc.savedPlan {
				if err := os.WriteFile(filepath.Join(d.workspaceDir, ".agent_plan.md"), []byte("Implement and verify."), 0644); err != nil {
					t.Fatal(err)
				}
			}
			prompt := &commandPrompt{answer: tc.answer, err: tc.promptErr}
			d.state = prompt
			got := d.dispatch("/EXECUTE")
			if got.exit || (got.prompt != "") != tc.approved || a.PlanMode == tc.approved {
				t.Fatalf("approval changed behavior: result=%+v plan=%v", got, a.PlanMode)
			}
			if tc.approved && got.prompt != "The plan in .agent_plan.md is approved. Execute it now, verify the result, and report what changed." {
				t.Fatalf("execution instruction changed: %q", got.prompt)
			}
			if (prompt.calls > 0) != tc.savedPlan || len(a.Messages) != 1 {
				t.Fatal("missing plan prompted for approval or command entered conversation")
			}
		})
	}
}

func TestCommandDispatchUsesActiveSessionAndClearKeepsOtherLoops(t *testing.T) {
	d := newTestDispatcher(t)
	root := d.sessions.Current().Agent
	root.AddUserMessage("main conversation")
	if _, err := d.loops.Create("main", "main task", time.Minute); err != nil {
		t.Fatal(err)
	}
	if got := d.dispatch("/fork"); got != (commandResult{}) || d.sessions.Current().ID != "session-1" {
		t.Fatal("fork did not switch sessions locally")
	}
	child := d.sessions.Current().Agent
	if got := d.dispatch("/goal finish the task"); !strings.Contains(got.prompt, "finish the task") || got.exit {
		t.Fatalf("goal did not return work for the runner: %+v", got)
	}
	if root.GoalStatus().Status != "" || child.GoalStatus().Status != "active" {
		t.Fatal("goal targeted the wrong session")
	}
	d.dispatch("/loop 1m child task")
	if got := d.dispatch("/clear"); got != (commandResult{}) {
		t.Fatal("clear triggered a model run")
	}
	if len(child.Messages) != 1 || child.GoalStatus().Status != "" || len(d.loops.List("session-1")) != 0 {
		t.Fatal("clear did not reset current session messages, goal and loops")
	}
	if len(root.Messages) != 2 || len(d.loops.List("main")) != 1 {
		t.Fatal("clearing a child affected the main session")
	}
	if got := d.dispatch("/switch main"); got != (commandResult{}) || d.sessions.Current().Agent != root {
		t.Fatal("switch did not return to main locally")
	}
}

func TestCommandDispatchRewindRetainsConfirmation(t *testing.T) {
	d := newTestDispatcher(t)
	a := d.sessions.Current().Agent
	a.AddUserMessage("first turn")
	prompt := &commandPrompt{answer: "n"}
	d.state = prompt
	if got := d.dispatch("/rewind 1 conversation"); got != (commandResult{}) || len(a.Messages) != 2 {
		t.Fatal("declined rewind changed conversation or triggered a run")
	}
	prompt.answer = "y"
	if got := d.dispatch("/rewind 1 conversation"); got != (commandResult{}) || len(a.Messages) != 1 || prompt.calls != 2 {
		t.Fatal("confirmed rewind did not restore the selected checkpoint locally")
	}
}
