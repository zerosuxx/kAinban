package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

// FormatStream turns a CLI's JSON event stream (claude and agy
// --output-format stream-json, codex exec --json) into a compact transcript for the pod log,
// which is what the board's `o` shows while the agent works: the agent's
// messages in full, one line per tool call, a short preview of each tool
// result and a summary at the end. Lines that are not JSON, or events it
// does not know, pass through unchanged or are skipped.
func FormatStream(agent string, in io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024) // tool results can be big
	w := bufio.NewWriter(out)
	f := &streamFormatter{w: w}
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		if line[0] != '{' || !json.Valid(line) {
			fmt.Fprintf(w, "%s\n", line)
		} else {
			switch agent {
			case "codex":
				f.codex(line)
			case "antigravity":
				f.agy(line)
			default:
				f.claude(line)
			}
		}
		if err := w.Flush(); err != nil { // line by line: the log is live
			return err
		}
	}
	f.endText()
	if err := w.Flush(); err != nil {
		return err
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if f.failed {
		return ErrRunFailed
	}
	return nil
}

// ErrRunFailed: the stream's final event reported a failed run, so the
// pipeline fails even when the CLI itself exits 0.
var ErrRunFailed = errors.New("the agent run failed")

type streamFormatter struct {
	w      io.Writer
	inText bool // agy: streaming a message's text deltas
	textNL bool // … and the last delta ended a line
	failed bool // the final event reported a failure
}

func (f *streamFormatter) printf(format string, a ...any) {
	fmt.Fprintf(f.w, format, a...)
}

type claudeEvent struct {
	Type       string  `json:"type"`
	Subtype    string  `json:"subtype"`
	Model      string  `json:"model"`
	IsError    bool    `json:"is_error"`
	NumTurns   int     `json:"num_turns"`
	DurationMS int64   `json:"duration_ms"`
	CostUSD    float64 `json:"total_cost_usd"`
	Result     string  `json:"result"`
	Message    struct {
		Content []claudeBlock `json:"content"`
	} `json:"message"`
}

type claudeBlock struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input"`
	Content json.RawMessage `json:"content"` // tool_result: a string or text blocks
	IsError bool            `json:"is_error"`
}

func (f *streamFormatter) claude(line []byte) {
	var ev claudeEvent
	if json.Unmarshal(line, &ev) != nil {
		return
	}
	switch ev.Type {
	case "system":
		if ev.Subtype == "init" {
			f.printf("· %s\n", ev.Model)
		}
	case "assistant":
		for _, b := range ev.Message.Content {
			switch b.Type {
			case "text":
				if t := strings.TrimSpace(b.Text); t != "" {
					f.printf("\n%s\n\n", t)
				}
			case "tool_use":
				f.printf("→ %s %s\n", b.Name, toolInput(b.Input))
			}
		}
	case "user":
		for _, b := range ev.Message.Content {
			if b.Type == "tool_result" {
				f.result(resultText(b.Content), b.IsError)
			}
		}
	case "result":
		took := (time.Duration(ev.DurationMS) * time.Millisecond).Round(time.Second)
		if ev.IsError || ev.Subtype != "success" {
			f.printf("✗ %s after %s, %d turns: %s\n", ev.Subtype, took, ev.NumTurns, preview(ev.Result, 3))
			f.failed = true
			return
		}
		f.printf("✓ done in %s, %d turns, $%.2f\n", took, ev.NumTurns, ev.CostUSD)
	}
}

