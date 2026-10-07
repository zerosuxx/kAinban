package tui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/zerosuxx/kainban/internal/auth"
)

// --- fakes ---

type fakeFlow struct {
	mu     sync.Mutex
	inputs []string
	closed int
	next   func(call int, input string) (auth.Step, error)
}

func (f *fakeFlow) Next(_ context.Context, input string) (auth.Step, error) {
	f.mu.Lock()
	f.inputs = append(f.inputs, input)
	call := len(f.inputs) - 1
	f.mu.Unlock()
	return f.next(call, input)
}

func (f *fakeFlow) Close() error { f.closed++; return nil }

type fakeProvider struct {
	id, title string
	status    auth.Status
	flows     []*fakeFlow
	newFlow   func() *fakeFlow
	mu        sync.Mutex
	checks    int
}

func (p *fakeProvider) ID() string            { return p.id }
func (p *fakeProvider) Title() string         { return p.title }
func (p *fakeProvider) SecretNames() []string { return []string{p.id + "-auth"} }
func (p *fakeProvider) Check(context.Context, auth.Store, bool) auth.Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.checks++
	return p.status
}
func (p *fakeProvider) NewFlow() auth.Flow {
	f := &fakeFlow{next: func(int, string) (auth.Step, error) {
		return auth.Step{Kind: auth.StepInput, Title: "token"}, nil
	}}
	if p.newFlow != nil {
		f = p.newFlow()
	}
	p.flows = append(p.flows, f)
	return f
}

type put struct {
	provider, name string
	secret         *auth.Secret
}

type fakeStore struct {
	mu   sync.Mutex
	puts []put
}

func (s *fakeStore) Get(context.Context, string) (*auth.Secret, error) { return nil, auth.ErrNotFound }
func (s *fakeStore) Put(_ context.Context, provider, name string, sec *auth.Secret) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts = append(s.puts, put{provider, name, sec})
	return nil
}

// --- driver ---

// drive executes cmd and every command produced by the resulting updates,
// synchronously, until nothing is left.
func drive(t *testing.T, m *model, cmd tea.Cmd) {
	t.Helper()
	queue := []tea.Cmd{cmd}
	for n := 0; len(queue) > 0; n++ {
		if n > 1000 {
			t.Fatal("too many commands")
		}
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		switch msg := c().(type) {
		case nil:
		case tea.BatchMsg:
			queue = append(queue, msg...)
		default:
			_, next := m.Update(msg)
			queue = append(queue, next)
		}
	}
}

func press(t *testing.T, m *model, keys ...string) {
	t.Helper()
	for _, k := range keys {
		var msg tea.KeyPressMsg
		switch k {
		case "enter":
			msg = tea.KeyPressMsg{Code: tea.KeyEnter}
		case "esc":
			msg = tea.KeyPressMsg{Code: tea.KeyEscape}
		case "down":
			msg = tea.KeyPressMsg{Code: tea.KeyDown}
		case "ctrl+s":
			msg = tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl}
		default:
			r := []rune(k)[0]
			msg = tea.KeyPressMsg{Code: r, Text: k}
		}
		_, cmd := m.Update(msg)
		drive(t, m, cmd)
	}
}

func typeText(t *testing.T, m *model, s string) {
	t.Helper()
	for _, r := range s {
		press(t, m, string(r))
	}
}

func setup(t *testing.T, ps ...*fakeProvider) (*model, *fakeStore) {
	t.Helper()
	providers := make([]auth.Provider, len(ps))
	for i, p := range ps {
		providers[i] = p
	}
	store := &fakeStore{}
	m := newModel(context.Background(), providers, store, Options{Namespace: "kainban"})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	drive(t, m, m.checkAll()) // Init minus the endless spinner tick
	return m, store
}

func viewOf(m *model) string { return m.View().Content }

// --- tests ---

func TestOverviewChecks(t *testing.T) {
	a := &fakeProvider{id: "codex", title: "Codex", status: auth.Status{State: auth.StateValid, Detail: "all good"}}
	b := &fakeProvider{id: "gh", title: "GitHub", status: auth.Status{State: auth.StateMissing, Detail: "no secret"}}
	m, _ := setup(t, a, b)

	if m.rows[0].status.State != auth.StateValid || m.rows[1].status.State != auth.StateMissing {
		t.Fatalf("rows not populated: %+v", m.rows)
	}
	v := viewOf(m)
	for _, want := range []string{"kAinban · auth", "kainban", "Codex", "GitHub", "valid", "missing", "all good", "no secret"} {
		if !strings.Contains(v, want) {
			t.Errorf("view missing %q:\n%s", want, v)
		}
	}

	press(t, m, "l")
	if !m.opts.Live || a.checks != 2 || b.checks != 2 {
		t.Errorf("l should toggle live and re-check: live=%v checks=%d/%d", m.opts.Live, a.checks, b.checks)
	}
	press(t, m, "r")
	if a.checks != 3 {
		t.Errorf("r should re-check, got %d checks", a.checks)
	}
}

