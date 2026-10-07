package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

const claudeGoodTok = ClaudeTokenPrefix + "GOOD"

// TestHelperClaude acts as a fake `claude` binary when re-executed.
func TestHelperClaude(t *testing.T) {
	if os.Getenv("KAINBAN_FAKE_CLAUDE") != "1" {
		return
	}
	switch {
	case os.Getenv("ANTHROPIC_API_KEY") != "":
		fmt.Println("ANTHROPIC_API_KEY leaked into environment")
		os.Exit(2)
	case os.Getenv(ClaudeTokenKey) == claudeGoodTok:
		fmt.Println("OK")
		os.Exit(0)
	case os.Getenv(ClaudeTokenKey) == ClaudeTokenPrefix+"NET":
		fmt.Println("Connection error: getaddrinfo ENOTFOUND api.anthropic.com")
		os.Exit(1)
	default:
		fmt.Println("Invalid API key · Please run /login")
		os.Exit(1)
	}
}

func fakeClaude(t *testing.T) *claudeProvider {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-api-should-be-removed")
	return &claudeProvider{
		argv:     []string{os.Args[0], "-test.run=^TestHelperClaude$", "--"},
		extraEnv: []string{"KAINBAN_FAKE_CLAUDE=1"},
	}
}

func TestClaudeCheck(t *testing.T) {
	fixedNow(t, testNow)
	ann := func(d time.Duration) map[string]string {
		return map[string]string{AnnotationExpiresAt: formatExpiry(testNow.Add(d))}
	}
	year := 300 * 24 * time.Hour
	tests := []struct {
		name   string
		store  *fakeStore
		live   bool
		want   State
		detail string
	}{
		{"missing", newFakeStore(), false, StateMissing, ""},
		{"store error", &fakeStore{err: errors.New("rbac")}, false, StateUnknown, "rbac"},
		{"malformed", newFakeStore().with(ClaudeSecretName, ClaudeTokenKey, "sk-ant-api03-x", nil), false, StateInvalid, "malformed"},
		{"valid", newFakeStore().with(ClaudeSecretName, ClaudeTokenKey, claudeGoodTok, ann(year)), false, StateValid, "expires in 300d"},
		{"soon", newFakeStore().with(ClaudeSecretName, ClaudeTokenKey, claudeGoodTok, ann(10*24*time.Hour)), false, StateValid, "renew soon"},
		{"expired", newFakeStore().with(ClaudeSecretName, ClaudeTokenKey, claudeGoodTok, ann(-24*time.Hour)), true, StateInvalid, "expired"},
		{"no annotation", newFakeStore().with(ClaudeSecretName, ClaudeTokenKey, claudeGoodTok, nil), false, StateValid, "expiry unknown"},
		{"live ok", newFakeStore().with(ClaudeSecretName, ClaudeTokenKey, claudeGoodTok, ann(year)), true, StateValid, "expires in 300d"},
		{"live rejected", newFakeStore().with(ClaudeSecretName, ClaudeTokenKey, ClaudeTokenPrefix+"BAD", ann(year)), true, StateInvalid, "Invalid API key"},
		{"live network", newFakeStore().with(ClaudeSecretName, ClaudeTokenKey, ClaudeTokenPrefix+"NET", ann(year)), true, StateUnknown, "Connection error"},
	}
	p := fakeClaude(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := p.Check(context.Background(), tt.store, tt.live)
			if st.State != tt.want || !strings.Contains(st.Detail, tt.detail) {
				t.Fatalf("got %v %q, want %v containing %q", st.State, st.Detail, tt.want, tt.detail)
			}
		})
	}
}

func TestClaudeLiveNotInstalled(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	st := NewClaude().Check(context.Background(),
		newFakeStore().with(ClaudeSecretName, ClaudeTokenKey, claudeGoodTok, nil), true)
	if st.State != StateUnknown || st.Detail != "claude CLI not found" {
		t.Fatalf("got %v %q", st.State, st.Detail)
	}
}

func TestClaudeEnv(t *testing.T) {
	env := claudeEnv([]string{"PATH=/bin", "ANTHROPIC_API_KEY=x", ClaudeTokenKey + "=old", "HOME=/h"}, "new")
	got := strings.Join(env, " ")
	if got != "PATH=/bin HOME=/h "+ClaudeTokenKey+"=new" {
		t.Fatalf("env = %q", got)
	}
}

