// Package boardui is the interactive kanban board: columns side by side,
// tickets as cards, keyboard-driven like OpenKanban.
package boardui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/zerosuxx/kainban/internal/board"
)

// Options configures Run.
type Options struct {
	Project    string // ticket key prefix for a new board (e.g. KAI)
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
	if err := applyProject(b, store, opts.Project); err != nil {
		return err
	}
	m := newModel(b, store, opts)
	m.ctx = ctx
	m.animate = true
	_, err = tea.NewProgram(m, tea.WithContext(ctx)).Run()
	return err
}

// applyProject sets the ticket key prefix of a board that has no tickets
// yet; an existing board keeps its prefix so keys stay consistent.
func applyProject(b *board.Board, store board.Store, project string) error {
	p := board.NormalizeProject(project)
	if p == "" || p == b.Project {
		return nil
	}
	if len(b.Tickets) > 0 || b.NextNumber > 1 {
		return fmt.Errorf("the board already uses project %s (tickets %s-N); --project only applies to a new board", b.Project, b.Project)
	}
	b.Project = p
	return store.Save(b)
}

type mode int

const (
	modeBoard   mode = iota
	modeForm         // edit popup (all fields) / new ticket
	modeConfirm      // y/N question, see confirmKind
	modeDetail
	modeHelp
	modeLogs
)

type (
	pollMsg struct{}
	logsMsg struct {
		pod, text string
		err       error
	}
	attachDoneMsg struct{ err error }
)

type confirmKind int

const (
	confirmDelete confirmKind = iota
	confirmStop
	confirmRestart
	confirmDone
	confirmOrphans
)

type model struct {
	ctx   context.Context
	b     *board.Board
	store board.Store
	opts  Options

	col    int   // selected column
	row    []int // selected row per column
	scroll []int // first visible card per column

	mode    mode
	confirm confirmKind
	form    *editForm

	notice string
	err    string

	orphans []string // agent pods no ticket run refers to

	animate     bool   // marquee on (off in tests: no tickers)
	marqueeOn   bool   // a marquee tick is scheduled
	marqueeID   string // ticket whose title is scrolling
	marqueeStep int

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
	if _, ok := msg.(marqueeMsg); ok {
		m.marqueeOn = false
		if !m.needsMarquee() {
			return m, nil
		}
		m.marqueeStep++
	}
	model, cmd := m.update(msg)
	// Start the marquee whenever the selected title stops fitting.
	if !m.marqueeOn && m.needsMarquee() {
		m.marqueeOn = true
		cmd = tea.Batch(cmd, tea.Tick(marqueeInterval, func(time.Time) tea.Msg { return marqueeMsg{} }))
	}
	return model, cmd
}

