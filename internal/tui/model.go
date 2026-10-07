// Package tui is kAinban's interactive credential bootstrap: an overview of
// every auth.Provider and a screen that walks through a provider's Flow.
package tui

import (
	"charm.land/bubbles/v2/textinput"
	"context"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/zerosuxx/kainban/internal/auth"
)

// Options configures Run.
type Options struct {
	Namespace  string
	Live       bool   // run live (vendor API) checks
	AppVersion string // shown in the title bar
}

// Run shows the TUI until the user quits or ctx is cancelled.
func Run(ctx context.Context, providers []auth.Provider, store auth.Store, opts Options) error {
	m := newModel(ctx, providers, store, opts)
	final, err := tea.NewProgram(m, tea.WithContext(ctx)).Run()
	if fm, ok := final.(*model); ok {
		fm.closeFlow() // in case the program ended without a quit key
	}
	return err
}

type mode int

const (
	modeOverview mode = iota
	modeFlow
)

type row struct {
	status   auth.Status
	checking bool
	checked  bool
	gen      int // drops stale check results
}

// flowState is the state of the provider flow currently on screen.
type flowState struct {
	idx       int // provider index
	flow      auth.Flow
	gen       int        // drops messages from abandoned flows
	step      *auth.Step // last step returned by Next; nil before the first
	busy      string     // non-empty while a Cmd runs ("working…", "saving…")
	err       error      // last error from Next / save; cleared on success
	execErr   error      // last StepExec failure; shown until the next enter
	lastInput string     // what was last submitted, for retries
	done      *auth.Step // StepDone whose save failed, for retry
	input     textinput.Model
	url       string // first URL in the current step's Body
	urlFile   string // where url was also written, if that worked
	copied    bool   // ctrl+y pressed for this step
}

type model struct {
	ctx       context.Context
	providers []auth.Provider
	store     auth.Store
	opts      Options

	rows    []row
	cursor  int
	mode    mode
	notice  string // one-line message on the overview
	spinner spinner.Model
	width   int
	height  int

	wizard bool
	queue  []int // remaining provider indices in the wizard

	fs      flowState
	flowGen int
}

func newModel(ctx context.Context, providers []auth.Provider, store auth.Store, opts Options) *model {
	return &model{
		ctx:       ctx,
		providers: providers,
		store:     store,
		opts:      opts,
		rows:      make([]row, len(providers)),
		spinner:   spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(titleStyle)),
		width:     80,
	}
}

// Messages.
type (
	checkMsg struct {
		idx    int
		gen    int
		status auth.Status
	}
	stepMsg struct {
		gen  int
		step auth.Step
		err  error
	}
	execDoneMsg struct {
		gen int
		err error
	}
	savedMsg struct {
		gen   int
		names []string
		err   error
	}
)

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, m.checkAll())
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case checkMsg:
		if msg.idx < len(m.rows) && msg.gen == m.rows[msg.idx].gen {
			m.rows[msg.idx].status = msg.status
			m.rows[msg.idx].checking = false
			m.rows[msg.idx].checked = true
		}
		return m, nil
	case stepMsg, execDoneMsg, savedMsg:
		return m.updateFlowMsg(msg)
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			m.closeFlow()
			return m, tea.Quit
		}
		if m.mode == modeFlow {
			return m.updateFlowKey(msg)
		}
		return m.updateOverviewKey(msg)
	}
	if m.mode == modeFlow && m.fs.step != nil && m.fs.step.Kind == auth.StepInput {
		var cmd tea.Cmd
		m.fs.input, cmd = m.fs.input.Update(msg) // pastes
		return m, cmd
	}
	return m, nil
}

func (m *model) View() tea.View {
	var s string
	if m.mode == modeFlow {
		s = m.flowView()
	} else {
		s = m.overviewView()
	}
	v := tea.NewView(s)
	v.AltScreen = true
	return v
}

// checkCmd re-runs one provider's Check.
func (m *model) checkCmd(idx int) tea.Cmd {
	m.rows[idx].gen++
	m.rows[idx].checking = true
	gen, p, ctx, store, live := m.rows[idx].gen, m.providers[idx], m.ctx, m.store, m.opts.Live
	return func() tea.Msg {
		return checkMsg{idx: idx, gen: gen, status: p.Check(ctx, store, live)}
	}
}

func (m *model) checkAll() tea.Cmd {
	cmds := make([]tea.Cmd, len(m.providers))
	for i := range m.providers {
		cmds[i] = m.checkCmd(i)
	}
	return tea.Batch(cmds...)
}

func lenRunes(s string) int { return len([]rune(s)) }
