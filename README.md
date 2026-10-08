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

### Board

`kainban board` (alias `kanban`) is the kanban board: Backlog → In Progress
(WIP limit 3) → Review → Done, plus a Blocked side column for failed agents
(moving skips it; moving out of it, or `s` to retry, goes back to In Progress), tickets as cards with priority, agent and
branch. `h/l` `j/k` navigate, `space`/`L` and `H` move a card, `n` new,
`e` edit, `a` cycle the agent, `p` priority, `d` delete, `enter` details,
`?` help. The board is saved on every change to
`~/.local/state/kainban/board.json` (on the PVC when persistence is enabled;
`--file` overrides).

`a` cycles the agent: `auto` (the orchestrator picks when the agent starts;
for now the first of claude, codex, copilot, antigravity with credentials,
later agent profiles; the card then shows e.g. `auto→claude`), or a fixed one.
Questions and messages appear under the header, the key bar always stays at
the bottom, and on narrow screens only the columns that fit are shown.

`s` starts the ticket's agent in its own pod, from the
orchestrator's image, and moves the ticket to In Progress. The pod runs the CLI
headless with the ticket title and description as the prompt (`claude -p`,
`codex exec`, `copilot -p`, `agy -p`; their own sandboxes are off, the pod is
the sandbox) and gets only that agent's credentials plus `GH_TOKEN`. The board
polls the pods every 5 s for the agent state and moves a ticket to Review when
its agent finishes successfully, to Blocked when it fails. `o` shows the agent's output, following new
lines while you are at the end; scroll it with `j/k`, `pgup/pgdn`, `g/G`, the
mouse wheel or touch swipes (Termux). `x` stops the agent after a y/N confirmation (deletes its pod with its
`/work` and session), deleting a ticket stops its agent.

`t` opens the agent's session in its pod to ask for changes: the pod keeps a
`shell` container (sharing `/work` and the CLIs' session dirs with the headless
`agent` container) and kAinban runs `kubectl exec -it … -c shell` with
`claude --continue`, `codex resume --last`, `copilot --continue` or
`agy --continue`; leaving the CLI returns to the board. `T` opens a plain shell
there. Agent pods stay until stopped with `x`.

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

### Test agent

`--set testAgent.enabled=true` adds a `<release>-kainban-test-agent`
Deployment that consumes these Secrets the way agents will: env vars for
Claude, gh, Copilot and Gemini, the stripped Codex `auth.json` copied into a
writable `CODEX_HOME`, `gh auth setup-git` for HTTPS git, and the Gemini
`modelProvider` setting for `agy`. Every Secret is optional (skipped providers
stay logged out), `testAgent.secrets.<provider>: false` withholds one, and the
pod gets neither `kainban-codex-refresh` nor a ServiceAccount token. Env vars
are read at pod start, so restart it after `kainban auth` changes a Secret:

```shell
kubectl -n kainban rollout restart deploy/kainban-test-agent
kubectl -n kainban exec -it deploy/kainban-test-agent -- bash
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
docker build --build-arg APP_VERSION=dev -t kainban:dev .
```

The runtime image is based on
[`ghcr.io/zerosuxx/nix-config`](https://github.com/zerosuxx/nix-config), which
provides `claude`, `codex`, `gh`, `github-copilot-cli`, `antigravity-cli` and
`bubblewrap` for user `ubuntu` (uid 1000).

## CI and image

[`.github/workflows/ci.yml`](.github/workflows/ci.yml) runs `gofmt`, `go vet`,
`go test -race`, `helm lint` and `helm template` on every push and pull request.
On `main`, `v*` tags and manual dispatch it builds `ghcr.io/zerosuxx/kainban`
natively for `linux/amd64` and `linux/arm64` and merges them into a multi-arch
manifest tagged `latest` (main), the branch name, `sha-<short>`, and
`X.Y.Z` / `X.Y` for version tags.
