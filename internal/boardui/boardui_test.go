package boardui

import (
	"context"
	"os"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/zerosuxx/kainban/internal/board"
)

type memStore struct {
	b     *board.Board
	saves int
}

func (s *memStore) Load() (*board.Board, error) { return s.b, nil }
func (s *memStore) Save(b *board.Board) error   { s.b = b; s.saves++; return nil }

func key(s string) tea.KeyPressMsg {
	switch s {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "space":
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	}
	r := []rune(s)[0]
	return tea.KeyPressMsg{Code: r, Text: s}
}

func typeText(m *model, s string) {
	for _, r := range s {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

func TestCreateMoveDelete(t *testing.T) {
	st := &memStore{b: board.New("t")}
	m := newModel(st.b, st, Options{})
	m.Update(key("n"))
	typeText(m, "Fix login")
	m.Update(key("enter"))
	if len(st.b.Tickets) != 1 || st.b.Tickets[0].Title != "Fix login" || st.saves != 1 {
		t.Fatalf("create: %+v saves=%d", st.b.Tickets, st.saves)
	}
	m.Update(key("space"))
	if st.b.Tickets[0].Status != board.StatusInProgress || m.col != 1 {
		t.Fatalf("move: status=%s col=%d", st.b.Tickets[0].Status, m.col)
	}
	m.Update(key("a"))
	if st.b.Tickets[0].Agent != "claude" {
		t.Fatalf("agent: %q", st.b.Tickets[0].Agent)
	}
	m.Update(key("d"))
	m.Update(key("n")) // anything but y keeps it
	if len(st.b.Tickets) != 1 {
		t.Fatal("deleted without confirmation")
	}
	m.Update(key("d"))
	m.Update(key("y"))
	if len(st.b.Tickets) != 0 {
		t.Fatal("not deleted after y")
	}
}

func TestViewShowsColumnsAndCards(t *testing.T) {
	b := board.New("t")
	b.Add("Write the orchestrator").Agent = "codex"
	x := b.Add("Review PR #12")
	x.Agent, x.AgentStatus = "claude", board.AgentRunning
	b.Move(x, 2)
	m := newModel(b, &memStore{b: b}, Options{AppVersion: "1.2.3"})
	m.width, m.height = 120, 30
	v := ansi.Strip(m.View().Content)
	for _, want := range []string{"Backlog", "In Progress", "Review", "Done", "Write the orchestrator", "Review PR #12", "1.2.3"} {
		if !strings.Contains(v, want) {
			t.Errorf("view missing %q", want)
		}
	}
	if os.Getenv("BOARD_SNAPSHOT") != "" {
		os.WriteFile(os.Getenv("BOARD_SNAPSHOT"), []byte(v), 0o644)
	}
}

type fakeRunner struct {
	spawned []string
	stopped []string
	status  map[string]board.AgentStatus
}

func (f *fakeRunner) Spawn(_ context.Context, t *board.Ticket) (string, error) {
	f.spawned = append(f.spawned, t.ID)
	return "pod-" + t.ID, nil
}
func (f *fakeRunner) Statuses(context.Context) (map[string]board.AgentStatus, error) {
	return f.status, nil
}
func (f *fakeRunner) Logs(context.Context, string, int64) (string, error) { return "hello\nworld", nil }
func (f *fakeRunner) Stop(_ context.Context, pod string) error {
	f.stopped = append(f.stopped, pod)
	return nil
}

// run executes a command and feeds its message back, like the runtime.
func run(m *model, cmd tea.Cmd) {
	if cmd != nil {
		if msg := cmd(); msg != nil {
			m.Update(msg)
		}
	}
}

func TestSpawnFlow(t *testing.T) {
	b := board.New("t")
	tk := b.Add("Fix login")
	st := &memStore{b: b}
	fr := &fakeRunner{status: map[string]board.AgentStatus{}}
	m := newModel(b, st, Options{Agents: fr})

	_, cmd := m.Update(key("s"))
	if cmd != nil || m.err == "" || len(fr.spawned) != 0 {
		t.Fatal("spawn without agent should be refused")
	}
	m.Update(key("a")) // claude
	_, cmd = m.Update(key("s"))
	run(m, cmd)
	if len(fr.spawned) != 1 || tk.AgentPod != "pod-"+tk.ID || tk.Status != board.StatusInProgress || tk.AgentStatus != board.AgentWaiting {
		t.Fatalf("after spawn: %+v spawned=%v", tk, fr.spawned)
	}
	_, cmd = m.Update(key("s"))
	if cmd != nil || len(fr.spawned) != 1 {
		t.Fatal("second spawn while waiting should be refused")
	}

	fr.status["pod-"+tk.ID] = board.AgentCompleted
	m.applyStatuses(fr.status)
	if tk.AgentStatus != board.AgentCompleted {
		t.Fatalf("status: %s", tk.AgentStatus)
	}

	_, cmd = m.Update(key("o"))
	run(m, cmd)
	if m.mode != modeLogs || !strings.Contains(m.logsView(), "world") {
		t.Fatalf("logs: mode=%v %q", m.mode, m.logs)
	}
	m.Update(key("q")) // back to the board

	_, cmd = m.Update(key("x"))
	run(m, cmd)
	if len(fr.stopped) != 1 || tk.AgentPod != "" || tk.AgentStatus != board.AgentNone {
		t.Fatalf("stop: %+v stopped=%v", tk, fr.stopped)
	}
}

func TestSpawnWithoutCluster(t *testing.T) {
	b := board.New("t")
	b.Add("x").Agent = "codex"
	m := newModel(b, &memStore{b: b}, Options{AgentsErr: "no cluster"})
	m.Update(key("s"))
	if !strings.Contains(m.err, "no cluster") {
		t.Fatalf("err %q", m.err)
	}
}
