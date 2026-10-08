package board

import (
	"os"
	"path/filepath"
	"strings"
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
	if err != nil || len(b.Columns) != 5 || len(b.Tickets) != 0 {
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

func TestBlockedIsASideColumn(t *testing.T) {
	b := New("t")
	x := b.Add("x")
	for _, want := range []Status{StatusInProgress, StatusReview, StatusDone, StatusDone} {
		if err := b.Move(x, 1); err != nil || x.Status != want {
			t.Fatalf("moving right: %s, want %s (%v)", x.Status, want, err)
		}
	}
	if err := b.MoveTo(x, StatusBlocked); err != nil {
		t.Fatal(err)
	}
	for _, d := range []int{1, -1} {
		x.Status = StatusBlocked
		if err := b.Move(x, d); err != nil || x.Status != StatusInProgress {
			t.Fatalf("out of Blocked (%+d): %s %v", d, x.Status, err)
		}
	}
}

func TestOldBoardGetsBlockedColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.json")
	old := `{"name":"old","columns":[{"status":"backlog","name":"Backlog"},{"status":"in_progress","name":"In Progress","limit":3},{"status":"review","name":"Review"},{"status":"done","name":"Done"}],"tickets":[]}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := FileStore{Path: path}.Load()
	if err != nil || b.ColumnIndex(StatusBlocked) != 4 || !b.Columns[4].Side {
		t.Fatalf("columns %+v %v", b.Columns, err)
	}
}

func TestLegacySingleRunMigrates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.json")
	old := `{"name":"old","tickets":[{"id":"t1","title":"x","status":"review","priority":3,"agent":"auto","agent_status":"completed","agent_pod":"p1","agent_run":"claude","agent_output":"done!"},{"id":"t2","title":"y","status":"backlog","priority":3,"agent_status":"none"}]}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	s := FileStore{Path: path}
	b, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	r := b.Tickets[0].Current()
	if r == nil || r.Pod != "p1" || r.Agent != "claude" || r.Requested != AgentAuto || r.Status != AgentCompleted || r.Output != "done!" {
		t.Fatalf("migrated run: %+v", r)
	}
	if b.Tickets[1].Current() != nil {
		t.Fatal("ticket without a run got one")
	}
	if err := s.Save(b); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "agent_pod") || strings.Contains(string(data), "agent_status") {
		t.Fatalf("legacy fields written back: %s", data)
	}
}

func TestRunHelpers(t *testing.T) {
	b := New("t")
	x := b.Add("x")
	x.Agent = "claude"
	if x.RunStatus() != AgentNone || x.Current() != nil {
		t.Fatal("new ticket has a run")
	}
	r := x.StartRun()
	r.Pod = "p"
	if !r.Live() || len(x.LiveRuns()) != 1 || x.RunByPod("p") != r {
		t.Fatal("live run not found")
	}
	r.SetStatus(AgentCompleted)
	if r.FinishedAt == nil || !r.Status.Finished() || r.Status.Active() {
		t.Fatal("finish not recorded")
	}
}

func TestTicketKeys(t *testing.T) {
	b := New("t")
	a, c := b.Add("a"), b.Add("b")
	if a.Key != "KAI-1" || c.Key != "KAI-2" {
		t.Fatalf("keys %s %s", a.Key, c.Key)
	}
	b.Delete(c)
	if d := b.Add("c"); d.Key != "KAI-3" {
		t.Fatalf("number reused after delete: %s", d.Key)
	}
	if NormalizeProject(" xy ") != "XY" {
		t.Fatal("normalize")
	}
}

func TestOldTicketsGetKeysInCreationOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.json")
	old := `{"name":"old","tickets":[
	  {"id":"b","title":"second","status":"backlog","priority":3,"created_at":"2026-10-08T02:00:00Z"},
	  {"id":"a","title":"first","status":"backlog","priority":3,"created_at":"2026-10-08T01:00:00Z"}]}`
	os.WriteFile(path, []byte(old), 0o600)
	b, err := FileStore{Path: path}.Load()
	if err != nil {
		t.Fatal(err)
	}
	if b.Project != DefaultProject || b.Tickets[1].Key != "KAI-1" || b.Tickets[0].Key != "KAI-2" || b.NextNumber != 3 {
		t.Fatalf("project=%s keys=%s,%s next=%d", b.Project, b.Tickets[0].Key, b.Tickets[1].Key, b.NextNumber)
	}
	if n := b.Add("third"); n.Key != "KAI-3" {
		t.Fatalf("next key %s", n.Key)
	}
}
