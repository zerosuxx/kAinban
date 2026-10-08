// Package boardui is the interactive kanban board: columns side by side,
// tickets as cards, keyboard-driven like OpenKanban.
package boardui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/zerosuxx/kainban/internal/board"
)

// AgentRunner starts and watches agent pods (internal/agent.Runner).
type AgentRunner interface {
	Spawn(ctx context.Context, t *board.Ticket) (string, error)
	Statuses(ctx context.Context) (map[string]board.AgentStatus, error)
	Logs(ctx context.Context, pod string, tail int64) (string, error)
	Stop(ctx context.Context, pod string) error
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
	modeConfirm      // delete?
	modeDetail
	modeHelp
	modeLogs
)

type (
	pollMsg    struct{}
	statusMsg  struct{ statuses map[string]board.AgentStatus }
	spawnedMsg struct {
		ticketID, pod string
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
	inputKind inputKind
	input     textinput.Model

	notice string
	err    string

	logs    string // modeLogs content
	logsPod string

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
func (m *model) applyStatuses(st map[string]board.AgentStatus) {
	if st == nil {
		return
	}
	changed := false
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
		}
	}
	if changed {
		if err := m.store.Save(m.b); err != nil {
			m.err = "save failed: " + err.Error()
		}
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
	if t.Status == board.StatusBacklog {
		if err := m.b.Move(t, 1); err != nil {
			m.err = "cannot start: " + err.Error()
			return m, nil
		}
		m.selectTicket(t)
	}
	t.AgentStatus = board.AgentWaiting
	m.save("starting " + string(t.Agent) + " for " + t.Title + "…")
	agents, ctx, tc := m.opts.Agents, m.ctx, *t
	return m, func() tea.Msg {
		pod, err := agents.Spawn(ctx, &tc)
		return spawnedMsg{ticketID: tc.ID, pod: pod, err: err}
	}
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
		return m, nil
	case pollMsg:
		return m, m.pollCmd()
	case statusMsg:
		m.applyStatuses(msg.statuses)
		return m, tea.Tick(pollInterval, func(time.Time) tea.Msg { return pollMsg{} })
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
		t.AgentPod = msg.pod
		m.save("started " + msg.pod)
		return m, nil
	case logsMsg:
		if msg.err != nil {
			m.logs = errStyle.Render("logs: " + msg.err.Error())
		} else {
			m.logs = msg.text
		}
		m.logsPod = msg.pod
		return m, nil
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
			if msg.String() == "y" {
				if t := m.selected(); t != nil {
					var cmd tea.Cmd
					if t.AgentPod != "" && m.opts.Agents != nil {
						cmd = m.stopCmd(t)
					}
					m.b.Delete(t)
					m.save("deleted " + t.Title)
					m.mode = modeBoard
					return m, cmd
				}
			}
			m.mode = modeBoard
			return m, nil
		case modeLogs:
			switch msg.String() {
			case "ctrl+c":
				return m, tea.Quit
			case "r":
				return m, m.logsCmd(m.logsPod)
			}
			m.mode = modeBoard
			return m, nil
		case modeDetail, modeHelp:
			switch msg.String() {
			case "ctrl+c":
				return m, tea.Quit
			case "e":
				if m.mode == modeDetail {
					return m.startInput(inputDescription)
				}
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
			m.mode = modeConfirm
		}
	case "enter":
		if t != nil {
			m.mode = modeDetail
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
			m.mode, m.logs, m.logsPod = modeLogs, "loading…", t.AgentPod
			return m, m.logsCmd(t.AgentPod)
		}
	case "x":
		if t != nil && t.AgentPod != "" && m.opts.Agents != nil {
			return m, m.stopCmd(t)
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
		"claude": "#fab387", "codex": "#94e2d5", "copilot": "#89dceb", "antigravity": "#f5c2e7",
	}
)

const cardLines = 5 // border (2) + title + meta + branch

func (m *model) View() tea.View {
	var b strings.Builder
	head := titleStyle.Render("kAinban")
	if m.opts.AppVersion != "" {
		head += " " + subtle.Render(m.opts.AppVersion)
	}
	head += titleStyle.Render(" · board")
	if m.opts.Location != "" {
		head += subtle.Render("  " + m.opts.Location)
	}
	b.WriteString(head + "\n\n")

	switch m.mode {
	case modeDetail:
		b.WriteString(m.detailView())
	case modeHelp:
		b.WriteString(helpText)
	case modeLogs:
		b.WriteString(m.logsView())
	default:
		b.WriteString(m.columnsView())
	}

	b.WriteString("\n")
	switch {
	case m.mode == modeInput:
		b.WriteString(m.input.View() + "\n" + subtle.Render("enter save · esc cancel"))
	case m.mode == modeConfirm:
		b.WriteString(errStyle.Render(fmt.Sprintf("delete %q? y/N", m.selected().Title)))
	case m.err != "":
		b.WriteString(errStyle.Render(m.err))
	case m.notice != "":
		b.WriteString(okStyle.Render(m.notice))
	case m.mode == modeDetail:
		b.WriteString(subtle.Render("e edit description · any key back"))
	case m.mode == modeHelp:
		b.WriteString(subtle.Render("any key back"))
	case m.mode == modeLogs:
		b.WriteString(subtle.Render("r refresh · any key back"))
	default:
		b.WriteString(subtle.Render("h/l j/k move · space/H/L move · n new · e edit · a agent · s start · o output · x stop · p prio · d delete · enter details · ? help · q quit"))
	}
	v := tea.NewView(b.String())
	v.AltScreen = true
	return v
}

func (m *model) columnsView() string {
	n := len(m.b.Columns)
	colW := max((m.width-(n-1))/n, 18)
	visible := max((m.height-8)/cardLines, 1) // header, column titles, footer

	cols := make([]string, n)
	for i, c := range m.b.Columns {
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
		cols[i] = lipgloss.NewStyle().Width(colW).Render(strings.Join(lines, "\n"))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, joinWithGap(cols)...)
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
	s := lipgloss.NewStyle().Foreground(lipgloss.Color(agentColors[t.Agent])).Render(string(t.Agent))
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

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func (m *model) detailView() string {
	t := m.selected()
	if t == nil {
		return ""
	}
	col := m.b.Columns[m.b.ColumnIndex(t.Status)]
	rows := [][2]string{
		{"Title", t.Title},
		{"Status", col.Name},
		{"Priority", fmt.Sprintf("P%d", t.Priority)},
		{"Agent", agentName(t.Agent) + " (" + string(t.AgentStatus) + ")"},
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
	b.WriteString("\n" + lipgloss.NewStyle().Width(max(m.width-2, 20)).Render(desc) + "\n")
	return b.String()
}

const helpText = `Navigation     h/l ←/→ columns · j/k ↑/↓ cards · g/G first/last
Move card      space or L next column · H or backspace previous column
Tickets        n new · e edit title · enter details (e there edits the description)
               a cycle agent (claude, codex, copilot, antigravity, none)
               p cycle priority (P1 highest … P4) · d delete
Agents         s start the ticket's agent in a pod (moves it to In Progress)
               o show the agent's output · x stop the agent
Other          ? this help · q quit

In Progress has a WIP limit of 3; moving a 4th card there is refused.
`

// logsView shows the tail of the agent output that fits the screen.
func (m *model) logsView() string {
	lines := strings.Split(strings.TrimRight(m.logs, "\n"), "\n")
	room := max(m.height-6, 5)
	if len(lines) > room {
		lines = lines[len(lines)-room:]
	}
	w := max(m.width-1, 20)
	for i, l := range lines {
		lines[i] = ansi.Truncate(l, w, "…")
	}
	return subtle.Render("output of "+m.logsPod) + "\n\n" + strings.Join(lines, "\n") + "\n"
}