func (m *model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
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
		capture := m.applyStatuses(msg.states)
		next := tea.Tick(pollInterval, func(time.Time) tea.Msg { return pollMsg{} })
		if m.mode == modeLogs && m.logsPod != "" {
			return m, tea.Batch(next, capture, m.logsCmd(m.logsPod)) // follow the output
		}
		return m, tea.Batch(next, capture)
	case outputMsg:
		m.applyOutput(msg)
		return m, nil
	case spawnedMsg:
		m.applySpawned(msg)
		return m, nil
	case stoppedMsg:
		m.applyStopped(msg)
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
	case tea.KeyPressMsg:
		switch m.mode {
		case modeForm:
			return m.updateForm(msg)
		case modeConfirm:
			m.mode = modeBoard
			t := m.selected()
			if msg.String() != "y" || t == nil {
				m.notice = "cancelled"
				return m, nil
			}
			return m.confirmed(t)
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
				return m.openForm(m.selected(), fieldDescription)
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
		if m.mode == modeForm {
			return m.updateForm(msg)
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
		return m, m.move(t, 1)
	case "H", "shift+left", "backspace":
		return m, m.move(t, -1)
	case "n":
		return m.openForm(nil, fieldTitle)
	case "e":
		if t != nil {
			return m.openForm(t, fieldTitle)
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
			// Finished runs whose output was not saved yet (older versions).
			var cmds []tea.Cmd
			for _, r := range t.Runs {
				if r.Output == "" && r.Status.Finished() && r.Live() {
					cmds = append(cmds, m.captureCmd(t, r))
				}
			}
			return m, tea.Batch(cmds...)
		}
	case "s":
		if t != nil {
			return m.spawn(t, false)
		}
	case "o":
		if t != nil {
			return m.openLogs(t)
		}
	case "x":
		switch {
		case t == nil:
		case len(t.LiveRuns()) == 0 || m.opts.Agents == nil:
			m.err = "no agent pod for this ticket"
		default:
			m.mode, m.confirm = modeConfirm, confirmStop
		}
	case "C":
		if len(m.orphans) > 0 && m.opts.Agents != nil {
			m.mode, m.confirm = modeConfirm, confirmOrphans
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

func (m *model) move(t *board.Ticket, delta int) tea.Cmd {
	if t == nil {
		return nil
	}
	from := t.Status
	if err := m.b.Move(t, delta); err != nil {
		m.err = err.Error()
		return nil
	}
	if t.Status == board.StatusDone && from != board.StatusDone && len(t.LiveRuns()) > 0 {
		if t.RunStatus().Active() {
			t.Status = from // ask first: the running agent's work would be lost
			m.mode, m.confirm = modeConfirm, confirmDone
			return nil
		}
		m.selectTicket(t)
		m.save(fmt.Sprintf("%s → Done, stopping its agent pods", t.Title))
		return m.stopRunsCmd(t, t.LiveRuns())
	}
	m.selectTicket(t)
	m.save(fmt.Sprintf("%s → %s", t.Title, m.b.Columns[m.col].Name))
	return nil
}

func (m *model) openForm(t *board.Ticket, focus formField) (tea.Model, tea.Cmd) {
	m.form = newEditForm(t, m.width, focus)
	m.mode = modeForm
	return m, m.form.focusCmd()
}

// updateForm feeds keys and pastes to the edit popup and saves on ctrl+s.
func (m *model) updateForm(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyPressMsg); ok && k.String() == "ctrl+c" {
		return m, tea.Quit
	}
	res, cmd := m.form.update(msg)
	switch res {
	case formCancel:
		m.mode, m.form, m.notice = modeBoard, nil, "cancelled"
	case formSave:
		f := m.form
		m.mode, m.form = modeBoard, nil
		if f.ticketID == "" {
			t := m.b.Add(strings.TrimSpace(f.title.Value()))
			f.apply(t)
			m.selectTicket(t)
			m.save("created " + t.Key + " " + t.Title)
		} else if t := m.findTicket(f.ticketID); t != nil {
			f.apply(t)
			m.save("saved " + t.Key)
		}
	}
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
	head += titleStyle.Render(" · board " + m.b.Project)
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
	screen := b.String()
	switch {
	case m.mode == modeForm && m.form != nil:
		screen = m.overlay(screen, m.form.view(m.width))
	case m.mode == modeConfirm:
		screen = m.overlay(screen, m.confirmBox())
	}
	v := tea.NewView(screen)
	v.AltScreen = true
	if m.mode == modeLogs || m.mode == modeDetail {
		// Wheel events; Termux turns touch swipes into them.
		v.MouseMode = tea.MouseModeCellMotion
	}
	return v
}

// overlay draws a popup centered over the screen.
func (m *model) overlay(screen, box string) string {
	x := max((m.width-lipgloss.Width(box))/2, 0)
	y := max((m.height-lipgloss.Height(box))/2, 1)
	return lipgloss.NewCompositor(
		lipgloss.NewLayer(screen),
		lipgloss.NewLayer(box).X(x).Y(y).Z(1),
	).Render()
}

// confirmBox is the y/N question as a popup.
func (m *model) confirmBox() string {
	w := min(60, max(m.width-4, 30))
	q := strings.TrimSuffix(m.confirmQuestion(), " y/N")
	keys := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("9")).Render("y") + subtle.Render(" yes   ") +
		lipgloss.NewStyle().Bold(true).Render("n") + subtle.Render("/esc no")
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("9")).
		Padding(1, 2).Width(w).
		Render(titleStyle.Render("Confirm") + "\n\n" + q + "\n\n" + keys)
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
	case m.err != "":
		return errStyle.Render(m.err)
	case m.notice != "":
		return okStyle.Render(m.notice)
	}
	return ""
}

