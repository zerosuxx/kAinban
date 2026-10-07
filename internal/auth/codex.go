package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Codex (ChatGPT login) credentials.
const (
	// CodexSecretName holds auth.json with refresh_token blanked; mounted
	// into agents.
	CodexSecretName = "kainban-codex"
	// CodexRefreshSecretName holds the full auth.json; orchestrator only.
	CodexRefreshSecretName = "kainban-codex-refresh"
	CodexAuthKey           = "auth.json"

	codexCallbackBase   = "http://localhost:1455"
	codexCallbackPath   = "/auth/callback"
	codexURLWait        = 30 * time.Second
	codexForwardTimeout = 15 * time.Second
	codexExitWait       = 60 * time.Second
)

type codexProvider struct{}

// NewCodex returns the Codex provider.
func NewCodex() Provider { return &codexProvider{} }

func (p *codexProvider) ID() string    { return ProviderCodex }
func (p *codexProvider) Title() string { return "Codex (ChatGPT)" }
func (p *codexProvider) SecretNames() []string {
	return []string{CodexSecretName, CodexRefreshSecretName}
}
func (p *codexProvider) NewFlow() Flow {
	return &codexFlow{bin: "codex", callbackBase: codexCallbackBase}
}

// codexAuth is the subset of auth.json kAinban inspects.
type codexAuth struct {
	Tokens *struct {
		AccessToken  *string `json:"access_token"`
		RefreshToken *string `json:"refresh_token"`
	} `json:"tokens"`
}

func parseCodexAuth(authJSON []byte) (*codexAuth, error) {
	var a codexAuth
	if err := json.Unmarshal(authJSON, &a); err != nil {
		return nil, errors.New("auth.json is not valid JSON")
	}
	if a.Tokens == nil {
		return nil, errors.New("auth.json has no tokens")
	}
	if a.Tokens.AccessToken == nil || *a.Tokens.AccessToken == "" {
		return nil, errors.New("auth.json has no access_token")
	}
	return &a, nil
}

// StripRefreshToken returns authJSON with tokens.refresh_token set to "".
// The field is kept (codex fails to deserialize auth.json without it) and
// all other fields are preserved.
func StripRefreshToken(authJSON []byte) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(authJSON, &top); err != nil {
		return nil, errors.New("auth.json is not a JSON object")
	}
	raw, ok := top["tokens"]
	if !ok {
		return nil, errors.New("auth.json has no tokens")
	}
	var tokens map[string]json.RawMessage
	if err := json.Unmarshal(raw, &tokens); err != nil || tokens == nil {
		return nil, errors.New("auth.json tokens is not an object")
	}
	tokens["refresh_token"] = json.RawMessage(`""`)
	nt, err := json.Marshal(tokens)
	if err != nil {
		return nil, err
	}
	top["tokens"] = nt
	return json.MarshalIndent(top, "", "  ")
}

// AccessTokenExpiry returns the exp claim of tokens.access_token (a JWT).
// The signature is not verified.
func AccessTokenExpiry(authJSON []byte) (time.Time, error) {
	a, err := parseCodexAuth(authJSON)
	if err != nil {
		return time.Time{}, err
	}
	return jwtExpiry(*a.Tokens.AccessToken)
}

