package tools

import (
	"context"
	"os"
	"path/filepath"

	"diegoc-agent/internal/permission"
)

// WritePlanTool stores the agent's proposed plan in a dedicated workspace file.
// It is the only write operation permitted while the agent is in explore mode.
type WritePlanTool struct{ WorkspaceDir string }

func (t *WritePlanTool) Name() string { return "write_plan" }
func (t *WritePlanTool) Description() string {
	return "Write or replace the proposed implementation plan in .agent_plan.md in the workspace. Use this only in Plan Mode; it does not modify project source files."
}
func (t *WritePlanTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"content": map[string]interface{}{"type": "string", "description": "Complete implementation plan, including files/areas, steps, and verification"},
		},
		"required": []interface{}{"content"},
	}
}
func (t *WritePlanTool) Execute(_ context.Context, args map[string]interface{}) (*ToolResult, error) {
	content, _ := args["content"].(string)
	if content == "" {
		return &ToolResult{Success: false, Error: "content is required"}, nil
	}
	path := filepath.Join(t.WorkspaceDir, ".agent_plan.md")
	if err := os.WriteFile(path, []byte(content+"\n"), 0644); err != nil {
		return &ToolResult{Success: false, Error: err.Error()}, nil
	}
	return &ToolResult{Success: true, Content: "Plan saved to .agent_plan.md. Present it to the user and wait for /execute approval before implementation."}, nil
}
func (t *WritePlanTool) CheckPermissions(_ map[string]interface{}, pCtx *permission.Context) permission.Decision {
	if pCtx.Mode == permission.ModeExplore {
		return permission.Decision{Behavior: permission.BehaviorALLOW}
	}
	return permission.Decision{Behavior: permission.BehaviorASK, Message: "write_plan is intended for Plan Mode"}
}
func (t *WritePlanTool) IsConcurrencySafe() bool { return false }
func (t *WritePlanTool) IsReadOnly() bool        { return false }
func (t *WritePlanTool) IsExternalTool() bool    { return false }
