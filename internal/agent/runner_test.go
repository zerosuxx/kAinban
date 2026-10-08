package agent

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/zerosuxx/kainban/internal/auth"
	"github.com/zerosuxx/kainban/internal/board"
)

func envNames(c corev1.Container) map[string]corev1.EnvVar {
	m := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		m[e.Name] = e
	}
	return m
}

func TestSpawnOnlyMountsTheTicketsAgentCredentials(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	r := NewRunner(cs, Config{Namespace: "kb", Image: "img:1", NodeSelector: map[string]string{"kubernetes.io/arch": "amd64"}})

	cases := map[board.AgentType][]string{
		"claude":      {"CLAUDE_CODE_OAUTH_TOKEN"},
		"copilot":     {"COPILOT_GITHUB_TOKEN"},
		"antigravity": {"GEMINI_API_KEY"},
		"codex":       nil,
	}
	all := []string{"CLAUDE_CODE_OAUTH_TOKEN", "COPILOT_GITHUB_TOKEN", "GEMINI_API_KEY"}
	for ag, want := range cases {
		tk := &board.Ticket{ID: "t1", Key: "KAI-7", Title: "Do it", Description: "details", Agent: ag}
		name, got, err := r.Spawn(ctx, tk)
		if err != nil {
			t.Fatal(err)
		}
		if got != ag {
			t.Errorf("spawned %q, want %q", got, ag)
		}
		p, err := cs.CoreV1().Pods("kb").Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Spec.Containers) != 2 || p.Spec.Containers[0].Name != "agent" || p.Spec.Containers[1].Name != "shell" ||
			!p.Spec.Containers[1].TTY || !strings.Contains(p.Spec.Containers[1].Command[2], "sleep infinity") {
			t.Fatalf("%s: containers %+v", ag, p.Spec.Containers)
		}
		c := p.Spec.Containers[0]
		if envNames(c)["CODEX_HOME"].Value != "/sessions/codex" || envNames(c)["CLAUDE_CONFIG_DIR"].Value != "/sessions/claude" {
			t.Errorf("%s: session dirs not on the shared volume", ag)
		}
		env := envNames(c)
		for _, n := range all {
			_, has := env[n]
			if has != strings.Contains(strings.Join(want, ","), n) {
				t.Errorf("%s: env %s present=%v", ag, n, has)
			}
		}
		if _, ok := env["GH_TOKEN"]; !ok {
			t.Errorf("%s: GH_TOKEN missing", ag)
		}
		if env["KAINBAN_PROMPT"].Value != "Task KAI-7: Do it\n\ndetails" || env["KAINBAN_TICKET_KEY"].Value != "KAI-7" || p.Labels[LabelTicketKey] != "KAI-7" {
			t.Errorf("%s: prompt %q", ag, env["KAINBAN_PROMPT"].Value)
		}
		for _, v := range p.Spec.Volumes {
			if v.Secret != nil && v.Secret.SecretName == auth.CodexRefreshSecretName {
				t.Fatalf("%s: refresh secret mounted", ag)
			}
			if v.Secret != nil && ag != "codex" {
				t.Errorf("%s: unexpected secret volume %s", ag, v.Secret.SecretName)
			}
		}
		if *p.Spec.AutomountServiceAccountToken || p.Spec.RestartPolicy != corev1.RestartPolicyNever ||
			p.Spec.NodeSelector["kubernetes.io/arch"] != "amd64" || c.Image != "img:1" ||
			p.Labels[LabelComponent] != ComponentAgent || p.Labels[LabelTicket] != "t1" {
			t.Errorf("%s: pod spec %+v", ag, p.Spec)
		}
		if !strings.Contains(c.Command[2], agentCommands[ag]) {
			t.Errorf("%s: command %q", ag, c.Command[2])
		}
	}
	if _, _, err := r.Spawn(ctx, &board.Ticket{ID: "x"}); err == nil {
		t.Error("spawn without agent should fail")
	}
}

