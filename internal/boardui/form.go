package boardui

import (
	"fmt"
	"slices"
	"strings"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/zerosuxx/kainban/internal/board"
)

// formField is one field of the edit popup, in tab order.
type formField int

const (
	fieldTitle formField = iota
	fieldDescription
	fieldPriority
	fieldAgent
	fieldLabels
	fieldBranch
	fieldCount
)

var fieldNames = [fieldCount]string{"Title", "Description", "Priority", "Agent", "Labels", "Branch"}

// editForm is the popup that edits every field of a ticket (or creates one).
type editForm struct {
	ticketID string // "" creates a new ticket
	heading  string
	focus    formField

	title, labels, branch textinput.Model
	desc                  textarea.Model
	priority              int
	agent                 board.AgentType

	err string
}

const formWidth = 72

func newTextInput(value, placeholder string, w int) textinput.Model {
	in := textinput.New()
	in.Prompt = ""
	in.Placeholder = placeholder
	in.SetWidth(w)
	in.SetValue(value)
	styles := in.Styles()
	styles.Cursor.Blink = false // no endless ticker (also keeps tests synchronous)
	in.SetStyles(styles)
	return in
}

// newEditForm opens the popup for t, or for a new ticket when t is nil.
func newEditForm(t *board.Ticket, width int, focus formField) *editForm {
	w := min(formWidth, max(width-4, 30)) - 6 // border, padding, label column gap
	f := &editForm{heading: "New ticket", priority: 3, focus: focus}
	if t != nil {
		f.ticketID = t.ID
		f.heading = "Edit " + t.Key
		f.priority, f.agent = t.Priority, t.Agent
	}
	value := func(get func(*board.Ticket) string) string {
		if t == nil {
			return ""
		}
		return get(t)
	}
	f.title = newTextInput(value(func(t *board.Ticket) string { return t.Title }), "what needs to be done", w)
	f.labels = newTextInput(value(func(t *board.Ticket) string { return strings.Join(t.Labels, ", ") }), "comma separated", w)
	f.branch = newTextInput(value(func(t *board.Ticket) string { return t.Branch }), "optional", w)

	f.desc = textarea.New()
	f.desc.Placeholder = "what the agent should do"
	f.desc.ShowLineNumbers = false
	f.desc.SetWidth(w)
	f.desc.SetHeight(6)
	f.desc.SetValue(value(func(t *board.Ticket) string { return t.Description }))
	styles := f.desc.Styles()
	styles.Cursor.Blink = false
	f.desc.SetStyles(styles)
	return f
}

// focusCmd focuses the current field and blurs the others.
func (f *editForm) focusCmd() tea.Cmd {
	f.title.Blur()
	f.labels.Blur()
	f.branch.Blur()
	f.desc.Blur()
	switch f.focus {
	case fieldTitle:
		return f.title.Focus()
	case fieldDescription:
		return f.desc.Focus()
	case fieldLabels:
		return f.labels.Focus()
	case fieldBranch:
		return f.branch.Focus()
	}
	return nil
}

func (f *editForm) moveFocus(delta int) tea.Cmd {
	f.focus = formField((int(f.focus) + delta + int(fieldCount)) % int(fieldCount))
	return f.focusCmd()
}

func (f *editForm) cycle(delta int) {
	switch f.focus {
	case fieldPriority:
		f.priority = (f.priority-1+delta+4)%4 + 1
	case fieldAgent:
		i := slices.Index(board.AgentTypes, f.agent)
		n := len(board.AgentTypes)
		f.agent = board.AgentTypes[(i+delta+n)%n]
	}
}

// formResult is what a key did to the form.
type formResult int

const (
	formEditing formResult = iota
	formSave
	formCancel
)

func (f *editForm) update(msg tea.Msg) (formResult, tea.Cmd) {
	if k, ok := msg.(tea.KeyPressMsg); ok {
		f.err = ""
		switch k.String() {
		case "esc":
			return formCancel, nil
		case "ctrl+s":
			if strings.TrimSpace(f.title.Value()) == "" {
				f.err = "the title is required"
				f.focus = fieldTitle
				return formEditing, f.focusCmd()
			}
			return formSave, nil
		case "tab":
			return formEditing, f.moveFocus(1)
		case "shift+tab":
			return formEditing, f.moveFocus(-1)
		case "enter":
			if f.focus != fieldDescription { // enter is a newline in the description
				return formEditing, f.moveFocus(1)
			}
		}
		if f.focus == fieldPriority || f.focus == fieldAgent {
			switch k.String() {
			case "left", "h", "shift+tab":
				f.cycle(-1)
			case "right", "l", "space":
				f.cycle(1)
			}
			return formEditing, nil
		}
	}
	var cmd tea.Cmd
	switch f.focus {
	case fieldTitle:
		f.title, cmd = f.title.Update(msg)
	case fieldDescription:
		f.desc, cmd = f.desc.Update(msg)
	case fieldLabels:
		f.labels, cmd = f.labels.Update(msg)
	case fieldBranch:
		f.branch, cmd = f.branch.Update(msg)
	}
	return formEditing, cmd
}

// apply writes the form into t.
func (f *editForm) apply(t *board.Ticket) {
	t.Title = strings.TrimSpace(f.title.Value())
	t.Description = strings.TrimSpace(f.desc.Value())
	t.Priority = f.priority
	t.Agent = f.agent
	t.Branch = strings.TrimSpace(f.branch.Value())
	t.Labels = nil
	for _, l := range strings.Split(f.labels.Value(), ",") {
		if l = strings.TrimSpace(l); l != "" {
			t.Labels = append(t.Labels, l)
		}
	}
	t.Touch()
}

func (f *editForm) view(width int) string {
	label := func(i formField) string {
		s := fmt.Sprintf("%-12s", fieldNames[i])
		if f.focus == i {
			return titleStyle.Render("▸ " + s)
		}
		return subtle.Render("  " + s)
	}
	choice := func(i formField, v string) string {
		if f.focus == i {
			return "‹ " + lipgloss.NewStyle().Bold(true).Render(v) + " ›"
		}
		return v
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render(f.heading) + "\n\n")
	b.WriteString(label(fieldTitle) + "\n" + f.title.View() + "\n\n")
	b.WriteString(label(fieldDescription) + "\n" + f.desc.View() + "\n\n")
	b.WriteString(label(fieldPriority) + choice(fieldPriority, priorityStyle(f.priority).Render(fmt.Sprintf("P%d", f.priority))) + "\n")
	b.WriteString(label(fieldAgent) + choice(fieldAgent, agentName(f.agent)) + "\n\n")
	b.WriteString(label(fieldLabels) + "\n" + f.labels.View() + "\n\n")
	b.WriteString(label(fieldBranch) + "\n" + f.branch.View())
	if f.err != "" {
		b.WriteString("\n\n" + errStyle.Render(f.err))
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("12")).
		Padding(1, 2).Width(min(formWidth, max(width-2, 32))).
		Render(b.String())
}