func jwtExpiry(jwt string) (time.Time, error) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return time.Time{}, errors.New("access_token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}, errors.New("access_token payload is not base64url")
	}
	var claims struct {
		Exp *json.Number `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, errors.New("access_token payload is not JSON")
	}
	if claims.Exp == nil {
		return time.Time{}, errors.New("access_token has no exp claim")
	}
	f, err := claims.Exp.Float64()
	if err != nil || f <= 0 {
		return time.Time{}, errors.New("access_token has an invalid exp claim")
	}
	return time.Unix(int64(f), 0).UTC(), nil
}

func (p *codexProvider) Check(ctx context.Context, store Store, live bool) Status {
	_, data, st := loadSecret(ctx, store, CodexSecretName, CodexAuthKey)
	if st != nil {
		return *st
	}
	a, err := parseCodexAuth([]byte(data))
	if err != nil {
		return Status{State: StateInvalid, Detail: err.Error()}
	}
	if a.Tokens.RefreshToken == nil {
		return Status{State: StateInvalid, Detail: "agent auth.json lacks the refresh_token field"}
	}
	if *a.Tokens.RefreshToken != "" {
		return Status{State: StateInvalid, Detail: "agent copy contains a refresh_token (leak)"}
	}
	exp, err := jwtExpiry(*a.Tokens.AccessToken)
	if err != nil {
		return Status{State: StateInvalid, Detail: err.Error()}
	}
	res := expiryStatus(exp, false)
	if res.State == StateInvalid {
		res.Detail = "access token " + res.Detail
		return res
	}

	_, full, st := loadSecret(ctx, store, CodexRefreshSecretName, CodexAuthKey)
	if st != nil {
		switch st.State {
		case StateMissing:
			return Status{State: StateInvalid, Detail: "orchestrator copy missing", ExpiresAt: res.ExpiresAt}
		default:
			st.ExpiresAt = res.ExpiresAt
			return *st
		}
	}
	fa, err := parseCodexAuth([]byte(full))
	if err != nil {
		return Status{State: StateInvalid, Detail: "orchestrator copy: " + err.Error(), ExpiresAt: res.ExpiresAt}
	}
	if fa.Tokens.RefreshToken == nil || *fa.Tokens.RefreshToken == "" {
		return Status{State: StateInvalid, Detail: "orchestrator copy has no refresh_token", ExpiresAt: res.ExpiresAt}
	}
	// No documented endpoint to validate ChatGPT tokens: live == local.
	return res
}

// codexAuthorizeURLRe matches the authorize URL codex login prints.
var codexAuthorizeURLRe = regexp.MustCompile(`https://auth\.openai\.com/[^\s"'<>\x1b]+`)

// extractAuthorizeURL finds the authorize URL in codex login output.
func extractAuthorizeURL(out string) string {
	return codexAuthorizeURLRe.FindString(out)
}

// callbackBaseFromAuthorizeURL derives the local listener from the
// authorize URL's redirect_uri, falling back to def.
func callbackBaseFromAuthorizeURL(authURL, def string) string {
	u, err := url.Parse(authURL)
	if err != nil {
		return def
	}
	r, err := url.Parse(u.Query().Get("redirect_uri"))
	if err != nil || r.Scheme != "http" {
		return def
	}
	switch r.Hostname() {
	case "localhost", "127.0.0.1":
		return "http://" + r.Host
	}
	return def
}

// parseCallbackURL validates the URL pasted from the browser and returns the
// path+query to replay against the local codex listener (query kept as-is).
func parseCallbackURL(in string) (string, error) {
	in = strings.TrimSpace(in)
	if in == "" {
		return "", errors.New("paste the full http://localhost:1455/auth/callback?... URL from the browser")
	}
	u, err := url.Parse(in)
	if err != nil {
		return "", errors.New("not a valid URL")
	}
	if u.Path != codexCallbackPath {
		return "", fmt.Errorf("expected path %s, got %q", codexCallbackPath, u.Path)
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", errors.New("malformed query string")
	}
	if e := q.Get("error"); e != "" {
		return "", fmt.Errorf("login failed: %s", e)
	}
	if q.Get("code") == "" || q.Get("state") == "" {
		return "", errors.New("URL lacks code and/or state parameters")
	}
	return u.Path + "?" + u.RawQuery, nil
}

// forwardCallback replays the callback against the codex listener at base.
func forwardCallback(ctx context.Context, client *http.Client, base, pathQuery string) error {
	ctx, cancel := context.WithTimeout(ctx, codexForwardTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+pathQuery, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("forwarding callback to codex: %w", unwrapURLError(err))
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("codex rejected the callback: HTTP %d", resp.StatusCode)
	}
	return nil
}

// buildCodexSecrets validates a full auth.json and returns both Secrets.
func buildCodexSecrets(full []byte) (map[string]*Secret, time.Time, error) {
	a, err := parseCodexAuth(full)
	if err != nil {
		return nil, time.Time{}, err
	}
	if a.Tokens.RefreshToken == nil || *a.Tokens.RefreshToken == "" {
		return nil, time.Time{}, errors.New("auth.json has no refresh_token")
	}
	exp, err := jwtExpiry(*a.Tokens.AccessToken)
	if err != nil {
		return nil, time.Time{}, err
	}
	stripped, err := StripRefreshToken(full)
	if err != nil {
		return nil, time.Time{}, err
	}
	ann := func() map[string]string { return map[string]string{AnnotationExpiresAt: formatExpiry(exp)} }
	return map[string]*Secret{
		CodexSecretName:        {Data: map[string][]byte{CodexAuthKey: stripped}, Annotations: ann()},
		CodexRefreshSecretName: {Data: map[string][]byte{CodexAuthKey: append([]byte(nil), full...)}, Annotations: ann()},
	}, exp, nil
}

// lockedBuffer collects process output from two pipes.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type codexFlow struct {
	bin          string
	callbackBase string
	client       *http.Client

	dir     string
	cmd     *exec.Cmd
	out     *lockedBuffer
	exited  chan struct{}
	authURL string
	started bool
}

func (f *codexFlow) Next(ctx context.Context, input string) (Step, error) {
	if !f.started {
		if err := f.start(ctx); err != nil {
			f.Close()
			return Step{}, err
		}
		f.started = true
		return f.inputStep(), nil
	}
	return f.finish(ctx, input)
}

