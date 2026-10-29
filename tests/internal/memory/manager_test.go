package memory

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestManager_SearchToolLifecycle 验证 SetFileStore → SearchTool → Start → Close 完整生命周期。
func TestManager_SearchToolLifecycle(t *testing.T) {
	tmp := t.TempDir()
	cfg := DefaultConfig()
	cfg.WorkingDir = tmp

	mgr := NewManager(cfg, nil)
	if mgr == nil {
		t.Fatal("NewManager returned nil for non-empty WorkingDir")
	}

	// 未设置 FileStore 时 SearchTool 应为 nil。
	if tool := mgr.SearchTool(); tool != nil {
		t.Error("SearchTool should be nil without FileStore")
	}

	// 设置 FileStore 后 SearchTool 应可用。
	store := newTestFileStore()
	mgr.SetFileStore(store)

	tool := mgr.SearchTool()
	if tool == nil {
		t.Fatal("SearchTool should not be nil after SetFileStore")
	}
	if tool.Name() != "memory_search" {
		t.Errorf("tool name = %q, want memory_search", tool.Name())
	}

	// Start 启动 FileWatcher（后台轮询）。
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// 写入记忆文件 → 手动索引 → SearchMemory 委托正常。
	memDir := filepath.Join(tmp, "memory")
	if err := os.MkdirAll(memDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := `---
conversation_date: 2026-08-13
---

## Preferences
- User prefers concise commit messages
`
	if err := os.WriteFile(filepath.Join(memDir, "2026-08-13.md"), []byte(content), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := store.IndexFileWithDate(context.Background(), "2026-08-13.md", "## Preferences\n- User prefers concise commit messages\n", "2026-08-13"); err != nil {
		t.Fatalf("index: %v", err)
	}

	results, err := mgr.SearchMemory(context.Background(), "commit messages", 3)
	if err != nil {
		t.Fatalf("SearchMemory failed: %v", err)
	}
	if len(results) == 0 {
		t.Error("SearchMemory returned no results for indexed content")
	}

	// Close 应停止 watcher 并关闭 FileStore，且可安全重复调用。
	if err := mgr.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	mgr.Close()
	time.Sleep(10 * time.Millisecond) // 让 goroutine 退出
}

// TestManager_NoFileStoreGracefulDegradation 验证无 FileStore 时管线核心功能正常。
func TestManager_NoFileStoreGracefulDegradation(t *testing.T) {
	tmp := t.TempDir()
	cfg := DefaultConfig()
	cfg.WorkingDir = tmp

	mgr := NewManager(cfg, nil)
	if mgr == nil {
		t.Fatal("NewManager returned nil")
	}

	// Start 无 FileStore 应为 no-op，不报错。
	if err := mgr.Start(context.Background()); err != nil {
		t.Errorf("Start without FileStore should be no-op: %v", err)
	}

	// SearchMemory 无 FileStore 返回 nil, nil。
	results, err := mgr.SearchMemory(context.Background(), "anything", 5)
	if err != nil {
		t.Errorf("SearchMemory should not error without FileStore: %v", err)
	}
	if results != nil {
		t.Errorf("SearchMemory should return nil without FileStore, got %v", results)
	}

	mgr.Close()
}
