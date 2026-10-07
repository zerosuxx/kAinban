package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/zerosuxx/kainban/internal/auth"
)

func (m *model) updateOverviewKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q":
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.providers)-1 {
			m.cursor++
		}
	case "enter":
		if len(m.providers) > 0 {
			m.wizard = false
			m.queue = nil
			return m, m.startFlow(m.cursor)
		}
	case "a":
		m.queue = m.queue[:0]
		for i, r := range m.rows {
			if r.status.State != auth.StateValid || !r.checked {
				m.queue = append(m.queue, i)
			}
		}
		m.wizard = true
		return m, m.nextInWizard()
	case "s":
		// On the overview there is no current provider; skip just moves the
		// selection to the next provider that still needs attention.
		for i := m.cursor + 1; i < len(m.rows); i++ {
			if m.rows[i].status.State != auth.StateValid {
				m.cursor = i
				break
			}
		}
	case "r":
		m.notice = ""
		return m, m.checkAll()
	case "l":
		m.opts.Live = !m.opts.Live
		m.notice = ""
		return m, m.checkAll()
	}
	return m, nil
}

// nextInWizard starts the next provider in the wizard queue that is not
// (by now) valid, or ends the wizard.
func (m *model) nextInWizard() tea.Cmd {
	for len(m.queue) > 0 {
		idx := m.queue[0]
		m.queue = m.queue[1:]
		r := m.rows[idx]
		if r.checked && !r.checking && r.status.State == auth.StateValid {
			continue
		}
		m.cursor = idx
		return m.startFlow(idx)
	}
	if m.wizard {
		m.wizard = false
		if m.notice == "" {
			m.notice = "bootstrap finished"
		}
	}
	m.mode = modeOverview
	return nil
}

func (m *model) overviewView() string {
	var b strings.Builder
	b.WriteString(m.titleBar("auth"))
	if m.opts.Namespace != "" {
		b.WriteString(subtleStyle.Render("  namespace: " + m.opts.Namespace))
	}
	live := "off"
	if m.opts.Live {
		live = "on"
	}
	b.WriteString(subtleStyle.Render("  live checks: " + live))
	b.WriteString("\n\n")

	titleW := 0
	for _, p := range m.providers {
		titleW = max(titleW, lenRunes(p.Title()))
	}
	for i, p := range m.providers {
		r := m.rows[i]
		pointer := "  "
		title := fmt.Sprintf("%-*s", titleW, p.Title())
		if i == m.cursor {
			pointer = "> "
			title = selectedStyle.Render(title)
		}
		var state, detail string
		if r.checking || !r.checked {
			state = m.spinner.View() + subtleStyle.Render(" checking")
			if r.checked {
				detail = subtleStyle.Render(r.status.Detail)
			}
		} else {
			state = badge(r.status.State)
			detail = r.status.Detail
			if r.status.ExpiresAt != nil {
				detail = strings.TrimSpace(detail + "  " + subtleStyle.Render(formatExpiry(*r.status.ExpiresAt)))
			}
		}
		fmt.Fprintf(&b, "%s%s  %s  %s\n", pointer, title, state, detail)
	}
	if len(m.providers) == 0 {
		b.WriteString(subtleStyle.Render("  no providers configured") + "\n")
	}
	if m.notice != "" {
		b.WriteString("\n" + m.notice + "\n")
	}
	b.WriteString("\n" + helpStyle.Render("↑/↓ j/k select · enter configure · a bootstrap all · s next pending · r re-check · l toggle live · q quit"))
	return b.String()
}

func formatExpiry(t time.Time) string {
	d := time.Until(t)
	when := t.Local().Format("2006-01-02 15:04")
	switch {
	case d < 0:
		return "expired " + when
	case d < time.Hour:
		return fmt.Sprintf("expires %s (in %dm)", when, int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("expires %s (in %dh)", when, int(d.Hours()))
	default:
		return fmt.Sprintf("expires %s (in %dd)", when, int(d.Hours()/24))
	}
}
