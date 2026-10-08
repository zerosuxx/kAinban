package boardui

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
	"github.com/creack/pty"

	"github.com/zerosuxx/kainban/internal/board"
)

// Experimental (v): the agent's session embedded in the board instead of
// handing the whole terminal over (t). The attach command runs on a local
// pseudo terminal; a terminal emulator keeps its screen, which is drawn
// under a header and next to a ticket panel. Keys and pastes go back to it.

// termPanelW is the ticket panel's width; it shows from termPanelMin columns.
const (
	termPanelW   = 32
	termPanelMin = 100
)

// embedTerm is one embedded session.
type embedTerm struct {
	ticketID string
	shell    bool
	started  time.Time

	emu    *vt.SafeEmulator
	pty    *os.File
	cmd    *exec.Cmd
	output chan struct{} // new output (coalesced)
	exited chan error

	cursorHidden atomic.Bool
}

type (
	termOutputMsg struct{ t *embedTerm }
	termExitMsg   struct {
		t   *embedTerm
		err error
	}
)

// termSize is the emulator's size for a w×h screen.
func termSize(w, h int) (cols, rows int) {
	cols = w
	if w >= termPanelMin {
		cols = w - termPanelW - 1 // the panel and its separator
	}
	return max(cols, 10), max(h-1, 3) // the header line
}

func startEmbedTerm(argv []string, ticketID string, shell bool, w, h int) (*embedTerm, error) {
	cols, rows := termSize(w, h)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, err
	}
	et := &embedTerm{
		ticketID: ticketID, shell: shell, started: time.Now(),
		emu: vt.NewSafeEmulator(cols, rows), pty: f, cmd: cmd,
		output: make(chan struct{}, 1), exited: make(chan error, 1),
	}
	et.emu.SetCallbacks(vt.Callbacks{
		CursorVisibility: func(visible bool) { et.cursorHidden.Store(!visible) },
	})
	// Keys and the emulator's answers to queries go to the session.
	go io.Copy(f, et.emu) //nolint:errcheck // ends when the pty closes
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				et.emu.Write(buf[:n]) //nolint:errcheck // never fails while open
				select {
				case et.output <- struct{}{}:
				default: // a redraw is already pending
				}
			}
			if err != nil {
				break
			}
		}
		werr := cmd.Wait()
		f.Close()
		// Ends the input copy. Not emu.Close: it races with Read in x/vt.
		if c, ok := et.emu.InputPipe().(io.Closer); ok {
			c.Close()
		}
		et.exited <- werr
	}()
	return et, nil
}

// wait delivers the next output or the end of the session.
func (et *embedTerm) wait() tea.Cmd {
	return func() tea.Msg {
		select {
		case <-et.output:
			return termOutputMsg{et}
		case err := <-et.exited:
			return termExitMsg{et, err}
		}
	}
}

func (et *embedTerm) resize(w, h int) {
	cols, rows := termSize(w, h)
	if cols == et.emu.Width() && rows == et.emu.Height() {
		return
	}
	et.emu.Resize(cols, rows)
	pty.Setsize(et.pty, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)}) //nolint:errcheck
}

// close detaches: the attach process ends, the session in the pod stays.
func (et *embedTerm) close() {
	if et.cmd.Process != nil {
		et.cmd.Process.Kill() //nolint:errcheck
	}
}

// openTerm embeds the session whose attach command is argv.
func (m *model) openTerm(t *board.Ticket, argv []string, shell bool) (tea.Model, tea.Cmd) {
	et, err := startEmbedTerm(argv, t.ID, shell, m.width, m.height)
	if err != nil {
		m.err = "session: " + err.Error()
		return m, nil
	}
	m.term, m.mode = et, modeTerm
	return m, et.wait()
}

func (m *model) updateTerm(msg tea.Msg) (tea.Model, tea.Cmd) {
	et := m.term
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+g" {
			et.close() // its exit message brings the board back
			return m, nil
		}
		et.emu.SendKey(uv.KeyPressEvent(msg))
	case tea.PasteMsg:
		et.emu.Paste(msg.Content)
	}
	return m, nil
}

