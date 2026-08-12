package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newTestFileStore creates a FileStore with mock backends for testing.
func newTestFileStore() *FileStore {
	return NewFileStore(
		&mockEmbedder{dim: 8},
		newMockVectorStore(),
		newMockKeywordStore(),
		&RuleTokenCounter{Divisor: 3.75},
		"test_collection",
		100, // chunkTokens
		20,  // overlap
	)
}

// writeMemoryFile creates a .md file in dir and returns the full path.
func writeMemoryFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	fullPath := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return fullPath
}

func TestFileWatcher_InitialScan(t *testing.T) {
	dir := t.TempDir()
	store := newTestFileStore()

	// Pre-create some .md files.
	writeMemoryFile(t, dir, "2026-08-01.md", `---
conversation_date: 2026-08-01
---

## Project Goals
- **2026-08-01**: Build the DiegoC Agent framework
`)
	writeMemoryFile(t, dir, "2026-08-02.md", `---
conversation_date: 2026-08-02
---

## Key Decisions
- **2026-08-02**: Use ChromaDB for vector storage
`)

	fw := NewFileWatcher(dir, store)
	fw.pollDelay = 100 * time.Millisecond // fast for tests

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer fw.Stop()

	if err := fw.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Verify both files were indexed.
	for _, name := range []string{"2026-08-01.md", "2026-08-02.md"} {
		meta := store.GetFileMeta(name)
		if meta == nil {
			t.Errorf("file %s not indexed after initial scan", name)
			continue
		}
		if meta.Hash == "" {
			t.Errorf("file %s indexed but hash is empty", name)
		}
		t.Logf("%s: hash=%s chunks=%d date=%q", name, meta.Hash, len(meta.Chunks), meta.ConversationDate)
	}
}

func TestFileWatcher_AddFile(t *testing.T) {
	dir := t.TempDir()
	store := newTestFileStore()

	fw := NewFileWatcher(dir, store)
	fw.pollDelay = 100 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer fw.Stop()

	if err := fw.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Initially empty — verify nothing indexed.
	if meta := store.GetFileMeta("new.md"); meta != nil {
		t.Error("unexpected file indexed before creation")
	}

	// Add a new file and wait for the watcher to pick it up.
	writeMemoryFile(t, dir, "new.md", `---
conversation_date: 2026-08-12
---

## New Topic
- **2026-08-12**: A new memory entry
`)

	// Wait for poll to detect.
	time.Sleep(300 * time.Millisecond)

	meta := store.GetFileMeta("new.md")
	if meta == nil {
		t.Fatal("new.md was not indexed after creation")
	}
	t.Logf("new.md indexed: hash=%s chunks=%d", meta.Hash, len(meta.Chunks))
}

func TestFileWatcher_ModifyFile(t *testing.T) {
	dir := t.TempDir()
	store := newTestFileStore()

	// Create file first.
	writeMemoryFile(t, dir, "memory.md", `---
conversation_date: 2026-08-10
---

## Original
- **2026-08-10**: Original content
`)

	fw := NewFileWatcher(dir, store)
	fw.pollDelay = 100 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer fw.Stop()

	if err := fw.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	originalMeta := store.GetFileMeta("memory.md")
	if originalMeta == nil {
		t.Fatal("file not indexed after initial scan")
	}
	originalHash := originalMeta.Hash

	// Modify the file.
	time.Sleep(200 * time.Millisecond) // let initial scan settle
	writeMemoryFile(t, dir, "memory.md", `---
conversation_date: 2026-08-10
---

## Updated
- **2026-08-10**: Updated content with new information
`)

	// Wait for poll + debounce.
	time.Sleep(600 * time.Millisecond)

	updatedMeta := store.GetFileMeta("memory.md")
	if updatedMeta == nil {
		t.Fatal("memory.md not found after modification")
	}
	if updatedMeta.Hash == originalHash {
		t.Error("hash should have changed after modification")
	}
	t.Logf("original hash=%s, updated hash=%s", originalHash, updatedMeta.Hash)
}

func TestFileWatcher_DeleteFile(t *testing.T) {
	dir := t.TempDir()
	store := newTestFileStore()

	writeMemoryFile(t, dir, "to_delete.md", `---
conversation_date: 2026-08-05
---

## To Delete
- **2026-08-05**: This will be removed
`)

	fw := NewFileWatcher(dir, store)
	fw.pollDelay = 100 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer fw.Stop()

	if err := fw.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if meta := store.GetFileMeta("to_delete.md"); meta == nil {
		t.Fatal("file not indexed after initial scan")
	}

	// Delete the file.
	time.Sleep(200 * time.Millisecond)
	if err := os.Remove(filepath.Join(dir, "to_delete.md")); err != nil {
		t.Fatalf("remove: %v", err)
	}

	// Wait for poll to detect deletion.
	time.Sleep(300 * time.Millisecond)

	if meta := store.GetFileMeta("to_delete.md"); meta != nil {
		t.Error("file meta should be nil after deletion")
	}
}

