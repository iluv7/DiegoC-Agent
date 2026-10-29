package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"diegoc-agent/internal/schema"
)

func newCounter() TokenCounter {
	return &RuleTokenCounter{Divisor: 3.75}
}

func TestInMemoryMemory_AddMessage(t *testing.T) {
	imm := NewInMemoryMemory(t.TempDir(), newCounter())

	imm.AddMessage(schema.Message{Role: "user", Content: "hello"})
	if imm.ContentLen() != 1 {
		t.Fatalf("expected 1 message, got %d", imm.ContentLen())
	}
	if imm.Content()[0].Mark != MarkNone {
		t.Error("new message should be MarkNone")
	}
}

func TestInMemoryMemory_AddMessages(t *testing.T) {
	imm := NewInMemoryMemory(t.TempDir(), newCounter())

	msgs := []schema.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
		{Role: "user", Content: "bye"},
	}
	imm.AddMessages(msgs)
	if imm.ContentLen() != 3 {
		t.Fatalf("expected 3 messages, got %d", imm.ContentLen())
	}
}

func TestInMemoryMemory_GetMemory_NoMemoryBlocks(t *testing.T) {
	imm := NewInMemoryMemory(t.TempDir(), newCounter())

	imm.AddMessage(schema.Message{Role: "user", Content: "hello"})
	imm.AddMessage(schema.Message{Role: "assistant", Content: "hi there"})

	result := imm.GetMemory()
	// Without longTermMemory or compressedSummary, no extra system message
	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result))
	}
	if result[0].Role != "user" || result[1].Role != "assistant" {
		t.Error("messages should be in insertion order")
	}
}

func TestInMemoryMemory_GetMemory_WithMemoryBlocks(t *testing.T) {
	imm := NewInMemoryMemory(t.TempDir(), newCounter())

	imm.SetLongTermMemory("- Project uses Go\n- DB is SQLite")
	imm.SetCompressedSummary("## Goal\nBuild agent\n\n## Progress\n### Done\n- Setup")

	imm.AddMessage(schema.Message{Role: "user", Content: "let's continue"})

	result := imm.GetMemory()
	// 1 system block + 1 user message = 2
	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result))
	}

	// First message should be system with memory blocks
	sysMsg := result[0]
	if sysMsg.Role != "system" {
		t.Error("first message should be system role")
	}
	content, ok := sysMsg.Content.(string)
	if !ok {
		t.Fatal("system message content should be string")
	}
	if !strings.Contains(content, "# Memories") {
		t.Error("should contain # Memories block")
	}
	if !strings.Contains(content, "Project uses Go") {
		t.Error("should contain long-term memory content")
	}
	if !strings.Contains(content, "# Summary of previous conversation") {
		t.Error("should contain # Summary block")
	}
	if !strings.Contains(content, "## Goal") {
		t.Error("should contain compressed summary content")
	}

	// Second message should be the user message
	if result[1].Role != "user" {
		t.Error("second message should be user")
	}
}

func TestInMemoryMemory_GetMemory_SummaryOnly(t *testing.T) {
	imm := NewInMemoryMemory(t.TempDir(), newCounter())
	imm.SetCompressedSummary("## Progress\nIn progress")

	result := imm.GetMemory()
	if len(result) != 1 {
		t.Fatalf("expected 1 block message, got %d", len(result))
	}
	content := result[0].Content.(string)
	if strings.Contains(content, "# Memories") {
		t.Error("should not contain # Memories when longTermMemory is empty")
	}
	if !strings.Contains(content, "# Summary") {
		t.Error("should contain # Summary block")
	}
}

func TestInMemoryMemory_GetMemory_FiltersCompressed(t *testing.T) {
	imm := NewInMemoryMemory(t.TempDir(), newCounter())

	imm.AddMessage(schema.Message{Role: "user", Content: "msg1"})
	imm.AddMessage(schema.Message{Role: "assistant", Content: "reply1"})
	imm.AddMessage(schema.Message{Role: "user", Content: "msg2"})

	// Mark first two as compressed
	err := imm.MarkMessagesCompressed([]schema.Message{
		{Role: "user", Content: "msg1"},
		{Role: "assistant", Content: "reply1"},
	})
	if err != nil {
		t.Fatalf("MarkMessagesCompressed: %v", err)
	}

	result := imm.GetMemory()
	if len(result) != 1 {
		t.Fatalf("expected 1 remaining message, got %d", len(result))
	}
	if result[0].Content.(string) != "msg2" {
		t.Errorf("expected 'msg2', got %v", result[0].Content)
	}
}

