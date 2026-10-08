// Package board is kAinban's kanban model: tickets in workflow columns, each
// of which may later be handed to an agent.
package board

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Status is the column a ticket is in.
type Status string

const (
	StatusBacklog    Status = "backlog"
	StatusInProgress Status = "in_progress"
	StatusReview     Status = "review"
	StatusDone       Status = "done"
	StatusBlocked    Status = "blocked" // side column: failed agents land here
)

// Column describes one board column.
type Column struct {
	Status Status `json:"status"`
	Name   string `json:"name"`
	Color  string `json:"color"` // hex, used for the header and selection
	Limit  int    `json:"limit"` // WIP limit; 0 = none
	// Side columns are outside the left-to-right workflow: moving skips
	// them, and moving out of one goes back to In Progress.
	Side bool `json:"side,omitempty"`
}

// DefaultColumns is the workflow every new board starts with.
func DefaultColumns() []Column {
	return []Column{
		{Status: StatusBacklog, Name: "Backlog", Color: "#89b4fa"},
		{Status: StatusInProgress, Name: "In Progress", Color: "#f9e2af", Limit: 3},
		{Status: StatusReview, Name: "Review", Color: "#cba6f7"},
		{Status: StatusDone, Name: "Done", Color: "#a6e3a1"},
		{Status: StatusBlocked, Name: "Blocked", Color: "#f38ba8", Side: true},
	}
}

// AgentType is the CLI a ticket's agent runs ("" = none chosen).
type AgentType string

// AgentAuto lets the orchestrator pick the agent when it starts one.
const AgentAuto AgentType = "auto"

// AgentTypes lists the selectable agents in cycling order ("" = none).
var AgentTypes = []AgentType{"", AgentAuto, "claude", "codex", "copilot", "antigravity"}

// AgentStatus is the state of an agent run.
type AgentStatus string

const (
	AgentNone      AgentStatus = "none"
	AgentRunning   AgentStatus = "running"
	AgentWaiting   AgentStatus = "waiting"
	AgentCompleted AgentStatus = "completed"
	AgentError     AgentStatus = "error"
	AgentStopped   AgentStatus = "stopped" // stopped by the user before it finished
)

// Finished reports whether the run is over (successfully or not).
func (s AgentStatus) Finished() bool {
	return s == AgentCompleted || s == AgentError || s == AgentStopped
}

// Active reports whether the run is starting or running.
func (s AgentStatus) Active() bool { return s == AgentWaiting || s == AgentRunning }

// Run is one agent run of a ticket, kept after it ends.
type Run struct {
	Agent      AgentType   `json:"agent"` // the agent that ran (auto resolved)
	Requested  AgentType   `json:"requested,omitempty"`
	Pod        string      `json:"pod,omitempty"`
	PodGone    bool        `json:"pod_gone,omitempty"` // pod deleted
	Status     AgentStatus `json:"status"`
	Output     string      `json:"output,omitempty"` // tail, saved when it ended
	StartedAt  time.Time   `json:"started_at"`
	FinishedAt *time.Time  `json:"finished_at,omitempty"`
}

// Live reports whether the run still has a pod.
func (r *Run) Live() bool { return r.Pod != "" && !r.PodGone }

// SetStatus updates the status and the finish time.
func (r *Run) SetStatus(s AgentStatus) {
	r.Status = s
	if s.Finished() && r.FinishedAt == nil {
		f := now().UTC()
		r.FinishedAt = &f
	}
}

// Ticket is one card.
type Ticket struct {
	ID          string    `json:"id"`
	Key         string    `json:"key"` // human-readable, e.g. KAI-12
	Title       string    `json:"title"`
	Description string    `json:"description,omitempty"`
	Status      Status    `json:"status"`
	Priority    int       `json:"priority"` // 1 (highest) .. 4
	Labels      []string  `json:"labels,omitempty"`
	Agent       AgentType `json:"agent,omitempty"` // chosen agent (may be auto)
	Runs        []*Run    `json:"runs,omitempty"`  // oldest first
	Branch      string    `json:"branch,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`

	// Single-run fields of older board files, migrated into Runs on load.
	LegacyStatus AgentStatus `json:"agent_status,omitempty"`
	LegacyPod    string      `json:"agent_pod,omitempty"`
	LegacyAgent  AgentType   `json:"agent_run,omitempty"`
	LegacyOutput string      `json:"agent_output,omitempty"`
}

// Current returns the latest run, or nil.
func (t *Ticket) Current() *Run {
	if len(t.Runs) == 0 {
		return nil
	}
	return t.Runs[len(t.Runs)-1]
}

// RunStatus is the latest run's status (AgentNone without runs).
func (t *Ticket) RunStatus() AgentStatus {
	if r := t.Current(); r != nil {
		return r.Status
	}
	return AgentNone
}

// LiveRuns returns the runs that still have a pod.
func (t *Ticket) LiveRuns() []*Run {
	var out []*Run
	for _, r := range t.Runs {
		if r.Live() {
			out = append(out, r)
		}
	}
	return out
}

// RunByPod finds the run of a pod.
func (t *Ticket) RunByPod(pod string) *Run {
	for _, r := range t.Runs {
		if r.Pod == pod {
			return r
		}
	}
	return nil
}

