package boardui

import (
	"time"

	"context"
	"fmt"
	"os/exec"
	"slices"
	"strings"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/zerosuxx/kainban/internal/board"
)

// AgentRunner starts and watches agent pods (internal/agent.Runner).
type AgentRunner interface {
	Spawn(ctx context.Context, t *board.Ticket) (pod string, agent board.AgentType, err error)
	// Statuses reports every agent pod with its state and ticket.
	Statuses(ctx context.Context) (map[string]board.PodState, error)
	Logs(ctx context.Context, pod string, tail int64) (string, error)
	Stop(ctx context.Context, pod string) error
	// StopTicket deletes every agent pod of a ticket, tracked or not.
	StopTicket(ctx context.Context, ticketID string) error
	// ModelFor is the model a ticket's (resolved) agent runs with ("" = default).
	ModelFor(t *board.Ticket) string
	// AttachCommand returns the argv that opens the agent's session (or a
	// shell) in its pod, run with the terminal handed over.
	AttachCommand(pod string, agent board.AgentType, shell bool) []string
}

// maxSavedOutput caps the agent output kept per run in the board file.
const maxSavedOutput = 64 << 10

type (
	statusMsg struct{ states map[string]board.PodState }
	outputMsg struct {
		ticketID, pod, text string
		err                 error
	}
	// stopResult is one stopped pod and the output saved before.
	stopResult struct {
		pod, output string
		err         error
	}
	spawnedMsg struct {
		ticketID string
		run      *board.Run
		pod      string
		agent    board.AgentType
		model    string
		err      error
		stopped  []stopResult // previous runs' pods stopped first
	}
	stoppedMsg struct {
		ticketID string // "" for orphans
		results  []stopResult
	}
)

func (m *model) pollCmd() tea.Cmd {
	if m.opts.Agents == nil {
		return nil
	}
	agents, ctx := m.opts.Agents, m.ctx
	return func() tea.Msg {
		st, err := agents.Statuses(ctx)
		if err != nil {
			return statusMsg{} // transient; keep the last known state
		}
		return statusMsg{st}
	}
}

