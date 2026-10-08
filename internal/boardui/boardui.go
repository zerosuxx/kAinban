// Package boardui is the interactive kanban board: columns side by side,
// tickets as cards, keyboard-driven like OpenKanban.
package boardui

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/zerosuxx/kainban/internal/board"
)

// AgentRunner starts and watches agent pods (internal/agent.Runner).
type AgentRunner interface {
	Spawn(ctx context.Context, t *board.Ticket) (pod string, agent board.AgentType, err error)
	Statuses(ctx context.Context) (map[string]board.AgentStatus, error)
	Logs(ctx context.Context, pod string, tail int64) (string, error)
	Stop(ctx context.Context, pod string) error
	// AttachCommand returns the argv that opens the agent's session (or a
	// shell) in its pod, run with the terminal handed over.
	AttachCommand(pod string, agent board.AgentType, shell bool) []string
}

// Options configures Run.
type Options struct {
	AppVersion string
	Location   string      // where the board is stored, shown in the header
	Agents     AgentRunner // nil: agents unavailable (not in the cluster)
	AgentsErr  string      // why Agents is nil, shown on s
}

const pollInterval = 5 * time.Second

// Run shows the board until the user quits. Every change is saved at once.
func Run(ctx context.Context, store board.Store, opts Options) error {
	b, err := store.Load()
	if err != nil {
		return err
	}
	m := newModel(b, store, opts)
	m.ctx = ctx
	_, err = tea.NewProgram(m, tea.WithContext(ctx)).Run()
	return err
}

type mode int

const (
	modeBoard   mode = iota
	modeInput        // new ticket / edit title / edit description
	modeConfirm      // y/N question, see confirmKind
	modeDetail
	modeHelp
	modeLogs
)

type (
	pollMsg    struct{}
	statusMsg  struct{ statuses map[string]board.AgentStatus }
	spawnedMsg struct {
		ticketID, pod string
		agent         board.AgentType
		err           error
	}
	logsMsg struct {
		pod, text string
		err       error
	}
	stoppedMsg struct {
		ticketID string
		err      error
	}
	attachDoneMsg struct{ err error }
	outputMsg     struct {
		ticketID, text string
		err            error
	}
)

type confirmKind int

const (
	confirmDelete confirmKind = iota
	confirmStop
)

type inputKind int

const (
	inputNew inputKind = iota
	inputTitle
	inputDescription
)

type model struct {
	ctx   context.Context
	b     *board.Board
	store board.Store
	opts  Options

	col    int   // selected column
	row    []int // selected row per column
	scroll []int // first visible card per column

	mode      mode
	confirm   confirmKind
	inputKind inputKind
	input     textinput.Model

	notice string
	err    string

	logsPod    string
	logsLoaded bool           // first content arrived (then jump to the end)
	vp         viewport.Model // modeLogs: scrollable agent output

	width, height int
}

func newModel(b *board.Board, store board.Store, opts Options) *model {
	return &model{
		ctx: context.Background(),
		b:   b, store: store, opts: opts,
		row: make([]int, len(b.Columns)), scroll: make([]int, len(b.Columns)),
		width: 100, height: 30,
	}
}

func (m *model) Init() tea.Cmd {
	if m.opts.Agents == nil {
		return nil
	}
	return m.pollCmd()
}

