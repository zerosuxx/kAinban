package boardui

import (
	"context"
	"fmt"
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

var (
	ctrlS    = tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl}
	tab      = tea.KeyPressMsg{Code: tea.KeyTab}
	escKey   = tea.KeyPressMsg{Code: tea.KeyEscape}
	rightKey = tea.KeyPressMsg{Code: tea.KeyRight}
)

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
	m.Update(ctrlS)
	if len(st.b.Tickets) != 1 || st.b.Tickets[0].Title != "Fix login" || st.saves != 1 {
		t.Fatalf("create: %+v saves=%d", st.b.Tickets, st.saves)
	}
	m.Update(key("space"))
	if st.b.Tickets[0].Status != board.StatusInProgress || m.col != 1 {
		t.Fatalf("move: status=%s col=%d", st.b.Tickets[0].Status, m.col)
	}
	m.Update(key("a"))
	if st.b.Tickets[0].Agent != board.AgentAuto { // first in the cycle
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
	x.Agent = "claude"
	x.Runs = []*board.Run{{Agent: "claude", Pod: "p", Status: board.AgentRunning}}
	b.Move(x, 2)
	m := newModel(b, &memStore{b: b}, Options{AppVersion: "1.2.3"})
	m.width, m.height = 190, 30
	v := ansi.Strip(m.View().Content)
	for _, want := range []string{"Backlog", "In Progress", "Review", "Done", "Blocked", "Write the orchestrator", "Review PR #12", "1.2.3"} {
		if !strings.Contains(v, want) {
			t.Errorf("view missing %q", want)
		}
	}
	if os.Getenv("BOARD_SNAPSHOT") != "" {
		os.WriteFile(os.Getenv("BOARD_SNAPSHOT"), []byte(v), 0o644)
	}
}

// fakeRunner simulates the cluster: pods live in a map.
type fakeRunner struct {
	pods     map[string]board.PodState
	spawned  []string
	stopped  []string
	logs     string
	n        int
	spawnErr error
}

func newFake() *fakeRunner {
	return &fakeRunner{pods: map[string]board.PodState{}, logs: "hello\nworld"}
}

func (f *fakeRunner) Spawn(_ context.Context, t *board.Ticket) (string, board.AgentType, error) {
	if f.spawnErr != nil {
		return "", "", f.spawnErr
	}
	f.n++
	pod := fmt.Sprintf("pod-%s-%d", t.ID, f.n)
	f.spawned = append(f.spawned, pod)
	f.pods[pod] = board.PodState{Status: board.AgentWaiting, Ticket: t.ID}
	ag := t.Agent
	if ag == board.AgentAuto {
		ag = "codex"
	}
	return pod, ag, nil
}
func (f *fakeRunner) Statuses(context.Context) (map[string]board.PodState, error) {
	out := map[string]board.PodState{}
	for k, v := range f.pods {
		out[k] = v
	}
	return out, nil
}
func (f *fakeRunner) Logs(_ context.Context, pod string, _ int64) (string, error) {
	return f.logs + " (" + pod + ")", nil
}
func (f *fakeRunner) Stop(_ context.Context, pod string) error {
	f.stopped = append(f.stopped, pod)
	delete(f.pods, pod)
	return nil
}
func (f *fakeRunner) StopTicket(_ context.Context, id string) error {
	for pod, s := range f.pods {
		if s.Ticket == id {
			f.Stop(context.Background(), pod)
		}
	}
	return nil
}
func (f *fakeRunner) AttachCommand(pod string, _ board.AgentType, shell bool) []string {
	return []string{"true", pod, fmt.Sprint(shell)}
}

// run executes a command (and batches) and feeds the messages back.
func run(m *model, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	switch msg := cmd().(type) {
	case nil:
	case tea.BatchMsg:
		for _, c := range msg {
			run(m, c)
		}
	default:
		if _, isTick := msg.(pollMsg); isTick {
			return
		}
		_, next := m.Update(msg)
		if _, isStatus := msg.(statusMsg); !isStatus { // avoid the endless poll loop
			run(m, next)
		}
	}
}

// poll applies the fake cluster's pod states (without the 5 s ticker).
func poll(m *model) {
	st, _ := m.opts.Agents.Statuses(context.Background())
	run(m, m.applyStatuses(st))
}

func agentBoard(t *testing.T) (*board.Board, *board.Ticket, *fakeRunner, *model) {
	t.Helper()
	b := board.New("t")
	x := b.Add("Fix login")
	x.Agent = "claude"
	fr := newFake()
	m := newModel(b, &memStore{b: b}, Options{Agents: fr})
	m.width, m.height = 140, 30
	return b, x, fr, m
}