func (m *model) findTicket(id string) *board.Ticket {
	for _, t := range m.b.Tickets {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// applyStatuses updates every live run from its pod, moves tickets whose
// latest run ended, saves finished runs' output and spots orphaned pods.
func (m *model) applyStatuses(st map[string]board.PodState) tea.Cmd {
	if st == nil {
		return nil
	}
	changed := false
	var cmds []tea.Cmd
	known := map[string]bool{}
	for _, t := range m.b.Tickets {
		for _, r := range t.Runs {
			if r.Pod != "" {
				known[r.Pod] = true
			}
			if !r.Live() {
				continue
			}
			ps, ok := st[r.Pod]
			if !ok { // pod gone (deleted outside the board)
				r.PodGone = true
				if r.Status.Active() {
					r.SetStatus(board.AgentError)
					if r == t.Current() {
						m.finished(t, board.AgentError)
					}
				}
				changed = true
				continue
			}
			if ps.Status == r.Status || r.Status == board.AgentStopped {
				continue
			}
			r.SetStatus(ps.Status)
			t.Touch()
			changed = true
			if !ps.Status.Finished() {
				continue
			}
			cmds = append(cmds, m.captureCmd(t, r)) // keep the output with the run
			if r != t.Current() {
				continue
			}
			m.finished(t, ps.Status)
		}
	}
	var orphans []string
	for pod := range st {
		if !known[pod] {
			orphans = append(orphans, pod)
		}
	}
	slices.Sort(orphans)
	if len(orphans) > 0 && len(m.orphans) == 0 && m.err == "" {
		m.notice = fmt.Sprintf("%d agent pod(s) belong to no ticket run — C stops them", len(orphans))
	}
	m.orphans = orphans
	if changed {
		if err := m.store.Save(m.b); err != nil {
			m.err = "save failed: " + err.Error()
		}
	}
	return tea.Batch(cmds...)
}

// finished reports the end of a ticket's latest run and hands the ticket on:
// to Review when it succeeded, to Blocked when it failed (from In Progress).
func (m *model) finished(t *board.Ticket, s board.AgentStatus) {
	target := board.StatusReview
	if s == board.AgentError {
		target = board.StatusBlocked
	}
	if t.Status == board.StatusInProgress {
		wasSelected := m.selected() == t
		if m.b.MoveTo(t, target) == nil && wasSelected {
			m.selectTicket(t) // the cursor follows the card
		}
	}
	msg := fmt.Sprintf("%s: agent %s", cardTitle(t), s)
	if t.Status == target {
		msg = cardTitle(t) + " → " + m.b.Columns[m.b.ColumnIndex(t.Status)].Name + " (agent " + string(s) + ")"
	}
	if s == board.AgentError {
		m.err = msg // a failed agent is an error, not a success
	} else {
		m.notice = msg
	}
}

func trimOutput(s string) string {
	s = strings.TrimRight(s, "\n")
	if len(s) > maxSavedOutput {
		s = "…\n" + s[len(s)-maxSavedOutput:]
	}
	return s
}

// captureCmd fetches a run's output to save it with the run.
func (m *model) captureCmd(t *board.Ticket, r *board.Run) tea.Cmd {
	if m.opts.Agents == nil || !r.Live() {
		return nil
	}
	agents, ctx, id, pod := m.opts.Agents, m.ctx, t.ID, r.Pod
	return func() tea.Msg {
		text, err := agents.Logs(ctx, pod, 2000)
		return outputMsg{ticketID: id, pod: pod, text: text, err: err}
	}
}

func (m *model) applyOutput(msg outputMsg) {
	t := m.findTicket(msg.ticketID)
	if t == nil || msg.err != nil {
		return // keep whatever was saved before
	}
	if r := t.RunByPod(msg.pod); r != nil {
		r.Output = trimOutput(msg.text)
		if err := m.store.Save(m.b); err != nil {
			m.err = "save failed: " + err.Error()
		}
	}
	if m.mode == modeDetail && m.selected() == t {
		m.openDetail()
	}
}

// stopPods saves each pod's output (unless already saved) and deletes it.
func stopPods(ctx context.Context, agents AgentRunner, pods []string, saved map[string]bool) []stopResult {
	var out []stopResult
	for _, pod := range pods {
		res := stopResult{pod: pod}
		if !saved[pod] {
			if text, err := agents.Logs(ctx, pod, 2000); err == nil {
				res.output = trimOutput(text)
			}
		}
		res.err = agents.Stop(ctx, pod)
		out = append(out, res)
	}
	return out
}

func livePods(runs []*board.Run) (pods []string, saved map[string]bool) {
	saved = map[string]bool{}
	for _, r := range runs {
		pods = append(pods, r.Pod)
		saved[r.Pod] = r.Output != ""
	}
	return pods, saved
}

// stopRunsCmd stops the given runs' pods, keeping their output.
func (m *model) stopRunsCmd(t *board.Ticket, runs []*board.Run) tea.Cmd {
	if m.opts.Agents == nil || len(runs) == 0 {
		return nil
	}
	agents, ctx, id := m.opts.Agents, m.ctx, t.ID
	pods, saved := livePods(runs)
	return func() tea.Msg {
		return stoppedMsg{ticketID: id, results: stopPods(ctx, agents, pods, saved)}
	}
}

// markStopped records stopped pods on their runs.
func markStopped(t *board.Ticket, results []stopResult) (failed []string) {
	for _, res := range results {
		r := t.RunByPod(res.pod)
		if r == nil {
			continue
		}
		if res.err != nil {
			failed = append(failed, res.pod+": "+res.err.Error())
			continue
		}
		r.PodGone = true
		if res.output != "" {
			r.Output = res.output
		}
		if r.Status.Active() {
			r.SetStatus(board.AgentStopped)
		}
	}
	return failed
}

func (m *model) applyStopped(msg stoppedMsg) {
	if msg.ticketID == "" { // orphans
		var failed []string
		for _, res := range msg.results {
			if res.err != nil {
				failed = append(failed, res.pod+": "+res.err.Error())
			}
		}
		if len(failed) > 0 {
			m.err = "stop failed: " + strings.Join(failed, "; ")
		} else {
			m.notice = fmt.Sprintf("stopped %d orphaned agent pod(s)", len(msg.results))
		}
		return
	}
	t := m.findTicket(msg.ticketID)
	if t == nil {
		return
	}
	if failed := markStopped(t, msg.results); len(failed) > 0 {
		m.save("")
		m.err = "stop failed: " + strings.Join(failed, "; ")
		return
	}
	t.Touch()
	m.save(fmt.Sprintf("stopped %d agent pod(s) of %s (output kept)", len(msg.results), cardTitle(t)))
}

// spawn starts a new run; a still-running agent is stopped first after
// asking (force skips the question). Previous runs' pods are stopped (their
// output saved) so nothing is left behind.
func (m *model) spawn(t *board.Ticket, force bool) (tea.Model, tea.Cmd) {
	switch {
	case m.opts.Agents == nil:
		m.err = "agents unavailable: " + m.opts.AgentsErr
		return m, nil
	case t.Agent == "":
		m.err = "pick an agent first (a)"
		return m, nil
	case t.RunStatus().Active() && !force:
		m.mode, m.confirm = modeConfirm, confirmRestart
		return m, nil
	}
	if t.Status != board.StatusInProgress { // e.g. a retry from Review or Blocked
		if err := m.b.MoveTo(t, board.StatusInProgress); err != nil {
			m.err = "cannot start: " + err.Error()
			return m, nil
		}
		m.selectTicket(t)
	}
	prev := t.LiveRuns()
	run := t.StartRun()
	m.save("starting " + string(t.Agent) + " for " + cardTitle(t) + "…")
	agents, ctx, tc := m.opts.Agents, m.ctx, *t
	pods, saved := livePods(prev)
	return m, func() tea.Msg {
		stopped := stopPods(ctx, agents, pods, saved)
		pod, agent, err := agents.Spawn(ctx, &tc)
		tc.Agent = agent
		return spawnedMsg{ticketID: tc.ID, run: run, pod: pod, agent: agent, model: agents.ModelFor(&tc), err: err, stopped: stopped}
	}
}

func (m *model) applySpawned(msg spawnedMsg) {
	t := m.findTicket(msg.ticketID)
	if t == nil {
		return
	}
	failed := markStopped(t, msg.stopped)
	if msg.err != nil {
		msg.run.SetStatus(board.AgentError)
		msg.run.Output = "start failed: " + msg.err.Error()
		m.finished(t, board.AgentError)
		m.save("")
		m.err = cardTitle(t) + ": start failed: " + msg.err.Error()
		return
	}
	msg.run.Pod, msg.run.Agent, msg.run.Model = msg.pod, msg.agent, msg.model
	started := cardTitle(t) + ": " + string(msg.agent) + " started"
	if msg.model != "" {
		started += " (" + msg.model + ")"
	}
	m.save(started)
	if len(failed) > 0 {
		m.err = "previous pod not stopped: " + strings.Join(failed, "; ")
	}
}

// confirmed runs the action of an answered y/N question.
func (m *model) confirmed(t *board.Ticket) (tea.Model, tea.Cmd) {
	if m.confirm == confirmOrphans {
		agents, ctx, pods := m.opts.Agents, m.ctx, m.orphans
		m.orphans = nil
		return m, func() tea.Msg {
			return stoppedMsg{results: stopPods(ctx, agents, pods, map[string]bool{})}
		}
	}
	if t == nil {
		return m, nil
	}
	switch m.confirm {
	case confirmStop:
		return m, m.stopRunsCmd(t, t.LiveRuns())
	case confirmRestart:
		return m.spawn(t, true)
	case confirmAttach:
		return m.attach(t, false, true)
	case confirmDone:
		if err := m.b.MoveTo(t, board.StatusDone); err != nil {
			m.err = err.Error()
			return m, nil
		}
		m.selectTicket(t)
		m.save(cardTitle(t) + " → Done, stopping its agent pods")
		return m, m.stopRunsCmd(t, t.LiveRuns())
	default: // confirmDelete
		var cmd tea.Cmd
		if m.opts.Agents != nil && len(t.Runs) > 0 {
			agents, ctx, id := m.opts.Agents, m.ctx, t.ID
			cmd = func() tea.Msg {
				err := agents.StopTicket(ctx, id)
				return stoppedMsg{results: []stopResult{{pod: "ticket " + id, err: err}}}
			}
		}
		m.b.Delete(t)
		m.save("deleted " + cardTitle(t))
		return m, cmd
	}
}

// attach hands the terminal to the agent's session (or a shell) in its pod.
// While the headless run is still working, the session (a second CLI on the
// same conversation) does not show its progress, so `t` asks first.
func (m *model) attach(t *board.Ticket, shell, confirmed bool) (tea.Model, tea.Cmd) {
	r := t.Current()
	switch {
	case m.opts.Agents == nil:
		m.err = "agents unavailable: " + m.opts.AgentsErr
		return m, nil
	case r == nil || !r.Live():
		m.err = "no agent pod for this ticket (s starts one)"
		return m, nil
	case r.Status == board.AgentWaiting:
		m.err = "the agent pod is still starting"
		return m, nil
	case r.Status == board.AgentRunning && !shell && !confirmed:
		m.mode, m.confirm = modeConfirm, confirmAttach
		return m, nil
	}
	argv := m.opts.Agents.AttachCommand(r.Pod, r.Agent, shell)
	if m.attachEmbed {
		return m.openTerm(t, argv, shell)
	}
	return m, tea.ExecProcess(exec.Command(argv[0], argv[1:]...), func(err error) tea.Msg {
		return attachDoneMsg{err}
	})
}

// openLogs shows the latest run's output: live from its pod, or the saved
// output once the pod is gone.
func (m *model) openLogs(t *board.Ticket) (tea.Model, tea.Cmd) {
	r := t.Current()
	if r == nil {
		m.err = "no agent run for this ticket (s starts one)"
		return m, nil
	}
	m.vp = viewport.New(viewport.WithWidth(m.width), viewport.WithHeight(m.logsHeight()))
	m.vp.SoftWrap = true // long lines readable on narrow (phone) screens
	m.mode, m.logsLoaded, m.logsFollow = modeLogs, false, true
	m.logsSeq++
	if !r.Live() || m.opts.Agents == nil {
		m.logsPod = ""
		out := r.Output
		if out == "" {
			out = subtle.Render("(no output saved)")
		}
		m.vp.SetContent(out)
		m.vp.GotoBottom()
		return m, nil
	}
	m.logsPod = r.Pod
	m.vp.SetContent(subtle.Render("loading…"))
	return m, tea.Batch(m.logsCmd(r.Pod), m.logsTick())
}

// logsTick schedules the next refresh of the open output view.
func (m *model) logsTick() tea.Cmd {
	seq := m.logsSeq
	return tea.Tick(logsInterval, func(time.Time) tea.Msg { return logsTickMsg{seq: seq} })
}

// followIfAtBottom pauses following when the user scrolled up and resumes it
// when they scrolled back to the end.
func (m *model) followIfAtBottom() {
	m.logsFollow = m.vp.AtBottom()
}

func (m *model) logsCmd(pod string) tea.Cmd {
	agents, ctx := m.opts.Agents, m.ctx
	return func() tea.Msg {
		text, err := agents.Logs(ctx, pod, 500)
		return logsMsg{pod: pod, text: text, err: err}
	}
}
