# kAinban

kAinban is a Kubernetes-native orchestrator for AI coding agents (Claude Code,
Codex, GitHub Copilot CLI, Antigravity/Gemini). It runs as a single pod in its
own namespace and will eventually spawn isolated agent pods.

**Current scope: auth bootstrap.** `kainban auth` is an interactive TUI
(Bubble Tea) that walks you through each CLI's login inside the orchestrator
pod and stores the resulting credentials as Kubernetes Secrets in the
orchestrator's namespace. Agent pod scheduling is not implemented yet.

## Quick start

Requirements: a Kubernetes cluster (amd64 and/or arm64 nodes — the image is
multi-arch), `kubectl`, Helm 3.

```bash
git clone https://github.com/zerosuxx/kAinban && cd kAinban
helm install kainban charts/kainban --namespace kainban --create-namespace
kubectl -n kainban rollout status deploy/kainban
kubectl -n kainban exec -it deploy/kainban -- kainban auth
```

The chart's image tag defaults to the chart `appVersion`; to run the latest
build from `main` instead, add `--set image.tag=latest`.

### Walking through `kainban auth`

Each provider is shown with its current status. Press `a` to walk through all
of them in order, or pick one; `ctrl+s` skips the current step. Providers:

| Provider | How you log in |
| --- | --- |
| Claude Code | `claude setup-token` runs in the pod; open the printed URL, approve, paste the code back. kAinban captures the printed token and pre-fills it. |
| Codex | Browser login with a manually pasted callback URL (see below). |
| GitHub | Paste a fine-grained personal access token, or leave it empty to run `gh auth login` (device flow: one-time code, no callback) and use its token. |
| GitHub Copilot CLI | Pre-filled with the GitHub CLI OAuth token (`gho_`, from `gh auth token` or the `kainban-github` Secret) when available; otherwise paste a fine-grained token with *Copilot Requests*. |
| Antigravity / Gemini | Paste a Gemini API key. |

**Codex callback URL, step by step.** Codex's browser login starts a callback
listener on `localhost:1455` — but *inside the pod*, not on your machine.

1. kainban starts the Codex login and shows an `https://auth.openai.com/...` URL.
2. Open it in your local browser and sign in.
3. The browser is redirected to `http://localhost:1455/auth/callback?code=...&state=...`.
   The page fails to load (nothing listens on your machine) — that's expected.
4. Copy the **full URL** from the address bar and paste it into the TUI.
5. kainban delivers it to the listener inside the pod, Codex finishes the
   login, and the resulting `auth.json` is stored.

No port-forward is needed. Device-code login is not used because it is
disabled by admin policy in some ChatGPT workspaces (including ours).

### Re-running and checking

Re-running `kainban auth` checks which Secrets already exist and whether they
are still valid, and only asks for what is missing or broken. For scripts:

```bash
kubectl -n kainban exec deploy/kainban -- kainban auth check            # offline checks
kubectl -n kainban exec deploy/kainban -- kainban auth check --live     # also call provider APIs
kubectl -n kainban exec deploy/kainban -- kainban auth check --json --require-all
```

`kainban auth check` exits 1 if any stored credential is invalid
(`--require-all`: also if any is missing).

## Secrets

All Secrets live in the release namespace.

| Secret | Key | Consumer |
| --- | --- | --- |
| `kainban-claude` | `CLAUDE_CODE_OAUTH_TOKEN` | Claude Code agents |
| `kainban-codex` | `auth.json` (refresh token stripped) | Codex agents |
| `kainban-codex-refresh` | `auth.json` (full, with refresh token) | **orchestrator only** |
| `kainban-github` | `GH_TOKEN` | `gh` / git in agents |
| `kainban-copilot` | `COPILOT_GITHUB_TOKEN` | GitHub Copilot CLI agents |
| `kainban-antigravity` | `GEMINI_API_KEY` | Antigravity / Gemini agents |

