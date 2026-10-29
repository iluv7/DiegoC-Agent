package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"diegoc-agent/internal/loop"
)

func TestLoopToolsUseRunContextAndIsolateSessions(t *testing.T) {
	scheduler := loop.New(nil)
	mainCtx := WithLoopSession(context.Background(), scheduler, "main")
	otherCtx := WithLoopSession(context.Background(), scheduler, "session-1")
	create := &LoopTool{"create"}
	list := &LoopTool{"list"}
	cancel := &LoopTool{"cancel"}
	result, err := create.Execute(mainCtx, map[string]interface{}{"prompt": "watch CI", "interval": "61s"})
	if err != nil || !result.Success {
		t.Fatalf("create: %+v %v", result, err)
	}
	var task loop.Task
	if err := json.Unmarshal([]byte(result.Content), &task); err != nil || task.Interval != 2*time.Minute {
		t.Fatalf("unexpected created task %+v %v", task, err)
	}
	result, _ = list.Execute(otherCtx, nil)
	if result.Content != "[]" {
		t.Fatal("other session can see the loop")
	}
	result, _ = cancel.Execute(otherCtx, map[string]interface{}{"id": task.ID})
	if result.Success || len(scheduler.List("main")) != 1 {
		t.Fatal("other session can cancel the loop")
	}
	for _, tool := range LoopTools() {
		result, err := tool.Execute(context.Background(), map[string]interface{}{"prompt": "task", "id": task.ID})
		if err != nil || result.Success {
			t.Fatalf("tool available without foreground context: %s", tool.Name())
		}
	}
	result, _ = cancel.Execute(mainCtx, map[string]interface{}{"id": task.ID})
	if !result.Success || len(scheduler.List("main")) != 0 {
		t.Fatal("owner cancellation failed")
	}
	for _, args := range []map[string]interface{}{{"prompt": "task", "interval": 3}, {"prompt": "task", "interval": "0m"}, {"prompt": " "}} {
		result, _ = create.Execute(mainCtx, args)
		if result.Success {
			t.Fatalf("accepted invalid arguments: %v", args)
		}
	}
	ctx, stop := context.WithCancel(mainCtx)
	stop()
	if _, err := create.Execute(ctx, map[string]interface{}{"prompt": "task"}); err != context.Canceled {
		t.Fatalf("cancelled context: %v", err)
	}
}
