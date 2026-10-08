package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// GitHub credentials used by agents for git push and gh.
const (
	GitHubSecretName = "kainban-github"
	GitHubTokenKey   = "GH_TOKEN"

	// AnnotationGitHubLogin records the account a GitHub token belongs to.
	AnnotationGitHubLogin = "kainban.io/github-login"

	gitHubAPIBase = "https://api.github.com"
	// gitHubExpiryHeader is set on API responses for expiring tokens.
	gitHubExpiryHeader = "github-authentication-token-expiration"
)

// gitHubTokenProvider implements both the github and copilot providers;
// they differ in Secret, accepted token types and wording.
type gitHubTokenProvider struct {
	id, title   string
	secretName  string
	key         string
	prefixes    []string
	rejectMsg   map[string]string // prefix -> error for explicitly unsupported tokens
	inputTitle  string
	inputBody   string
	placeholder string

	client  *http.Client
	baseURL string

	// prefill, if set, proposes a token for the input (source describes
	// where it came from); store may be nil.
	prefill func(ctx context.Context, store Store) (tok, source string)
	// deviceLogin lets an empty input run `gh auth login` (device flow) and
	// then pre-fill its token.
	deviceLogin bool
}

// NewGitHub returns the GitHub provider (GH_TOKEN for git/gh).
func NewGitHub() Provider { return newGitHub(defaultHTTPClient(), gitHubAPIBase) }

func newGitHub(client *http.Client, baseURL string) *gitHubTokenProvider {
	return &gitHubTokenProvider{
		id:         ProviderGitHub,
		title:      "GitHub",
		secretName: GitHubSecretName,
		key:        GitHubTokenKey,
		prefixes:   []string{"github_pat_", "gho_", "ghu_", "ghp_"},
		inputTitle: "GitHub: paste a token",
		inputBody: "Create a fine-grained personal access token (github_pat_...) at\n" +
			"https://github.com/settings/personal-access-tokens/new\n" +
			"limited to the repositories agents may push to, with these repository permissions:\n" +
			"  Contents: Read and write\n  Pull requests: Read and write\n\n" +
			"Alternatively, if gh is logged in locally, `gh auth token` prints its token, " +
			"but that one is broad (all your repos and scopes). Classic tokens (ghp_...) are accepted " +
			"but also broad; prefer a fine-grained token.",
		placeholder: "github_pat_...",
		client:      client,
		baseURL:     baseURL,
		prefill:     gitHubCLIPrefill,
		deviceLogin: true,
	}
}

// gitHubCLIPrefill proposes the token of a logged-in gh CLI.
func gitHubCLIPrefill(ctx context.Context, _ Store) (string, string) {
	if tok := ghAuthToken(ctx); strings.HasPrefix(tok, "gho_") {
		return tok, "`gh auth token`"
	}
	return "", ""
}

func (p *gitHubTokenProvider) ID() string            { return p.id }
func (p *gitHubTokenProvider) Title() string         { return p.title }
func (p *gitHubTokenProvider) SecretNames() []string { return []string{p.secretName} }
func (p *gitHubTokenProvider) NewFlow() Flow         { return &gitHubTokenFlow{p: p} }

func (p *gitHubTokenProvider) checkFormat(tok string) error {
	for pre, msg := range p.rejectMsg {
		if strings.HasPrefix(tok, pre) {
			return fmt.Errorf("%s", msg)
		}
	}
	for _, pre := range p.prefixes {
		if strings.HasPrefix(tok, pre) && len(tok) > len(pre) {
			return nil
		}
	}
	return fmt.Errorf("unrecognised token: expected one of %s", strings.Join(p.prefixes, ", "))
}

// gitHubUser is the result of a successful GET /user.
type gitHubUser struct {
	Login     string
	ExpiresAt *time.Time // nil for tokens without expiry
}

// gitHubAPIError carries a non-200 response; Invalid reports whether the
// token itself was rejected (as opposed to rate limits or outages).
type gitHubAPIError struct {
	Status  int
	Host    string
	Invalid bool
}

func (e *gitHubAPIError) Error() string { return fmt.Sprintf("HTTP %d from %s", e.Status, e.Host) }

// validateGitHubToken calls GET /user with tok.
func validateGitHubToken(ctx context.Context, client *http.Client, baseURL, tok string) (*gitHubUser, error) {
	u := strings.TrimRight(baseURL, "/") + "/user"
	resp, err := apiGet(ctx, client, u, map[string]string{
		"Authorization":        "Bearer " + tok,
		"Accept":               "application/vnd.github+json",
		"X-GitHub-Api-Version": "2022-11-28",
	})
	if err != nil {
		return nil, err
	}
	if resp.Status != http.StatusOK {
		invalid := resp.Status == http.StatusUnauthorized ||
			(resp.Status == http.StatusForbidden && resp.Header.Get("x-ratelimit-remaining") != "0")
		return nil, &gitHubAPIError{Status: resp.Status, Host: hostOf(u), Invalid: invalid}
	}
	var body struct {
		Login string `json:"login"`
	}
	_ = json.Unmarshal(resp.Body, &body)
	user := &gitHubUser{Login: body.Login}
	if t, ok := parseGitHubExpiry(resp.Header.Get(gitHubExpiryHeader)); ok {
		user.ExpiresAt = &t
	}
	return user, nil
}

