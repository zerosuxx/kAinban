// Package agent runs a ticket's coding agent as a Kubernetes pod.
package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/zerosuxx/kainban/internal/auth"
	"github.com/zerosuxx/kainban/internal/board"
)

const (
	LabelComponent = "app.kubernetes.io/component"
	ComponentAgent = "agent"
	LabelTicket    = "kainban.io/ticket"
	LabelAgent     = "kainban.io/agent"
)

// Config describes the agent pods. FromEnv fills it from the variables the
// Helm chart sets on the orchestrator.
type Config struct {
	Namespace    string
	Image        string
	NodeSelector map[string]string
	KubectlArgs  []string // e.g. --context/--kubeconfig, for AttachCommand
}

// FromEnv reads KAINBAN_AGENT_IMAGE and KAINBAN_AGENT_NODE_SELECTOR (JSON).
func FromEnv(namespace string) (Config, error) {
	c := Config{Namespace: namespace, Image: os.Getenv("KAINBAN_AGENT_IMAGE")}
	if c.Image == "" {
		return c, fmt.Errorf("KAINBAN_AGENT_IMAGE is not set (run kainban in the orchestrator pod)")
	}
	if ns := os.Getenv("KAINBAN_AGENT_NODE_SELECTOR"); ns != "" && ns != "null" {
		if err := json.Unmarshal([]byte(ns), &c.NodeSelector); err != nil {
			return c, fmt.Errorf("KAINBAN_AGENT_NODE_SELECTOR: %w", err)
		}
	}
	return c, nil
}

// Runner starts, watches and stops agent pods.
type Runner struct {
	client kubernetes.Interface
	cfg    Config
}

func NewRunner(client kubernetes.Interface, cfg Config) *Runner {
	return &Runner{client: client, cfg: cfg}
}

// Spawn creates the agent pod for t and returns its name and the agent it
// runs (auto resolved to a concrete one).
func (r *Runner) Spawn(ctx context.Context, t *board.Ticket) (string, board.AgentType, error) {
	if t.Agent == "" {
		return "", "", fmt.Errorf("no agent chosen for this ticket (press a)")
	}
	ag := t.Agent
	if ag == board.AgentAuto {
		var err error
		if ag, err = r.chooseAgent(ctx, t); err != nil {
			return "", "", err
		}
	}
	tc := *t
	tc.Agent = ag
	pod, err := r.podFor(&tc)
	if err != nil {
		return "", "", err
	}
	created, err := r.client.CoreV1().Pods(r.cfg.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return "", "", fmt.Errorf("create pod: %w", err)
	}
	return created.Name, ag, nil
}

// autoPreference is the order auto tries agents in, with the Secret each
// one needs.
//
// TODO: replace with agent profiles (task type, cost, rate limits, current
// load) once they exist; for now the first agent with credentials wins.
var autoPreference = []struct {
	agent  board.AgentType
	secret string
}{
	{"claude", auth.ClaudeSecretName},
	{"codex", auth.CodexSecretName},
	{"copilot", auth.CopilotSecretName},
	{"antigravity", auth.AntigravitySecretName},
}

func (r *Runner) chooseAgent(ctx context.Context, _ *board.Ticket) (board.AgentType, error) {
	for _, p := range autoPreference {
		_, err := r.client.CoreV1().Secrets(r.cfg.Namespace).Get(ctx, p.secret, metav1.GetOptions{})
		if err == nil {
			return p.agent, nil
		}
		if !apierrors.IsNotFound(err) {
			return "", fmt.Errorf("auto: check %s: %w", p.secret, err)
		}
	}
	return "", fmt.Errorf("auto: no agent has credentials yet (run kainban auth)")
}

// Statuses maps every agent pod's name to its state and ticket.
func (r *Runner) Statuses(ctx context.Context) (map[string]board.PodState, error) {
	pods, err := r.client.CoreV1().Pods(r.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: LabelComponent + "=" + ComponentAgent,
	})
	if err != nil {
		return nil, err
	}
	out := map[string]board.PodState{}
	for _, p := range pods.Items {
		if p.DeletionTimestamp != nil {
			continue // being deleted: treat as gone
		}
		out[p.Name] = board.PodState{Status: phaseStatus(&p), Ticket: p.Labels[LabelTicket]}
	}
	return out, nil
}

// StopTicket deletes every agent pod of a ticket, including ones the board
// no longer tracks.
func (r *Runner) StopTicket(ctx context.Context, ticketID string) error {
	pods, err := r.client.CoreV1().Pods(r.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: LabelComponent + "=" + ComponentAgent + "," + LabelTicket + "=" + ticketID,
	})
	if err != nil {
		return err
	}
	for _, p := range pods.Items {
		if err := r.Stop(ctx, p.Name); err != nil {
			return err
		}
	}
	return nil
}

