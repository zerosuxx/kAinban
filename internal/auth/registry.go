package auth

// All returns every provider in the order the TUI shows them.
func All() []Provider {
	return []Provider{
		NewClaude(),
		NewCodex(),
		NewGitHub(),
		NewCopilot(),
		NewAntigravity(),
	}
}
