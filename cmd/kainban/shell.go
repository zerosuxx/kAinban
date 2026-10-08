package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/zerosuxx/kainban/internal/auth"
	"github.com/zerosuxx/kainban/internal/kube"
)

// runWithCredentials execs argv (a shell when empty) with the credentials
// `kainban auth` stored, read fresh from the Secrets, so CLIs work in the
// orchestrator pod without restarting it after each `kainban auth`.
func runWithCredentials(ctx context.Context, cf commonFlags, argv []string, stderr io.Writer) int {
	t, err := cf.connect()
	if err != nil {
		fmt.Fprintf(stderr, "kainban: %v\n", err)
		return 2
	}
	ae, err := auth.LoadAgentEnv(ctx, kube.NewStore(t.Client, t.Namespace))
	if err != nil {
		fmt.Fprintf(stderr, "kainban: %v\n", err)
		return 1
	}
	home, _ := os.UserHomeDir()
	codexHome := ""
	if ae.CodexAuth != nil {
		codexHome = filepath.Join(stateDir(home), "codex-home")
		if err := writeCodexHome(codexHome, ae.CodexAuth); err != nil {
			fmt.Fprintf(stderr, "kainban: codex: %v\n", err)
			return 1
		}
	}
	if _, ok := ae.Vars["GEMINI_API_KEY"]; ok {
		ensureAntigravitySettings(home)
	}

	if len(argv) == 0 {
		argv = []string{defaultShell()}
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		fmt.Fprintf(stderr, "kainban: %v\n", err)
		return 127
	}
	loaded := "none"
	if len(ae.Loaded) > 0 {
		loaded = strings.Join(ae.Loaded, ", ")
	}
	fmt.Fprintf(stderr, "kainban: credentials from %s: %s\n", t.Describe(), loaded)
	if err := syscall.Exec(path, argv, buildEnv(os.Environ(), ae.Vars, codexHome)); err != nil {
		fmt.Fprintf(stderr, "kainban: exec %s: %v\n", argv[0], err)
		return 126
	}
	return 0 // not reached
}

// buildEnv adds the credential variables to base. ANTHROPIC_API_KEY and
// ANTHROPIC_AUTH_TOKEN are dropped when a Claude OAuth token is set, as they
// would take precedence over it (and bill the API instead of the plan).
func buildEnv(base []string, vars map[string]string, codexHome string) []string {
	drop := map[string]bool{}
	for k := range vars {
		drop[k] = true
	}
	if _, ok := vars["CLAUDE_CODE_OAUTH_TOKEN"]; ok {
		drop["ANTHROPIC_API_KEY"], drop["ANTHROPIC_AUTH_TOKEN"] = true, true
	}
	if codexHome != "" {
		drop["CODEX_HOME"] = true
	}
	var env []string
	for _, kv := range base {
		if k, _, _ := strings.Cut(kv, "="); !drop[k] {
			env = append(env, kv)
		}
	}
	for k, v := range vars {
		env = append(env, k+"="+v)
	}
	if codexHome != "" {
		env = append(env, "CODEX_HOME="+codexHome)
	}
	return append(env, "KAINBAN_SHELL=1")
}

// writeCodexHome prepares a writable CODEX_HOME holding the
// refresh-token-less auth.json, like an agent pod's.
func writeCodexHome(dir string, authJSON []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	cfg := filepath.Join(dir, "config.toml")
	if _, err := os.Stat(cfg); os.IsNotExist(err) {
		if err := os.WriteFile(cfg, []byte("cli_auth_credentials_store = \"file\"\n"), 0o600); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(dir, "auth.json"), authJSON, 0o600)
}

// ensureAntigravitySettings selects the Gemini API key in agy's settings
// unless the user already has a settings file.
func ensureAntigravitySettings(home string) {
	f := filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
	if _, err := os.Stat(f); err == nil {
		return
	}
	if os.MkdirAll(filepath.Dir(f), 0o700) == nil {
		os.WriteFile(f, []byte("{\"modelProvider\": \"gemini\"}\n"), 0o600)
	}
}

func stateDir(home string) string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "kainban")
	}
	return filepath.Join(home, ".local", "state", "kainban")
}

func defaultShell() string {
	if s := os.Getenv("SHELL"); s != "" {
		return s
	}
	if _, err := exec.LookPath("bash"); err == nil {
		return "bash"
	}
	return "sh"
}