func (m *model) pollCmd() tea.Cmd {
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

// applyStatuses updates the tickets' agent state from their pods.
func (m *model) applyStatuses(st map[string]board.AgentStatus) tea.Cmd {
	if st == nil {
		return nil
	}
	changed := false
	var cmds []tea.Cmd
	for _, t := range m.b.Tickets {
		if t.AgentPod == "" {
			continue
		}
		s, ok := st[t.AgentPod]
		if !ok {
			s = board.AgentNone // pod gone
		}
		if s != t.AgentStatus {
			t.AgentStatus = s
			t.Touch()
			changed = true
			// A finished agent hands the ticket over for review, a failed
			// one blocks it.
			target := map[board.AgentStatus]board.Status{
				board.AgentCompleted: board.StatusReview, board.AgentError: board.StatusBlocked,
			}[s]
			if target != "" {
				cmds = append(cmds, m.captureCmd(t)) // keep the output with the ticket
			}
			if target != "" && t.Status == board.StatusInProgress {
				wasSelected := m.selected() == t
				if m.b.MoveTo(t, target) == nil {
					if wasSelected {
						m.selectTicket(t) // the cursor follows the card
					}
					m.notice = t.Title + " → " + m.b.Columns[m.b.ColumnIndex(t.Status)].Name + " (agent " + string(s) + ")"
				}
			}
		}
	}
	if changed {
		if err := m.store.Save(m.b); err != nil {
			m.err = "save failed: " + err.Error()
		}
	}
	return tea.Batch(cmds...)
}

// maxSavedOutput caps the agent output kept in the board file.
const maxSavedOutput = 64 << 10

// captureCmd fetches the finished agent's output to save it on the ticket.
func (m *model) captureCmd(t *board.Ticket) tea.Cmd {
	if m.opts.Agents == nil || t.AgentPod == "" {
		return nil
	}
	agents, ctx, id, pod := m.opts.Agents, m.ctx, t.ID, t.AgentPod
	return func() tea.Msg {
		text, err := agents.Logs(ctx, pod, 2000)
		return outputMsg{ticketID: id, text: text, err: err}
	}
}

func (m *model) spawn(t *board.Ticket) (tea.Model, tea.Cmd) {
	switch {
	case m.opts.Agents == nil:
		m.err = "agents unavailable: " + m.opts.AgentsErr
		return m, nil
	case t.Agent == "":
		m.err = "pick an agent first (a)"
		return m, nil
	case t.AgentStatus == board.AgentRunning || t.AgentStatus == board.AgentWaiting:
		m.err = "agent already running (x stops it)"
		return m, nil
	}
	if t.Status == board.StatusBacklog || t.Status == board.StatusBlocked {
		if err := m.b.MoveTo(t, board.StatusInProgress); err != nil {
			m.err = "cannot start: " + err.Error()
			return m, nil
		}
		m.selectTicket(t)
	}
	t.AgentStatus, t.AgentRun = board.AgentWaiting, ""
	m.save("starting " + string(t.Agent) + " for " + t.Title + "…")
	agents, ctx, tc := m.opts.Agents, m.ctx, *t
	return m, func() tea.Msg {
		pod, agent, err := agents.Spawn(ctx, &tc)
		return spawnedMsg{ticketID: tc.ID, pod: pod, agent: agent, err: err}
	}
}

// attach hands the terminal to the agent's session (or a shell) in its pod.
func (m *model) attach(t *board.Ticket, shell bool) (tea.Model, tea.Cmd) {
	switch {
	case m.opts.Agents == nil:
		m.err = "agents unavailable: " + m.opts.AgentsErr
		return m, nil
	case t.AgentPod == "":
		m.err = "no agent pod for this ticket (s starts one)"
		return m, nil
	case t.AgentStatus == board.AgentWaiting:
		m.err = "the agent pod is still starting"
		return m, nil
	}
	argv := m.opts.Agents.AttachCommand(t.AgentPod, t.EffectiveAgent(), shell)
	return m, tea.ExecProcess(exec.Command(argv[0], argv[1:]...), func(err error) tea.Msg {
		return attachDoneMsg{err}
	})
}

func (m *model) logsCmd(pod string) tea.Cmd {
	agents, ctx := m.opts.Agents, m.ctx
	return func() tea.Msg {
		text, err := agents.Logs(ctx, pod, 500)
		return logsMsg{pod: pod, text: text, err: err}
	}
}

func (m *model) stopCmd(t *board.Ticket) tea.Cmd {
	agents, ctx, id, pod := m.opts.Agents, m.ctx, t.ID, t.AgentPod
	return func() tea.Msg { return stoppedMsg{ticketID: id, err: agents.Stop(ctx, pod)} }
}

// selected returns the selected ticket, or nil in an empty column.
func (m *model) selected() *board.Ticket {
	ts := m.b.Column(m.b.Columns[m.col].Status)
	if len(ts) == 0 {
		return nil
	}
	m.row[m.col] = min(max(m.row[m.col], 0), len(ts)-1)
	return ts[m.row[m.col]]
}

// selectTicket moves the cursor onto t (after it changed column).
func (m *model) selectTicket(t *board.Ticket) {
	m.col = m.b.ColumnIndex(t.Status)
	for i, x := range m.b.Column(t.Status) {
		if x.ID == t.ID {
			m.row[m.col] = i
		}
	}
}

func (m *model) save(notice string) {
	if err := m.store.Save(m.b); err != nil {
		m.err = "save failed: " + err.Error()
		return
	}
	m.err, m.notice = "", notice
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.vp.SetWidth(m.width)
		m.vp.SetHeight(m.logsHeight())
		return m, nil
	case tea.MouseWheelMsg:
		if m.mode == modeLogs || m.mode == modeDetail {
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(msg)
			return m, cmd
		}
		return m, nil
	case pollMsg:
		return m, m.pollCmd()
	case statusMsg:
		capture := m.applyStatuses(msg.statuses)
		next := tea.Tick(pollInterval, func(time.Time) tea.Msg { return pollMsg{} })
		if m.mode == modeLogs {
			return m, tea.Batch(next, capture, m.logsCmd(m.logsPod)) // follow the output
		}
		return m, tea.Batch(next, capture)
	case outputMsg:
		t := m.findTicket(msg.ticketID)
		if t == nil || msg.err != nil {
			return m, nil // keep whatever was saved before
		}
		out := strings.TrimRight(msg.text, "\n")
		if len(out) > maxSavedOutput {
			out = "…\n" + out[len(out)-maxSavedOutput:]
		}
		t.AgentOutput = out
		if err := m.store.Save(m.b); err != nil {
			m.err = "save failed: " + err.Error()
		}
		if m.mode == modeDetail && m.selected() == t {
			m.openDetail()
		}
		return m, nil
	case spawnedMsg:
		t := m.findTicket(msg.ticketID)
		if t == nil {
			return m, nil
		}
		if msg.err != nil {
			t.AgentStatus = board.AgentError
			m.save("")
			m.err = "start failed: " + msg.err.Error()
			return m, nil
		}
		t.AgentPod, t.AgentRun = msg.pod, msg.agent
		m.save("started " + string(msg.agent) + " in " + msg.pod)
		return m, nil
	case logsMsg:
		if msg.pod != m.logsPod {
			return m, nil // output of a view already left
		}
		follow := !m.logsLoaded || m.vp.AtBottom()
		if msg.err != nil {
			m.vp.SetContent(errStyle.Render("logs: " + msg.err.Error()))
		} else {
			m.vp.SetContent(strings.TrimRight(msg.text, "\n"))
		}
		m.logsLoaded = true
		if follow {
			m.vp.GotoBottom()
		}
		return m, nil
	case attachDoneMsg:
		if msg.err != nil {
			m.err = "session: " + msg.err.Error()
		} else {
			m.notice = "back from the agent session"
		}
		return m, m.pollCmd()
	case stoppedMsg:
		if msg.err != nil {
			m.err = "stop failed: " + msg.err.Error()
			return m, nil
		}
		if t := m.findTicket(msg.ticketID); t != nil {
			t.AgentStatus, t.AgentPod = board.AgentNone, ""
			m.save("stopped agent of " + t.Title)
		}
		return m, nil
	case tea.KeyPressMsg:
		switch m.mode {
		case modeInput:
			return m.updateInput(msg)
		case modeConfirm:
			m.mode = modeBoard
			t := m.selected()
			if msg.String() != "y" || t == nil {
				m.notice = "cancelled"
				return m, nil
			}
			switch m.confirm {
			case confirmStop:
				return m, m.stopCmd(t)
			default: // confirmDelete
				var cmd tea.Cmd
				if t.AgentPod != "" && m.opts.Agents != nil {
					cmd = m.stopCmd(t)
				}
				m.b.Delete(t)
				m.save("deleted " + t.Title)
				return m, cmd
			}
		case modeLogs:
			switch msg.String() {
			case "ctrl+c":
				return m, tea.Quit
			case "q", "esc", "o":
				m.mode, m.logsPod = modeBoard, ""
				return m, nil
			case "r":
				return m, m.logsCmd(m.logsPod)
			case "g", "home":
				m.vp.GotoTop()
				return m, nil
			case "G", "end":
				m.vp.GotoBottom()
				return m, nil
			}
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(msg) // j/k, arrows, pgup/pgdn, space/b, u/d
			return m, cmd
		case modeDetail:
			switch msg.String() {
			case "ctrl+c":
				return m, tea.Quit
			case "e":
				return m.startInput(inputDescription)
			case "q", "esc", "enter":
				m.mode = modeBoard
				return m, nil
			case "g", "home":
				m.vp.GotoTop()
				return m, nil
			case "G", "end":
				m.vp.GotoBottom()
				return m, nil
			}
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(msg)
			return m, cmd
		case modeHelp:
			if msg.String() == "ctrl+c" {
				return m, tea.Quit
			}
			m.mode = modeBoard
			return m, nil
		}
		return m.updateBoard(msg)
	case tea.PasteMsg:
		if m.mode == modeInput {
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			return m, cmd
		}
	}
	return m, nil
}

