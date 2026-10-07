package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Claude Code credentials: a 1-year OAuth token from `claude setup-token`.
const (
	ClaudeSecretName  = "kainban-claude"
	ClaudeTokenKey    = "CLAUDE_CODE_OAUTH_TOKEN"
	ClaudeTokenPrefix = "sk-ant-oat01-"

	claudeTokenLifetime = 365 * 24 * time.Hour
	claudeLiveTimeout   = 60 * time.Second
)

type claudeProvider struct {
	// argv runs the claude CLI; nil means look up "claude" on PATH.
	argv []string
	// extraEnv is appended to the live-check environment (tests).
	extraEnv []string
}

// NewClaude returns the Claude Code provider.
func NewClaude() Provider { return &claudeProvider{} }

func (p *claudeProvider) ID() string            { return ProviderClaude }
func (p *claudeProvider) Title() string         { return "Claude Code" }
func (p *claudeProvider) SecretNames() []string { return []string{ClaudeSecretName} }
func (p *claudeProvider) NewFlow() Flow         { return &claudeFlow{} }

func validateClaudeToken(tok string) error {
	if !strings.HasPrefix(tok, ClaudeTokenPrefix) || len(tok) <= len(ClaudeTokenPrefix) {
		return fmt.Errorf("token must start with %s", ClaudeTokenPrefix)
	}
	return nil
}

func (p *claudeProvider) Check(ctx context.Context, store Store, live bool) Status {
	s, tok, st := loadSecret(ctx, store, ClaudeSecretName, ClaudeTokenKey)
	if st != nil {
		return *st
	}
	if validateClaudeToken(tok) != nil {
		return Status{State: StateInvalid, Detail: "malformed token (expected " + ClaudeTokenPrefix + "...)"}
	}
	res := Status{State: StateValid, Detail: "expiry unknown"}
	if exp, ok := annotationExpiry(s); ok {
		res = expiryStatus(exp, true)
		if res.State == StateInvalid {
			return res
		}
	}
	if !live {
		return res
	}
	lst := p.liveCheck(ctx, tok)
	if lst.State == StateValid {
		lst.Detail = res.Detail
	}
	lst.ExpiresAt = res.ExpiresAt
	return lst
}

// liveCheck runs a one-turn prompt with the token. There is no cheap
// documented endpoint for validating OAuth tokens.
func (p *claudeProvider) liveCheck(ctx context.Context, tok string) Status {
	argv := p.argv
	if argv == nil {
		path, err := exec.LookPath("claude")
		if err != nil {
			return Status{State: StateUnknown, Detail: "claude CLI not found"}
		}
		argv = []string{path}
	}
	ctx, cancel := context.WithTimeout(ctx, claudeLiveTimeout)
	defer cancel()
	args := append(append([]string{}, argv[1:]...), "-p", "Reply with OK", "--max-turns", "1")
	cmd := exec.CommandContext(ctx, argv[0], args...)
	cmd.Env = append(claudeEnv(os.Environ(), tok), p.extraEnv...)
	cmd.Stdin = nil // /dev/null
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return Status{State: StateUnknown, Detail: "claude -p timed out"}
	}
	if err != nil {
		line := firstLine(strings.ReplaceAll(out.String(), tok, "[token]"))
		lower := strings.ToLower(line)
		state := StateUnknown
		for _, w := range []string{"401", "403", "invalid", "unauthorized", "expired", "revoked", "login", "authenticat"} {
			if strings.Contains(lower, w) {
				state = StateInvalid
				break
			}
		}
		d := "claude -p failed: " + err.Error()
		if line != "" {
			d = "claude -p: " + line
		}
		return Status{State: state, Detail: d}
	}
	return Status{State: StateValid}
}

// claudeEnv drops variables that would override the OAuth token and adds it.
func claudeEnv(base []string, tok string) []string {
	env := make([]string, 0, len(base)+1)
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", ClaudeTokenKey:
			continue
		}
		env = append(env, kv)
	}
	return append(env, ClaudeTokenKey+"="+tok)
}

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			if len(l) > 120 {
				l = l[:120] + "..."
			}
			return l
		}
	}
	return ""
}

type claudeFlow struct{ step int }

func (f *claudeFlow) Close() error { return nil }

func (f *claudeFlow) Next(ctx context.Context, input string) (Step, error) {
	switch f.step {
	case 0:
		f.step = 1
		return Step{
			Kind:  StepExec,
			Title: "Claude Code: create a long-lived token",
			Body: "kAinban will now run `claude setup-token`. Open the URL it shows in a browser, " +
				"sign in and paste the code back if asked.\n\n" +
				"It then PRINTS a token starting with " + ClaudeTokenPrefix + " (valid for 1 year) and does " +
				"not save it anywhere: copy the printed token before continuing.",
			Command: []string{"claude", "setup-token"},
		}, nil
	case 1:
		f.step = 2
		return f.inputStep(), nil
	default:
		tok := normalizeToken(input)
		if err := validateClaudeToken(tok); err != nil {
			return Step{}, err
		}
		exp := nowFunc().Add(claudeTokenLifetime)
		return Step{
			Kind:  StepDone,
			Title: "Claude Code token saved",
			Body:  "Stored in Secret " + ClaudeSecretName + ", " + expiryDetail(exp, nowFunc()) + ".",
			Secrets: map[string]*Secret{
				ClaudeSecretName: {
					Data:        map[string][]byte{ClaudeTokenKey: []byte(tok)},
					Annotations: map[string]string{AnnotationExpiresAt: formatExpiry(exp)},
				},
			},
		}, nil
	}
}

func (f *claudeFlow) inputStep() Step {
	return Step{
		Kind:  StepInput,
		Title: "Claude Code: paste the token",
		Body: "Paste the token printed by `claude setup-token`. Agents receive it as " + ClaudeTokenKey + ".\n\n" +
			"Note: ANTHROPIC_API_KEY, if set in an agent's environment, overrides this token.",
		Prompt:      "Token",
		Placeholder: ClaudeTokenPrefix + "...",
		Secret:      true,
	}
}
