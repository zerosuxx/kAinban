package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	ghGoodTok = "github_pat_GOOD"
	ghNoExp   = "gho_NOEXP"
)

// gitHubServer emulates GET /user.
func gitHubServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user" || r.Header.Get("X-GitHub-Api-Version") != "2022-11-28" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		switch r.Header.Get("Authorization") {
		case "Bearer " + ghGoodTok:
			w.Header().Set(gitHubExpiryHeader, "2026-12-31 00:00:00 UTC")
			_, _ = w.Write([]byte(`{"login":"octocat"}`))
		case "Bearer " + ghNoExp:
			_, _ = w.Write([]byte(`{"login":"octocat"}`))
		case "Bearer ghu_RATELIMITED":
			w.Header().Set("x-ratelimit-remaining", "0")
			http.Error(w, "rate limited", http.StatusForbidden)
		case "Bearer ghu_BROKEN":
			http.Error(w, "oops", http.StatusBadGateway)
		default:
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestValidateGitHubToken(t *testing.T) {
	srv := gitHubServer(t)
	exp := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		tok         string
		wantLogin   string
		wantExp     *time.Time
		wantStatus  int
		wantInvalid bool
	}{
		{"ok with expiry", ghGoodTok, "octocat", &exp, 0, false},
		{"ok no expiry", ghNoExp, "octocat", nil, 0, false},
		{"401", "github_pat_BAD", "", nil, 401, true},
		{"rate limited", "ghu_RATELIMITED", "", nil, 403, false},
		{"5xx", "ghu_BROKEN", "", nil, 502, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := validateGitHubToken(context.Background(), srv.Client(), srv.URL, tt.tok)
			if tt.wantStatus != 0 {
				var ae *gitHubAPIError
				if !errors.As(err, &ae) || ae.Status != tt.wantStatus || ae.Invalid != tt.wantInvalid {
					t.Fatalf("err = %#v", err)
				}
				if strings.Contains(err.Error(), tt.tok) {
					t.Fatal("error leaks token")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if u.Login != tt.wantLogin {
				t.Fatalf("login = %q", u.Login)
			}
			if (u.ExpiresAt == nil) != (tt.wantExp == nil) || (u.ExpiresAt != nil && !u.ExpiresAt.Equal(*tt.wantExp)) {
				t.Fatalf("expires = %v, want %v", u.ExpiresAt, tt.wantExp)
			}
		})
	}
	srv.Close()
	if _, err := validateGitHubToken(context.Background(), srv.Client(), srv.URL, ghGoodTok); err == nil {
		t.Fatal("want network error")
	} else if strings.Contains(err.Error(), ghGoodTok) {
		t.Fatal("error leaks token")
	}
}

