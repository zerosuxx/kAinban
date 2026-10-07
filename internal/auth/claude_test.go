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
	s, err := f.Next(ctx, "")
	if err != nil || s.Kind != StepExec || strings.Join(s.Command, " ") != "claude setup-token" {
		t.Fatalf("%+v %v", s, err)
	}
	s, err = f.Next(ctx, "")
	if err != nil || s.Kind != StepInput || !s.Secret || !strings.Contains(s.Body, "ANTHROPIC_API_KEY") {
		t.Fatalf("%+v %v", s, err)
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
