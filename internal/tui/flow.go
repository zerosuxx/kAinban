package tui

import (
	"charm.land/bubbles/v2/textinput"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/zerosuxx/kainban/internal/auth"
)

// startFlow opens a new Flow for provider idx and asks for its first step.
func (m *model) startFlow(idx int) tea.Cmd {
	m.closeFlow()
	m.flowGen++
	m.mode = modeFlow
	m.notice = ""
	m.fs = flowState{
		idx:  idx,
		flow: m.providers[idx].NewFlow(),
		gen:  m.flowGen,
	}
	return m.nextCmd("")
}

// closeFlow releases the current flow, if any. Safe to call repeatedly.
func (m *model) closeFlow() {
	if m.fs.flow != nil {
		_ = m.fs.flow.Close()
		m.fs.flow = nil
	}
	m.fs.gen = -1
}

// leaveFlow closes the flow and continues the wizard or returns to the
// overview. Extra commands (e.g. a re-check) are batched in.
func (m *model) leaveFlow(cmds ...tea.Cmd) tea.Cmd {
	m.closeFlow()
	m.mode = modeOverview
	if m.wizard {
		cmds = append(cmds, m.nextInWizard())
	}
	return tea.Batch(cmds...)
}

func (m *model) nextCmd(input string) tea.Cmd {
	m.fs.busy = "working…"
	m.fs.lastInput = input
	flow, gen, ctx := m.fs.flow, m.fs.gen, m.ctx
	return func() tea.Msg {
		step, err := flow.Next(ctx, input)
		return stepMsg{gen: gen, step: step, err: err}
	}
}

func (m *model) saveCmd(step *auth.Step) tea.Cmd {
	m.fs.busy = "saving…"
	m.fs.done = step
	id, gen, ctx, store := m.providers[m.fs.idx].ID(), m.fs.gen, m.ctx, m.store
	names := make([]string, 0, len(step.Secrets))
	for name := range step.Secrets {
		names = append(names, name)
	}
	sort.Strings(names)
	secrets := step.Secrets
	return func() tea.Msg {
		for _, name := range names {
			if err := store.Put(ctx, id, name, secrets[name]); err != nil {
				return savedMsg{gen: gen, names: names, err: fmt.Errorf("saving %s: %w", name, err)}
			}
		}
		return savedMsg{gen: gen, names: names}
	}
}

func (m *model) execCmd(step *auth.Step) tea.Cmd {
	if len(step.Command) == 0 {
		return m.nextCmd("")
	}
	m.fs.busy = "running " + step.Command[0] + "…"
	c := exec.Command(step.Command[0], step.Command[1:]...)
	c.Env = append(os.Environ(), step.Env...)
	gen := m.fs.gen
	return tea.ExecProcess(c, func(err error) tea.Msg {
		return execDoneMsg{gen: gen, err: err}
	})
}

func (m *model) updateFlowMsg(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case stepMsg:
		if msg.gen != m.fs.gen {
			return m, nil
		}
		m.fs.busy = ""
		if msg.err != nil {
			m.fs.err = msg.err // keep the current step and input for a retry
			return m, nil
		}
		m.fs.err = nil
		step := msg.step
		m.fs.step = &step
		m.fs.url, m.fs.urlFile, m.fs.copied = firstURL(step.Body), "", false
		if m.fs.url != "" {
			// Fallback for terminals without OSC 52: read it from another shell.
			f := filepath.Join(os.TempDir(), "kainban-url.txt")
			if os.WriteFile(f, []byte(m.fs.url+"\n"), 0o600) == nil {
				m.fs.urlFile = f
			}
		}
		switch step.Kind {
		case auth.StepInput:
			in := textinput.New()
			in.Prompt = "> "
			if step.Prompt != "" {
				in.Prompt = step.Prompt + " "
			}
			in.Placeholder = step.Placeholder
			in.SetValue(step.Value)
			if step.Secret {
				in.EchoMode = textinput.EchoPassword
			}
			in.SetWidth(max(m.width-len(in.Prompt)-2, 20))
			// A static cursor avoids the endless blink ticker, which also
			// keeps the synchronous test driver from looping.
			styles := in.Styles()
			styles.Cursor.Blink = false
			in.SetStyles(styles)
			focus := in.Focus()
			m.fs.input = in
			return m, focus
		case auth.StepDone:
			return m, m.saveCmd(&step)
		}
		return m, nil

	case execDoneMsg:
		if msg.gen != m.fs.gen {
			return m, nil
		}
		m.fs.err = nil
		m.fs.execErr = msg.err
		return m, m.nextCmd("")

	case savedMsg:
		if msg.gen != m.fs.gen {
			return m, nil
		}
		m.fs.busy = ""
		if msg.err != nil {
			m.fs.err = msg.err
			return m, nil
		}
		idx := m.fs.idx
		m.notice = okStyle.Render(fmt.Sprintf("%s saved: %s", m.providers[idx].Title(), strings.Join(msg.names, ", ")))
		return m, m.leaveFlow(m.checkCmd(idx))
	}
	return m, nil
}

