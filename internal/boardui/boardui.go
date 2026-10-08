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

// logsInterval is how often `o` refreshes a live pod's output (a var for the
// tests).
var logsInterval = 2 * time.Second

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
	logsTickMsg   struct{ seq int } // refresh the open output view
	attachDoneMsg struct{ err error }
)

type confirmKind int

const (
	confirmDelete confirmKind = iota
	confirmStop
	confirmRestart
	confirmDone
	confirmOrphans
	confirmAttach // t while the headless run is still working
)

type model struct {
	ctx   context.Context
	b     *board.Board
	store board.Store
	opts  Options

	col    int   // selected column
	row    []int // selected row per column
	scroll []int // first visible card per column
	areaH  int   // screen lines above the key bar (set by View)

	mode    mode
	confirm confirmKind
	form    *editForm

	notice  string // shown as a toast; see toastBox
	info    string // neutral toast (e.g. "cancelled")
	err     string
	toastID int // the latest message; its timer clears it

	orphans []string // agent pods no ticket run refers to

	animate     bool   // marquee on (off in tests: no tickers)
	marqueeOn   bool   // a marquee tick is scheduled
	marqueeID   string // ticket whose title is scrolling
	marqueeStep int

	logsPod    string
	logsLoaded bool           // first content arrived (then jump to the end)
	logsFollow bool           // keep the view at the end as output arrives
	logsSeq    int            // the open view's refresh loop; older ticks stop
	vp         viewport.Model // modeLogs: scrollable agent output

	width, height int

	cardHits  []cardHit // where View drew the cards and columns, for clicks
	colHits   []cardHit
	lastClick struct {
		id string
		at time.Time
	}
	popupX, popupY int // where overlay drew the last popup
}

// cardHit is a clickable area of the board: a card, or a whole column (row -1).
type cardHit struct {
	x, y, w, h int
	col, row   int
	id         string
}

func (h cardHit) contains(x, y int) bool {
	return x >= h.x && x < h.x+h.w && y >= h.y && y < h.y+h.h
}

// doubleClick is the longest gap between the clicks of a double click.
const doubleClick = 500 * time.Millisecond

