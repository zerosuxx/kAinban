package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/zerosuxx/kainban/internal/auth"
)

// checkResult is one row of `kainban auth check`.
type checkResult struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	State     string     `json:"state"`
	Detail    string     `json:"detail,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// runChecks runs every provider's Check concurrently and returns the results
// in provider order.
func runChecks(ctx context.Context, providers []auth.Provider, store auth.Store, live bool) []checkResult {
	out := make([]checkResult, len(providers))
	var wg sync.WaitGroup
	for i, p := range providers {
		wg.Go(func() {
			st := p.Check(ctx, store, live)
			out[i] = checkResult{
				ID:        p.ID(),
				Title:     p.Title(),
				State:     st.State.String(),
				Detail:    st.Detail,
				ExpiresAt: st.ExpiresAt,
			}
		})
	}
	wg.Wait()
	return out
}

// checkExitCode is 0 when every provider is valid or missing (missing only
// when requireAll is false), else 1.
func checkExitCode(results []checkResult, requireAll bool) int {
	for _, r := range results {
		switch r.State {
		case auth.StateValid.String():
		case auth.StateMissing.String():
			if requireAll {
				return 1
			}
		default:
			return 1
		}
	}
	return 0
}

// writeCheckTable prints the results as an aligned table.
func writeCheckTable(w io.Writer, results []checkResult, now time.Time) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATE\tEXPIRES\tDETAIL")
	for _, r := range results {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.ID, r.State, formatExpiry(r.ExpiresAt, now), r.Detail)
	}
	return tw.Flush()
}

// writeCheckJSON prints the results as a JSON array.
func writeCheckJSON(w io.Writer, results []checkResult) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(results)
}

func formatExpiry(t *time.Time, now time.Time) string {
	if t == nil {
		return "-"
	}
	s := t.UTC().Format(time.RFC3339)
	d := t.Sub(now)
	if d < 0 {
		return s + " (expired)"
	}
	return s + " (in " + humanDuration(d) + ")"
}

func humanDuration(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
}