func TestClaudeFlow(t *testing.T) {
	fixedNow(t, testNow)
	f := NewClaude().NewFlow()
	ctx := context.Background()
	defer f.Close()
	s, err := f.Next(ctx, "")
	if err != nil || s.Kind != StepExec || len(s.Command) != 5 || s.Command[0] != "sh" ||
		!strings.Contains(s.Command[2], "claude setup-token") {
		t.Fatalf("%+v %v", s, err)
	}
	typescript := s.Command[4]
	if _, err := os.Stat(typescript); err != nil {
		t.Fatalf("typescript file: %v", err)
	}
	s, err = f.Next(ctx, "") // nothing captured: plain paste step
	if err != nil || s.Kind != StepInput || !s.Secret || s.Value != "" || !strings.Contains(s.Body, "ANTHROPIC_API_KEY") {
		t.Fatalf("%+v %v", s, err)
	}
	if _, err := os.Stat(typescript); !os.IsNotExist(err) {
		t.Fatalf("typescript not removed: %v", err)
	}
	if _, err := f.Next(ctx, "sk-ant-api03-nope"); err == nil {
		t.Fatal("want prefix error")
	}
	s, err = f.Next(ctx, "  "+ClaudeTokenPrefix+"abc\ndef  ")
	if err != nil || s.Kind != StepDone {
		t.Fatalf("%+v %v", s, err)
	}
	sec := s.Secrets[ClaudeSecretName]
	if string(sec.Data[ClaudeTokenKey]) != ClaudeTokenPrefix+"abcdef" {
		t.Fatalf("token = %q", sec.Data[ClaudeTokenKey])
	}
	if sec.Annotations[AnnotationExpiresAt] != "2027-10-08T12:00:00Z" {
		t.Fatalf("expires-at = %q", sec.Annotations[AnnotationExpiresAt])
	}
}

func TestRegistry(t *testing.T) {
	var ids []string
	for _, p := range All() {
		ids = append(ids, p.ID())
		if len(p.SecretNames()) == 0 || p.Title() == "" {
			t.Errorf("%s: incomplete", p.ID())
		}
	}
	if got := strings.Join(ids, ","); got != "claude,codex,github,copilot,antigravity" {
		t.Fatalf("order = %s", got)
	}
}

func TestClaudeFlowCapturesToken(t *testing.T) {
	fixedNow(t, testNow)
	f := NewClaude().NewFlow()
	defer f.Close()
	ctx := context.Background()
	s, _ := f.Next(ctx, "")
	tok := ClaudeTokenPrefix + strings.Repeat("Ab3_-", 20)
	out := "\x1b[32mLong-lived token created:\x1b[0m\r\n\r\n" + tok + "\r\n\r\nStore this token securely.\r\n"
	if err := os.WriteFile(s.Command[4], []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := f.Next(ctx, "")
	if err != nil || s.Kind != StepInput || s.Value != tok || strings.Contains(s.Body, tok) {
		t.Fatalf("value=%q body=%q err=%v", s.Value, s.Body, err)
	}
}

func TestExtractClaudeToken(t *testing.T) {
	tok := ClaudeTokenPrefix + strings.Repeat("xY9-_", 18)
	half := len(tok) / 2
	cases := []struct{ name, in, want string }{
		{"plain", "token:\n" + tok + "\n\nStore this token securely.", tok},
		{"ansi", "\x1b[1m\x1b[38;5;10m" + tok + "\x1b[39m\x1b[22m\r\n", tok},
		{"wrapped", "  " + tok[:half] + "\r\n  " + tok[half:] + "  \r\n\r\nStore this", tok},
		{"same line text", tok + " (valid for 1 year)", tok},
		{"last wins", ClaudeTokenPrefix + strings.Repeat("old00", 18) + "\n" + tok + "\n", tok},
		{"osc title", "\x1b]0;claude\x07" + tok + "\n", tok},
		{"none", "Error: login failed\n", ""},
		{"too short", ClaudeTokenPrefix + "abc\n", ""},
	}
	for _, c := range cases {
		if got := extractClaudeToken(c.in); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
