package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/zerosuxx/kainban/internal/board"
	"github.com/zerosuxx/kainban/internal/boardui"
)

// runBoard shows the kanban board stored in path (default: board.json in the
// state directory, i.e. on the orchestrator's PVC).
func runBoard(ctx context.Context, path string, stderr io.Writer) int {
	if path == "" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(stateDir(home), "board.json")
	}
	err := boardui.Run(ctx, board.FileStore{Path: path}, boardui.Options{AppVersion: appVersion, Location: path})
	if errors.Is(err, context.Canceled) {
		return 130
	}
	if err != nil {
		fmt.Fprintf(stderr, "kainban: %v\n", err)
		return 1
	}
	return 0
}