func (m *model) confirmQuestion() string {
	if m.confirm == confirmOrphans {
		return fmt.Sprintf("stop %d agent pod(s) that belong to no ticket run? y/N", len(m.orphans))
	}
	t := m.selected()
	if t == nil {
		return ""
	}
	switch m.confirm {
	case confirmStop:
		return fmt.Sprintf("stop the agent of %q? Its pod, /work and session are deleted (the output is kept). y/N", t.Title)
	case confirmRestart:
		return fmt.Sprintf("the agent of %q is still running. Stop it and start a new run? y/N", t.Title)
	case confirmDone:
		return fmt.Sprintf("the agent of %q is still running. Move to Done and stop it? y/N", t.Title)
	}
	q := fmt.Sprintf("delete %q?", t.Title)
	if len(t.LiveRuns()) > 0 {
		q += " Its agent pods are stopped too."
	}
	return q + " y/N"
}

// footer is the key bar for the current mode.
func (m *model) footer() string {
	switch m.mode {
	case modeForm:
		return subtle.Render("tab/shift+tab field · ←/→ change priority/agent · enter next field (newline in description) · ctrl+s save · esc cancel")
	case modeConfirm:
		return subtle.Render("y confirm · n/esc or any other key cancel")
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
	shown, first, colW := m.columnLayout()
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

// columnLayout picks the columns that fit (on narrow phone screens only
// those around the selected one) and their width.
func (m *model) columnLayout() (shown, first, colW int) {
	const minColW = 18
	n := len(m.b.Columns)
	shown = min(max((m.width+1)/(minColW+1), 1), n)
	first = min(max(m.col-shown/2, 0), n-shown)
	colW = max((m.width-(shown-1))/shown, minColW)
	return shown, first, colW
}

func cardTitle(t *board.Ticket) string {
	if t.Key != "" {
		return t.Key + " " + t.Title
	}
	return t.Title
}

func cardInner(colW int) int { return max(colW-4, 8) } // border + padding

// ---- marquee: the selected card's title scrolls when it does not fit ----

type marqueeMsg struct{}

const (
	marqueeInterval = 200 * time.Millisecond
	marqueePause    = 6 // ticks to show the start before scrolling
	marqueeGap      = "   ·   "
)

// needsMarquee reports whether the selected card's title is cut off.
func (m *model) needsMarquee() bool {
	if !m.animate || m.mode != modeBoard {
		return false
	}
	t := m.selected()
	if t == nil {
		return false
	}
	_, _, colW := m.columnLayout()
	return ansi.StringWidth(cardTitle(t)) > cardInner(colW)
}

// marqueeText is the visible window of a scrolling title.
func marqueeText(s string, width, step int) string {
	loop := []rune(s + marqueeGap)
	off := max(step-marqueePause, 0) % len(loop)
	rotated := string(append(loop[off:], loop[:off]...))
	return ansi.Truncate(rotated, width, "")
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
	inner := cardInner(w)
	title := cardTitle(t)
	if selected && m.animate && ansi.StringWidth(title) > inner {
		if m.marqueeID != t.ID { // a new selection starts from the beginning
			m.marqueeID, m.marqueeStep = t.ID, 0
		}
		title = marqueeText(title, inner, m.marqueeStep)
	} else {
		title = ansi.Truncate(title, inner, "…")
	}
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
	if t.Agent == "" && t.Current() == nil {
		return subtle.Render("no agent")
	}
	name := string(t.Agent)
	if r := t.Current(); r != nil && t.Agent == board.AgentAuto && r.Agent != board.AgentAuto {
		name = "auto→" + string(r.Agent)
	}
	s := lipgloss.NewStyle().Foreground(lipgloss.Color(agentColors[t.EffectiveAgent()])).Render(name)
	s += " " + statusIcon(t.RunStatus())
	if n := len(t.Runs); n > 1 {
		s += subtle.Render(fmt.Sprintf(" #%d", n))
	}
	return strings.TrimRight(s, " ")
}

func statusIcon(s board.AgentStatus) string {
	switch s {
	case board.AgentRunning:
		return okStyle.Render("●")
	case board.AgentWaiting:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("11")).Render("◐")
	case board.AgentCompleted:
		return okStyle.Render("✓")
	case board.AgentError:
		return errStyle.Render("✗")
	case board.AgentStopped:
		return subtle.Render("■")
	}
	return ""
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
		{"Key", orDash(t.Key)},
		{"Title", t.Title},
		{"Status", col.Name},
		{"Priority", fmt.Sprintf("P%d", t.Priority)},
		{"Agent", agentName(t.Agent)},
		{"Branch", orDash(t.Branch)},
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

	// Runs, newest first, each with its saved output.
	for i := len(t.Runs) - 1; i >= 0; i-- {
		r := t.Runs[i]
		head := fmt.Sprintf("Run #%d · %s · %s %s · %s", i+1, r.Agent, statusIcon(r.Status), r.Status,
			r.StartedAt.Local().Format("01-02 15:04"))
		if r.FinishedAt != nil {
			head += " → " + r.FinishedAt.Local().Format("15:04")
		}
		pod := r.Pod
		switch {
		case pod == "":
			pod = "—"
		case r.PodGone:
			pod += " (deleted)"
		}
		b.WriteString("\n" + titleStyle.Render(head) + "\n" + subtle.Render("pod "+pod) + "\n")
		switch {
		case r.Output != "":
			b.WriteString(r.Output + "\n")
		case r.Status.Active():
			b.WriteString(subtle.Render("running — the output is saved here when it ends (o shows it live)") + "\n")
		case r.Live():
			b.WriteString(subtle.Render("fetching the output…") + "\n")
		default:
			b.WriteString(subtle.Render("(no output saved)") + "\n")
		}
	}
	return b.String()
}

const helpText = `Navigation     h/l ←/→ columns · j/k ↑/↓ cards · g/G first/last
Move card      space or L next column · H or backspace previous column
Tickets        n new · e edit (popup with every field) · enter details
               a cycle agent (auto, claude, codex, copilot, antigravity, none);
                 auto lets the orchestrator pick when the agent starts
               p cycle priority (P1 highest … P4) · d delete
Agents         s start the ticket's agent in a pod (moves it to In Progress)
               t open the agent's session in its pod (claude --continue, codex resume,
                 ...) to ask for changes; exit it to return here · T plain shell there
               o show the agent's output (scroll with j/k, pgup/pgdn, wheel or touch)
               x stop the agent (all its pods) · C stop orphaned agent pods
               a finished agent moves its ticket to Review,
                 a failed one to Blocked (space/s there retries in In Progress)
Other          ? this help · q quit

In Progress has a WIP limit of 3; moving a 4th card there is refused.
Blocked is a side column: moving skips it, moving out of it goes to In Progress.
Moving a ticket to Done stops its agent pods (asks if one is still running);
s on a ticket with a previous run stops that pod first. Every run and its
output stay listed in the details.
`

func (m *model) logsHeight() int { return max(m.height-8, 5) }

// logsView shows the scrollable agent output; it follows new output while
// scrolled to the end.
func (m *model) logsView() string {
	src := "saved output (pod deleted)"
	if m.logsPod != "" {
		src = "output of " + m.logsPod
	}
	return subtle.Render(src) + "\n\n" + m.vp.View() + "\n"
}