func newModel(b *board.Board, store board.Store, opts Options) *model {
	return &model{
		ctx: context.Background(),
		b:   b, store: store, opts: opts,
		row: make([]int, len(b.Columns)), scroll: make([]int, len(b.Columns)),
		width: 100, height: 30, areaH: 28,
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
	m.err, m.notice, m.info = "", notice, ""
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, ok := msg.(marqueeMsg); ok {
		m.marqueeOn = false
		if !m.needsMarquee() {
			return m, nil
		}
		m.marqueeStep++
	}
	if d, ok := msg.(toastDoneMsg); ok {
		if d.id == m.toastID { // a newer message keeps its own timer
			m.notice, m.info, m.err = "", "", ""
		}
		return m, nil
	}
	before := m.notice + "\x00" + m.info + "\x00" + m.err
	model, cmd := m.update(msg)
	// A new message pops up as a toast and disappears after a while.
	if after := m.notice + "\x00" + m.info + "\x00" + m.err; after != before && (m.notice != "" || m.info != "" || m.err != "") {
		m.toastID++
		if m.animate {
			d, id := toastNotice, m.toastID
			if m.err != "" {
				d = toastError
			}
			cmd = tea.Batch(cmd, tea.Tick(d, func(time.Time) tea.Msg { return toastDoneMsg{id} }))
		}
	}
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
			if m.mode == modeLogs {
				m.followIfAtBottom()
			}
			return m, cmd
		}
		if m.mode == modeBoard {
			m.wheelBoard(msg.Mouse())
		}
		return m, nil
	case tea.MouseClickMsg:
		mouse := msg.Mouse()
		if mouse.Button != tea.MouseLeft {
			return m, nil
		}
		switch m.mode {
		case modeBoard:
			return m.clickBoard(mouse)
		case modeForm:
			return m, m.form.click(mouse.X-m.popupX, mouse.Y-m.popupY)
		}
		return m, nil
	case pollMsg:
		return m, m.pollCmd()
	case statusMsg:
		capture := m.applyStatuses(msg.states)
		next := tea.Tick(pollInterval, func(time.Time) tea.Msg { return pollMsg{} })
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
		follow := !m.logsLoaded || m.logsFollow
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
	case logsTickMsg:
		if m.mode != modeLogs || m.logsPod == "" || msg.seq != m.logsSeq {
			return m, nil // view left or replaced: this loop ends
		}
		return m, tea.Batch(m.logsCmd(m.logsPod), m.logsTick())
	case attachDoneMsg:
		if msg.err != nil {
			m.err = "session: " + msg.err.Error()
		} else {
			m.notice = "back from the agent session (t re-attaches)"
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
				m.notice, m.info = "", "cancelled"
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
				if m.logsPod == "" {
					return m, nil
				}
				return m, m.logsCmd(m.logsPod)
			case "f":
				m.logsFollow = !m.logsFollow
				if m.logsFollow {
					m.vp.GotoBottom()
				}
				return m, nil
			case "g", "home":
				m.vp.GotoTop()
				m.logsFollow = false
				return m, nil
			case "G", "end":
				m.vp.GotoBottom()
				m.logsFollow = true
				return m, nil
			}
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(msg) // j/k, arrows, pgup/pgdn, space/b, u/d
			m.followIfAtBottom()
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
			m.save(fmt.Sprintf("%s: agent %s", cardTitle(t), agentName(t.Agent)))
		}
	case "p":
		if t != nil {
			t.CyclePriority()
			m.save(fmt.Sprintf("%s: priority P%d", cardTitle(t), t.Priority))
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
			return m.attach(t, msg.String() == "T", false)
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
		m.save(fmt.Sprintf("%s → Done, stopping its agent pods", cardTitle(t)))
		return m.stopRunsCmd(t, t.LiveRuns())
	}
	m.selectTicket(t)
	m.save(fmt.Sprintf("%s → %s", cardTitle(t), m.b.Columns[m.col].Name))
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
		m.mode, m.form, m.notice, m.info = modeBoard, nil, "", "cancelled"
	case formSave:
		f := m.form
		m.mode, m.form = modeBoard, nil
		if f.ticketID == "" {
			t := m.b.Add(strings.TrimSpace(f.title.Value()))
			f.apply(t)
			m.selectTicket(t)
			m.save("created " + t.Key + " " + t.Title)
			if t.Agent != "" { // created with an agent: start it right away
				_, start := m.spawn(t, false)
				return m, tea.Batch(cmd, start)
			}
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
		"auto": "#b4befe", "claude": "#fab387", "codex": "#94e2d5", "copilot": "#89dceb", "antigravity": "#f5c2e7", "ollama": "#a6e3a1",
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

	// The key bar is drawn last, under everything: popups and toasts only
	// cover the area above it, so the keys are always visible.
	footer := lipgloss.NewStyle().Width(max(m.width, 20)).Render(m.footer())
	m.areaH = max(m.height-lineCount(footer), 7)
	bodyH := m.areaH - 2

	var body string
	switch m.mode {
	case modeDetail:
		body = m.columnsView(bodyH) // the details pop up over the board
	case modeHelp:
		body = helpText
	case modeLogs:
		m.vp.SetHeight(max(bodyH-2, 3))
		body = m.logsView()
	default:
		body = m.columnsView(bodyH)
	}

	var b strings.Builder
	b.WriteString(head + "\n\n")
	b.WriteString(body)
	if pad := bodyH - lineCount(body); pad > 0 {
		b.WriteString(strings.Repeat("\n", pad))
	}
	screen := strings.TrimRight(b.String(), "\n")
	switch {
	case m.mode == modeForm && m.form != nil:
		screen = m.overlay(screen, m.form.view(m.width, m.areaH-1))
	case m.mode == modeConfirm:
		screen = m.overlay(screen, m.confirmBox())
	case m.mode == modeDetail:
		screen = m.overlay(screen, m.detailBox())
	}
	if box := m.toastBox(); box != "" {
		x := max(m.width-lipgloss.Width(box)-1, 0)
		screen = lipgloss.NewCompositor(
			lipgloss.NewLayer(screen),
			lipgloss.NewLayer(box).X(x).Y(1).Z(2),
		).Render()
	}
	v := tea.NewView(screen + "\n" + footer)
	v.AltScreen = true
	if m.mode != modeHelp && m.mode != modeConfirm {
		// Clicks and wheel events; Termux turns taps into clicks and
		// touch swipes into the wheel.
		v.MouseMode = tea.MouseModeCellMotion
	}
	return v
}

