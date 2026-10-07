package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Antigravity CLI (agy) credentials: a Gemini API key. Google-account logins
// live in the OS keyring, which containers do not have.
const (
	AntigravitySecretName = "kainban-antigravity"
	AntigravityKeyKey     = "GEMINI_API_KEY"

	geminiAPIBase = "https://generativelanguage.googleapis.com"
)

type antigravityProvider struct {
	client  *http.Client
	baseURL string
}

// NewAntigravity returns the Antigravity CLI provider.
func NewAntigravity() Provider { return newAntigravity(defaultHTTPClient(), geminiAPIBase) }

func newAntigravity(client *http.Client, baseURL string) *antigravityProvider {
	return &antigravityProvider{client: client, baseURL: baseURL}
}

func (p *antigravityProvider) ID() string            { return ProviderAntigravity }
func (p *antigravityProvider) Title() string         { return "Antigravity CLI (Gemini)" }
func (p *antigravityProvider) SecretNames() []string { return []string{AntigravitySecretName} }
func (p *antigravityProvider) NewFlow() Flow         { return &antigravityFlow{p: p} }

// geminiKeyError is a non-200 answer; Invalid marks a rejected key.
type geminiKeyError struct {
	Status  int
	Host    string
	Invalid bool
}

func (e *geminiKeyError) Error() string { return fmt.Sprintf("HTTP %d from %s", e.Status, e.Host) }

// validateGeminiKey lists models with the key in a header (kept out of URLs
// and therefore out of logs).
func validateGeminiKey(ctx context.Context, client *http.Client, baseURL, key string) error {
	u := strings.TrimRight(baseURL, "/") + "/v1beta/models"
	resp, err := apiGet(ctx, client, u, map[string]string{"x-goog-api-key": key})
	if err != nil {
		return err
	}
	if resp.Status == http.StatusOK {
		return nil
	}
	invalid := resp.Status == http.StatusBadRequest || resp.Status == http.StatusUnauthorized ||
		resp.Status == http.StatusForbidden
	return &geminiKeyError{Status: resp.Status, Host: hostOf(u), Invalid: invalid}
}

func (p *antigravityProvider) Check(ctx context.Context, store Store, live bool) Status {
	_, key, st := loadSecret(ctx, store, AntigravitySecretName, AntigravityKeyKey)
	if st != nil {
		return *st
	}
	if strings.TrimSpace(key) != key {
		return Status{State: StateInvalid, Detail: "API key contains whitespace"}
	}
	if !live {
		return Status{State: StateValid, Detail: "API key present"}
	}
	if err := validateGeminiKey(ctx, p.client, p.baseURL, key); err != nil {
		var ge *geminiKeyError
		if errors.As(err, &ge) && ge.Invalid {
			return Status{State: StateInvalid, Detail: err.Error()}
		}
		return Status{State: StateUnknown, Detail: err.Error()}
	}
	return Status{State: StateValid, Detail: "API key accepted"}
}

type antigravityFlow struct {
	p       *antigravityProvider
	started bool
}

func (f *antigravityFlow) Close() error { return nil }

func (f *antigravityFlow) Next(ctx context.Context, input string) (Step, error) {
	if !f.started {
		f.started = true
		return Step{
			Kind:  StepInput,
			Title: "Antigravity CLI: paste a Gemini API key",
			Body: "The Antigravity CLI (agy) keeps Google-account logins in the OS keyring, which is not " +
				"available in containers, so agents use a Gemini API key instead.\n\n" +
				"Create one in Google AI Studio: https://aistudio.google.com/apikey\n\n" +
				"Agents receive it as " + AntigravityKeyKey + " and get " +
				"`\"modelProvider\": \"gemini\"` in ~/.gemini/antigravity-cli/settings.json via their config.",
			Prompt:      "API key",
			Placeholder: "AIza...",
			Secret:      true,
		}, nil
	}
	key := normalizeToken(input)
	if key == "" {
		return Step{}, errors.New("enter an API key")
	}
	if err := validateGeminiKey(ctx, f.p.client, f.p.baseURL, key); err != nil {
		return Step{}, fmt.Errorf("key check failed: %w", err)
	}
	return Step{
		Kind:  StepDone,
		Title: "Gemini API key saved",
		Body:  "Stored in Secret " + AntigravitySecretName + ".",
		Secrets: map[string]*Secret{
			AntigravitySecretName: {Data: map[string][]byte{AntigravityKeyKey: []byte(key)}, Annotations: map[string]string{}},
		},
	}, nil
}
