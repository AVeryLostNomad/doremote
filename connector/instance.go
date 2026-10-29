package main

import (
	"errors"
	"os"
	"path/filepath"
)

var errAlreadyRunning = errors.New("doremote is already running on this computer")

// lockInstance holds an exclusive lock on a file in the config directory for the life of the
// process; the OS releases it if the process dies.
func lockInstance() (unlock func(), err error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	p := filepath.Join(dir, "doremote", "connector.lock")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, errAlreadyRunning
	}
	return func() { f.Close() }, nil
}