// phaseStatus maps a pod to the agent state. The "agent" container runs the
// headless task and exits; the "shell" container keeps the pod (and the
// session) around for `t`, so the agent container's state is what counts.
func phaseStatus(p *corev1.Pod) board.AgentStatus {
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Name != agentContainer {
			continue
		}
		switch {
		case cs.State.Terminated != nil && cs.State.Terminated.ExitCode == 0:
			return board.AgentCompleted
		case cs.State.Terminated != nil:
			return board.AgentError
		case cs.State.Running != nil:
			return board.AgentRunning
		}
	}
	switch p.Status.Phase {
	case corev1.PodSucceeded:
		return board.AgentCompleted
	case corev1.PodFailed:
		return board.AgentError
	case corev1.PodPending:
		for _, cs := range p.Status.ContainerStatuses {
			if w := cs.State.Waiting; w != nil && w.Reason != "ContainerCreating" && w.Reason != "PodInitializing" {
				return board.AgentError // ImagePullBackOff, CreateContainerConfigError, ...
			}
		}
		return board.AgentWaiting
	default:
		return board.AgentRunning
	}
}

// Logs returns the last lines of the agent's output.
func (r *Runner) Logs(ctx context.Context, pod string, tail int64) (string, error) {
	rc, err := r.client.CoreV1().Pods(r.cfg.Namespace).GetLogs(pod, &corev1.PodLogOptions{Container: agentContainer, TailLines: &tail}).Stream(ctx)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, 1<<20))
	return string(b), err
}

