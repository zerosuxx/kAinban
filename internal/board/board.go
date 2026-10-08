// Package board is kAinban's kanban model: tickets in workflow columns, each
// of which may later be handed to an agent.
package board

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"time"
)

// Status is the column a ticket is in.
type Status string

const (
	StatusBacklog    Status = "backlog"
	StatusInProgress Status = "in_progress"
	StatusReview     Status = "review"
	StatusDone       Status = "done"
)

// Column describes one board column.
type Column struct {
	Status Status `json:"status"`
	Name   string `json:"name"`
	Color  string `json:"color"` // hex, used for the header and selection
	Limit  int    `json:"limit"` // WIP limit; 0 = none
}

// DefaultColumns is the workflow every new board starts with.
func DefaultColumns() []Column {
	return []Column{
		{Status: StatusBacklog, Name: "Backlog", Color: "#89b4fa"},
		{Status: StatusInProgress, Name: "In Progress", Color: "#f9e2af", Limit: 3},
		{Status: StatusReview, Name: "Review", Color: "#cba6f7"},
		{Status: StatusDone, Name: "Done", Color: "#a6e3a1"},
	}
}

// AgentType is the CLI a ticket's agent runs ("" = none chosen).
type AgentType string

// AgentAuto lets the orchestrator pick the agent when it starts one.
const AgentAuto AgentType = "auto"

// AgentTypes lists the selectable agents in cycling order ("" = none).
var AgentTypes = []AgentType{"", AgentAuto, "claude", "codex", "copilot", "antigravity"}

// AgentStatus is the state of a ticket's agent.
type AgentStatus string

const (
	AgentNone      AgentStatus = "none"
	AgentRunning   AgentStatus = "running"
	AgentWaiting   AgentStatus = "waiting"
	AgentCompleted AgentStatus = "completed"
	AgentError     AgentStatus = "error"
)

// Ticket is one card.
type Ticket struct {
	ID          string      `json:"id"`
	Title       string      `json:"title"`
	Description string      `json:"description,omitempty"`
	Status      Status      `json:"status"`
	Priority    int         `json:"priority"` // 1 (highest) .. 4
	Labels      []string    `json:"labels,omitempty"`
	Agent       AgentType   `json:"agent,omitempty"`
	AgentStatus AgentStatus `json:"agent_status"`
	AgentPod    string      `json:"agent_pod,omitempty"` // pod running the agent
	AgentRun    AgentType   `json:"agent_run,omitempty"` // agent actually started (resolves auto)
	Branch      string      `json:"branch,omitempty"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
}

// Board is the whole state persisted by a Store.
type Board struct {
	Name    string    `json:"name"`
	Columns []Column  `json:"columns"`
	Tickets []*Ticket `json:"tickets"`
}

// New returns an empty board with the default columns.
func New(name string) *Board {
	return &Board{Name: name, Columns: DefaultColumns(), Tickets: []*Ticket{}}
}

var now = time.Now

// Add creates a ticket in the first column.
func (b *Board) Add(title string) *Ticket {
	t := &Ticket{
		ID:          newID(),
		Title:       title,
		Status:      b.Columns[0].Status,
		Priority:    3,
		AgentStatus: AgentNone,
		CreatedAt:   now().UTC(),
	}
	t.UpdatedAt = t.CreatedAt
	b.Tickets = append(b.Tickets, t)
	return t
}

// Column returns the tickets in a column, in board order.
func (b *Board) Column(s Status) []*Ticket {
	var out []*Ticket
	for _, t := range b.Tickets {
		if t.Status == s {
			out = append(out, t)
		}
	}
	return out
}

// ColumnIndex returns the index of the column with status s, or -1.
func (b *Board) ColumnIndex(s Status) int {
	return slices.IndexFunc(b.Columns, func(c Column) bool { return c.Status == s })
}

// Move shifts a ticket by delta columns (clamped). It refuses to exceed the
// target column's WIP limit.
func (b *Board) Move(t *Ticket, delta int) error {
	from := b.ColumnIndex(t.Status)
	to := min(max(from+delta, 0), len(b.Columns)-1)
	if to == from {
		return nil
	}
	col := b.Columns[to]
	if col.Limit > 0 && len(b.Column(col.Status)) >= col.Limit {
		return fmt.Errorf("%s is at its WIP limit (%d)", col.Name, col.Limit)
	}
	t.Status = col.Status
	t.UpdatedAt = now().UTC()
	return nil
}

// Delete removes a ticket.
func (b *Board) Delete(t *Ticket) {
	b.Tickets = slices.DeleteFunc(b.Tickets, func(x *Ticket) bool { return x.ID == t.ID })
}

// CycleAgent switches the ticket to the next agent type.
func (t *Ticket) CycleAgent() {
	i := slices.Index(AgentTypes, t.Agent)
	t.Agent = AgentTypes[(i+1)%len(AgentTypes)]
	t.UpdatedAt = now().UTC()
}

// EffectiveAgent is the agent that runs (or would run) for the ticket.
func (t *Ticket) EffectiveAgent() AgentType {
	if t.AgentRun != "" {
		return t.AgentRun
	}
	return t.Agent
}

// CyclePriority switches the ticket to the next priority (1..4).
func (t *Ticket) CyclePriority() {
	t.Priority = t.Priority%4 + 1
	t.UpdatedAt = now().UTC()
}

// Touch marks the ticket as modified.
func (t *Ticket) Touch() { t.UpdatedAt = now().UTC() }

func newID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}
