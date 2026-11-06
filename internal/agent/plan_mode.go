package agent

import "diegoc-agent/internal/permission"

// PlanMode reports whether this session is planning. PermissionCtx.Mode is the
// authoritative state; there is no separate flag to get out of sync on forks.
func (a *Agent) PlanMode() bool {
	return a.PermissionCtx.Mode == permission.ModeExplore
}

// SetPlanMode switches the same ReAct agent between read-only planning and execution.
// The permission context is restored to its prior mode when planning is approved.
func (a *Agent) SetPlanMode(enabled bool) {
	if enabled == a.PlanMode() {
		return
	}
	if enabled {
		a.PermissionCtx.PrePlanMode = a.PermissionCtx.Mode
		a.PermissionCtx.Mode = permission.ModeExplore
	} else {
		mode := a.PermissionCtx.PrePlanMode
		if mode == "" || mode == permission.ModeExplore {
			mode = permission.ModeDefault
		}
		a.PermissionCtx.Mode = mode
		a.PermissionCtx.PrePlanMode = ""
	}
	a.updateModePrompt()
}

// updateModePrompt renders the current permission mode after a transition.
func (a *Agent) updateModePrompt() {
	if len(a.Messages) > 0 {
		base := a.SystemPrompt
		if a.PlanMode() {
			base += "\n\n## Plan Mode\nExplore and reason before editing. Do not modify project files or run commands that change state. Write the complete proposed implementation plan to `.agent_plan.md` using `write_plan` when ready. Then summarize the plan and tell the user to approve it with `/execute`. If the request changes materially, update the plan first."
		} else {
			base += "\n\n## Execute Mode\nFollow the approved plan in `.agent_plan.md`. Implement it, verify the changes, and adapt to new user instructions."
		}
		a.Messages[0].Content = base
	}
}

// planModeSnapshot also retains the mode to restore when a saved plan is
// approved. The checkpoint's message snapshot carries the matching prompt.
type planModeSnapshot struct {
	Mode, PrePlanMode permission.Mode
}

func (a *Agent) snapshotPlanMode() planModeSnapshot {
	return planModeSnapshot{a.PermissionCtx.Mode, a.PermissionCtx.PrePlanMode}
}

func (a *Agent) restorePlanMode(state planModeSnapshot) {
	a.PermissionCtx.Mode = state.Mode
	a.PermissionCtx.PrePlanMode = state.PrePlanMode
}