// Stop deletes the agent pod; a missing pod is not an error.
func (r *Runner) Stop(ctx context.Context, pod string) error {
	err := r.client.CoreV1().Pods(r.cfg.Namespace).Delete(ctx, pod, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

const (
	agentContainer = "agent"
	shellContainer = "shell"
)

// resumeCommands continue the agent's most recent session interactively in
// /work (`t` on the board).
var resumeCommands = map[board.AgentType]string{
	"claude":      "claude --continue --dangerously-skip-permissions",
	"codex":       "codex resume --last --dangerously-bypass-approvals-and-sandbox",
	"copilot":     "copilot --continue --allow-all-tools",
	"antigravity": "agy --continue --dangerously-skip-permissions",
}

// AttachCommand is the kubectl invocation that opens the agent's session
// (or, with shell, a plain shell) in the pod's shell container.
func (r *Runner) AttachCommand(pod string, agent board.AgentType, shell bool) []string {
	inner := "cd /work && exec " + resumeCommands[agent]
	if shell || resumeCommands[agent] == "" {
		inner = "cd /work && exec bash -l"
	}
	argv := append([]string{"kubectl"}, r.cfg.KubectlArgs...)
	return append(argv, "-n", r.cfg.Namespace, "exec", "-it", pod, "-c", shellContainer, "--", "sh", "-c", inner)
}

// agentCommands is the headless invocation per agent; the prompt is in
// $KAINBAN_PROMPT. Pods are the sandbox (bubblewrap does not work in them),
// so the CLIs' own sandboxes and approval prompts are off.
var agentCommands = map[board.AgentType]string{
	"claude":      `claude -p "$KAINBAN_PROMPT" --dangerously-skip-permissions`,
	"codex":       `codex exec --skip-git-repo-check --dangerously-bypass-approvals-and-sandbox "$KAINBAN_PROMPT" </dev/null`,
	"copilot":     `copilot -p "$KAINBAN_PROMPT" --allow-all-tools`,
	"antigravity": `agy -p "$KAINBAN_PROMPT" --dangerously-skip-permissions`,
}

// setupScript prepares credentials like the chart's test agent, then runs
// the agent in /work.
const setupScript = `set -e
mkdir -p /sessions/claude /sessions/codex /sessions/copilot /sessions/gemini /work
for d in copilot gemini; do
  [ -L "$HOME/.$d" ] || { rm -rf "$HOME/.$d"; ln -s "/sessions/$d" "$HOME/.$d"; }
done
if [ -f /secrets/codex/auth.json ]; then
  printf 'cli_auth_credentials_store = "file"\n' > "$CODEX_HOME/config.toml"
  cp /secrets/codex/auth.json "$CODEX_HOME/auth.json"
fi
if [ -n "${GEMINI_API_KEY:-}" ]; then
  mkdir -p "$HOME/.gemini/antigravity-cli"
  printf '{"modelProvider": "gemini"}\n' > "$HOME/.gemini/antigravity-cli/settings.json"
fi
if [ -n "${GH_TOKEN:-}" ]; then gh auth setup-git >/dev/null 2>&1 || true; fi
cd /work
echo "kainban: ticket $KAINBAN_TICKET_ID, agent $KAINBAN_AGENT"
`

// Prompt is what the agent is asked to do for t.
func Prompt(t *board.Ticket) string {
	p := "Task: " + t.Title
	if d := strings.TrimSpace(t.Description); d != "" {
		p += "\n\n" + d
	}
	return p
}

func (r *Runner) podFor(t *board.Ticket) (*corev1.Pod, error) {
	run, ok := agentCommands[t.Agent]
	if !ok {
		return nil, fmt.Errorf("unknown agent %q", t.Agent)
	}
	env := []corev1.EnvVar{
		{Name: "HOME", Value: "/home/ubuntu"},
		// Session/config dirs live on the volume both containers share.
		{Name: "CODEX_HOME", Value: "/sessions/codex"},
		{Name: "CLAUDE_CONFIG_DIR", Value: "/sessions/claude"},
		{Name: "KAINBAN_PROMPT", Value: Prompt(t)},
		{Name: "KAINBAN_TICKET_ID", Value: t.ID},
		{Name: "KAINBAN_AGENT", Value: string(t.Agent)},
		secretEnv("GH_TOKEN", auth.GitHubSecretName, auth.GitHubTokenKey),
	}
	var mounts []corev1.VolumeMount
	volumes := []corev1.Volume{
		{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "sessions", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	// /sessions holds the CLIs' session/config dirs, shared by both
	// containers so `t` can resume what the headless run did.
	mounts = append(mounts,
		corev1.VolumeMount{Name: "work", MountPath: "/work"},
		corev1.VolumeMount{Name: "tmp", MountPath: "/tmp"},
		corev1.VolumeMount{Name: "sessions", MountPath: "/sessions"})

	// Only the credentials of this ticket's agent.
	switch t.Agent {
	case "claude":
		env = append(env, secretEnv("CLAUDE_CODE_OAUTH_TOKEN", auth.ClaudeSecretName, auth.ClaudeTokenKey))
	case "copilot":
		env = append(env, secretEnv("COPILOT_GITHUB_TOKEN", auth.CopilotSecretName, auth.CopilotTokenKey))
	case "antigravity":
		env = append(env, secretEnv("GEMINI_API_KEY", auth.AntigravitySecretName, auth.AntigravityKeyKey))
	case "codex":
		mode := int32(0o440) // group-readable via fsGroup
		optional := true
		volumes = append(volumes, corev1.Volume{Name: "codex-auth", VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: auth.CodexSecretName, Optional: &optional, DefaultMode: &mode},
		}})
		mounts = append(mounts, corev1.VolumeMount{Name: "codex-auth", MountPath: "/secrets/codex", ReadOnly: true})
	}

	uid, noEscalation, nonRoot := int64(1000), false, true
	automount := false
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("kainban-agent-%s-%s", t.ID, randSuffix()),
			Namespace: r.cfg.Namespace,
			Labels: map[string]string{
				auth.LabelPartOf: auth.PartOfValue,
				LabelComponent:   ComponentAgent,
				LabelTicket:      t.ID,
				LabelAgent:       string(t.Agent),
				"role":           "agent",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: &automount,
			NodeSelector:                 r.cfg.NodeSelector,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsUser: &uid, RunAsGroup: &uid, FSGroup: &uid, RunAsNonRoot: &nonRoot,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{
				container(agentContainer, r.cfg.Image, setupScript+run, env, mounts, &noEscalation, false),
				// Keeps the pod and its sessions alive for `t` until x stops it.
				container(shellContainer, r.cfg.Image, setupScript+"exec sleep infinity", env, mounts, &noEscalation, true),
			},
			Volumes: volumes,
		},
	}, nil
}

func container(name, image, script string, env []corev1.EnvVar, mounts []corev1.VolumeMount, noEscalation *bool, tty bool) corev1.Container {
	return corev1.Container{
		Name:    name,
		Image:   image,
		Command: []string{"sh", "-c", script},
		Env:     env,
		Stdin:   tty,
		TTY:     tty,
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: noEscalation,
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		VolumeMounts: mounts,
	}
}

func secretEnv(name, secret, key string) corev1.EnvVar {
	optional := true
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: key, Optional: &optional,
	}}}
}

func randSuffix() string {
	b := make([]byte, 2)
	rand.Read(b)
	return hex.EncodeToString(b)
}
