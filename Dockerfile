# syntax=docker/dockerfile:1

# --- build ------------------------------------------------------------------
# The builder always runs on the build host's native platform and
# cross-compiles for the target (pure Go, CGO disabled), so multi-arch builds
# never need QEMU for this stage.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build

ARG TARGETOS
ARG TARGETARCH
ARG APP_VERSION=dev

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.appVersion=${APP_VERSION}" \
      -o /out/kainban ./cmd/kainban

# --- runtime ----------------------------------------------------------------
# nix-config ships the agent CLIs (claude, codex, gh, github-copilot-cli,
# antigravity-cli, bubblewrap) in a Nix profile for user `ubuntu` (uid 1000).
FROM ghcr.io/zerosuxx/nix-config:latest

LABEL org.opencontainers.image.source="https://github.com/zerosuxx/kAinban" \
      org.opencontainers.image.description="kAinban orchestrator: agent CLI auth bootstrap for Kubernetes"

COPY --from=build --chmod=0755 /out/kainban /usr/local/bin/kainban

USER ubuntu
WORKDIR /home/ubuntu

# The pod idles; users `kubectl exec -it ... -- kainban auth` into it.
CMD ["sleep", "infinity"]
