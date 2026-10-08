package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
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

type claudeFlow struct {
	step       int
	typescript string // `script` recording of `claude setup-token`
}

func (f *claudeFlow) Close() error {
	if f.typescript != "" {
		os.Remove(f.typescript)
		f.typescript = ""
	}
	return nil
}

// claudeSetupScript runs `claude setup-token` under `script`, recording its
// output (the token) to $1. The command writes to the normal screen, which
// would still show the token after the TUI exits, so the screen and the
// scrollback are wiped as soon as the token has been captured; only if no
// token is found does it pause so the user can copy it by hand.
const claudeSetupScript = `wipe() { printf '\033[2J\033[3J\033[H'; }
if command -v script >/dev/null 2>&1; then
  script -q -c "claude setup-token" "$1"
else
  claude setup-token
fi
if ! grep -q 'sk-ant-oat01-' "$1" 2>/dev/null; then
  printf '\n\nkAinban: no token found in the output. Copy it from above, then press Enter... '
  read -r _
fi
wipe`

func (f *claudeFlow) Next(ctx context.Context, input string) (Step, error) {
	switch f.step {
	case 0:
		ts, err := os.CreateTemp("", "kainban-claude-*.typescript")
		if err != nil {
			return Step{}, fmt.Errorf("create temp file: %w", err)
		}
		ts.Close()
		f.typescript = ts.Name()
		f.step = 1
		return Step{
			Kind:  StepExec,
			Title: "Claude Code: create a long-lived token",
			Body: "kAinban will now run `claude setup-token`. Open the URL it shows in a browser, " +
				"sign in and paste the code back if asked.\n\n" +
				"It then prints a token starting with " + ClaudeTokenPrefix + " (valid for 1 year). " +
				"kAinban captures the token, fills it in for you and then clears the screen and scrollback " +
				"so it is not left in your terminal.",
			Command: []string{"sh", "-c", claudeSetupScript, "sh", f.typescript},
		}, nil
	case 1:
		f.step = 2
		step := f.inputStep()
		if out, err := os.ReadFile(f.typescript); err == nil {
			if tok := extractClaudeToken(string(out)); tok != "" {
				step.Value = tok
				step.Body = "Captured the token printed by `claude setup-token` (…" + tok[len(tok)-4:] +
					"). Press Enter to save it, or paste another one.\n\n" +
					"Note: ANTHROPIC_API_KEY, if set in an agent's environment, overrides this token."
			}
		}
		f.Close()
		return step, nil
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

var (
	ansiSeq        = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-Z\\-_]`)
	tokenLineChars = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// extractClaudeToken finds the last token printed in a `script` recording of
// `claude setup-token`. Escape sequences are stripped; a token wrapped over
// several lines is joined while the following lines contain token characters
// only.
func extractClaudeToken(typescript string) string {
	s := strings.ReplaceAll(ansiSeq.ReplaceAllString(typescript, ""), "\r", "\n")
	i := strings.LastIndex(s, ClaudeTokenPrefix)
	if i < 0 {
		return ""
	}
	lines := strings.Split(s[i:], "\n")
	tok := strings.TrimSpace(lines[0])
	if j := strings.IndexFunc(tok, func(r rune) bool { return !isTokenRune(r) }); j >= 0 {
		return validOrEmpty(tok[:j])
	}
	for _, l := range lines[1:] {
		l = strings.TrimSpace(l)
		if l == "" {
			continue // `script` turns \r\n into an empty line
		}
		if !tokenLineChars.MatchString(l) {
			break
		}
		tok += l
	}
	return validOrEmpty(tok)
}

func isTokenRune(r rune) bool {
	return r == '-' || r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
}

func validOrEmpty(tok string) string {
	if len(tok) < len(ClaudeTokenPrefix)+20 || validateClaudeToken(tok) != nil {
		return ""
	}
	return tok
}