// StartRun appends a new run in the waiting state.
func (t *Ticket) StartRun() *Run {
	r := &Run{Agent: t.Agent, Requested: t.Agent, Status: AgentWaiting, StartedAt: now().UTC()}
	t.Runs = append(t.Runs, r)
	t.Touch()
	return r
}

// migrateLegacy turns the single-run fields of an older board into a Run.
func (t *Ticket) migrateLegacy() {
	if t.LegacyPod != "" || t.LegacyOutput != "" || (t.LegacyStatus != "" && t.LegacyStatus != AgentNone) {
		ag := t.LegacyAgent
		if ag == "" {
			ag = t.Agent
		}
		st := t.LegacyStatus
		if st == "" {
			st = AgentNone
		}
		t.Runs = append(t.Runs, &Run{Agent: ag, Requested: t.Agent, Pod: t.LegacyPod, Status: st,
			Output: t.LegacyOutput, StartedAt: t.UpdatedAt})
	}
	t.LegacyStatus, t.LegacyPod, t.LegacyAgent, t.LegacyOutput = "", "", "", ""
}

// PodState is what the cluster reports about an agent pod.
type PodState struct {
	Status AgentStatus
	Ticket string // ticket ID label
}

// Board is the whole state persisted by a Store.
type Board struct {
	Name string `json:"name"`
	// Project is the key prefix of the tickets (KAI in KAI-12).
	Project    string    `json:"project"`
	NextNumber int       `json:"next_number"` // never reused, even after deletes
	Columns    []Column  `json:"columns"`
	Tickets    []*Ticket `json:"tickets"`
}

// DefaultProject is the ticket key prefix of a board created without one.
const DefaultProject = "KAI"

// New returns an empty board with the default columns.
func New(name string) *Board {
	return &Board{Name: name, Project: DefaultProject, NextNumber: 1, Columns: DefaultColumns(), Tickets: []*Ticket{}}
}

// NormalizeProject turns a project ID into a ticket key prefix ("kai" -> "KAI").
func NormalizeProject(p string) string {
	return strings.ToUpper(strings.TrimSpace(p))
}

var now = time.Now

// Add creates a ticket in the first column.
func (b *Board) Add(title string) *Ticket {
	if b.NextNumber < 1 {
		b.NextNumber = 1
	}
	t := &Ticket{
		ID:        newID(),
		Key:       fmt.Sprintf("%s-%d", b.Project, b.NextNumber),
		Title:     title,
		Status:    b.Columns[0].Status,
		Priority:  3,
		CreatedAt: now().UTC(),
	}
	t.UpdatedAt = t.CreatedAt
	b.NextNumber++
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

// Move shifts a ticket by delta workflow columns (clamped), skipping side
// columns; from a side column it goes back to In Progress. It refuses to
// exceed the target column's WIP limit.
func (b *Board) Move(t *Ticket, delta int) error {
	from := b.ColumnIndex(t.Status)
	if from >= 0 && b.Columns[from].Side {
		return b.MoveTo(t, StatusInProgress)
	}
	to := from
	for step := 0; step != delta; {
		dir := 1
		if delta < 0 {
			dir = -1
		}
		next := to + dir
		for next >= 0 && next < len(b.Columns) && b.Columns[next].Side {
			next += dir
		}
		if next < 0 || next >= len(b.Columns) {
			break
		}
		to = next
		step += dir
	}
	if to == from {
		return nil
	}
	return b.MoveTo(t, b.Columns[to].Status)
}

// MoveTo puts a ticket into the column with status s, respecting its WIP
// limit.
func (b *Board) MoveTo(t *Ticket, s Status) error {
	if t.Status == s {
		return nil
	}
	i := b.ColumnIndex(s)
	if i < 0 {
		return fmt.Errorf("no %s column", s)
	}
	col := b.Columns[i]
	if col.Limit > 0 && len(b.Column(col.Status)) >= col.Limit {
		return fmt.Errorf("%s is at its WIP limit (%d)", col.Name, col.Limit)
	}
	t.Status = col.Status
	t.UpdatedAt = now().UTC()
	return nil
}

// Migrate upgrades a board loaded from an older file.
func (b *Board) Migrate() {
	b.EnsureDefaultColumns()
	for _, t := range b.Tickets {
		t.migrateLegacy()
	}
	if b.Project == "" {
		b.Project = DefaultProject
	}
	if b.NextNumber < 1 {
		b.NextNumber = 1
	}
	// Tickets from before keys existed get numbers in creation order.
	var unkeyed []*Ticket
	for _, t := range b.Tickets {
		if t.Key == "" {
			unkeyed = append(unkeyed, t)
		}
	}
	slices.SortStableFunc(unkeyed, func(x, y *Ticket) int { return x.CreatedAt.Compare(y.CreatedAt) })
	for _, t := range unkeyed {
		t.Key = fmt.Sprintf("%s-%d", b.Project, b.NextNumber)
		b.NextNumber++
	}
}

// EnsureDefaultColumns adds default columns missing from an older board.
func (b *Board) EnsureDefaultColumns() {
	for _, c := range DefaultColumns() {
		if b.ColumnIndex(c.Status) < 0 {
			b.Columns = append(b.Columns, c)
		}
	}
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
	if r := t.Current(); r != nil && r.Agent != "" && r.Agent != AgentAuto {
		return r.Agent
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