// parseGitHubExpiry parses values like "2026-12-31 00:00:00 UTC".
func parseGitHubExpiry(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{
		"2006-01-02 15:04:05 MST",
		"2006-01-02 15:04:05 -0700",
		"2006-01-02 15:04:05 -07:00",
		"2006-01-02 15:04:05",
		time.RFC3339,
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, v); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func (p *gitHubTokenProvider) Check(ctx context.Context, store Store, live bool) Status {
	s, tok, st := loadSecret(ctx, store, p.secretName, p.key)
	if st != nil {
		return *st
	}
	if err := p.checkFormat(tok); err != nil {
		return Status{State: StateInvalid, Detail: err.Error()}
	}
	res := Status{State: StateValid, Detail: "no expiry recorded"}
	if exp, ok := annotationExpiry(s); ok {
		res = expiryStatus(exp, true)
		if res.State == StateInvalid {
			return res
		}
	}
	if login := s.Annotations[AnnotationGitHubLogin]; login != "" {
		res.Detail = login + ", " + res.Detail
	}
	if !live {
		return res
	}
	user, err := validateGitHubToken(ctx, p.client, p.baseURL, tok)
	if err != nil {
		state := StateUnknown
		if ae, ok := err.(*gitHubAPIError); ok && ae.Invalid {
			state = StateInvalid
		}
		return Status{State: state, Detail: err.Error(), ExpiresAt: res.ExpiresAt}
	}
	detail := "no expiry"
	if user.ExpiresAt != nil {
		r := expiryStatus(*user.ExpiresAt, true)
		res.ExpiresAt, detail = r.ExpiresAt, r.Detail
	}
	if user.Login != "" {
		detail = user.Login + ", " + detail
	}
	res.State, res.Detail = StateValid, detail
	return res
}

type gitHubTokenFlow struct {
	p          *gitHubTokenProvider
	store      Store
	started    bool
	awaitLogin bool // StepExec `gh auth login` was handed out
	loggedIn   bool // ... and has run
}

// ghLoginCommand signs gh in with the device flow (one-time code, no
// callback). GH_TOKEN/GITHUB_TOKEN are cleared: gh refuses to log in while
// they are set (e.g. inside `kainban shell`).
var ghLoginCommand = []string{"gh", "auth", "login", "--hostname", "github.com", "--git-protocol", "https", "--web"}

func (f *gitHubTokenFlow) UseStore(s Store) { f.store = s }

func (f *gitHubTokenFlow) Close() error { return nil }

func (f *gitHubTokenFlow) Next(ctx context.Context, input string) (Step, error) {
	if !f.started {
		f.started = true
		return f.inputStep(ctx), nil
	}
	if f.awaitLogin {
		f.awaitLogin, f.loggedIn = false, true
		step := f.inputStep(ctx)
		if step.Value == "" {
			step.Body = "`gh auth login` did not leave a token (`gh auth token` is empty); paste one instead.\n\n" + step.Body
		}
		return step, nil
	}
	tok := normalizeToken(input)
	if tok == "" && f.p.deviceLogin && !f.loggedIn {
		f.awaitLogin = true
		return Step{
			Kind:  StepExec,
			Title: f.p.title + ": sign in with gh",
			Body: "kAinban will run `gh auth login`. It prints a one-time code: open https://github.com/login/device " +
				"in your browser, enter the code and authorize GitHub CLI. The token is then filled in for you.\n\n" +
				"Note: this OAuth token (gho_) can access all your repositories.",
			Command: ghLoginCommand,
			Env:     []string{"GH_TOKEN=", "GITHUB_TOKEN="},
		}, nil
	}
	if err := f.p.checkFormat(tok); err != nil {
		return Step{}, err
	}
	user, err := validateGitHubToken(ctx, f.p.client, f.p.baseURL, tok)
	if err != nil {
		return Step{}, fmt.Errorf("token check failed: %w", err)
	}
	ann := map[string]string{}
	body := "Stored in Secret " + f.p.secretName
	if user.Login != "" {
		ann[AnnotationGitHubLogin] = user.Login
		body += " (account " + user.Login + ")"
	}
	if user.ExpiresAt != nil {
		ann[AnnotationExpiresAt] = formatExpiry(*user.ExpiresAt)
		body += ", " + expiryDetail(*user.ExpiresAt, nowFunc())
	} else {
		body += ", no expiry"
	}
	return Step{
		Kind:  StepDone,
		Title: f.p.title + " token saved",
		Body:  body + ".",
		Secrets: map[string]*Secret{
			f.p.secretName: {Data: map[string][]byte{f.p.key: []byte(tok)}, Annotations: ann},
		},
	}, nil
}

// inputStep asks for the token, pre-filled when a prefill source has one.
func (f *gitHubTokenFlow) inputStep(ctx context.Context) Step {
	step := Step{
		Kind:        StepInput,
		Title:       f.p.inputTitle,
		Body:        f.p.inputBody,
		Prompt:      "Token",
		Placeholder: f.p.placeholder,
		Secret:      true,
	}
	if f.p.prefill != nil {
		if tok, source := f.p.prefill(ctx, f.store); tok != "" {
			step.Value = tok
			step.Body = "Pre-filled with the token from " + source + " (…" + tok[len(tok)-4:] +
				"). Press Enter to use it, or paste another one.\n\n" + step.Body
			return step
		}
	}
	if f.p.deviceLogin && !f.loggedIn {
		step.Body += "\n\nOr leave it empty and press Enter to sign in with `gh auth login` (GitHub device flow)."
	}
	return step
}
