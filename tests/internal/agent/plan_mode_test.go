package agent

import (
	"reflect"
	"strings"
	"testing"

	"diegoc-agent/internal/permission"
	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

func TestPlanModeRoundTripRestoresPermissionPolicy(t *testing.T) {
	for _, mode := range []permission.Mode{permission.ModeDefault, permission.ModeAcceptEdits, permission.ModeBypass, permission.ModeDontAsk} {
		t.Run(string(mode), func(t *testing.T) {
			p := permission.NewContext()
			p.Mode = mode
			a := NewWithPermission(nil, "project rules", 5, 10000, nil, p)
			if a.PlanMode() || a.Messages[0].Content != a.SystemPrompt {
				t.Fatal("new execution session unexpectedly entered planning")
			}
			for round := 0; round < 2; round++ {
				a.SetPlanMode(true)
				before := cloneMessages(a.Messages)
				a.SetPlanMode(true)
				if !a.PlanMode() || a.PermissionCtx.PrePlanMode != mode || !reflect.DeepEqual(before, a.Messages) || !strings.Contains(a.Messages[0].Content.(string), "## Plan Mode\n") {
					t.Fatal("planning lost the previous permission policy or duplicated the prompt")
				}
				a.SetPlanMode(false)
				before = cloneMessages(a.Messages)
				a.SetPlanMode(false)
				if a.PlanMode() || a.PermissionCtx.Mode != mode || a.PermissionCtx.PrePlanMode != "" || !reflect.DeepEqual(before, a.Messages) || !strings.Contains(a.Messages[0].Content.(string), "## Execute Mode\n") || strings.Count(a.Messages[0].Content.(string), "project rules") != 1 {
					t.Fatal("approval did not restore the original policy and execution prompt")
				}
			}
		})
	}
}

func TestPlanModeFromPermissionContextAndClear(t *testing.T) {
	for _, previous := range []permission.Mode{"", permission.ModeExplore, permission.ModeAcceptEdits} {
		p := permission.NewContext()
		p.Mode, p.PrePlanMode = permission.ModeExplore, previous
		a := NewWithPermission(nil, "project rules", 5, 10000, nil, p)
		if !a.PlanMode() || !strings.Contains(a.Messages[0].Content.(string), "## Plan Mode\n") {
			t.Fatal("custom exploration context was not recognized as planning")
		}
		a.AddUserMessage("plan this")
		a.ClearConversation()
		if !a.PlanMode() || len(a.Messages) != 1 || !strings.Contains(a.Messages[0].Content.(string), "## Plan Mode\n") || a.PermissionCtx.PrePlanMode != previous {
			t.Fatal("clear lost the plan prompt or its approval policy")
		}
		a.SetPlanMode(false)
		want := previous
		if want == "" || want == permission.ModeExplore {
			want = permission.ModeDefault
		}
		if a.PlanMode() || a.PermissionCtx.Mode != want {
			t.Fatal("custom exploration session could not leave planning safely")
		}
		a.ClearConversation()
		if a.Messages[0].Content != "project rules" || a.PlanMode() || a.PermissionCtx.Mode != want {
			t.Fatal("clear changed the execution permission policy")
		}
	}
}

func TestPlanModeClonesCanApproveWithoutChangingParent(t *testing.T) {
	for _, kind := range []sessionCloneKind{foregroundSessionClone, backgroundTaskClone} {
		p := permission.NewContext()
		p.Mode = permission.ModeBypass
		workspace := t.TempDir()
		write := &tools.WriteTool{WorkspaceDir: workspace}
		parent := NewWithPermission(nil, "project rules", 5, 10000, []tools.Tool{write}, p)
		parent.SetPlanMode(true)
		parent.AddUserMessage("prepare a plan")
		child := parent.cloneSession(kind, nil, "inspect only")
		call := schema.ToolCall{ID: "write", Function: schema.FunctionCall{Name: "write_file"}}
		args := map[string]interface{}{"path": "file", "content": "data"}
		if !child.PlanMode() || child.PermissionCtx.PrePlanMode != permission.ModeBypass || child.Messages[0].Content != parent.Messages[0].Content || child.executor.checkPermission(call, write, args).Behavior != permission.BehaviorDENY {
			t.Fatal("clone lost planning or bypassed its read-only boundary")
		}
		child.SetPlanMode(false)
		if child.PlanMode() || child.PermissionCtx.Mode != permission.ModeBypass || child.executor.checkPermission(call, write, args).Behavior != permission.BehaviorALLOW || !strings.Contains(child.Messages[0].Content.(string), "## Execute Mode\n") {
			t.Fatal("clone approval left permissions in exploration mode")
		}
		if !parent.PlanMode() || parent.PermissionCtx.PrePlanMode != permission.ModeBypass || !strings.Contains(parent.Messages[0].Content.(string), "## Plan Mode\n") {
			t.Fatal("clone approval changed its parent")
		}
	}
}

func TestPlanModeRewindRestoresApprovalPolicyAndPrompt(t *testing.T) {
	for _, rewindMode := range []string{"conversation", "both"} {
		t.Run(rewindMode, func(t *testing.T) {
			p := permission.NewContext()
			p.Mode = permission.ModeBypass
			a := NewWithPermission(nil, "project rules", 5, 10000, nil, p)
			a.AddUserMessage("before planning")
			a.SetPlanMode(true)
			a.AddUserMessage("during planning")
			a.SetPlanMode(false)
			a.PermissionCtx.Mode = permission.ModeDontAsk
			a.SetPlanMode(true)
			if _, err := a.Rewind(2, rewindMode); err != nil {
				t.Fatal(err)
			}
			if !a.PlanMode() || a.PermissionCtx.PrePlanMode != permission.ModeBypass || !strings.Contains(a.Messages[0].Content.(string), "## Plan Mode\n") {
				t.Fatal("rewind restored only part of the saved planning state")
			}
			a.SetPlanMode(false)
			if a.PermissionCtx.Mode != permission.ModeBypass {
				t.Fatal("approval restored the newer turn's policy instead of the checkpoint's policy")
			}
			a.SetPlanMode(true)
			if _, err := a.Rewind(1, rewindMode); err != nil {
				t.Fatal(err)
			}
			if a.PlanMode() || a.PermissionCtx.Mode != permission.ModeBypass || a.PermissionCtx.PrePlanMode != "" || a.Messages[0].Content != "project rules" {
				t.Fatal("rewind before planning retained planning state or instructions")
			}
		})
	}
}

func TestFilesOnlyRewindDoesNotChangePlanMode(t *testing.T) {
	dir := t.TempDir()
	a := New(nil, "project rules", 5, 10000, nil)
	a.AddUserMessage("write first")
	writeTracked(t, a, dir, "file", "data")
	a.SetPlanMode(true)
	before := cloneMessages(a.Messages)
	if _, err := a.Rewind(1, "files"); err != nil {
		t.Fatal(err)
	}
	if !a.PlanMode() || a.PermissionCtx.PrePlanMode != permission.ModeDefault || !reflect.DeepEqual(before, a.Messages) {
		t.Fatal("files-only rewind changed planning state or conversation")
	}
	// Permissions after the rewind still enforce the current plan boundary.
	decision := a.executor.checkPermission(schema.ToolCall{Function: schema.FunctionCall{Name: "write_file"}}, &tools.WriteTool{WorkspaceDir: dir}, map[string]interface{}{"path": "file", "content": "blocked"})
	if decision.Behavior != permission.BehaviorDENY {
		t.Fatal("files-only rewind enabled project edits during planning")
	}
}