func (m *model) updateBoard(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	m.notice, m.err = "", ""
	t := m.selected()
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "h", "left":
		m.col = max(m.col-1, 0)
	case "l", "right":
		m.col = min(m.col+1, len(m.b.Columns)-1)
	case "k", "up":
		m.row[m.col]--
	case "j", "down":
		m.row[m.col]++
	case "g", "home":
		m.row[m.col] = 0
	case "G", "end":
		m.row[m.col] = len(m.b.Column(m.b.Columns[m.col].Status)) - 1
	case "space", "L", "shift+right":
		m.move(t, 1)
	case "H", "shift+left", "backspace":
		m.move(t, -1)
	case "n":
		return m.startInput(inputNew)
	case "e":
		if t != nil {
			return m.startInput(inputTitle)
		}
	case "a":
		if t != nil {
			t.CycleAgent()
			m.save(fmt.Sprintf("%s: agent %s", t.Title, agentName(t.Agent)))
		}
	case "p":
		if t != nil {
			t.CyclePriority()
			m.save(fmt.Sprintf("%s: priority P%d", t.Title, t.Priority))
		}
	case "d", "delete":
		if t != nil {
			m.mode, m.confirm = modeConfirm, confirmDelete
		}
	case "enter":
		if t != nil {
			m.mode = modeDetail
			m.openDetail()
			// Finished before its output was saved (e.g. by an older version)?
			if t.AgentOutput == "" && (t.AgentStatus == board.AgentCompleted || t.AgentStatus == board.AgentError) {
				return m, m.captureCmd(t)
			}
		}
	case "s":
		if t != nil {
			return m.spawn(t)
		}
	case "o":
		if t != nil {
			if t.AgentPod == "" || m.opts.Agents == nil {
				m.err = "no agent pod for this ticket (s starts one)"
				return m, nil
			}
			m.vp = viewport.New(viewport.WithWidth(m.width), viewport.WithHeight(m.logsHeight()))
			m.vp.SoftWrap = true // long lines readable on narrow (phone) screens
			m.vp.SetContent(subtle.Render("loading…"))
			m.mode, m.logsPod, m.logsLoaded = modeLogs, t.AgentPod, false
			return m, m.logsCmd(t.AgentPod)
		}
	case "x":
		switch {
		case t == nil:
		case t.AgentPod == "" || m.opts.Agents == nil:
			m.err = "no agent pod for this ticket"
		default:
			m.mode, m.confirm = modeConfirm, confirmStop
		}
	case "t", "T":
		if t != nil {
			return m.attach(t, msg.String() == "T")
		}
	case "?":
		m.mode = modeHelp
	}
	m.selected() // clamp the row
	return m, nil
}

