package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

func updateAssetKey() string { return "windows-" + runtime.GOARCH }

type windowsUpdate struct {
	exe string // installed executable
	new string // verified download next to it (same volume, so renames work)
}

func installedExe() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

func stageUpdate(ctx context.Context, url string, sha []byte) (stagedUpdate, error) {
	exe, err := installedExe()
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(filepath.Dir(exe), ".doremote-update-*.exe")
	if err != nil {
		return nil, err
	}
	name := f.Name()
	f.Close()
	os.Remove(name) // downloadVerified creates it exclusively
	if err := downloadVerified(ctx, url, name, sha); err != nil {
		return nil, err
	}
	return &windowsUpdate{exe: exe, new: name}, nil
}

// install relies on Windows allowing a running executable to be renamed (not deleted).
func (u *windowsUpdate) install(args []string) error {
	old := u.exe + ".old"
	os.Remove(old)
	if err := os.Rename(u.exe, old); err != nil {
		return err
	}
	rollback := func(err error) error {
		os.Remove(u.exe)
		if rerr := os.Rename(old, u.exe); rerr != nil {
			return fmt.Errorf("%w; restoring the previous version also failed: %v", err, rerr)
		}
		return err
	}
	if err := os.Rename(u.new, u.exe); err != nil {
		return rollback(err)
	}
	cmd := exec.Command(u.exe, args...)
	if err := cmd.Start(); err != nil {
		return rollback(fmt.Errorf("launching: %w", err))
	}
	cmd.Process.Release()
	return nil
}

func (u *windowsUpdate) discard() { os.Remove(u.new) }

// cleanupUpdate removes the previous version and unfinished downloads next to the executable.
func cleanupUpdate() {
	exe, err := installedExe()
	if err != nil {
		return
	}
	os.Remove(exe + ".old")
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(exe), ".doremote-update-*.exe"))
	for _, p := range leftovers {
		os.Remove(p)
	}
}
