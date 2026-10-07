package tui

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/zerosuxx/kainban/internal/auth"
)

var (
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	subtleStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	selectedStyle = lipgloss.NewStyle().Bold(true)
	errorStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	okStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	helpStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	codeStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))

	badgeBase = lipgloss.NewStyle().Bold(true).Width(9)
)

// badge renders a fixed-width, colored state label.
func badge(s auth.State) string {
	var c string
	switch s {
	case auth.StateValid:
		c = "10" // green
	case auth.StateInvalid:
		c = "9" // red
	case auth.StateMissing:
		c = "11" // yellow
	default:
		c = "13" // magenta
	}
	return badgeBase.Foreground(lipgloss.Color(c)).Render(s.String())
}

// wrapText word-wraps s to width columns. URLs are never broken and always
// sit on a line of their own so they can be copied in one piece; lipgloss is
// deliberately not used for wrapping because it would split long URLs.
func wrapText(s string, width int) string {
	if width < 20 {
		width = 20
	}
	var out []string
	for _, para := range strings.Split(s, "\n") {
		words := strings.Fields(para)
		if len(words) == 0 {
			out = append(out, "")
			continue
		}
		var line string
		flush := func() {
			if line != "" {
				out = append(out, line)
				line = ""
			}
		}
		for _, w := range words {
			if isURL(w) {
				flush()
				out = append(out, w)
				continue
			}
			switch {
			case line == "":
				line = w
			case lipgloss.Width(line)+1+lipgloss.Width(w) > width:
				flush()
				line = w
			default:
				line += " " + w
			}
		}
		flush()
	}
	return strings.Join(out, "\n")
}

func isURL(w string) bool {
	return strings.Contains(w, "://")
}

// titleBar renders "kAinban <version> · <section>", the version dimmed.
func (m *model) titleBar(section string) string {
	s := titleStyle.Render("kAinban")
	if v := m.opts.AppVersion; v != "" {
		s += " " + subtleStyle.Render(v)
	}
	return s + titleStyle.Render(" · "+section)
}