func (m *model) move(t *board.Ticket, delta int) {
	if t == nil {
		return
	}
	if err := m.b.Move(t, delta); err != nil {
		m.err = err.Error()
		return
	}
	m.selectTicket(t)
	m.save(fmt.Sprintf("%s → %s", t.Title, m.b.Columns[m.col].Name))
}

func (m *model) startInput(k inputKind) (tea.Model, tea.Cmd) {
	in := textinput.New()
	in.SetWidth(max(m.width-20, 20))
	styles := in.Styles()
	styles.Cursor.Blink = false
	in.SetStyles(styles)
	switch k {
	case inputNew:
		in.Prompt, in.Placeholder = "New ticket: ", "title"
	case inputTitle:
		in.Prompt = "Title: "
		in.SetValue(m.selected().Title)
	case inputDescription:
		in.Prompt, in.Placeholder = "Description: ", "what the agent should do"
		in.SetValue(m.selected().Description)
	}
	focus := in.Focus()
	m.input, m.inputKind, m.mode = in, k, modeInput
	return m, focus
}

func (m *model) updateInput(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeBoard
		return m, nil
	case "ctrl+c":
		return m, tea.Quit
	case "enter":
		v := strings.TrimSpace(m.input.Value())
		m.mode = modeBoard
		switch m.inputKind {
		case inputNew:
			if v != "" {
				t := m.b.Add(v)
				m.selectTicket(t)
				m.save("created " + v)
			}
		case inputTitle:
			if t := m.selected(); t != nil && v != "" {
				t.Title = v
				t.Touch()
				m.save("renamed to " + v)
			}
		case inputDescription:
			if t := m.selected(); t != nil {
				t.Description = v
				t.Touch()
				m.save("description updated")
				m.mode = modeDetail
				m.openDetail()
			}
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// ---- view ----

var (
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	subtle      = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	okStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	dimBorder   = lipgloss.Color("240")
	agentColors = map[board.AgentType]string{
		"auto": "#b4befe", "claude": "#fab387", "codex": "#94e2d5", "copilot": "#89dceb", "antigravity": "#f5c2e7",
	}
)

const cardLines = 5 // border (2) + title + meta + branch

func (m *model) View() tea.View {
	head := titleStyle.Render("kAinban")
	if m.opts.AppVersion != "" {
		head += " " + subtle.Render(m.opts.AppVersion)
	}
	head += titleStyle.Render(" · board")
	if m.opts.Location != "" {
		head += subtle.Render("  " + m.opts.Location)
	}

	// Messages, questions and inputs go to the top so the key bar at the
	// bottom always stays visible.
	status, footer := m.statusLine(), m.footer()
	wrap := lipgloss.NewStyle().Width(max(m.width, 20))
	if status != "" {
		status = wrap.Render(status)
	}
	footer = wrap.Render(footer)
	bodyH := max(m.height-3-lineCount(status)-lineCount(footer), 5)

	var body string
	switch m.mode {
	case modeDetail:
		m.vp.SetHeight(bodyH)
		body = m.vp.View()
	case modeHelp:
		body = helpText
	case modeLogs:
		m.vp.SetHeight(max(bodyH-2, 3))
		body = m.logsView()
	default:
		body = m.columnsView(bodyH)
	}

	var b strings.Builder
	b.WriteString(head + "\n" + status + "\n\n")
	b.WriteString(body)
	if pad := bodyH - lineCount(body); pad > 0 {
		b.WriteString(strings.Repeat("\n", pad))
	}
	b.WriteString("\n" + footer)
	v := tea.NewView(b.String())
	v.AltScreen = true
	if m.mode == modeLogs || m.mode == modeDetail {
		// Wheel events; Termux turns touch swipes into them.
		v.MouseMode = tea.MouseModeCellMotion
	}
	return v
}

func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(strings.TrimRight(s, "\n"), "\n") + 1
}

// statusLine is the line under the header: an input, a question, or the
// last message.
func (m *model) statusLine() string {
	switch {
	case m.mode == modeInput:
		return m.input.View()
	case m.mode == modeConfirm:
		return errStyle.Render(m.confirmQuestion())
	case m.err != "":
		return errStyle.Render(m.err)
	case m.notice != "":
		return okStyle.Render(m.notice)
	}
	return ""
}

func (m *model) confirmQuestion() string {
	t := m.selected()
	if t == nil {
		return ""
	}
	if m.confirm == confirmStop {
		return fmt.Sprintf("stop the agent of %q? Its pod, /work and session are deleted. y/N", t.Title)
	}
	q := fmt.Sprintf("delete %q?", t.Title)
	if t.AgentPod != "" {
		q += " Its agent pod is stopped too."
	}
	return q + " y/N"
}

// footer is the key bar for the current mode.
func (m *model) footer() string {
	switch m.mode {
	case modeInput:
		return subtle.Render("enter save · esc cancel")
	case modeConfirm:
		return subtle.Render("y confirm · any other key cancel")
	case modeDetail:
		return subtle.Render(fmt.Sprintf("%3.0f%% · j/k pgup/pgdn scroll (touch/wheel too) · g/G top/end · e edit description · q back", m.vp.ScrollPercent()*100))
	case modeHelp:
		return subtle.Render("any key back")
	case modeLogs:
		return subtle.Render(fmt.Sprintf("%3.0f%% · j/k ↑/↓ pgup/pgdn scroll (touch/wheel too) · g/G top/end · r refresh · q back", m.vp.ScrollPercent()*100))
	}
	return subtle.Render("h/l j/k move · space/H/L move · n new · e edit · a agent · s start · t session · o output · x stop · p prio · d del · enter details · ? help · q quit")
}

func (m *model) columnsView(height int) string {
	n := len(m.b.Columns)
	// On narrow (phone) screens show only the columns that fit, around the
	// selected one.
	const minColW = 18
	shown := min(max((m.width+1)/(minColW+1), 1), n)
	first := min(max(m.col-shown/2, 0), n-shown)
	colW := max((m.width-(shown-1))/shown, minColW)
	visible := max((height-3)/cardLines, 1) // column titles, scroll markers

	cols := make([]string, 0, shown)
	for i := first; i < first+shown; i++ {
		c := m.b.Columns[i]
		ts := m.b.Column(c.Status)
		count := fmt.Sprint(len(ts))
		if c.Limit > 0 {
			count = fmt.Sprintf("%d/%d", len(ts), c.Limit)
		}
		hdr := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(c.Color)).Render(c.Name) +
			subtle.Render(" "+count)
		if i == m.col {
			hdr = lipgloss.NewStyle().Foreground(lipgloss.Color(c.Color)).Render("▶ ") + hdr
		} else {
			hdr = "  " + hdr
		}
		lines := []string{hdr, ""}

		// Keep the selected card in view.
		sel := m.row[i]
		if sel < m.scroll[i] {
			m.scroll[i] = sel
		}
		if sel >= m.scroll[i]+visible {
			m.scroll[i] = sel - visible + 1
		}
		m.scroll[i] = min(max(m.scroll[i], 0), max(len(ts)-visible, 0))
		if m.scroll[i] > 0 {
			lines = append(lines, subtle.Render(fmt.Sprintf("  ↑ %d more", m.scroll[i])))
		}
		end := min(m.scroll[i]+visible, len(ts))
		for j := m.scroll[i]; j < end; j++ {
			lines = append(lines, m.card(ts[j], c, colW, i == m.col && j == sel))
		}
		if end < len(ts) {
			lines = append(lines, subtle.Render(fmt.Sprintf("  ↓ %d more", len(ts)-end)))
		}
		if len(ts) == 0 {
			lines = append(lines, subtle.Render("  (empty)"))
		}
		cols = append(cols, lipgloss.NewStyle().Width(colW).Render(strings.Join(lines, "\n")))
	}
	board := lipgloss.JoinHorizontal(lipgloss.Top, joinWithGap(cols)...)
	if shown == n {
		return board
	}
	hint := ""
	if first > 0 {
		hint += fmt.Sprintf("‹ %d more ", first)
	}
	if rest := n - first - shown; rest > 0 {
		hint += fmt.Sprintf(" %d more ›", rest)
	}
	return board + "\n" + subtle.Render(strings.TrimSpace(hint)+"  (h/l)")
}