func TestWizardSkipsValid(t *testing.T) {
	a := &fakeProvider{id: "a", title: "A", status: auth.Status{State: auth.StateValid}}
	b := &fakeProvider{id: "b", title: "B", status: auth.Status{State: auth.StateMissing}}
	c := &fakeProvider{id: "c", title: "C", status: auth.Status{State: auth.StateInvalid}}
	m, _ := setup(t, a, b, c)

	press(t, m, "a")
	if m.mode != modeFlow || m.fs.idx != 1 {
		t.Fatalf("wizard should start with B, mode=%v idx=%d", m.mode, m.fs.idx)
	}
	if len(a.flows) != 0 {
		t.Fatal("valid provider A must be skipped")
	}
	press(t, m, "ctrl+s")
	if b.flows[0].closed != 1 {
		t.Error("skipped flow not closed")
	}
	if m.mode != modeFlow || m.fs.idx != 2 {
		t.Fatalf("wizard should move to C, mode=%v idx=%d", m.mode, m.fs.idx)
	}
	press(t, m, "ctrl+s")
	if m.mode != modeOverview || m.wizard {
		t.Fatalf("wizard should end, mode=%v wizard=%v", m.mode, m.wizard)
	}
	if c.flows[0].closed != 1 {
		t.Error("C flow not closed")
	}
}

func TestInputSubmitAndDone(t *testing.T) {
	p := &fakeProvider{id: "codex", title: "Codex", status: auth.Status{State: auth.StateMissing}}
	p.newFlow = func() *fakeFlow {
		return &fakeFlow{next: func(call int, input string) (auth.Step, error) {
			if call == 0 {
				return auth.Step{Kind: auth.StepInput, Title: "Paste token", Prompt: "token:", Secret: true}, nil
			}
			return auth.Step{Kind: auth.StepDone, Secrets: map[string]*auth.Secret{
				"codex-auth":  {Data: map[string][]byte{"k": []byte(input)}},
				"codex-extra": {Data: map[string][]byte{"x": []byte("y")}},
			}}, nil
		}}
	}
	m, store := setup(t, p)

	press(t, m, "enter")
	if m.fs.step == nil || m.fs.step.Kind != auth.StepInput {
		t.Fatalf("expected input step, got %+v", m.fs.step)
	}
	typeText(t, m, "s3cret")
	v := viewOf(m)
	if strings.Contains(v, "s3cret") || !strings.Contains(v, "******") {
		t.Fatalf("secret input must be masked:\n%s", v)
	}

	p.status = auth.Status{State: auth.StateValid}
	press(t, m, "enter")

	f := p.flows[0]
	if len(f.inputs) != 2 || f.inputs[0] != "" || f.inputs[1] != "s3cret" {
		t.Fatalf("Next inputs = %q", f.inputs)
	}
	if len(store.puts) != 2 {
		t.Fatalf("puts = %+v", store.puts)
	}
	for i, name := range []string{"codex-auth", "codex-extra"} {
		if store.puts[i].provider != "codex" || store.puts[i].name != name {
			t.Errorf("put %d = %s/%s", i, store.puts[i].provider, store.puts[i].name)
		}
	}
	if f.closed != 1 {
		t.Errorf("flow closed %d times", f.closed)
	}
	if m.mode != modeOverview || m.rows[0].status.State != auth.StateValid {
		t.Errorf("should be back on overview with re-checked state: mode=%v state=%v", m.mode, m.rows[0].status.State)
	}
	if v := viewOf(m); !strings.Contains(v, "saved: codex-auth, codex-extra") || strings.Contains(v, "s3cret") {
		t.Errorf("overview notice wrong:\n%s", v)
	}
}