// overlay draws a popup centered over the screen.
// The box is centered in the area above the key bar and cut to fit it.
func (m *model) overlay(screen, box string) string {
	x := max((m.width-lipgloss.Width(box))/2, 0)
	y := max((m.areaH-lipgloss.Height(box))/2, 1)
	m.popupX, m.popupY = x, y
	if lines := strings.Split(box, "\n"); len(lines) > m.areaH-y {
		box = strings.Join(lines[:max(m.areaH-y, 1)], "\n")
	}
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

// Toasts: a message shows top right and fades after a while.
type toastDoneMsg struct{ id int }

const (
	toastNotice = 3 * time.Second
	toastError  = 6 * time.Second
)

func (m *model) toastBox() string {
	text, color, icon := m.notice, lipgloss.Color("10"), "✓ "
	switch {
	case m.err != "":
		text, color, icon = m.err, lipgloss.Color("9"), "✗ "
	case m.notice == "" && m.info != "":
		text, color, icon = m.info, lipgloss.Color("8"), ""
	}
	if text == "" {
		return ""
	}
	w := min(46, max(m.width-4, 20))
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(color).
		Padding(0, 1).Width(w).
		Render(lipgloss.NewStyle().Foreground(color).Render(icon) + text)
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
		return fmt.Sprintf("stop the agent of %q? Its pod, /work and session are deleted (the output is kept). y/N", cardTitle(t))
	case confirmRestart:
		return fmt.Sprintf("the agent of %q is still running. Stop it and start a new run? y/N", cardTitle(t))
	case confirmDone:
		return fmt.Sprintf("the agent of %q is still running. Move to Done and stop it? y/N", cardTitle(t))
	case confirmAttach:
		return fmt.Sprintf("the agent of %q is still working: its session will not show the run's progress until it finishes (o shows the output). Open it anyway? y/N", cardTitle(t))
	}
	q := fmt.Sprintf("delete %q?", cardTitle(t))
	if len(t.LiveRuns()) > 0 {
		q += " Its agent pods are stopped too."
	}
	return q + " y/N"
}

// footer is the key bar for the current mode.
func (m *model) footer() string {
	switch m.mode {
	case modeForm:
		return subtle.Render("tab/shift+tab/↑/↓ field · ←/→ priority/agent · enter next field (newline in description) · ctrl+s save · esc cancel")
	case modeConfirm:
		return subtle.Render("y confirm · n/esc or any other key cancel")
	case modeDetail:
		return subtle.Render("j/k pgup/pgdn scroll (wheel/touch too) · g/G top/end · e edit · q/esc back")
	case modeHelp:
		return subtle.Render("any key back")
	case modeLogs:
		follow := "f follow: off"
		if m.logsFollow {
			follow = "f follow: on"
		}
		if m.logsPod == "" {
			follow = "saved"
		}
		return subtle.Render(fmt.Sprintf("%3.0f%% · %s · j/k ↑/↓ pgup/pgdn scroll (touch/wheel too) · g/G top/end · r refresh · q back", m.vp.ScrollPercent()*100, follow))
	}
	return subtle.Render("h/l j/k move · space/H/L move · n new · e edit · a agent · s start · t session · o output · x stop · p prio · d del · enter details · ? help · q quit")
}

