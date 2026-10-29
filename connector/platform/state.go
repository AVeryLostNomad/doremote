package platform

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// isolationStateFile records which displays a live session switched off. It is written before
// isolating and removed after the layout is restored, so a crashed connector can put the
// machine's screens back on its next start (see Recover).
const isolationStateFile = "isolation-state.json"

func statePath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "doremote", isolationStateFile), nil
}

func writeIsolationState(v any) error {
	p, err := statePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

// readIsolationState returns false when no state was recorded.
func readIsolationState(v any) (bool, error) {
	p, err := statePath()
	if err != nil {
		return false, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(b, v)
}

func clearIsolationState() {
	if p, err := statePath(); err == nil {
		os.Remove(p)
	}
}