type codexEvent struct {
	Type    string `json:"type"`
	Message string `json:"message"` // type error
	Error   struct {
		Message string `json:"message"`
	} `json:"error"` // type turn.failed
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		CachedTokens int `json:"cached_input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Item struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Command  string `json:"command"`
		ExitCode *int   `json:"exit_code"`
		Output   string `json:"aggregated_output"`
		Server   string `json:"server"`
		Tool     string `json:"tool"`
		Query    string `json:"query"`
		Message  string `json:"message"`
		Changes  []struct {
			Path string `json:"path"`
			Kind string `json:"kind"`
		} `json:"changes"`
	} `json:"item"`
}

// bashLC unwraps codex's `/bin/bash -lc '…'` around every command.
var bashLC = regexp.MustCompile(`^\S*bash -lc '(.*)'$`)

func (f *streamFormatter) codex(line []byte) {
	var ev codexEvent
	if json.Unmarshal(line, &ev) != nil {
		return
	}
	it := ev.Item
	switch ev.Type {
	case "item.started":
		switch it.Type {
		case "command_execution":
			cmd := it.Command
			if m := bashLC.FindStringSubmatch(cmd); m != nil {
				cmd = m[1]
			}
			f.printf("→ $ %s\n", oneLine(cmd, 200))
		case "mcp_tool_call":
			f.printf("→ %s.%s\n", it.Server, it.Tool)
		case "web_search":
			f.printf("→ search %s\n", oneLine(it.Query, 160))
		}
	case "item.completed":
		switch it.Type {
		case "agent_message":
			if t := strings.TrimSpace(it.Text); t != "" {
				f.printf("\n%s\n\n", t)
			}
		case "command_execution":
			failed := it.ExitCode != nil && *it.ExitCode != 0
			out := it.Output
			if failed {
				out = fmt.Sprintf("exit %d\n%s", *it.ExitCode, out)
			}
			f.result(out, failed)
		case "error": // a warning; failures end the turn with turn.failed
			f.printf("! %s\n", oneLine(it.Message, 300))
		case "file_change":
			for _, c := range it.Changes {
				f.printf("→ %s %s\n", c.Kind, workPath(c.Path))
			}
		}
	case "turn.completed":
		u := ev.Usage
		f.printf("✓ done, %d input tokens (%d cached), %d output\n", u.InputTokens, u.CachedTokens, u.OutputTokens)
	case "turn.failed":
		f.printf("✗ %s\n", ev.Error.Message)
		f.failed = true
	case "error":
		f.printf("✗ %s\n", ev.Message)
	}
}

type agyEvent struct {
	Event string `json:"event"`
	Init  struct {
		Model string `json:"model"`
	} `json:"init"`
	Step struct {
		State    string `json:"state"`
		Type     string `json:"step_type"`
		Delta    string `json:"text_delta"`
		ToolName string `json:"tool_name"`
		ToolInfo struct {
			Parameters json.RawMessage `json:"parameters"`
			Output     string          `json:"output"`
		} `json:"tool_info"`
	} `json:"step_update"`
	Result struct {
		Status   string  `json:"status"`
		Response string  `json:"response"`
		Error    string  `json:"error"`
		Seconds  float64 `json:"duration_seconds"`
		Usage    struct {
			Total int `json:"total_tokens"`
		} `json:"usage"`
	} `json:"result"`
}

func (f *streamFormatter) agy(line []byte) {
	var ev agyEvent
	if json.Unmarshal(line, &ev) != nil {
		return
	}
	st := ev.Step
	if st.Delta == "" {
		f.endText()
	}
	switch {
	case ev.Event == "init":
		f.printf("· %s\n", ev.Init.Model)
	case st.Delta != "": // the answer, streamed as it is written
		if !f.inText {
			f.printf("\n")
			f.inText = true
		}
		f.printf("%s", st.Delta)
		f.textNL = strings.HasSuffix(st.Delta, "\n")
	case st.Type == "tool" && st.State == "ACTIVE":
		f.printf("→ %s %s\n", st.ToolName, toolInput(st.ToolInfo.Parameters))
	case st.Type == "tool" && st.State == "DONE":
		f.result(st.ToolInfo.Output, false)
	case ev.Event == "result":
		r := ev.Result
		took := time.Duration(r.Seconds * float64(time.Second)).Round(time.Second)
		if r.Status != "ERROR" || strings.TrimSpace(r.Response) != "" {
			f.printf("✓ done in %s, %d tokens\n", took, r.Usage.Total)
			if r.Error != "" { // agy reports retries it recovered from as errors
				f.printf("  (agy: %s)\n", oneLine(r.Error, 160))
			}
			return
		}
		f.printf("✗ %s after %s: %s\n", strings.ToLower(r.Status), took, oneLine(r.Error, 300))
		f.failed = true
	}
}

// endText closes a streamed message.
func (f *streamFormatter) endText() {
	if f.inText {
		if !f.textNL {
			f.printf("\n")
		}
		f.printf("\n")
		f.inText = false
	}
}

// result previews a tool result under its call.
func (f *streamFormatter) result(text string, failed bool) {
	p := preview(text, 3)
	if p == "" {
		return
	}
	mark := "  ← "
	if failed {
		mark = "  ✗ "
	}
	f.printf("%s%s\n", mark, strings.ReplaceAll(p, "\n", "\n    "))
}

// toolInput is the one-line gist of a tool call's input: its command, path,
// pattern or URL when it has one, else the compact JSON.
func toolInput(raw json.RawMessage) string {
	var in map[string]any
	if json.Unmarshal(raw, &in) != nil {
		return oneLine(string(raw), 160)
	}
	for _, k := range []string{"command", "CommandLine", "file_path", "TargetFile", "AbsolutePath", "pattern", "path", "url", "Url", "query", "Query", "prompt", "description"} {
		if v, ok := in[k].(string); ok && v != "" {
			s := v
			if k == "file_path" || k == "path" || k == "TargetFile" || k == "AbsolutePath" {
				s = workPath(v)
			}
			if k == "pattern" {
				if p, ok := in["path"].(string); ok && p != "" {
					s += " in " + workPath(p)
				}
			}
			return oneLine(s, 200)
		}
	}
	b, _ := json.Marshal(in)
	return oneLine(string(b), 160)
}

// resultText reads a tool_result's content: a string or a list of blocks.
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		} else {
			parts = append(parts, "["+b.Type+"]")
		}
	}
	return strings.Join(parts, "\n")
}

// preview keeps the first n non-empty lines (each cut to 160 runes) and says
// how many more there were.
func preview(text string, n int) string {
	var lines []string
	for l := range strings.SplitSeq(strings.TrimSpace(text), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	out := make([]string, 0, n+1)
	for _, l := range lines[:min(n, len(lines))] {
		out = append(out, cut(l, 160))
	}
	if len(lines) > n {
		out = append(out, fmt.Sprintf("… (+%d lines)", len(lines)-n))
	}
	return strings.Join(out, "\n")
}

// cut keeps a line's indentation (tabs as spaces) and cuts it to max runes.
func cut(s string, max int) string {
	s = strings.TrimRight(strings.ReplaceAll(s, "\t", "  "), " \r")
	if r := []rune(s); len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}

// oneLine collapses whitespace and cuts s to max runes.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}

func workPath(p string) string {
	if r, ok := strings.CutPrefix(p, "/work/"); ok {
		return r
	}
	return p
}
