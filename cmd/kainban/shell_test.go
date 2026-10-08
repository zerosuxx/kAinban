package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestBuildEnv(t *testing.T) {
	base := []string{"PATH=/bin", "GH_TOKEN=old", "ANTHROPIC_API_KEY=sk-api", "ANTHROPIC_AUTH_TOKEN=x", "CODEX_HOME=/old"}
	env := buildEnv(base, map[string]string{"GH_TOKEN": "new", "CLAUDE_CODE_OAUTH_TOKEN": "oat"}, "/state/codex-home")
	for _, want := range []string{"PATH=/bin", "GH_TOKEN=new", "CLAUDE_CODE_OAUTH_TOKEN=oat", "CODEX_HOME=/state/codex-home", "KAINBAN_SHELL=1"} {
		if !slices.Contains(env, want) {
			t.Errorf("missing %q in %v", want, env)
		}
	}
	for _, gone := range []string{"GH_TOKEN=old", "ANTHROPIC_API_KEY=sk-api", "ANTHROPIC_AUTH_TOKEN=x", "CODEX_HOME=/old"} {
		if slices.Contains(env, gone) {
			t.Errorf("%q should be dropped", gone)
		}
	}
	// Without a Claude token an API key the user set stays.
	if env := buildEnv([]string{"ANTHROPIC_API_KEY=k"}, nil, ""); !slices.Contains(env, "ANTHROPIC_API_KEY=k") {
		t.Errorf("API key dropped without OAuth token: %v", env)
	}
}

func TestWriteCodexHome(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "codex-home")
	if err := writeCodexHome(dir, []byte(`{"tokens":{}}`)); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "auth.json"))
	if err != nil || string(b) != `{"tokens":{}}` {
		t.Fatalf("auth.json: %q %v", b, err)
	}
	if st, _ := os.Stat(filepath.Join(dir, "auth.json")); st.Mode().Perm() != 0o600 {
		t.Errorf("auth.json mode %v", st.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(dir, "config.toml")); err != nil {
		t.Errorf("config.toml: %v", err)
	}
}
