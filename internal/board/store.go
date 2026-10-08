package board

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Store persists a Board. The file store is the first implementation; a
// shared database comes later.
type Store interface {
	Load() (*Board, error)
	Save(*Board) error
}

// FileStore keeps the board as JSON in a single file (on the orchestrator's
// PVC by default).
type FileStore struct{ Path string }

// Load returns the stored board, or a new one if the file does not exist.
func (s FileStore) Load() (*Board, error) {
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return New("kAinban"), nil
	}
	if err != nil {
		return nil, err
	}
	b := &Board{}
	if err := json.Unmarshal(data, b); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.Path, err)
	}
	b.EnsureDefaultColumns()
	return b, nil
}

// Save writes the board atomically (temp file + rename).
func (s FileStore) Save(b *Board) error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.Path), ".board-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.Path)
}
