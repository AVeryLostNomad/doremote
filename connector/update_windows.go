package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
)

// Windows updates run the release's per-user installer (packaging/windows/doremote.iss)
// silently. It needs no admin rights, waits for this process to quit, replaces doremote.exe in
// %LOCALAPPDATA%\Programs\doremote and starts it again with -updated (the previous copy, if
// installing failed). Other command-line flags aren't carried over.
func updateAssetKey() string { return "windows-setup" }

const setupPattern = "doremote-setup-*.exe"

type windowsUpdate struct {
	setup string // verified installer in the temp directory
}

func stageUpdate(ctx context.Context, url string, sha []byte) (stagedUpdate, error) {
	f, err := os.CreateTemp("", setupPattern)
	if err != nil {
		return nil, err
	}
	name := f.Name()
	f.Close()
	os.Remove(name) // downloadVerified creates it exclusively
	if err := downloadVerified(ctx, url, name, sha); err != nil {
		return nil, err
	}
	return &windowsUpdate{setup: name}, nil
}

func (u *windowsUpdate) install([]string) error {
	cmd := exec.Command(u.setup, "/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART", "/SP-",
		"/LOG="+filepath.Join(os.TempDir(), "doremote-setup.log"))
	if err := cmd.Start(); err != nil {
		return err
	}
	cmd.Process.Release()
	return nil
}

func (u *windowsUpdate) discard() { os.Remove(u.setup) }

// cleanupUpdate removes installers left by earlier updates, and the previous executable that
// in-place updates of versions before the installer left next to this one.
func cleanupUpdate() {
	leftovers, _ := filepath.Glob(filepath.Join(os.TempDir(), setupPattern))
	for _, p := range leftovers {
		os.Remove(p)
	}
	if exe, err := os.Executable(); err == nil {
		if exe, err = filepath.EvalSymlinks(exe); err == nil {
			os.Remove(exe + ".old")
		}
	}
}
