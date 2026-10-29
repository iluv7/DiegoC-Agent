package agent

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"diegoc-agent/internal/schema"
	"diegoc-agent/internal/tools"
)

func TestFileChangesUseRecordedTurnSnapshotsAndOwnedCopies(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "existing.txt")
	if err := os.WriteFile(path, []byte("original\n"), 0640); err != nil {
		t.Fatal(err)
	}
	a := New(nil, "system", 5, 100000, nil)
	a.AddUserMessage("first change")
	writeTracked(t, a, dir, "existing.txt", "intermediate\n")
	writeTracked(t, a, dir, "existing.txt", "first final\n")
	writeTracked(t, a, dir, "added.txt", "new file\n")
	a.AddUserMessage("second change")
	writeTracked(t, a, dir, "existing.txt", "second final\n")
	if err := os.WriteFile(path, []byte("external modification\n"), 0600); err != nil {
		t.Fatal(err)
	}
	turn, err := a.FileChanges(1)
	if err != nil || turn.ID != 1 || turn.Prompt != "first change" || len(turn.Files) != 2 {
		t.Fatalf("turn diff: %+v %v", turn, err)
	}
	if filepath.Base(turn.Files[0].Path) != "added.txt" || turn.Files[0].Before.Exists || !turn.Files[0].After.Exists {
		t.Fatal("new file state or stable file ordering lost")
	}
	existing := turn.Files[1]
	if string(existing.Before.Data) != "original\n" || string(existing.After.Data) != "first final\n" {
		t.Fatal("diff included an intermediate write, later turn, or external edit")
	}
	existing.Before.Data[0] = '?'
	existing.After.Data[0] = '?'
	again, _ := a.FileChanges(1)
	if string(again.Files[1].Before.Data) != "original\n" || string(again.Files[1].After.Data) != "first final\n" {
		t.Fatal("caller changed rewind backups through returned bytes")
	}
	latest, _ := a.FileChanges(0)
	if latest.ID != 2 || len(latest.Files) != 1 || string(latest.Files[0].Before.Data) != "first final\n" || string(latest.Files[0].After.Data) != "second final\n" {
		t.Fatal("latest turn did not use its own before/after versions")
	}
	requireFile(t, path, "external modification\n")
	if len(a.Checkpoints()) != 2 {
		t.Fatal("viewing diffs changed checkpoints")
	}
}

func TestFileChangesRespectEmptyTurnsSessionsAndRewind(t *testing.T) {
	dir := t.TempDir()
	a := New(nil, "system", 5, 100000, nil)
	if got, err := a.FileChanges(0); err != nil || got.ID != 0 {
		t.Fatal("empty session diff should be empty")
	}
	a.AddUserMessage("new file")
	writeTracked(t, a, dir, "new.txt", "content")
	sessions := NewConversationSessions(a)
	child := mustForkSession(t, sessions).Agent
	if got, _ := child.FileChanges(0); got.ID != 0 || len(got.Files) != 0 {
		t.Fatal("foreground fork inherited another session's file checkpoints")
	}
	a.AddUserMessage("no file edits")
	if got, _ := a.FileChanges(0); got.ID != 2 || len(got.Files) != 0 {
		t.Fatal("empty latest turn incorrectly fell back to an older change")
	}
	if _, err := a.FileChanges(999); err == nil {
		t.Fatal("missing turn accepted")
	}
	if _, err := a.FileChanges(-1); err == nil {
		t.Fatal("negative turn accepted")
	}
	if _, err := a.Rewind(1, "files"); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.FileChanges(1); len(got.Files) != 0 {
		t.Fatal("files-only rewind left consumed changes visible")
	}
	a.ClearConversation()
	if got, _ := a.FileChanges(0); got.ID != 0 {
		t.Fatal("clear retained turn diffs")
	}
}

func TestFileChangesDistinguishNoOpFromMissingFinalSnapshot(t *testing.T) {
	dir := t.TempDir()
	a := New(nil, "system", 5, 100000, nil)
	a.AddUserMessage("write twice")
	writeTracked(t, a, dir, "file.txt", "original")
	a.AddUserMessage("write same bytes")
	writeTracked(t, a, dir, "file.txt", "original")
	if got, _ := a.FileChanges(0); len(got.Files) != 0 {
		t.Fatal("same-content write reported as a change")
	}
	for path := range a.checkpoints[1].After {
		delete(a.checkpoints[1].After, path)
	}
	if got, _ := a.FileChanges(0); len(got.Unavailable) != 1 || len(got.Files) != 0 {
		t.Fatal("missing final version was presented as a deletion or silently hidden")
	}
}

func TestFileChangesSurviveConversationCompaction(t *testing.T) {
	client := forkClientFunc(func(context.Context, []schema.Message, []tools.Tool) (*schema.LLMResponse, error) {
		return &schema.LLMResponse{Content: compactTestSummary}, nil
	})
	a := New(client, "system", 5, 100000, nil)
	a.AddUserMessage(strings.Repeat("user requirements ", 300))
	writeTracked(t, a, t.TempDir(), "file.txt", "new contents\n")
	a.Messages = append(a.Messages, schema.Message{Role: "assistant", Content: strings.Repeat("completed work ", 300)})
	before, _ := a.FileChanges(0)
	if _, err := a.Compact(context.Background(), "keep completed work"); err != nil {
		t.Fatal(err)
	}
	after, err := a.FileChanges(0)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("compaction changed recorded file diffs")
	}
}
