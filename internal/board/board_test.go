package board

import (
	"path/filepath"
	"testing"
)

func TestMoveRespectsWIPLimit(t *testing.T) {
	b := New("t")
	var ts []*Ticket
	for _, n := range []string{"a", "b", "c", "d"} {
		ts = append(ts, b.Add(n))
	}
	for _, x := range ts[:3] {
		if err := b.Move(x, 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Move(ts[3], 1); err == nil {
		t.Fatal("4th ticket into In Progress should hit the WIP limit")
	}
	if err := b.Move(ts[0], 10); err != nil || ts[0].Status != StatusDone {
		t.Fatalf("clamped move: %v %s", err, ts[0].Status)
	}
	if err := b.Move(ts[3], -1); err != nil || ts[3].Status != StatusBacklog {
		t.Fatalf("move left of first column: %v %s", err, ts[3].Status)
	}
}

func TestCyclesAndDelete(t *testing.T) {
	b := New("t")
	x := b.Add("x")
	for _, want := range []AgentType{AgentAuto, "claude", "codex", "copilot", "antigravity", ""} {
		x.CycleAgent()
		if x.Agent != want {
			t.Fatalf("agent %q, want %q", x.Agent, want)
		}
	}
	x.CyclePriority()
	if x.Priority != 4 {
		t.Fatalf("priority %d", x.Priority)
	}
	x.CyclePriority()
	if x.Priority != 1 {
		t.Fatalf("priority wraps to 1, got %d", x.Priority)
	}
	b.Delete(x)
	if len(b.Tickets) != 0 {
		t.Fatal("not deleted")
	}
}

func TestFileStoreRoundTrip(t *testing.T) {
	s := FileStore{Path: filepath.Join(t.TempDir(), "sub", "board.json")}
	b, err := s.Load()
	if err != nil || len(b.Columns) != 4 || len(b.Tickets) != 0 {
		t.Fatalf("missing file should give a new board: %+v %v", b, err)
	}
	b.Add("persist me").Agent = "codex"
	if err := s.Save(b); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil || len(got.Tickets) != 1 || got.Tickets[0].Title != "persist me" || got.Tickets[0].Agent != "codex" {
		t.Fatalf("reloaded: %+v %v", got, err)
	}
}