func TestParseGitHubExpiry(t *testing.T) {
	want := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	for _, in := range []string{
		"2026-12-31 00:00:00 UTC",
		" 2026-12-31 00:00:00 UTC ",
		"2026-12-31 01:00:00 +0100",
		"2026-12-31 00:00:00",
		"2026-12-31T00:00:00Z",
		"2026-12-31",
	} {
		got, ok := parseGitHubExpiry(in)
		if !ok || !got.Equal(want) {
			t.Errorf("%q: got %v %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "never", "31/12/2026"} {
		if _, ok := parseGitHubExpiry(in); ok {
			t.Errorf("%q: want failure", in)
		}
	}
}

func TestGitHubCheck(t *testing.T) {
	fixedNow(t, testNow)
	srv := gitHubServer(t)
	ann := func(exp time.Time) map[string]string {
		return map[string]string{AnnotationExpiresAt: formatExpiry(exp), AnnotationGitHubLogin: "octocat"}
	}
	far := testNow.Add(84 * 24 * time.Hour)
	tests := []struct {
		name   string
		store  *fakeStore
		live   bool
		want   State
		detail string
	}{
		{"missing", newFakeStore(), false, StateMissing, ""},
		{"store error", &fakeStore{err: errors.New("forbidden")}, false, StateUnknown, "forbidden"},
		{"local valid", newFakeStore().with(GitHubSecretName, GitHubTokenKey, ghGoodTok, ann(far)), false, StateValid, "octocat, expires in 84d"},
		{"local soon", newFakeStore().with(GitHubSecretName, GitHubTokenKey, ghGoodTok, ann(testNow.Add(5*24*time.Hour))), false, StateValid, "renew soon"},
		{"local expired", newFakeStore().with(GitHubSecretName, GitHubTokenKey, ghGoodTok, ann(testNow.Add(-time.Hour))), false, StateInvalid, "expired"},
		{"malformed", newFakeStore().with(GitHubSecretName, GitHubTokenKey, "hunter2", nil), false, StateInvalid, "unrecognised"},
		{"no annotation", newFakeStore().with(GitHubSecretName, GitHubTokenKey, ghGoodTok, nil), false, StateValid, "no expiry"},
		{"live valid", newFakeStore().with(GitHubSecretName, GitHubTokenKey, ghGoodTok, nil), true, StateValid, "octocat, expires in 83d"},
		{"live no expiry", newFakeStore().with(GitHubSecretName, GitHubTokenKey, ghNoExp, nil), true, StateValid, "octocat, no expiry"},
		{"live 401", newFakeStore().with(GitHubSecretName, GitHubTokenKey, "ghp_REVOKED", ann(far)), true, StateInvalid, "HTTP 401 from 127.0.0.1"},
		{"live 5xx", newFakeStore().with(GitHubSecretName, GitHubTokenKey, "ghu_BROKEN", nil), true, StateUnknown, "HTTP 502"},
	}
	p := newGitHub(srv.Client(), srv.URL)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := p.Check(context.Background(), tt.store, tt.live)
			if st.State != tt.want || !strings.Contains(st.Detail, tt.detail) {
				t.Fatalf("got %v %q, want %v containing %q", st.State, st.Detail, tt.want, tt.detail)
			}
		})
	}
	srv.Close()
	st := p.Check(context.Background(), newFakeStore().with(GitHubSecretName, GitHubTokenKey, ghGoodTok, ann(far)), true)
	if st.State != StateUnknown || st.ExpiresAt == nil {
		t.Fatalf("network error: got %v %q", st.State, st.Detail)
	}
}

func TestGitHubFlow(t *testing.T) {
	fixedNow(t, testNow)
	srv := gitHubServer(t)
	f := newGitHub(srv.Client(), srv.URL).NewFlow()
	defer f.Close()
	ctx := context.Background()
	step, err := f.Next(ctx, "")
	if err != nil || step.Kind != StepInput || !step.Secret {
		t.Fatalf("first step: %+v %v", step, err)
	}
	if _, err := f.Next(ctx, "nope"); err == nil {
		t.Fatal("want format error")
	}
	if _, err := f.Next(ctx, "github_pat_BAD"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want 401 error, got %v", err)
	}
	step, err = f.Next(ctx, "  github_pat_\nGOOD \n")
	if err != nil || step.Kind != StepDone {
		t.Fatalf("done: %+v %v", step, err)
	}
	s := step.Secrets[GitHubSecretName]
	if string(s.Data[GitHubTokenKey]) != ghGoodTok {
		t.Fatalf("token = %q", s.Data[GitHubTokenKey])
	}
	if s.Annotations[AnnotationGitHubLogin] != "octocat" || s.Annotations[AnnotationExpiresAt] != "2026-12-31T00:00:00Z" {
		t.Fatalf("annotations = %v", s.Annotations)
	}
	if strings.Contains(step.Body, ghGoodTok) {
		t.Fatal("body leaks token")
	}
}

func TestCopilotTokens(t *testing.T) {
	srv := gitHubServer(t)
	p := newCopilot(srv.Client(), srv.URL)
	if p.ID() != ProviderCopilot || p.SecretNames()[0] != CopilotSecretName {
		t.Fatal("wrong identity")
	}
	for tok, ok := range map[string]bool{
		"github_pat_x": true, "gho_x": true, "ghu_x": true,
		"ghp_x": false, "ghs_x": false, "github_pat_": false,
	} {
		if err := p.checkFormat(tok); (err == nil) != ok {
			t.Errorf("%s: err = %v", tok, err)
		}
	}
	f := p.NewFlow()
	ctx := context.Background()
	_, _ = f.Next(ctx, "")
	if _, err := f.Next(ctx, "ghp_CLASSIC"); err == nil || !strings.Contains(err.Error(), "classic") {
		t.Fatalf("want classic rejection, got %v", err)
	}
	step, err := f.Next(ctx, ghNoExp)
	if err != nil || step.Kind != StepDone {
		t.Fatalf("%+v %v", step, err)
	}
	s := step.Secrets[CopilotSecretName]
	if string(s.Data[CopilotTokenKey]) != ghNoExp {
		t.Fatal("wrong key")
	}
	if _, ok := s.Annotations[AnnotationExpiresAt]; ok {
		t.Fatal("unexpected expires-at")
	}
	st := p.Check(ctx, newFakeStore().with(CopilotSecretName, CopilotTokenKey, "ghp_CLASSIC", nil), false)
	if st.State != StateInvalid {
		t.Fatalf("classic token: %v", st.State)
	}
}