func TestSpawnFlow(t *testing.T) {
	b := board.New("t")
	tk := b.Add("Fix login")
	fr := newFake()
	m := newModel(b, &memStore{b: b}, Options{Agents: fr})

	if _, cmd := m.Update(key("s")); cmd != nil || m.err == "" {
		t.Fatal("spawn without agent should be refused")
	}
	m.Update(key("a"))
	m.Update(key("a")) // auto -> claude
	_, cmd := m.Update(key("s"))
	run(m, cmd)
	r := tk.Current()
	if len(fr.spawned) != 1 || r == nil || r.Pod != fr.spawned[0] || r.Agent != "claude" ||
		tk.Status != board.StatusInProgress || r.Status != board.AgentWaiting {
		t.Fatalf("after spawn: %+v run=%+v", tk, r)
	}
	// Still running: s asks before restarting.
	if _, cmd := m.Update(key("s")); cmd != nil || m.mode != modeConfirm || m.confirm != confirmRestart {
		t.Fatal("restart while running should ask")
	}
	m.Update(key("n"))

	fr.pods[r.Pod] = board.PodState{Status: board.AgentCompleted, Ticket: tk.ID}
	poll(m)
	if r.Status != board.AgentCompleted || tk.Status != board.StatusReview || !strings.Contains(r.Output, "world") || r.FinishedAt == nil {
		t.Fatalf("finished: status=%s ticket=%s out=%q", r.Status, tk.Status, r.Output)
	}

	_, cmd = m.Update(key("o"))
	run(m, cmd)
	if m.mode != modeLogs || !strings.Contains(m.logsView(), "world") {
		t.Fatalf("logs: mode=%v %q", m.mode, m.logsView())
	}
	m.Update(key("q"))

	_, cmd = m.Update(key("x"))
	if cmd != nil || m.mode != modeConfirm {
		t.Fatal("x should ask first")
	}
	_, cmd = m.Update(key("y"))
	run(m, cmd)
	if len(fr.stopped) != 1 || r.Live() || r.Output == "" || r.Status != board.AgentCompleted {
		t.Fatalf("stop: %+v stopped=%v", r, fr.stopped)
	}
}

func TestRestartStopsPreviousPodAndKeepsHistory(t *testing.T) {
	_, x, fr, m := agentBoard(t)
	m.selectTicket(x)
	_, cmd := m.Update(key("s"))
	run(m, cmd)
	first := x.Current()
	fr.pods[first.Pod] = board.PodState{Status: board.AgentError, Ticket: x.ID}
	poll(m) // -> Blocked, output saved
	if x.Status != board.StatusBlocked {
		t.Fatalf("status %s", x.Status)
	}
	m.selectTicket(x)
	m.Update(key("a")) // claude -> codex: switching the agent keeps the old run
	_, cmd = m.Update(key("s"))
	run(m, cmd)
	if len(x.Runs) != 2 || first.Live() || !slicesContains(fr.stopped, first.Pod) {
		t.Fatalf("previous pod not stopped: runs=%d stopped=%v", len(x.Runs), fr.stopped)
	}
	if first.Output == "" || first.Status != board.AgentError || x.Current().Agent != "codex" {
		t.Fatalf("history lost: first=%+v current=%+v", first, x.Current())
	}
	if len(fr.pods) != 1 {
		t.Fatalf("pods left: %v", fr.pods)
	}
	m.Update(key("enter"))
	v := ansi.Strip(m.View().Content)
	if !strings.Contains(v, "Run #2 · codex") || !strings.Contains(v, "Run #1 · claude") {
		t.Fatalf("details should list both runs:\n%s", v)
	}
}

func slicesContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestDoneStopsPods(t *testing.T) {
	b, x, fr, m := agentBoard(t)
	m.selectTicket(x)
	_, cmd := m.Update(key("s"))
	run(m, cmd)
	r := x.Current()

	// Running: moving to Done asks first and stays put on "no".
	b.MoveTo(x, board.StatusReview)
	m.selectTicket(x)
	fr.pods[r.Pod] = board.PodState{Status: board.AgentRunning, Ticket: x.ID}
	poll(m)
	m.Update(key("space"))
	if m.mode != modeConfirm || m.confirm != confirmDone || x.Status != board.StatusReview {
		t.Fatalf("done while running should ask: mode=%v status=%s", m.mode, x.Status)
	}
	m.Update(key("n"))
	if x.Status != board.StatusReview || len(fr.stopped) != 0 {
		t.Fatal("cancelled done changed something")
	}
	m.Update(key("space"))
	_, cmd = m.Update(key("y"))
	run(m, cmd)
	if x.Status != board.StatusDone || r.Live() || r.Status != board.AgentStopped || len(fr.pods) != 0 {
		t.Fatalf("done: status=%s run=%+v pods=%v", x.Status, r, fr.pods)
	}

	// Finished agent: Done stops the pod without asking.
	y := b.Add("y")
	y.Agent = "codex"
	m.selectTicket(y)
	_, cmd = m.Update(key("s"))
	run(m, cmd)
	ry := y.Current()
	fr.pods[ry.Pod] = board.PodState{Status: board.AgentCompleted, Ticket: y.ID}
	poll(m) // -> Review
	m.selectTicket(y)
	_, cmd = m.Update(key("space"))
	run(m, cmd)
	if y.Status != board.StatusDone || ry.Live() || ry.Status != board.AgentCompleted || ry.Output == "" {
		t.Fatalf("done after finish: status=%s run=%+v", y.Status, ry)
	}
}

func TestDeleteStopsAllPodsOfTicket(t *testing.T) {
	_, x, fr, m := agentBoard(t)
	m.selectTicket(x)
	_, cmd := m.Update(key("s"))
	run(m, cmd)
	fr.pods["stray"] = board.PodState{Status: board.AgentRunning, Ticket: x.ID} // untracked
	m.Update(key("d"))
	_, cmd = m.Update(key("y"))
	run(m, cmd)
	if len(fr.pods) != 0 {
		t.Fatalf("pods left: %v", fr.pods)
	}
}

func TestOrphans(t *testing.T) {
	_, _, fr, m := agentBoard(t)
	fr.pods["lost-1"] = board.PodState{Status: board.AgentCompleted, Ticket: "gone"}
	poll(m)
	if len(m.orphans) != 1 || !strings.Contains(m.notice, "C stops") {
		t.Fatalf("orphans=%v notice=%q", m.orphans, m.notice)
	}
	m.Update(key("C"))
	if m.mode != modeConfirm {
		t.Fatal("C should ask")
	}
	_, cmd := m.Update(key("y"))
	run(m, cmd)
	if len(fr.pods) != 0 {
		t.Fatalf("orphan not stopped: %v", fr.pods)
	}
}