func joinWithGap(cols []string) []string {
	out := make([]string, 0, 2*len(cols))
	for i, c := range cols {
		if i > 0 {
			out = append(out, " ")
		}
		out = append(out, c)
	}
	return out
}

func (m *model) card(t *board.Ticket, c board.Column, w int, selected bool) string {
	inner := max(w-4, 8) // border + padding
	title := ansi.Truncate(t.Title, inner, "…")
	border := dimBorder
	if selected {
		border = lipgloss.Color(c.Color)
		title = lipgloss.NewStyle().Bold(true).Render(title)
	}
	meta := priorityStyle(t.Priority).Render(fmt.Sprintf("P%d", t.Priority)) + " " + agentBadge(t)
	branch := subtle.Render(ansi.Truncate(orDash(t.Branch), inner, "…"))
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(border).
		Padding(0, 1).Width(w).
		Render(title + "\n" + ansi.Truncate(meta, inner, "…") + "\n" + branch)
}

func agentBadge(t *board.Ticket) string {
	if t.Agent == "" {
		return subtle.Render("no agent")
	}
	name := string(t.Agent)
	if t.Agent == board.AgentAuto && t.AgentRun != "" {
		name = "auto→" + string(t.AgentRun)
	}
	s := lipgloss.NewStyle().Foreground(lipgloss.Color(agentColors[t.EffectiveAgent()])).Render(name)
	switch t.AgentStatus {
	case board.AgentRunning:
		s += " " + okStyle.Render("●")
	case board.AgentWaiting:
		s += " " + lipgloss.NewStyle().Foreground(lipgloss.Color("11")).Render("◐")
	case board.AgentCompleted:
		s += " " + okStyle.Render("✓")
	case board.AgentError:
		s += " " + errStyle.Render("✗")
	}
	return s
}

