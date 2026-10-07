package auth

import (
	"context"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// GitHub Copilot CLI credentials. The CLI reads COPILOT_GITHUB_TOKEN before
// GH_TOKEN and GITHUB_TOKEN.
const (
	CopilotSecretName = "kainban-copilot"
	CopilotTokenKey   = "COPILOT_GITHUB_TOKEN"
)

// NewCopilot returns the GitHub Copilot CLI provider.
func NewCopilot() Provider { return newCopilot(defaultHTTPClient(), gitHubAPIBase) }

func newCopilot(client *http.Client, baseURL string) *gitHubTokenProvider {
	return &gitHubTokenProvider{
		id:         ProviderCopilot,
		title:      "GitHub Copilot CLI",
		secretName: CopilotSecretName,
		key:        CopilotTokenKey,
		prefixes:   []string{"github_pat_", "gho_", "ghu_"},
		rejectMsg: map[string]string{
			"ghp_": "classic tokens (ghp_) are not supported by the Copilot CLI; use a fine-grained token",
		},
		inputTitle: "Copilot CLI: paste a token",
		inputBody: "Recommended: a dedicated fine-grained personal access token (github_pat_...) at\n" +
			"https://github.com/settings/personal-access-tokens/new\n" +
			"owned by your user account (not an organization), with only the account permission\n" +
			"  Copilot Requests\n\n" +
			"OAuth tokens (gho_, e.g. from `gh auth token`) and GitHub App user tokens (ghu_) also work. " +
			"Classic tokens (ghp_) are not supported. Agents receive it as " + CopilotTokenKey + ".",
		placeholder: "github_pat_...",
		client:      client,
		baseURL:     baseURL,
		prefill:     copilotPrefill,
	}
}

// ghAuthToken runs `gh auth token`; replaced in tests.
var ghAuthToken = func(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// copilotPrefill proposes the GitHub CLI OAuth token (gho_), which the
// Copilot CLI accepts: from `gh auth token`, else from the GitHub step's
// Secret. Fine-grained PATs are not reused, as they usually lack the
// Copilot Requests permission.
func copilotPrefill(ctx context.Context, store Store) (string, string) {
	if tok := ghAuthToken(ctx); strings.HasPrefix(tok, "gho_") {
		return tok, "`gh auth token`"
	}
	if store != nil {
		if s, err := store.Get(ctx, GitHubSecretName); err == nil {
			if tok := strings.TrimSpace(string(s.Data[GitHubTokenKey])); strings.HasPrefix(tok, "gho_") {
				return tok, "Secret " + GitHubSecretName
			}
		}
	}
	return "", ""
}