func TestInMemoryMemory_MarkMessagesCompressed_Persists(t *testing.T) {
	dialogDir := filepath.Join(t.TempDir(), "dialog")
	imm := NewInMemoryMemory(dialogDir, newCounter())

	imm.AddMessage(schema.Message{Role: "user", Content: "hello"})
	imm.AddMessage(schema.Message{Role: "assistant", Content: "hi", ToolCallID: "tc1"})

	err := imm.MarkMessagesCompressed([]schema.Message{
		{Role: "user", Content: "hello"},
	})
	if err != nil {
		t.Fatalf("MarkMessagesCompressed: %v", err)
	}

	// Verify content: should only have the assistant message left
	if imm.ContentLen() != 1 {
		t.Fatalf("expected 1 message remaining, got %d", imm.ContentLen())
	}
	if imm.Content()[0].Mark != MarkNone {
		t.Error("unmarked message should stay MarkNone")
	}

	// Verify dialog file was written
	files, err := os.ReadDir(dialogDir)
	if err != nil {
		t.Fatalf("read dialog dir: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("expected dialog file to be created")
	}

	data, err := os.ReadFile(filepath.Join(dialogDir, files[0].Name()))
	if err != nil {
		t.Fatalf("read dialog file: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "hello") {
		t.Error("dialog file should contain the marked message")
	}
	if !strings.Contains(content, "compressed") {
		t.Error("dialog entry should have mark=compressed")
	}
}

func TestInMemoryMemory_ClearContent(t *testing.T) {
	dialogDir := filepath.Join(t.TempDir(), "dialog")
	imm := NewInMemoryMemory(dialogDir, newCounter())

	imm.AddMessage(schema.Message{Role: "user", Content: "msg1"})
	imm.AddMessage(schema.Message{Role: "assistant", Content: "msg2"})

	err := imm.ClearContent()
	if err != nil {
		t.Fatalf("ClearContent: %v", err)
	}

	if imm.ContentLen() != 0 {
		t.Errorf("content should be empty after ClearContent, got %d", imm.ContentLen())
	}

	// Dialog should have both messages
	files, _ := os.ReadDir(dialogDir)
	if len(files) == 0 {
		t.Fatal("expected dialog file")
	}
	data, _ := os.ReadFile(filepath.Join(dialogDir, files[0].Name()))
	content := string(data)
	if !strings.Contains(content, "msg1") || !strings.Contains(content, "msg2") {
		t.Error("dialog should contain both messages")
	}
}

func TestInMemoryMemory_ClearContent_Empty(t *testing.T) {
	imm := NewInMemoryMemory(t.TempDir(), newCounter())
	err := imm.ClearContent()
	if err != nil {
		t.Errorf("ClearContent on empty should not error: %v", err)
	}
}

func TestInMemoryMemory_AppendToDialog(t *testing.T) {
	dialogDir := filepath.Join(t.TempDir(), "dialog")
	imm := NewInMemoryMemory(dialogDir, newCounter())

	msgs := []schema.Message{
		{Role: "user", Content: "note 1"},
		{Role: "assistant", Content: "note 2"},
	}

	err := imm.AppendToDialog(msgs)
	if err != nil {
		t.Fatalf("AppendToDialog: %v", err)
	}

	// Content should be unchanged (AppendToDialog doesn't modify content)
	if imm.ContentLen() != 0 {
		t.Error("AppendToDialog should not modify content")
	}

	// But dialog file should exist with the messages
	files, _ := os.ReadDir(dialogDir)
	if len(files) == 0 {
		t.Fatal("expected dialog file")
	}
	data, _ := os.ReadFile(filepath.Join(dialogDir, files[0].Name()))
	content := string(data)
	if !strings.Contains(content, "note 1") {
		t.Error("dialog should contain note 1")
	}
}

func TestInMemoryMemory_AppendToDialog_Empty(t *testing.T) {
	imm := NewInMemoryMemory(t.TempDir(), newCounter())
	err := imm.AppendToDialog(nil)
	if err != nil {
		t.Error("nil should not error")
	}
	err = imm.AppendToDialog([]schema.Message{})
	if err != nil {
		t.Error("empty should not error")
	}
}

func TestInMemoryMemory_EstimateTokens(t *testing.T) {
	imm := NewInMemoryMemory(t.TempDir(), newCounter())

	imm.AddMessage(schema.Message{Role: "user", Content: "hello world"})
	imm.AddMessage(schema.Message{Role: "assistant", Content: "hi back"})

	used, free := imm.EstimateTokens(100000)
	if used <= 0 {
		t.Errorf("used should be > 0, got %d", used)
	}
	if free >= 100000 {
		t.Errorf("free should be < maxInputLength, got %d", free)
	}
	if used+free != 100000 {
		t.Errorf("used + free should equal maxInputLength: %d + %d != 100000", used, free)
	}
}

func TestInMemoryMemory_EstimateTokens_OverLimit(t *testing.T) {
	imm := NewInMemoryMemory(t.TempDir(), newCounter())
	imm.AddMessage(schema.Message{Role: "user", Content: "this is a longer message that uses more tokens"})

	// Very small limit — message will exceed it
	used, free := imm.EstimateTokens(1)
	if free != 0 {
		t.Errorf("free should be clamped to 0 when over limit, got %d", free)
	}
	if used <= 1 {
		t.Errorf("used should be > 1 when over limit, got %d", used)
	}
}

func TestInMemoryMemory_SetAndGet(t *testing.T) {
	imm := NewInMemoryMemory(t.TempDir(), newCounter())

	imm.SetCompressedSummary("## Goal\nTest")
	imm.SetLongTermMemory("- memory item")

	if imm.CompressedSummary() != "## Goal\nTest" {
		t.Error("CompressedSummary mismatch")
	}
	if imm.LongTermMemory() != "- memory item" {
		t.Error("LongTermMemory mismatch")
	}
}

func TestInMemoryMemory_LoadDialog(t *testing.T) {
	dialogDir := filepath.Join(t.TempDir(), "dialog")
	imm := NewInMemoryMemory(dialogDir, newCounter())

	// Write some messages to dialog first
	imm.AddMessage(schema.Message{Role: "user", Content: "alpha"})
	imm.AddMessage(schema.Message{Role: "assistant", Content: "beta"})
	if err := imm.ClearContent(); err != nil {
		t.Fatal(err)
	}

	// Now load back
	imm2 := NewInMemoryMemory(dialogDir, newCounter())
	files, _ := os.ReadDir(dialogDir)
	if len(files) == 0 {
		t.Fatal("no dialog files")
	}
	// Extract date from filename, e.g. "2026-07-02.jsonl" → "2026-07-02"
	fname := files[0].Name()
	date := fname[:len(fname)-len(".jsonl")]

	msgs, err := imm2.LoadDialog(date)
	if err != nil {
		t.Fatalf("LoadDialog: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 loaded messages, got %d", len(msgs))
	}
	if msgs[0].Content.(string) != "alpha" {
		t.Errorf("first message mismatch: %v", msgs[0].Content)
	}
}

func TestInMemoryMemory_LoadDialog_Missing(t *testing.T) {
	imm := NewInMemoryMemory(t.TempDir(), newCounter())
	msgs, err := imm.LoadDialog("2099-01-01")
	if err != nil {
		t.Errorf("missing file should not error: %v", err)
	}
	if msgs != nil {
		t.Errorf("missing file should return nil, got %d messages", len(msgs))
	}
}

func TestInMemoryMemory_MarkMessagesCompressed_NoMatch(t *testing.T) {
	imm := NewInMemoryMemory(t.TempDir(), newCounter())
	imm.AddMessage(schema.Message{Role: "user", Content: "existing"})

	// Try to mark a message that doesn't exist
	err := imm.MarkMessagesCompressed([]schema.Message{
		{Role: "user", Content: "nonexistent"},
	})
	if err != nil {
		t.Fatalf("no-match should not error: %v", err)
	}
	if imm.ContentLen() != 1 {
		t.Error("unmatched message should not be removed")
	}
}

func TestInMemoryMemory_MarkMessagesCompressed_Empty(t *testing.T) {
	imm := NewInMemoryMemory(t.TempDir(), newCounter())
	imm.AddMessage(schema.Message{Role: "user", Content: "test"})

	err := imm.MarkMessagesCompressed(nil)
	if err != nil {
		t.Error("nil should not error")
	}
	if imm.ContentLen() != 1 {
		t.Error("content should be unchanged")
	}

	err = imm.MarkMessagesCompressed([]schema.Message{})
	if err != nil {
		t.Error("empty should not error")
	}
}

func TestMsgFingerprint(t *testing.T) {
	a := schema.Message{Role: "user", Content: "hello", ToolCallID: "tc1"}
	b := schema.Message{Role: "user", Content: "hello", ToolCallID: "tc1"}
	c := schema.Message{Role: "assistant", Content: "hello", ToolCallID: "tc1"}

	if msgFingerprint(a) != msgFingerprint(b) {
		t.Error("identical messages should have same fingerprint")
	}
	if msgFingerprint(a) == msgFingerprint(c) {
		t.Error("different role should give different fingerprint")
	}
}

func TestMemoryMark_String(t *testing.T) {
	if MarkNone.String() != "none" {
		t.Errorf("MarkNone = %q", MarkNone.String())
	}
	if MarkCompressed.String() != "compressed" {
		t.Errorf("MarkCompressed = %q", MarkCompressed.String())
	}
}