func TestNextErrorKeepsStep(t *testing.T) {
	p := &fakeProvider{id: "gh", title: "GitHub"}
	p.newFlow = func() *fakeFlow {
		return &fakeFlow{next: func(call int, input string) (auth.Step, error) {
			switch call {
			case 0:
				return auth.Step{Kind: auth.StepInput, Title: "Enter code"}, nil
			case 1:
				return auth.Step{}, errors.New("bad code")
			default:
				return auth.Step{Kind: auth.StepInput, Title: "Second"}, nil
			}
		}}
	}
	m, _ := setup(t, p)
	press(t, m, "enter")
	typeText(t, m, "abc")
	press(t, m, "enter")

	if m.fs.step.Title != "Enter code" || m.fs.input.Value() != "abc" {
		t.Fatalf("step/input not kept: %q %q", m.fs.step.Title, m.fs.input.Value())
	}
	if v := viewOf(m); !strings.Contains(v, "bad code") || !strings.Contains(v, "abc") {
		t.Fatalf("error not shown:\n%s", v)
	}
	press(t, m, "enter")
	f := p.flows[0]
	if f.inputs[2] != "abc" || m.fs.step.Title != "Second" || m.fs.err != nil {
		t.Fatalf("retry failed: inputs=%q step=%q err=%v", f.inputs, m.fs.step.Title, m.fs.err)
	}
}

func TestAbortCloses(t *testing.T) {
	p := &fakeProvider{id: "gh", title: "GitHub"}
	m, store := setup(t, p)
	press(t, m, "enter")
	typeText(t, m, "q") // 'q' and 's' are typeable in the flow
	if m.mode != modeFlow || m.fs.input.Value() != "q" {
		t.Fatalf("q should be typed, mode=%v value=%q", m.mode, m.fs.input.Value())
	}
	press(t, m, "esc")
	if p.flows[0].closed != 1 || m.mode != modeOverview {
		t.Fatalf("esc: closed=%d mode=%v", p.flows[0].closed, m.mode)
	}
	if len(store.puts) != 0 {
		t.Fatal("abort must not save")
	}
}

func TestExecStep(t *testing.T) {
	p := &fakeProvider{id: "claude", title: "Claude"}
	p.newFlow = func() *fakeFlow {
		return &fakeFlow{next: func(call int, input string) (auth.Step, error) {
			if call == 0 {
				return auth.Step{Kind: auth.StepExec, Title: "Login", Command: []string{"claude", "login"}}, nil
			}
			return auth.Step{Kind: auth.StepInput, Title: "after"}, nil
		}}
	}
	m, _ := setup(t, p)
	press(t, m, "enter")
	if v := viewOf(m); !strings.Contains(v, "press enter to run") || !strings.Contains(v, "claude login") {
		t.Fatalf("exec prompt missing:\n%s", v)
	}
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter should return an ExecProcess cmd")
	}
	// Simulate the process exiting with an error instead of running it.
	_, cmd = m.Update(execDoneMsg{gen: m.fs.gen, err: errors.New("exit status 1")})
	drive(t, m, cmd)
	f := p.flows[0]
	if len(f.inputs) != 2 || f.inputs[1] != "" || m.fs.step.Title != "after" {
		t.Fatalf("Next not called after exec: %q", f.inputs)
	}
	if !strings.Contains(viewOf(m), "exit status 1") {
		t.Error("exec error not shown")
	}
}

func TestQuitClosesFlow(t *testing.T) {
	p := &fakeProvider{id: "gh", title: "GitHub"}
	m, _ := setup(t, p)
	press(t, m, "enter")
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if p.flows[0].closed != 1 {
		t.Error("ctrl+c must close the flow")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("ctrl+c must quit")
	}
}

func TestWrapKeepsURLs(t *testing.T) {
	url := "https://auth.openai.com/oauth/authorize?client_id=abc&redirect_uri=http%3A%2F%2Flocalhost%3A1455&state=" + strings.Repeat("x", 80)
	out := wrapText("Open this link in your browser to sign in: "+url+" and then paste the code below please.", 30)
	found := false
	for _, line := range strings.Split(out, "\n") {
		if line == url {
			found = true
		}
		if line != url && len(line) > 30 {
			t.Errorf("line too long: %q", line)
		}
	}
	if !found {
		t.Fatalf("URL not on its own line:\n%s", out)
	}
}

func TestTitleShowsAppVersion(t *testing.T) {
	m := newModel(context.Background(), nil, nil, Options{AppVersion: "1.2.3"})
	if v := m.overviewView(); !strings.Contains(v, "1.2.3") {
		t.Fatalf("app version missing from title: %q", v)
	}
}