func priorityStyle(p int) lipgloss.Style {
	colors := map[int]string{1: "9", 2: "11", 3: "12", 4: "8"}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(colors[p]))
}

func agentName(a board.AgentType) string {
	if a == "" {
		return "none"
	}
	return string(a)
}

func agentRunSuffix(t *board.Ticket) string {
	if t.Agent == board.AgentAuto && t.AgentRun != "" {
		return " → " + string(t.AgentRun)
	}
	return ""
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// openDetail fills the viewport with the selected ticket's details and its
// saved agent output.
func (m *model) openDetail() {
	m.vp = viewport.New(viewport.WithWidth(m.width), viewport.WithHeight(m.logsHeight()))
	m.vp.SoftWrap = true
	m.vp.SetContent(m.detailText())
}

func (m *model) detailText() string {
	t := m.selected()
	if t == nil {
		return ""
	}
	col := m.b.Columns[m.b.ColumnIndex(t.Status)]
	rows := [][2]string{
		{"Title", t.Title},
		{"Status", col.Name},
		{"Priority", fmt.Sprintf("P%d", t.Priority)},
		{"Agent", agentName(t.Agent) + agentRunSuffix(t) + " (" + string(t.AgentStatus) + ")"},
		{"Branch", orDash(t.Branch)},
		{"Pod", orDash(t.AgentPod)},
		{"ID", t.ID},
		{"Created", t.CreatedAt.Local().Format("2006-01-02 15:04")},
		{"Updated", t.UpdatedAt.Local().Format("2006-01-02 15:04")},
	}
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(subtle.Render(fmt.Sprintf("%-10s", r[0])) + r[1] + "\n")
	}
	desc := t.Description
	if desc == "" {
		desc = subtle.Render("(no description — press e to add one)")
	}
	b.WriteString("\n" + desc + "\n")

	switch {
	case t.AgentOutput != "":
		b.WriteString("\n" + titleStyle.Render("Agent output") + "\n" + t.AgentOutput + "\n")
	case t.AgentStatus == board.AgentRunning || t.AgentStatus == board.AgentWaiting:
		b.WriteString("\n" + subtle.Render("Agent is running — its output is saved here when it finishes (o shows it live).") + "\n")
	case t.AgentStatus == board.AgentCompleted || t.AgentStatus == board.AgentError:
		b.WriteString("\n" + subtle.Render("Fetching the agent output…") + "\n")
	}
	return b.String()
}

const helpText = `Navigation     h/l ←/→ columns · j/k ↑/↓ cards · g/G first/last
Move card      space or L next column · H or backspace previous column
Tickets        n new · e edit title · enter details (e there edits the description)
               a cycle agent (auto, claude, codex, copilot, antigravity, none);
                 auto lets the orchestrator pick when the agent starts
               p cycle priority (P1 highest … P4) · d delete
Agents         s start the ticket's agent in a pod (moves it to In Progress)
               t open the agent's session in its pod (claude --continue, codex resume,
                 ...) to ask for changes; exit it to return here · T plain shell there
               o show the agent's output (scroll with j/k, pgup/pgdn, wheel or touch)
               x stop the agent · a finished agent moves its ticket to Review,
                 a failed one to Blocked (space/s there retries in In Progress)
Other          ? this help · q quit

In Progress has a WIP limit of 3; moving a 4th card there is refused.
Blocked is a side column: moving skips it, moving out of it goes to In Progress.
`

func (m *model) logsHeight() int { return max(m.height-8, 5) }

// logsView shows the scrollable agent output; it follows new output while
// scrolled to the end.
func (m *model) logsView() string {
	return subtle.Render("output of "+m.logsPod) + "\n\n" + m.vp.View() + "\n"
}