func TestStatusesAndStop(t *testing.T) {
	ctx := context.Background()
	mk := func(name string, phase corev1.PodPhase, waiting string) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kb", Labels: map[string]string{LabelComponent: ComponentAgent}},
			Status: corev1.PodStatus{Phase: phase}}
		if waiting != "" {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: waiting}}}}
		}
		return p
	}
	cs := fake.NewSimpleClientset(
		mk("run", corev1.PodRunning, ""), mk("ok", corev1.PodSucceeded, ""), mk("bad", corev1.PodFailed, ""),
		mk("pull", corev1.PodPending, "ImagePullBackOff"), mk("new", corev1.PodPending, "ContainerCreating"),
	)
	r := NewRunner(cs, Config{Namespace: "kb", Image: "i"})
	st, err := r.Statuses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]board.AgentStatus{"run": board.AgentRunning, "ok": board.AgentCompleted, "bad": board.AgentError,
		"pull": board.AgentError, "new": board.AgentWaiting}
	for k, v := range want {
		if st[k].Status != v {
			t.Errorf("%s: %s, want %s", k, st[k].Status, v)
		}
	}
	if err := r.Stop(ctx, "run"); err != nil {
		t.Fatal(err)
	}
	if err := r.Stop(ctx, "run"); err != nil {
		t.Fatalf("stopping a missing pod: %v", err)
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv("KAINBAN_AGENT_IMAGE", "")
	if _, err := FromEnv("kb"); err == nil {
		t.Error("missing image should fail")
	}
	t.Setenv("KAINBAN_AGENT_IMAGE", "img")
	t.Setenv("KAINBAN_AGENT_NODE_SELECTOR", `{"a":"b"}`)
	c, err := FromEnv("kb")
	if err != nil || c.NodeSelector["a"] != "b" || c.Image != "img" {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestStatusFromAgentContainer(t *testing.T) {
	pod := func(st corev1.ContainerState) *corev1.Pod {
		return &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{
			{Name: "shell", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			{Name: "agent", State: st},
		}}}
	}
	cases := []struct {
		st   corev1.ContainerState
		want board.AgentStatus
	}{
		{corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}, board.AgentRunning},
		{corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}, board.AgentCompleted},
		{corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 2}}, board.AgentError},
	}
	for _, c := range cases {
		if got := phaseStatus(pod(c.st)); got != c.want {
			t.Errorf("%+v: %s, want %s", c.st, got, c.want)
		}
	}
}

func TestAttachCommand(t *testing.T) {
	r := NewRunner(nil, Config{Namespace: "kb", AttachArgs: []string{"--context", "c"}})
	argv := r.AttachCommand("p1", "codex", false)
	got := strings.Join(argv[1:], " ")
	if !strings.HasPrefix(got, "attach --context c --namespace kb --pod p1 --container shell -- sh -c") || !strings.Contains(got, "codex resume --last") {
		t.Fatalf("attach: %s", got)
	}
	inner := argv[len(argv)-1]
	for _, want := range []string{"exec tmux -u new-session -A -s agent 'codex resume --last", `\; bind -n C-z detach-client`, "TERM="} {
		if !strings.Contains(inner, want) {
			t.Fatalf("attach: missing %q in %s", want, inner)
		}
	}
	if got := strings.Join(r.AttachCommand("p1", "claude", true), " "); !strings.Contains(got, "-s shell 'bash'") || strings.Contains(got, "bash -l") {
		t.Fatalf("shell: %s", got)
	}
}

func TestShQuote(t *testing.T) {
	out, err := exec.Command("sh", "-c", "printf %s "+shQuote(`it's "$X" \; ok`)).Output()
	if err != nil || string(out) != `it's "$X" \; ok` {
		t.Fatalf("shQuote: %q %v", out, err)
	}
}

