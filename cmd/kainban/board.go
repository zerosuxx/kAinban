package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/zerosuxx/kainban/internal/agent"
	"github.com/zerosuxx/kainban/internal/board"
	"github.com/zerosuxx/kainban/internal/boardui"
)

// runBoard shows the kanban board stored in path (default: board.json in the
// state directory, i.e. on the orchestrator's PVC). Agents can be started
// when the cluster and the agent image (set by the Helm chart) are known.
func runBoard(ctx context.Context, cf commonFlags, path string, stderr io.Writer) int {
	if path == "" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(stateDir(home), "board.json")
	}
	opts := boardui.Options{AppVersion: appVersion, Location: path}
	if t, err := cf.connect(); err != nil {
		opts.AgentsErr = err.Error()
	} else if cfg, err := agent.FromEnv(t.Namespace); err != nil {
		opts.AgentsErr = err.Error()
	} else {
		if cf.kubeconfig != "" {
			cfg.KubectlArgs = append(cfg.KubectlArgs, "--kubeconfig", cf.kubeconfig)
		}
		if cf.kubeContext != "" {
			cfg.KubectlArgs = append(cfg.KubectlArgs, "--context", cf.kubeContext)
		}
		opts.Agents = agent.NewRunner(t.Client, cfg)
	}
	err := boardui.Run(ctx, board.FileStore{Path: path}, opts)
	if errors.Is(err, context.Canceled) {
		return 130
	}
	if err != nil {
		fmt.Fprintf(stderr, "kainban: %v\n", err)
		return 1
	}
	return 0
}
