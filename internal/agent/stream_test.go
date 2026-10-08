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

func TestFormatAgyStream(t *testing.T) {
	got := formatFile(t, "antigravity", "agy-stream.jsonl")
	for _, want := range []string{
		"· gemini-3.8-flash-medium",
		"→ write_to_file /tmp/tmp.av8tBNQfWA/hello.py",
		"→ run_command python3 hello.py\n  ← [0, 1, 1, 2, 3, 5, 8, 13, 21, 34]",
		"→ view_file /tmp/tmp.av8tBNQfWA/hello.py\n  ← 11 lines, 205 bytes",
		"\n### Output\n\nRunning `python3 hello.py` produced:",
		"✓ done in 1m10s, 63297 tokens\n  (agy: API error (attempt 2): Error 503",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	var out strings.Builder
	err := FormatStream("antigravity", strings.NewReader(`{"event":"result","result":{"status":"ERROR","error":"quota","duration_seconds":2}}`), &out)
	if out.String() != "✗ error after 2s: quota\n" || err != ErrRunFailed {
		t.Fatalf("failed run: %q %v", out.String(), err)
	}
}

func TestFormatStreamPassesOtherLines(t *testing.T) {
	var out strings.Builder
	in := "Error: not logged in\n{\"type\":\"turn.failed\",\"error\":{\"message\":\"boom\"}}\n{broken\n"
	if err := FormatStream("codex", strings.NewReader(in), &out); err != ErrRunFailed {
		t.Fatalf("turn.failed should fail the run: %v", err)
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
