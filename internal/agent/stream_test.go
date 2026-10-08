package agent

import (
	"os"
	"strings"
	"testing"
)

func formatFile(t *testing.T, agent, name string) string {
	t.Helper()
	in, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	var out strings.Builder
	if err := FormatStream(agent, in, &out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestFormatClaudeStream(t *testing.T) {
	got := formatFile(t, "claude", "claude-stream.jsonl")
	for _, want := range []string{
		"· claude-sonnet-5-5",
		"→ Write /tmp/tmp.44DV8X5eou/hello.py",
		"→ Bash python3 hello.py",
		"  ← 0\n    1\n    1\n    … (+7 lines)",
		"    3      print(a)",
		"I created `hello.py`",
		"✓ done in 4s, 4 turns, $0.03",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Count(got, "I created `hello.py`") != 1 {
		t.Errorf("the answer should be printed once:\n%s", got)
	}
}

func TestFormatCodexStream(t *testing.T) {
	got := formatFile(t, "codex", "codex-json.jsonl")
	for _, want := range []string{
		"I’ll create `hello.py`",
		"→ add /tmp/tmp.EJr5hrzgtl/hello.py",
		"→ $ python3 hello.py\n  ← 0",
		"→ $ cat hello.py",
		"✓ done, 28683 input tokens (24576 cached), 289 output",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestFormatStreamPassesOtherLines(t *testing.T) {
	var out strings.Builder
	in := "Error: not logged in\n{\"type\":\"turn.failed\",\"error\":{\"message\":\"boom\"}}\n{broken\n"
	if err := FormatStream("codex", strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "Error: not logged in\n✗ boom\n{broken\n" {
		t.Fatalf("%q", out.String())
	}
}

func TestPreviewAndToolInput(t *testing.T) {
	if p := preview("a\n\nb\nc\nd\ne", 3); p != "a\nb\nc\n… (+2 lines)" {
		t.Fatalf("preview: %q", p)
	}
	if s := toolInput([]byte(`{"pattern":"TODO","path":"/work/src"}`)); s != "TODO in src" {
		t.Fatalf("grep: %q", s)
	}
	if s := toolInput([]byte(`{"todos":[1,2]}`)); s != `{"todos":[1,2]}` {
		t.Fatalf("fallback: %q", s)
	}
}