func TestPodDeletedOutsideMarksRunGone(t *testing.T) {
	_, x, fr, m := agentBoard(t)
	m.selectTicket(x)
	_, cmd := m.Update(key("s"))
	run(m, cmd)
	r := x.Current()
	delete(fr.pods, r.Pod)
	poll(m)
	if r.Live() || r.Status != board.AgentError {
		t.Fatalf("run %+v", r)
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

func TestSpawnFailureRecordedOnRun(t *testing.T) {
	_, x, fr, m := agentBoard(t)
	fr.spawnErr = fmt.Errorf("boom")
	m.selectTicket(x)
	_, cmd := m.Update(key("s"))
	run(m, cmd)
	if r := x.Current(); r == nil || r.Status != board.AgentError || !strings.Contains(r.Output, "boom") {
		t.Fatalf("run %+v", r)
	}
}

func TestLogsScroll(t *testing.T) {
	_, x, fr, m := agentBoard(t)
	var lines []string
	for i := range 200 {
		lines = append(lines, fmt.Sprintf("line %03d", i))
	}
	fr.logs = strings.Join(lines, "\n")
	m.width, m.height = 80, 20
	m.selectTicket(x)
	_, cmd := m.Update(key("s"))
	run(m, cmd)
	fr.pods[x.Current().Pod] = board.PodState{Status: board.AgentRunning, Ticket: x.ID}
	poll(m)

	_, cmd = m.Update(key("o"))
	run(m, cmd)
	if m.mode != modeLogs || !m.vp.AtBottom() || !strings.Contains(m.logsView(), "line 199") {
		t.Fatalf("should open at the end: mode=%v", m.mode)
	}
	m.Update(key("k"))
	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if m.mode != modeLogs || m.vp.AtBottom() {
		t.Fatal("scrolling up should stay in the logs, off the bottom")
	}
	m.Update(key("g"))
	if !strings.Contains(m.logsView(), "line 000") {
		t.Fatal("g should jump to the top")
	}
	run(m, m.logsCmd(m.logsPod))
	if m.vp.AtBottom() {
		t.Fatal("refresh moved the view while scrolled up")
	}
	m.Update(key("q"))
	if m.mode != modeBoard {
		t.Fatal("q should leave the logs")
	}
}

func TestSavedOutputAfterPodGone(t *testing.T) {
	b := board.New("t")
	x := b.Add("x")
	x.Runs = []*board.Run{{Agent: "claude", Pod: "p", PodGone: true, Status: board.AgentCompleted, Output: "saved text"}}
	m := newModel(b, &memStore{b: b}, Options{Agents: newFake()})
	m.Update(key("o"))
	if m.mode != modeLogs || !strings.Contains(m.logsView(), "saved text") {
		t.Fatalf("o should show the saved output: %q", m.logsView())
	}
}

func TestAttach(t *testing.T) {
	_, x, fr, m := agentBoard(t)
	m.selectTicket(x)
	if _, cmd := m.Update(key("t")); cmd != nil || !strings.Contains(m.err, "no agent pod") {
		t.Fatalf("attach without pod: err=%q", m.err)
	}
	_, cmd := m.Update(key("s"))
	run(m, cmd)
	if _, cmd := m.Update(key("t")); cmd != nil || !strings.Contains(m.err, "starting") {
		t.Fatalf("attach while starting: err=%q", m.err)
	}
	fr.pods[x.Current().Pod] = board.PodState{Status: board.AgentRunning, Ticket: x.ID}
	poll(m)
	if _, cmd := m.Update(key("t")); cmd == nil {
		t.Fatal("attach should hand the terminal to a process")
	}
	m.Update(attachDoneMsg{})
	if m.notice == "" {
		t.Fatal("no notice after returning")
	}
}

func TestStopNeedsConfirmation(t *testing.T) {
	b, x, fr, m := agentBoard(t)
	m.selectTicket(x)
	_, cmd := m.Update(key("s"))
	run(m, cmd)
	m.width, m.height = 100, 30
	m.Update(key("x"))
	v := ansi.Strip(m.View().Content)
	lines := strings.Split(v, "\n")
	if !strings.Contains(v, "Confirm") || !strings.Contains(v, "stop the agent") || !strings.Contains(v, "/esc no") {
		t.Fatalf("the question should be a popup:\n%s", v)
	}
	if !strings.Contains(lines[len(lines)-1], "y confirm") {
		t.Fatalf("key bar should stay at the bottom:\n%s", v)
	}
	_, cmd = m.Update(key("n"))
	run(m, cmd)
	if len(fr.stopped) != 0 || !x.Current().Live() {
		t.Fatal("stopped without y")
	}
	b.Add("no pod")
	m.selectTicket(b.Tickets[1])
	m.Update(key("x"))
	if m.mode == modeConfirm || !strings.Contains(m.err, "no agent pod") {
		t.Fatalf("x without pod: mode=%v err=%q", m.mode, m.err)
	}
}

func TestAutoAgentShowsChoice(t *testing.T) {
	_, x, _, m := agentBoard(t)
	x.Agent = board.AgentAuto
	m.selectTicket(x)
	_, cmd := m.Update(key("s"))
	run(m, cmd)
	if x.Current().Agent != "codex" || x.EffectiveAgent() != "codex" {
		t.Fatalf("agent run: %+v", x.Current())
	}
	if v := ansi.Strip(m.View().Content); !strings.Contains(v, "auto→codex") {
		t.Fatalf("card should show the choice:\n%s", v)
	}
}

func TestDetailsScrollAndBack(t *testing.T) {
	b := board.New("t")
	x := b.Add("x")
	x.Runs = []*board.Run{{Agent: "claude", Pod: "p", PodGone: true, Status: board.AgentCompleted, Output: "hello\nworld"}}
	m := newModel(b, &memStore{b: b}, Options{})
	m.width, m.height = 100, 30
	m.Update(key("enter"))
	v := ansi.Strip(m.View().Content)
	if m.mode != modeDetail || !strings.Contains(v, "Run #1 · claude") || !strings.Contains(v, "world") {
		t.Fatalf("details:\n%s", v)
	}
	m.Update(key("j"))
	if m.mode != modeDetail {
		t.Fatal("j left the details")
	}
	m.Update(key("q"))
	if m.mode != modeBoard {
		t.Fatal("q should go back")
	}
}

func TestProjectKeys(t *testing.T) {
	b := board.New("t")
	st := &memStore{b: b}
	if err := applyProject(b, st, "xy"); err != nil || b.Project != "XY" {
		t.Fatalf("new board: %v %s", err, b.Project)
	}
	x := b.Add("Fix login")
	if x.Key != "XY-1" {
		t.Fatalf("key %s", x.Key)
	}
	if err := applyProject(b, st, "ab"); err == nil {
		t.Fatal("changing the project of a board with tickets should fail")
	}
	if err := applyProject(b, st, "xy"); err != nil {
		t.Fatal("same project should be fine")
	}
	m := newModel(b, st, Options{})
	m.width, m.height = 140, 30
	if v := ansi.Strip(m.View().Content); !strings.Contains(v, "XY-1 Fix login") || !strings.Contains(v, "board XY") {
		t.Fatalf("card/header should show the key:\n%s", v)
	}
}

func TestEditPopup(t *testing.T) {
	b := board.New("t")
	x := b.Add("Old title")
	st := &memStore{b: b}
	m := newModel(b, st, Options{})
	m.width, m.height = 100, 40
	m.selectTicket(x)

	m.Update(key("e"))
	if m.mode != modeForm || m.form.ticketID != x.ID || m.form.title.Value() != "Old title" {
		t.Fatalf("form: mode=%v", m.mode)
	}
	v := ansi.Strip(m.View().Content)
	for _, want := range []string{"Edit KAI-1", "Title", "Description", "Priority", "Agent", "Labels", "Branch"} {
		if !strings.Contains(v, want) {
			t.Fatalf("popup missing %q:\n%s", want, v)
		}
	}
	// Replace the title.
	for range len("Old title") {
		m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	}
	typeText(m, "New title")
	m.Update(tab) // description: enter is a newline here
	typeText(m, "line one")
	m.Update(key("enter"))
	typeText(m, "line two")
	m.Update(tab)      // priority
	m.Update(rightKey) // P3 -> P4
	m.Update(tab)      // agent
	m.Update(rightKey) // none -> auto
	m.Update(rightKey) // -> claude
	m.Update(tab)      // labels
	typeText(m, "backend, urgent ,")
	m.Update(key("enter")) // enter moves on in single-line fields
	typeText(m, "feature/login")
	m.Update(ctrlS)
	if m.mode != modeBoard || x.Title != "New title" || x.Description != "line one\nline two" ||
		x.Priority != 4 || x.Agent != "claude" || x.Branch != "feature/login" ||
		strings.Join(x.Labels, "|") != "backend|urgent" {
		t.Fatalf("saved: %+v", x)
	}

	// esc discards.
	m.Update(key("e"))
	typeText(m, "XXX")
	m.Update(escKey)
	if x.Title != "New title" || m.mode != modeBoard {
		t.Fatalf("esc should discard: %q", x.Title)
	}

	// A new ticket needs a title.
	m.Update(key("n"))
	m.Update(ctrlS)
	if m.mode != modeForm || m.form.err == "" || len(b.Tickets) != 1 {
		t.Fatal("empty title should be refused")
	}
	typeText(m, "Second")
	m.Update(ctrlS)
	if len(b.Tickets) != 2 || b.Tickets[1].Key != "KAI-2" {
		t.Fatalf("new ticket: %+v", b.Tickets)
	}
	if os.Getenv("BOARD_SNAPSHOT") != "" {
		m.selectTicket(x)
		m.Update(key("e"))
		os.WriteFile(os.Getenv("BOARD_SNAPSHOT"), []byte(ansi.Strip(m.View().Content)), 0o644)
	}
}

func TestMarquee(t *testing.T) {
	if got := marqueeText("KAI-1 long title", 8, 0); got != "KAI-1 lo" {
		t.Fatalf("start: %q", got)
	}
	if got := marqueeText("KAI-1 long title", 8, marqueePause+6); got != "long tit" {
		t.Fatalf("scrolled: %q", got)
	}
	// Wraps around through the gap back to the start.
	loop := len([]rune("abcdefghij" + marqueeGap))
	if got := marqueeText("abcdefghij", 4, marqueePause+loop); got != "abcd" {
		t.Fatalf("wrap: %q", got)
	}

	b := board.New("t")
	short := b.Add("short")
	long := b.Add("a very long title that cannot fit on a phone sized card at all")
	m := newModel(b, &memStore{b: b}, Options{})
	m.width, m.height = 60, 24
	m.selectTicket(long)
	if m.needsMarquee() {
		t.Fatal("no marquee without animation (tests, non-interactive)")
	}
	m.animate = true
	if !m.needsMarquee() {
		t.Fatal("long selected title should scroll")
	}
	if _, cmd := m.Update(marqueeMsg{}); cmd == nil || m.marqueeStep != 1 {
		t.Fatalf("tick should advance and reschedule: step=%d", m.marqueeStep)
	}
	m.selectTicket(short)
	if m.needsMarquee() {
		t.Fatal("short title should not scroll")
	}
	m.selectTicket(long)
	m.Update(key("?")) // help: no marquee outside the board
	if m.needsMarquee() {
		t.Fatal("marquee outside the board view")
	}
}