func TestAutoPicksFirstAgentWithCredentials(t *testing.T) {
	ctx := context.Background()
	secret := func(name string) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kb"}}
	}
	auto := &board.Ticket{ID: "a", Title: "x", Agent: board.AgentAuto}

	r := NewRunner(fake.NewSimpleClientset(), Config{Namespace: "kb", Image: "i"})
	if _, _, err := r.Spawn(ctx, auto); err == nil || !strings.Contains(err.Error(), "no agent has credentials") {
		t.Fatalf("no credentials: %v", err)
	}

	cs := fake.NewSimpleClientset(secret(auth.CopilotSecretName), secret(auth.CodexSecretName))
	r = NewRunner(cs, Config{Namespace: "kb", Image: "i"})
	name, ag, err := r.Spawn(ctx, auto)
	if err != nil || ag != "codex" {
		t.Fatalf("auto picked %q: %v", ag, err)
	}
	p, _ := cs.CoreV1().Pods("kb").Get(ctx, name, metav1.GetOptions{})
	if p.Labels[LabelAgent] != "codex" || !strings.Contains(p.Spec.Containers[0].Command[2], "codex exec") {
		t.Fatalf("pod not for codex: %v", p.Labels)
	}
}

func TestStopTicketDeletesAllItsPods(t *testing.T) {
	ctx := context.Background()
	pod := func(name, ticket string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kb",
			Labels: map[string]string{LabelComponent: ComponentAgent, LabelTicket: ticket}}}
	}
	cs := fake.NewSimpleClientset(pod("a1", "t1"), pod("a2", "t1"), pod("b1", "t2"))
	r := NewRunner(cs, Config{Namespace: "kb", Image: "i"})
	if err := r.StopTicket(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	st, _ := r.Statuses(ctx)
	if len(st) != 1 || st["b1"].Ticket != "t2" {
		t.Fatalf("left: %v", st)
	}
}

func TestModels(t *testing.T) {
	t.Setenv("KAINBAN_AGENT_IMAGE", "img")
	t.Setenv("KAINBAN_AGY_MODEL", "gemini-flash")
	c, err := FromEnv("kb")
	if err != nil || c.AgyModel != "gemini-flash" {
		t.Fatalf("%+v %v", c, err)
	}
	cs := fake.NewSimpleClientset()
	r := NewRunner(cs, c)
	spawn := func(tk *board.Ticket) (string, string) {
		t.Helper()
		name, _, err := r.Spawn(context.Background(), tk)
		if err != nil {
			t.Fatal(err)
		}
		p, _ := cs.CoreV1().Pods("kb").Get(context.Background(), name, metav1.GetOptions{})
		return envNames(p.Spec.Containers[0])["KAINBAN_MODEL"].Value, p.Spec.Containers[0].Command[2]
	}
	if m, cmd := spawn(&board.Ticket{ID: "a", Title: "x", Agent: "antigravity"}); m != "gemini-flash" || !strings.Contains(cmd, `--model "$KAINBAN_MODEL"`) {
		t.Fatalf("agy default model: %q %s", m, cmd)
	}
	if m, _ := spawn(&board.Ticket{ID: "b", Title: "x", Agent: "antigravity", Model: "gemini-3.8-flash-high"}); m != "gemini-3.8-flash-high" {
		t.Fatalf("ticket model should win: %q", m)
	}
	if m, cmd := spawn(&board.Ticket{ID: "c", Title: "x", Agent: "codex", Model: "gpt-5-codex"}); m != "gpt-5-codex" || !strings.Contains(cmd, `-m "$KAINBAN_MODEL"`) {
		t.Fatalf("codex model: %q %s", m, cmd)
	}
	if m, _ := spawn(&board.Ticket{ID: "d", Title: "x", Agent: "claude"}); m != "" {
		t.Fatalf("no model: %q", m)
	}
	if got := strings.Join(r.AttachCommand("p", "codex", false), " "); !strings.Contains(got, `codex resume --last`) || !strings.Contains(got, `-m "$KAINBAN_MODEL"`) {
		t.Fatalf("resume keeps the model: %s", got)
	}
}
