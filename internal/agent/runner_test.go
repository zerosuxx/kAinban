package agent

import (
	"context"
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
		tk := &board.Ticket{ID: "t1", Title: "Do it", Description: "details", Agent: ag}
		name, err := r.Spawn(ctx, tk)
		if err != nil {
			t.Fatal(err)
		}
		p, err := cs.CoreV1().Pods("kb").Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		c := p.Spec.Containers[0]
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
		if env["KAINBAN_PROMPT"].Value != "Task: Do it\n\ndetails" {
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
	if _, err := r.Spawn(ctx, &board.Ticket{ID: "x"}); err == nil {
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
		if st[k] != v {
			t.Errorf("%s: %s, want %s", k, st[k], v)
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
