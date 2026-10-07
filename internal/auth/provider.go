// Package auth defines the CLI credentials kAinban bootstraps for its agents
// and how each one is obtained, stored and validated.
//
// A Provider owns one or more Kubernetes Secrets. The TUI (internal/tui)
// shows every provider's Status, lets the user run its Flow or skip it, and
// writes the Flow's result through a Store (internal/kube).
package auth

import (
	"context"
	"errors"
	"time"
)

// Labels and annotations put on every Secret kAinban writes.
const (
	LabelPartOf   = "app.kubernetes.io/part-of"
	LabelProvider = "kainban.io/provider"
	PartOfValue   = "kainban"

	AnnotationUpdatedAt = "kainban.io/updated-at" // RFC 3339
	AnnotationExpiresAt = "kainban.io/expires-at" // RFC 3339, when known
)

// ErrNotFound is returned by Store.Get when the Secret does not exist.
var ErrNotFound = errors.New("secret not found")

// Secret is the provider-facing view of a Kubernetes Secret.
type Secret struct {
	Data        map[string][]byte
	Annotations map[string]string
}

// Store reads and writes Secrets in kAinban's namespace.
type Store interface {
	// Get returns ErrNotFound (possibly wrapped) when the Secret is missing.
	Get(ctx context.Context, name string) (*Secret, error)
	// Put creates or replaces the Secret. The store adds the part-of and
	// provider labels and the updated-at annotation.
	Put(ctx context.Context, provider, name string, s *Secret) error
}

// State summarises a provider's credentials.
type State int

const (
	StateMissing State = iota // no Secret
	StateValid                // present and validated
	StateInvalid              // present but rejected, expired or malformed
	StateUnknown              // present, but validation could not run (e.g. network error)
)

func (s State) String() string {
	switch s {
	case StateMissing:
		return "missing"
	case StateValid:
		return "valid"
	case StateInvalid:
		return "invalid"
	default:
		return "unknown"
	}
}

// Status is the result of Provider.Check.
type Status struct {
	State     State
	Detail    string     // one line for the TUI, e.g. "expires in 9 days" or the API error
	ExpiresAt *time.Time // when known
}

// StepKind tells the TUI how to render a Step.
type StepKind int

const (
	// StepInput shows Title/Body and a text input; the entered value is
	// passed to the next Flow.Next call.
	StepInput StepKind = iota
	// StepExec hands the terminal to Cmd (tea.ExecProcess), then calls
	// Flow.Next with an empty input.
	StepExec
	// StepDone ends the flow; Secrets are written through the Store.
	StepDone
)

// Step is one screen of a Flow.
type Step struct {
	Kind  StepKind
	Title string
	Body  string // instructions; may contain URLs the user must copy

	// StepInput
	Prompt      string
	Placeholder string
	Secret      bool   // mask the input (tokens, keys)
	Value       string // pre-filled value (e.g. a token captured from a command)

	// StepExec
	Command []string
	Env     []string // appended to os.Environ()

	// StepDone: Secret name -> content to write.
	Secrets map[string]*Secret
}

// Flow walks the user through obtaining credentials. Next may block (it is
// called from a tea.Cmd, with a spinner); input is the value entered for the
// previous StepInput, or "" after a StepExec and on the first call. Returning
// an error shows it and lets the user retry the same step.
type Flow interface {
	Next(ctx context.Context, input string) (Step, error)
	// Close releases resources (e.g. a background `codex login`) when the
	// flow is finished, skipped or aborted.
	Close() error
}

// Provider is one CLI whose credentials kAinban manages.
type Provider interface {
	ID() string    // stable, e.g. "codex"; used in labels and flags
	Title() string // e.g. "Codex (ChatGPT)"
	// SecretNames lists every Secret this provider owns, primary first.
	SecretNames() []string
	// Check reports whether the credentials exist and are valid. With live
	// set it may call the vendor's API; otherwise only local checks (format,
	// expiry) run.
	Check(ctx context.Context, store Store, live bool) Status
	NewFlow() Flow
}