func (m *model) columnsView(height int) string {
	n := len(m.b.Columns)
	shown, first, colW := m.columnLayout()
	visible := max((height-3)/cardLines, 1) // column titles, scroll markers

	m.cardHits, m.colHits = m.cardHits[:0], m.colHits[:0]
	const top = 2 // the header line and a blank line above the board
	cols := make([]string, 0, shown)
	for i := first; i < first+shown; i++ {
		x := (i - first) * (colW + 1)
		m.colHits = append(m.colHits, cardHit{x: x, y: top, w: colW, h: height, col: i, row: -1})
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
			m.cardHits = append(m.cardHits, cardHit{x: x, y: top + strings.Count(strings.Join(lines, "\n"), "\n") + 1, w: colW, h: cardLines, col: i, row: j, id: ts[j].ID})
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

// clickBoard selects the clicked card (or column); a second click on the
// same card soon after opens its details.
func (m *model) clickBoard(mouse tea.Mouse) (tea.Model, tea.Cmd) {
	for _, h := range m.cardHits {
		if !h.contains(mouse.X, mouse.Y) {
			continue
		}
		m.col, m.row[h.col] = h.col, h.row
		now := time.Now()
		if m.lastClick.id == h.id && now.Sub(m.lastClick.at) < doubleClick {
			m.lastClick.id = ""
			m.mode = modeDetail
			m.openDetail()
			return m, nil
		}
		m.lastClick.id, m.lastClick.at = h.id, now
		return m, nil
	}
	m.lastClick.id = ""
	for _, h := range m.colHits {
		if h.contains(mouse.X, mouse.Y) {
			m.col = h.col
		}
	}
	return m, nil
}

// wheelBoard moves the selection in the column under the pointer.
func (m *model) wheelBoard(mouse tea.Mouse) {
	for _, h := range m.colHits {
		if !h.contains(mouse.X, mouse.Y) {
			continue
		}
		m.col = h.col
		switch mouse.Button {
		case tea.MouseWheelUp:
			m.row[h.col] = max(m.row[h.col]-1, 0)
		case tea.MouseWheelDown:
			m.row[h.col]++ // selected() clamps it
		}
	}
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

func orDefault(s string) string {
	if s == "" {
		return subtle.Render("agent default")
	}
	return s
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// openDetail fills the viewport with the selected ticket's details and its
// saved agent output.
// detailSize is the inner size of the details popup's viewport.
func (m *model) detailSize() (w, h int) {
	boxW := min(100, max(m.width-4, 30))
	boxH := max(m.areaH-2, 8)
	return boxW - 6, boxH - 4 // border 2 + padding 4; border 2 + heading 2
}

// detailBox is the scrollable details popup.
func (m *model) detailBox() string {
	w, h := m.detailSize()
	m.vp.SetWidth(w)
	m.vp.SetHeight(h)
	head := "Details"
	if t := m.selected(); t != nil {
		head = t.Key + " · " + t.Title
	}
	head = ansi.Truncate(head, w-6, "…")
	pct := subtle.Render(fmt.Sprintf("%3.0f%%", m.vp.ScrollPercent()*100))
	gap := max(w-lipgloss.Width(head)-lipgloss.Width(pct), 1)
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("12")).
		Padding(0, 2).Width(w + 6).
		Render(titleStyle.Render(head) + strings.Repeat(" ", gap) + pct + "\n\n" + m.vp.View())
}

func (m *model) openDetail() {
	w, h := m.detailSize()
	m.vp = viewport.New(viewport.WithWidth(w), viewport.WithHeight(h))
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
		{"Model", orDefault(t.Model)},
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
		agentModel := string(r.Agent)
		if r.Model != "" {
			agentModel += " (" + r.Model + ")"
		}
		head := fmt.Sprintf("Run #%d · %s · %s %s · %s", i+1, agentModel, statusIcon(r.Status), r.Status,
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
Mouse          click selects a card, double click opens it · wheel/swipe walks a column
               in the edit popup a click focuses a field (again: next priority/agent)
Move card      space or L next column · H or backspace previous column
Tickets        n new · e edit (popup with every field, incl. an optional model) · enter details
               a cycle agent (auto, claude, codex, copilot, antigravity, ollama, none);
                 auto lets the orchestrator pick when the agent starts
               p cycle priority (P1 highest … P4) · d delete
Agents         s start the ticket's agent in a pod (moves it to In Progress)
               t open the agent's session in its pod (claude --continue, codex resume,
                 ...) to ask for changes, in tmux: ctrl+z returns here and keeps it
                 running, t again re-attaches · T plain shell there (same)
               o show the agent's output, live (refreshes every 2 s, follows the end;
                 f toggles following, scrolling up pauses it, G resumes)
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

// logsView shows the scrollable agent output. A live pod's output refreshes
// every logsInterval; with follow on (the default, f toggles) the view stays
// at the end. Scrolling up pauses following, scrolling back to the end (or
// G) resumes it.
func (m *model) logsView() string {
	src := "saved output (pod deleted)"
	if m.logsPod != "" {
		src = "output of " + m.logsPod + " · live"
		if m.logsFollow {
			src += ", following"
		}
	}
	return subtle.Render(src) + "\n\n" + m.vp.View() + "\n"
}
