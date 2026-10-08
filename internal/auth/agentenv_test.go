package auth

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
)

func TestLoadAgentEnv(t *testing.T) {
	full := `{"auth_mode":"chatgpt","tokens":{"access_token":"a","refresh_token":"SECRET-RT"}}`
	store := newFakeStore().
		with(ClaudeSecretName, ClaudeTokenKey, ClaudeTokenPrefix+"x", nil).
		with(GitHubSecretName, GitHubTokenKey, "gho_abc\n", nil).
		with(CodexSecretName, CodexAuthKey, full, nil). // must be stripped anyway
		with(CodexRefreshSecretName, CodexAuthKey, full, nil)
	ae, err := LoadAgentEnv(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if ae.Vars["GH_TOKEN"] != "gho_abc" || ae.Vars["CLAUDE_CODE_OAUTH_TOKEN"] == "" {
		t.Fatalf("vars: %v", ae.Vars)
	}
	if _, ok := ae.Vars["GEMINI_API_KEY"]; ok {
		t.Fatal("missing secret must be skipped")
	}
	var auth struct{ Tokens map[string]string }
	if err := json.Unmarshal(ae.CodexAuth, &auth); err != nil || auth.Tokens["refresh_token"] != "" {
		t.Fatalf("codex auth not stripped: %s %v", ae.CodexAuth, err)
	}
	if !slices.Equal(ae.Loaded, []string{ProviderClaude, ProviderGitHub, ProviderCodex}) {
		t.Fatalf("loaded: %v", ae.Loaded)
	}
}