func TestCopilotPrefill(t *testing.T) {
	orig := ghAuthToken
	t.Cleanup(func() { ghAuthToken = orig })
	ctx := context.Background()
	gho := "gho_" + strings.Repeat("a", 36)

	cases := []struct {
		name, ghOut, secret, want, source string
	}{
		{"gh cli wins", gho, "gho_other0000000000000000000000000000", gho, "`gh auth token`"},
		{"from github secret", "", gho, gho, "Secret " + GitHubSecretName},
		{"fine-grained not reused", "", "github_pat_" + strings.Repeat("b", 30), "", ""},
		{"gh pat ignored", "github_pat_x", "", "", ""},
		{"nothing", "", "", "", ""},
	}
	for _, c := range cases {
		ghAuthToken = func(context.Context) string { return c.ghOut }
		store := newFakeStore()
		if c.secret != "" {
			store.with(GitHubSecretName, GitHubTokenKey, c.secret, nil)
		}
		tok, source := copilotPrefill(ctx, store)
		if tok != c.want || source != c.source {
			t.Errorf("%s: got %q from %q, want %q from %q", c.name, tok, source, c.want, c.source)
		}
	}

	// The flow shows the pre-filled token as the input's value, never in the body.
	ghAuthToken = func(context.Context) string { return "" }
	f := NewCopilot().NewFlow()
	f.(StoreAware).UseStore(newFakeStore().with(GitHubSecretName, GitHubTokenKey, gho, nil))
	s, err := f.Next(ctx, "")
	if err != nil || s.Value != gho || strings.Contains(s.Body, gho) || !strings.Contains(s.Body, "Pre-filled") {
		t.Fatalf("value=%q body=%q err=%v", s.Value, s.Body, err)
	}
}

func TestGitHubDeviceLoginFlow(t *testing.T) {
	orig := ghAuthToken
	t.Cleanup(func() { ghAuthToken = orig })
	ctx := context.Background()
	gho := "gho_" + strings.Repeat("c", 36)

	loggedIn := false
	ghAuthToken = func(context.Context) string {
		if loggedIn {
			return gho
		}
		return ""
	}
	f := NewGitHub().NewFlow()
	s, err := f.Next(ctx, "")
	if err != nil || s.Kind != StepInput || s.Value != "" || !strings.Contains(s.Body, "gh auth login") {
		t.Fatalf("first step: %+v %v", s, err)
	}
	// Empty input: hand out `gh auth login` with the env tokens cleared.
	s, err = f.Next(ctx, "  ")
	if err != nil || s.Kind != StepExec || s.Command[0] != "gh" || !slices.Contains(s.Env, "GH_TOKEN=") {
		t.Fatalf("exec step: %+v %v", s, err)
	}
	loggedIn = true
	s, err = f.Next(ctx, "")
	if err != nil || s.Kind != StepInput || s.Value != gho || strings.Contains(s.Body, gho) {
		t.Fatalf("after login: value=%q body=%q err=%v", s.Value, s.Body, err)
	}
	// After one login attempt an empty input is an error, not another login.
	if _, err := f.Next(ctx, ""); err == nil {
		t.Fatal("empty input after login should fail format check")
	}

	// Login that leaves no token: input step explains it.
	loggedIn = false
	f = NewGitHub().NewFlow()
	f.Next(ctx, "")
	f.Next(ctx, "")
	s, _ = f.Next(ctx, "")
	if s.Kind != StepInput || s.Value != "" || !strings.Contains(s.Body, "did not leave a token") {
		t.Fatalf("failed login: %+v", s)
	}

	// Already logged in: pre-filled right away.
	loggedIn = true
	s, _ = NewGitHub().NewFlow().Next(ctx, "")
	if s.Value != gho {
		t.Fatalf("prefill: %q", s.Value)
	}
}