func TestFileWatcher_Debounce(t *testing.T) {
	dir := t.TempDir()
	store := newTestFileStore()

	writeMemoryFile(t, dir, "rapid.md", `---
conversation_date: 2026-08-12
---

## Rapid Changes
- **2026-08-12**: Version 1
`)

	fw := NewFileWatcher(dir, store)
	fw.pollDelay = 50 * time.Millisecond
	fw.debounce = 500 * time.Millisecond // debounce window longer than poll

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer fw.Stop()

	if err := fw.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Rapidly modify the file multiple times.
	time.Sleep(100 * time.Millisecond) // let initial scan settle
	for i := 0; i < 5; i++ {
		content := fmt.Sprintf(`---
conversation_date: 2026-08-12
---

## Version %d
- **2026-08-12**: Rapid change %d
`, i, i)
		_ = os.WriteFile(filepath.Join(dir, "rapid.md"), []byte(content), 0644)
		time.Sleep(80 * time.Millisecond) // faster than debounce
	}

	// Wait for debounce window to pass + one more poll cycle.
	time.Sleep(700 * time.Millisecond)

	meta := store.GetFileMeta("rapid.md")
	if meta == nil {
		t.Fatal("rapid.md not indexed")
	}

	// The content should reflect the LAST version (debounce prevented intermediate reindexes).
	// Verify by searching for the final content.
	results, err := store.HybridSearch(ctx, "Version 4", 3)
	if err != nil {
		t.Fatalf("HybridSearch failed: %v", err)
	}
	if len(results) == 0 {
		t.Error("search for 'Version 4' returned no results — final version may not have been indexed")
	}
	t.Logf("found %d results for 'Version 4'", len(results))
}

func TestFileWatcher_Stop(t *testing.T) {
	dir := t.TempDir()
	store := newTestFileStore()

	fw := NewFileWatcher(dir, store)
	fw.pollDelay = 100 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := fw.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Stop the watcher.
	fw.Stop()

	// Add a file after stopping — it should NOT be indexed.
	writeMemoryFile(t, dir, "after_stop.md", `---
conversation_date: 2026-08-12
---

## After Stop
- **2026-08-12**: Should not be picked up
`)

	time.Sleep(300 * time.Millisecond)

	if meta := store.GetFileMeta("after_stop.md"); meta != nil {
		t.Error("file should NOT be indexed after Stop()")
	}
}

func TestFileWatcher_NonMdIgnored(t *testing.T) {
	dir := t.TempDir()
	store := newTestFileStore()

	fw := NewFileWatcher(dir, store)
	fw.pollDelay = 100 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer fw.Stop()

	if err := fw.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Write a non-.md file.
	writeMemoryFile(t, dir, "notes.txt", "Just some text notes")

	time.Sleep(300 * time.Millisecond)

	if meta := store.GetFileMeta("notes.txt"); meta != nil {
		t.Error("non-.md file should NOT be indexed")
	}
}

func TestFileWatcher_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	store := newTestFileStore()

	fw := NewFileWatcher(dir, store)
	fw.pollDelay = 100 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer fw.Stop()

	// Start on empty directory should succeed.
	if err := fw.Start(ctx); err != nil {
		t.Fatalf("Start on empty dir failed: %v", err)
	}
}

func TestFileWatcher_StartCreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nonexistent")
	store := newTestFileStore()

	fw := NewFileWatcher(dir, store)
	fw.pollDelay = 100 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer fw.Stop()

	if err := fw.Start(ctx); err != nil {
		t.Fatalf("Start should create dir: %v", err)
	}

	// Directory should now exist.
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Error("watchDir was not created by Start()")
	}
}

func TestFileWatcher_FrontmatterDate(t *testing.T) {
	dir := t.TempDir()
	store := newTestFileStore()

	writeMemoryFile(t, dir, "dated.md", `---
conversation_date: 2026-08-12
---

## Dated Topic
- **2026-08-12**: Something with a date
`)

	fw := NewFileWatcher(dir, store)
	fw.pollDelay = 100 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer fw.Stop()

	if err := fw.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	meta := store.GetFileMeta("dated.md")
	if meta == nil {
		t.Fatal("file not indexed")
	}
	if meta.ConversationDate != "2026-08-12" {
		t.Errorf("ConversationDate = %q, want %q", meta.ConversationDate, "2026-08-12")
	}
}

func TestFileWatcher_NoFrontmatter(t *testing.T) {
	dir := t.TempDir()
	store := newTestFileStore()

	// File without frontmatter.
	writeMemoryFile(t, dir, "plain.md", `# Just a heading

Some content without any YAML frontmatter.
`)

	fw := NewFileWatcher(dir, store)
	fw.pollDelay = 100 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	defer fw.Stop()

	if err := fw.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	meta := store.GetFileMeta("plain.md")
	if meta == nil {
		t.Fatal("plain.md should be indexed even without frontmatter")
	}
	if meta.ConversationDate != "" {
		t.Errorf("ConversationDate should be empty for no-frontmatter file, got %q", meta.ConversationDate)
	}
}