// termView is the embedded session: a header, the session's screen and,
// on wide screens, the ticket panel.
func (m *model) termView() tea.View {
	et := m.term
	et.resize(m.width, m.height)
	cols, rows := termSize(m.width, m.height)

	t := m.findTicket(et.ticketID)
	what := "session"
	if et.shell {
		what = "shell"
	}
	head := subtle.Render("Board → ")
	if t != nil {
		head += titleStyle.Render(cardTitle(t)) + " " + agentBadge(t)
	}
	head += subtle.Render(fmt.Sprintf("  %s · %s", what, time.Since(et.started).Truncate(time.Second)))
	hint := subtle.Render("experimental · ") + lipgloss.NewStyle().Bold(true).Render("ctrl+g") + subtle.Render(" board")
	head = ansi.Truncate(head, max(m.width-lipgloss.Width(hint)-1, 10), "…")
	head += strings.Repeat(" ", max(m.width-lipgloss.Width(head)-lipgloss.Width(hint), 1)) + hint

	screen := strings.Split(et.emu.Render(), "\n")
	var panel []string
	if m.width >= termPanelMin {
		panel = m.termPanel(t, rows)
	}
	var b strings.Builder
	b.WriteString(head)
	sep := lipgloss.NewStyle().Foreground(dimBorder).Render("│")
	for y := range rows {
		line := ""
		if y < len(screen) {
			line = ansi.Truncate(screen[y], cols, "")
		}
		b.WriteString("\n" + line)
		if panel != nil {
			pad := max(cols-ansi.StringWidth(line), 0)
			b.WriteString("\x1b[m" + strings.Repeat(" ", pad) + sep + panel[y])
		}
	}
	v := tea.NewView(b.String())
	v.AltScreen = true
	if !et.cursorHidden.Load() {
		pos := et.emu.CursorPosition()
		v.Cursor = tea.NewCursor(pos.X, pos.Y+1)
	}
	return v
}

// termPanel is the ticket beside the session, rows lines of termPanelW.
func (m *model) termPanel(t *board.Ticket, rows int) []string {
	w := termPanelW - 2
	var lines []string
	add := func(s string) { lines = append(lines, s) }
	field := func(name, value string) {
		add(subtle.Render(name))
		add(ansi.Truncate(value, w, "…"))
		add("")
	}
	if t == nil {
		add(subtle.Render("(ticket deleted)"))
	} else {
		for _, l := range strings.Split(lipgloss.NewStyle().Width(w).Render(cardTitle(t)), "\n") {
			add(lipgloss.NewStyle().Bold(true).Render(l))
		}
		add("")
		col := string(t.Status)
		if i := m.b.ColumnIndex(t.Status); i >= 0 {
			col = m.b.Columns[i].Name
		}
		field("Column", fmt.Sprintf("%s · P%d", col, t.Priority))
		if r := t.Current(); r != nil {
			agent := agentName(r.Agent)
			if r.Model != "" {
				agent += " · " + r.Model
			}
			field("Agent", agent)
			field("Run", statusIcon(r.Status)+" "+string(r.Status))
			field("Pod", r.Pod)
		}
		field("Branch", orDash(t.Branch))
		if len(t.Labels) > 0 {
			field("Labels", strings.Join(t.Labels, ", "))
		}
	}
	out := make([]string, rows)
	for i := range out {
		if i < len(lines) {
			out[i] = " " + lines[i]
		}
	}
	return out
}

// leaveTerm is the board again after the session ended or was left.
func (m *model) leaveTerm(msg termExitMsg) {
	if m.term != msg.t {
		return
	}
	m.term = nil
	if m.mode == modeTerm {
		m.mode = modeBoard
	}
	var exit *exec.ExitError
	switch {
	case msg.err == nil:
		m.notice = "back from the agent session (v re-opens it)"
	case errors.As(msg.err, &exit) && !exit.Exited(): // ctrl+g killed it
		m.notice = "left the session; it keeps running (v re-opens it)"
	default:
		m.err = "session: " + msg.err.Error()
	}
}
