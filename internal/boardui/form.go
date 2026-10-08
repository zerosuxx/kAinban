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

	spans []fieldSpan // lines of the last render, for clicks
	boxW  int
}

// fieldSpan is the lines a field takes in the rendered popup.
type fieldSpan struct {
	field     formField
	from, end int // [from, end)
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
	f.model = newTextInput(value(func(t *board.Ticket) string { return t.Model }), modelPlaceholder(f.agent), w)
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

// descAtTop and descAtBottom report whether the description cursor is on its
// first or last visual row, where up/down leave the field.
func (f *editForm) descAtTop() bool {
	return f.desc.Line() == 0 && f.desc.LineInfo().RowOffset == 0
}

func (f *editForm) descAtBottom() bool {
	li := f.desc.LineInfo()
	return f.desc.Line() == f.desc.LineCount()-1 && li.RowOffset+1 >= li.Height
}

func (f *editForm) cycle(delta int) {
	switch f.focus {
	case fieldPriority:
		f.priority = (f.priority-1+delta+4)%4 + 1
	case fieldAgent:
		i := slices.Index(board.AgentTypes, f.agent)
		n := len(board.AgentTypes)
		f.agent = board.AgentTypes[(i+delta+n)%n]
		f.model.Placeholder = modelPlaceholder(f.agent)
	}
}

// modelPlaceholder fits the model field from a 40 column screen up.
func modelPlaceholder(a board.AgentType) string {
	switch a {
	case "claude":
		return "default or claude-opus-5-5"
	case "ollama":
		return "default or e.g. qwen3:4b"
	}
	return "empty = agent default"
}

// click focuses the field at x, y (relative to the popup); a click on the
// focused priority or agent picks the next value.
func (f *editForm) click(x, y int) tea.Cmd {
	if x < 0 || x >= f.boxW {
		return nil
	}
	for _, s := range f.spans {
		if y < s.from || y >= s.end {
			continue
		}
		if s.field == f.focus {
			f.cycle(1) // only priority and agent cycle
			return nil
		}
		f.focus = s.field
		return f.focusCmd()
	}
	return nil
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
		case "up":
			if f.focus != fieldDescription || f.descAtTop() {
				return formEditing, f.moveFocus(-1)
			}
		case "down":
			if f.focus != fieldDescription || f.descAtBottom() {
				return formEditing, f.moveFocus(1)
			}
		}
		if f.focus == fieldPriority || f.focus == fieldAgent {
			switch k.String() {
			case "left", "h":
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
	top := 2 // border + padding
	if compact {
		top = 1
	}
	f.spans, f.boxW = f.spans[:0], w
	var b strings.Builder
	mark := func(i formField, lines int) {
		from := top + strings.Count(b.String(), "\n")
		f.spans = append(f.spans, fieldSpan{i, from, from + lines})
	}
	inputLines := 2 // label, then the input
	if compact {
		inputLines = 1
	}
	b.WriteString(titleStyle.Render(f.heading) + gap)
	mark(fieldTitle, inputLines)
	b.WriteString(input(fieldTitle, &f.title) + gap)
	mark(fieldDescription, 1+f.desc.Height())
	b.WriteString(label(fieldDescription) + "\n" + f.desc.View() + gap)
	mark(fieldPriority, 1)
	b.WriteString(label(fieldPriority) + choice(fieldPriority, priorityStyle(f.priority).Render(fmt.Sprintf("P%d", f.priority))) + "\n")
	mark(fieldAgent, 1)
	b.WriteString(label(fieldAgent) + choice(fieldAgent, agentName(f.agent)) + gap)
	mark(fieldModel, inputLines)
	b.WriteString(input(fieldModel, &f.model) + gap)
	mark(fieldLabels, inputLines)
	b.WriteString(input(fieldLabels, &f.labels) + gap)
	mark(fieldBranch, inputLines)
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