A ticket can name a model (edit popup, *Model*; empty = the agent's default):
it is passed as `--model` (`-m` for codex) to the headless run and to `t`, and
recorded on each run. The Gemini API free tier has no quota for the Pro
models, so without a ticket model agents run `agy` with
`--model gemini-3.8-flash-medium` (chart value `agents.antigravityModel`;
set it to `""` for agy's default model with a billed key).

### Board

`kainban board` (alias `kanban`) is the kanban board: Backlog → In Progress
(WIP limit 3) → Review → Done, plus a Blocked side column for failed agents
(moving skips it; moving out of it, or `s` to retry, goes back to In Progress), tickets as cards with priority, agent and
branch. `h/l` `j/k` navigate, `space`/`L` and `H` move a card, `n` new and
`e` edit in a popup with every field (title, description, priority, agent,
labels, branch; `tab` between fields, `ctrl+s` saves, `esc` cancels), `a` cycle the agent, `p` priority, `d` delete, `enter` details,
`?` help. The board is saved on every change to
`~/.local/state/kainban/board.json` (on the PVC when persistence is enabled;
`--file` overrides).

Tickets have keys like `KAI-12`: the board's project prefix (default `KAI`,
`kainban board --project XY` for a new board) and a number that is never
reused. The key is on the card, in the details, in the agent's prompt
(`Task KAI-12: …`), in `KAINBAN_TICKET_KEY` and in the pod label
`kainban.io/ticket-key`.

`a` cycles the agent: `auto` (the orchestrator picks when the agent starts;
for now the first of claude, codex, copilot, antigravity with credentials,
later agent profiles; the card then shows e.g. `auto→claude`), or a fixed one.
Confirmations (stop, delete, Done with a running agent, restart, orphans)
are popups; messages pop up as toasts in the top right corner and disappear
after a few seconds (errors a bit later), the key bar always stays at the
bottom, and on narrow screens only the columns that fit are shown.

`s` starts the ticket's agent in its own pod (a ticket created with an agent
starts right away), from the
orchestrator's image, and moves the ticket to In Progress. The pod runs the CLI
headless with the ticket title and description as the prompt (`claude -p`,
`codex exec`, `copilot -p`, `agy -p`; their own sandboxes are off, the pod is
the sandbox) and gets only that agent's credentials plus `GH_TOKEN`. The board
polls the pods every 5 s for the agent state and moves a ticket to Review when
its agent finishes successfully, to Blocked when it fails; the tail of the
agent's output (up to 64 KB) is then saved with the ticket and shown, scrollable,
in the details (`enter`), even after the pod is gone. `o` shows the agent's output, following new
lines while you are at the end; scroll it with `j/k`, `pgup/pgdn`, `g/G`, the
mouse wheel or touch swipes (Termux). `x` stops the agent after a y/N confirmation (deletes its pod with its
`/work` and session; the output is kept), deleting a ticket stops all its
agent pods.

Every run is kept on the ticket (agent, pod, status, times, saved output);
the card shows the latest and the details (`enter`, a popup scrollable with
the keys, the mouse wheel or touch) list them all. Starting a new run
(`s`, also after switching the agent with `a`) stops the previous run's pod
first, asking if it is still running. Moving a ticket to Done stops its agent
pods (asking if one is still running). Agent pods that belong to no run (e.g.
left by an older version) are reported, and `C` stops them.

`t` opens the agent's session in its pod to ask for changes: the pod keeps a
`shell` container (sharing `/work` and the CLIs' session dirs with the headless
`agent` container) and kAinban runs `kainban attach` (client-go exec, like
`kubectl exec -it … -c shell`; the image has no kubectl) with
`claude --continue`, `codex resume --last`, `copilot --continue` or
`agy --continue`, in a tmux session in the pod: `ctrl+z` detaches back to the
board and leaves the CLI running, the next `t` re-attaches to it (a dropped
connection loses nothing either), and leaving the CLI ends the session and
returns to the board. `T` opens a plain shell there, in its own tmux session. Agent pods stay until stopped with `x`.