func (m *model) updateFlowKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	step := m.fs.step
	typing := step != nil && step.Kind == auth.StepInput && m.fs.busy == ""

	switch msg.String() {
	case "esc":
		m.wizard = false
		m.queue = nil
		m.notice = subtleStyle.Render("aborted " + m.providers[m.fs.idx].Title())
		return m, m.leaveFlow()
	case "ctrl+s":
		return m.skip()
	case "ctrl+y":
		if m.fs.url != "" {
			m.fs.copied = true
			return m, tea.SetClipboard(m.fs.url)
		}
		return m, nil
	case "s":
		if !typing {
			return m.skip()
		}
	case "enter":
		if m.fs.busy != "" {
			return m, nil
		}
		m.fs.execErr = nil
		switch {
		case m.fs.done != nil: // save failed: retry it
			return m, m.saveCmd(m.fs.done)
		case step == nil: // first Next failed: retry it
			return m, m.nextCmd(m.fs.lastInput)
		case step.Kind == auth.StepInput:
			return m, m.nextCmd(m.fs.input.Value())
		case step.Kind == auth.StepExec:
			return m, m.execCmd(step)
		}
		return m, nil
	}
	if typing {
		var cmd tea.Cmd
		m.fs.input, cmd = m.fs.input.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *model) skip() (tea.Model, tea.Cmd) {
	m.notice = subtleStyle.Render("skipped " + m.providers[m.fs.idx].Title())
	return m, m.leaveFlow()
}

func (m *model) flowView() string {
	var b strings.Builder
	p := m.providers[m.fs.idx]
	b.WriteString(m.titleBar("auth · " + p.Title()))
	if m.wizard {
		b.WriteString(subtleStyle.Render(fmt.Sprintf("  (bootstrap, %d more after this)", len(m.queue))))
	}
	b.WriteString("\n\n")

	width := m.width - 2
	step := m.fs.step
	if step != nil {
		if step.Title != "" {
			b.WriteString(selectedStyle.Render(step.Title) + "\n\n")
		}
		if step.Body != "" {
			b.WriteString(wrapText(step.Body, width) + "\n\n")
		}
		switch step.Kind {
		case auth.StepInput:
			if m.fs.busy == "" {
				b.WriteString(m.fs.input.View() + "\n")
			}
		case auth.StepExec:
			if m.fs.busy == "" {
				b.WriteString("press enter to run " + codeStyle.Render("`"+strings.Join(step.Command, " ")+"`") + "\n")
			}
		}
	}
	if m.fs.busy != "" {
		b.WriteString(m.spinner.View() + " " + m.fs.busy + "\n")
	}
	if m.fs.execErr != nil {
		b.WriteString("\n" + errorStyle.Render(wrapText("command failed: "+m.fs.execErr.Error(), width)) + "\n")
	}
	if m.fs.url != "" {
		hint := "ctrl+y copies the URL to your clipboard (OSC 52)"
		if m.fs.copied {
			hint = "URL sent to the clipboard (needs OSC 52 support in your terminal)"
		}
		if m.fs.urlFile != "" {
			hint += "; it is also in " + m.fs.urlFile + " (kubectl exec <pod> -- cat " + m.fs.urlFile + ")"
		}
		b.WriteString(subtleStyle.Render(wrapText(hint, width)) + "\n")
	}
	if m.fs.err != nil {
		b.WriteString("\n" + errorStyle.Render(wrapText("error: "+m.fs.err.Error(), width)) + "\n")
		if m.fs.busy == "" {
			b.WriteString(subtleStyle.Render("press enter to retry") + "\n")
		}
	}

	help := "enter submit · esc abort"
	if step != nil && step.Kind == auth.StepExec {
		help = "enter run · esc abort"
	}
	if m.wizard {
		help += " · ctrl+s skip to next"
	} else {
		help += " · ctrl+s skip"
	}
	if m.fs.url != "" {
		help += " · ctrl+y copy URL"
	}
	help += " · ctrl+c quit"
	b.WriteString("\n" + helpStyle.Render(help))
	return b.String()
}
