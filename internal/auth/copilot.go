package auth

import "net/http"

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
	}
}