```shell
kubectl -n kainban exec -it deploy/kainban -- kainban board
```

### Using the credentials in the orchestrator pod

`kainban shell` starts a shell and `kainban run -- CMD` runs a command with the
stored credentials, read from the Secrets at start (so no restart is needed
after `kainban auth`): the same set an agent gets, i.e. Codex uses the
refresh-token-less `auth.json` in a writable `CODEX_HOME`, so nothing in the
pod can rotate the orchestrator's refresh token.

```shell
kubectl -n kainban exec -it deploy/kainban -- kainban shell
kubectl -n kainban exec -it deploy/kainban -- kainban run -- claude
```

## Security notes

- **Least privilege per agent.** An agent pod must mount only the Secret of its
  own runtime (plus `kainban-github` if it needs repo access) — never all of them.
- **`kainban-codex-refresh` is orchestrator-only.** OpenAI rotates the refresh
  token on every use, so if several pods shared it, the first refresh would
  invalidate everyone else's copy (and a `codex logout` in any pod would revoke
  it). Agents get `kainban-codex`, whose `refresh_token` is an empty string:
  Codex works until the access token expires (~10 days after the last refresh)
  and can never rotate the shared token. The orchestrator is meant to refresh
  centrally and re-seed `kainban-codex`.
- **`ANTHROPIC_API_KEY` wins.** If it is set in a Claude pod, Claude Code uses
  it instead of `CLAUDE_CODE_OAUTH_TOKEN`. Don't set both.
- **RBAC.** The chart grants the orchestrator `get/list/watch/create/update/patch`
  on Secrets in its own namespace only — no `delete`, no cluster-wide access.
- The pod runs as uid 1000, non-root, no privilege escalation, all capabilities
  dropped, `RuntimeDefault` seccomp.

## Helm values

See [`charts/kainban/values.yaml`](charts/kainban/values.yaml). Commonly used:
`image.tag`, `resources`, `nodeSelector` / `tolerations` / `affinity`,
`extraEnv`, `serviceAccount.create` / `serviceAccount.name`, `rbac.create`,
`persistence.enabled` (PVC at `/home/ubuntu/.local/state/kainban`, off by default).

## Development

```bash
go test ./...
go vet ./...
go build -o kainban ./cmd/kainban
helm lint charts/kainban
helm template kainban charts/kainban
nix build .#kainban                     # the binary (+ `k` alias)
nix build .#image && ./result | docker load   # the image
```

The image is built from [`flake.nix`](flake.nix) with
`dockerTools.streamLayeredImage` (no Dockerfile): one layer per package, so an
update only changes the layers of the packages that changed. It contains
`kainban` (alias `k`), the agent CLIs (`claude`, `codex`, `copilot`, `agy`)
from nixpkgs master, `gh`, `git`, `tmux`, the usual shell tools, terminfo, and
the current Node.js LTS and Python 3, for user `ubuntu` (uid 1000). The
board's `t`/`T` use `kainban attach` (client-go exec), so there is no kubectl.
After changing `go.mod`, update `vendorHash` in `flake.nix` (build once and
copy the `got:` hash).

## CI and image

[`.github/workflows/ci.yml`](.github/workflows/ci.yml) runs `gofmt`, `go vet`,
`go test -race`, `helm lint` and `helm template` on every push and pull request.
On `main`, `v*` tags and manual dispatch it builds `ghcr.io/zerosuxx/kainban`
with Nix (`nix build .#image`, cached by magic-nix-cache) natively for
`linux/amd64` and `linux/arm64` and merges them into a multi-arch
manifest tagged `latest` (main), the branch name, `sha-<short>`, and
`X.Y.Z` / `X.Y` for version tags.
