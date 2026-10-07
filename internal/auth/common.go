package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Provider IDs (also used as the provider label value).
const (
	ProviderClaude      = "claude"
	ProviderCodex       = "codex"
	ProviderGitHub      = "github"
	ProviderCopilot     = "copilot"
	ProviderAntigravity = "antigravity"
)

// httpTimeout bounds every live API call.
const httpTimeout = 10 * time.Second

// renewWarning is how close to expiry a token gets flagged in Detail.
const renewWarning = 30 * 24 * time.Hour

// nowFunc is replaced in tests.
var nowFunc = time.Now

func defaultHTTPClient() *http.Client {
	return &http.Client{Timeout: httpTimeout}
}

// humanDuration renders d coarsely: "9d", "5h", "12m".
func humanDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
}

// expiryDetail returns "expires in 9d" or "expired 2d ago".
func expiryDetail(exp, now time.Time) string {
	if !exp.After(now) {
		return "expired " + humanDuration(now.Sub(exp)) + " ago"
	}
	return "expires in " + humanDuration(exp.Sub(now))
}

// expiryStatus turns a known expiry into a Status (Valid or Invalid). When
// warn is set, tokens closer than renewWarning to expiry get a hint.
func expiryStatus(exp time.Time, warn bool) Status {
	now := nowFunc()
	e := exp
	st := Status{State: StateValid, Detail: expiryDetail(exp, now), ExpiresAt: &e}
	if !exp.After(now) {
		st.State = StateInvalid
	} else if warn && exp.Sub(now) < renewWarning {
		st.Detail += " (renew soon)"
	}
	return st
}

// annotationExpiry parses the expires-at annotation, if present.
func annotationExpiry(s *Secret) (time.Time, bool) {
	if s == nil || s.Annotations == nil {
		return time.Time{}, false
	}
	v := s.Annotations[AnnotationExpiresAt]
	if v == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func formatExpiry(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// loadSecret fetches name and returns the value under key. On failure it
// returns a non-nil Status to report as-is.
func loadSecret(ctx context.Context, store Store, name, key string) (*Secret, string, *Status) {
	s, err := store.Get(ctx, name)
	if errors.Is(err, ErrNotFound) {
		return nil, "", &Status{State: StateMissing, Detail: "secret " + name + " not found"}
	}
	if err != nil {
		return nil, "", &Status{State: StateUnknown, Detail: "cannot read secret " + name + ": " + err.Error()}
	}
	if s == nil || len(s.Data[key]) == 0 {
		return s, "", &Status{State: StateInvalid, Detail: "secret " + name + " has no " + key}
	}
	return s, string(s.Data[key]), nil
}

// normalizeToken removes all whitespace (pasted tokens often carry line
// breaks from terminal wrapping).
func normalizeToken(in string) string {
	return strings.Join(strings.Fields(in), "")
}

// apiResponse is the part of an HTTP response the validators need.
type apiResponse struct {
	Status int
	Header http.Header
	Body   []byte
}

// apiGet performs a GET with the given headers and a bounded timeout. The
// returned error never contains header values.
func apiGet(ctx context.Context, client *http.Client, rawURL string, header map[string]string) (*apiResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach %s: %w", hostOf(rawURL), unwrapURLError(err))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return &apiResponse{Status: resp.StatusCode, Header: resp.Header, Body: body}, nil
}

func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return rawURL
	}
	return u.Host
}
