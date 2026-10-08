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
	fieldModel
	fieldLabels
	fieldBranch
	fieldCount
)

var fieldNames = [fieldCount]string{"Title", "Description", "Priority", "Agent", "Model", "Labels", "Branch"}

// editForm is the popup that edits every field of a ticket (or creates one).
type editForm struct {
	ticketID string // "" creates a new ticket
	heading  string
	focus    formField

	title, model, labels, branch textinput.Model
	desc                         textarea.Model
	priority                     int
	agent                        board.AgentType

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
	f.model = newTextInput(value(func(t *board.Ticket) string { return t.Model }), "optional, e.g. claude-opus-5-5 or qwen3:4b for ollama (empty = the agent's default)", w)
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
	f.model.Blur()
	f.labels.Blur()
	f.branch.Blur()
	f.desc.Blur()
	switch f.focus {
	case fieldTitle:
		return f.title.Focus()
	case fieldDescription:
		return f.desc.Focus()
	case fieldModel:
		return f.model.Focus()
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
	case fieldModel:
		f.model, cmd = f.model.Update(msg)
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
	t.Model = strings.TrimSpace(f.model.Value())
	t.Branch = strings.TrimSpace(f.branch.Value())
	t.Labels = nil
	for _, l := range strings.Split(f.labels.Value(), ",") {
		if l = strings.TrimSpace(l); l != "" {
			t.Labels = append(t.Labels, l)
		}
	}
	t.Touch()
}

// view renders the popup; when the full layout is taller than maxH (phones)
// it switches to a compact one: label and value on one line, no blank lines
// and a lower description box.
func (f *editForm) view(width, maxH int) string {
	w := min(formWidth, max(width-2, 32))
	f.desc.SetHeight(6)
	if box := f.render(w, false); lipgloss.Height(box) <= maxH {
		return box
	}
	f.desc.SetHeight(min(max(maxH-13, 1), 6))
	return f.render(w, true)
}

func (f *editForm) render(w int, compact bool) string {
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
	inner := w - 6 // border + padding
	gap, sep := "\n\n", "\n"
	if compact {
		inner = w - 4
		gap = "\n"
	}
	input := func(i formField, in *textinput.Model) string {
		if compact { // value next to the label
			in.SetWidth(max(inner-15, 8))
			return label(i) + in.View()
		}
		in.SetWidth(inner)
		return label(i) + sep + in.View()
	}
	f.desc.SetWidth(inner)
	var b strings.Builder
	b.WriteString(titleStyle.Render(f.heading) + gap)
	b.WriteString(input(fieldTitle, &f.title) + gap)
	b.WriteString(label(fieldDescription) + "\n" + f.desc.View() + gap)
	b.WriteString(label(fieldPriority) + choice(fieldPriority, priorityStyle(f.priority).Render(fmt.Sprintf("P%d", f.priority))) + "\n")
	b.WriteString(label(fieldAgent) + choice(fieldAgent, agentName(f.agent)) + gap)
	b.WriteString(input(fieldModel, &f.model) + gap)
	b.WriteString(input(fieldLabels, &f.labels) + gap)
	b.WriteString(input(fieldBranch, &f.branch))
	if f.err != "" {
		b.WriteString(gap + errStyle.Render(f.err))
	}
	style := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("12")).
		Padding(1, 2).Width(w)
	if compact {
		style = style.Padding(0, 1)
	}
	return style.Render(b.String())
}
