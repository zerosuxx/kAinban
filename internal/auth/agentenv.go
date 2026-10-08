package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// AgentEnv is what a CLI session needs from the stored credentials: the same
// set an agent pod gets (never the orchestrator's kainban-codex-refresh).
type AgentEnv struct {
	Vars      map[string]string // environment variables, e.g. GH_TOKEN
	CodexAuth []byte            // auth.json without refresh token; nil if missing
	Loaded    []string          // provider IDs whose credentials were found
}

// LoadAgentEnv reads the agent-facing Secrets. Missing Secrets are skipped.
func LoadAgentEnv(ctx context.Context, store Store) (*AgentEnv, error) {
	env := &AgentEnv{Vars: map[string]string{}}
	vars := []struct{ provider, secret, key, envVar string }{
		{ProviderClaude, ClaudeSecretName, ClaudeTokenKey, "CLAUDE_CODE_OAUTH_TOKEN"},
		{ProviderGitHub, GitHubSecretName, GitHubTokenKey, "GH_TOKEN"},
		{ProviderCopilot, CopilotSecretName, CopilotTokenKey, "COPILOT_GITHUB_TOKEN"},
		{ProviderAntigravity, AntigravitySecretName, AntigravityKeyKey, "GEMINI_API_KEY"},
	}
	for _, v := range vars {
		val, err := secretValue(ctx, store, v.secret, v.key)
		if err != nil {
			return nil, err
		}
		if val != "" {
			env.Vars[v.envVar] = val
			env.Loaded = append(env.Loaded, v.provider)
		}
	}
	codex, err := secretValue(ctx, store, CodexSecretName, CodexAuthKey)
	if err != nil {
		return nil, err
	}
	if codex != "" {
		// Defence in depth: the agent copy must never carry a refresh token.
		stripped, err := StripRefreshToken([]byte(codex))
		if err != nil {
			return nil, fmt.Errorf("secret %s: %w", CodexSecretName, err)
		}
		env.CodexAuth = stripped
		env.Loaded = append(env.Loaded, ProviderCodex)
	}
	return env, nil
}

func secretValue(ctx context.Context, store Store, name, key string) (string, error) {
	s, err := store.Get(ctx, name)
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read secret %s: %w", name, err)
	}
	return strings.TrimSpace(string(s.Data[key])), nil
}