func (f *codexFlow) start(ctx context.Context) error {
	if _, err := exec.LookPath(f.bin); err != nil {
		return errors.New("codex CLI not found on PATH")
	}
	dir, err := os.MkdirTemp("", "kainban-codex-*")
	if err != nil {
		return err
	}
	f.dir = dir
	cfg := "cli_auth_credentials_store = \"file\"\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(cfg), 0o600); err != nil {
		return err
	}
	f.out = &lockedBuffer{}
	f.cmd = exec.Command(f.bin, "login")
	f.cmd.Env = append(os.Environ(), "CODEX_HOME="+dir)
	f.cmd.Stdin = nil
	f.cmd.Stdout = f.out
	f.cmd.Stderr = f.out
	if err := f.cmd.Start(); err != nil {
		return fmt.Errorf("starting codex login: %w", err)
	}
	f.exited = make(chan struct{})
	go func() { _ = f.cmd.Wait(); close(f.exited) }()

	deadline := time.NewTimer(codexURLWait)
	defer deadline.Stop()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		if u := extractAuthorizeURL(f.out.String()); u != "" {
			f.authURL = u
			f.callbackBase = callbackBaseFromAuthorizeURL(u, f.callbackBase)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-f.exited:
			return fmt.Errorf("codex login exited early: %s", firstLine(f.out.String()))
		case <-deadline.C:
			return errors.New("codex login did not print an authorize URL within 30s")
		case <-tick.C:
		}
	}
}

func (f *codexFlow) inputStep() Step {
	return Step{
		Kind:  StepInput,
		Title: "Codex: sign in with ChatGPT",
		Body: "1. Open this URL in your own browser and sign in:\n\n" + f.authURL + "\n\n" +
			"2. The browser is redirected to " + f.callbackBase + codexCallbackPath + "?code=...&state=... " +
			"and the page will most likely fail to load. That is expected.\n" +
			"3. Copy that full URL from the address bar and paste it here.\n\n" +
			"(If the page loaded and reported success, paste the URL anyway.)",
		Prompt:      "Callback URL",
		Placeholder: "http://localhost:1455/auth/callback?code=...&state=...",
		Secret:      true,
	}
}

func (f *codexFlow) finish(ctx context.Context, input string) (Step, error) {
	authPath := filepath.Join(f.dir, "auth.json")
	var fwdErr error
	if pq, err := parseCallbackURL(input); err != nil {
		// codex may have completed on its own (browser on the same host).
		if _, statErr := os.Stat(authPath); statErr != nil {
			return Step{}, err
		}
	} else {
		select {
		case <-f.exited:
		default:
			client := f.client
			if client == nil {
				client = &http.Client{}
			}
			fwdErr = forwardCallback(ctx, client, f.callbackBase, pq)
		}
	}

	// Wait for codex to persist auth.json and exit.
	deadline := time.NewTimer(codexExitWait)
	defer deadline.Stop()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for done := false; !done; {
		select {
		case <-f.exited:
			done = true
		case <-ctx.Done():
			return Step{}, ctx.Err()
		case <-deadline.C:
			done = true
		case <-tick.C:
			if fwdErr != nil {
				// Nothing to wait for unless codex already wrote the file.
				done = true
			}
		}
	}

	full, err := os.ReadFile(authPath)
	if err != nil {
		if fwdErr != nil {
			return Step{}, fwdErr
		}
		select {
		case <-f.exited:
			return Step{}, fmt.Errorf("codex login finished without writing auth.json: %s", lastLine(f.out.String()))
		default:
			return Step{}, errors.New("codex login did not write auth.json within 60s")
		}
	}
	secrets, exp, err := buildCodexSecrets(full)
	if err != nil {
		return Step{}, err
	}
	return Step{
		Kind:  StepDone,
		Title: "Codex credentials saved",
		Body: "Agents get " + CodexSecretName + " (refresh_token blanked, access token " + expiryDetail(exp, nowFunc()) +
			"); the full copy with the refresh token is in " + CodexRefreshSecretName + " for the orchestrator only.",
		Secrets: secrets,
	}, nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return firstLine(lines[len(lines)-1])
}

// Close kills a running codex login and removes its temporary CODEX_HOME.
func (f *codexFlow) Close() error {
	if f.cmd != nil && f.cmd.Process != nil && f.exited != nil {
		select {
		case <-f.exited:
		default:
			_ = f.cmd.Process.Kill()
			select {
			case <-f.exited:
			case <-time.After(5 * time.Second):
			}
		}
	}
	f.cmd = nil
	var err error
	if f.dir != "" {
		err = os.RemoveAll(f.dir)
		f.dir = ""
	}
	return err
}
