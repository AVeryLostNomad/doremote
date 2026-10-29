package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// updateRequirement is what the downloaded app's code signature must satisfy: Apple-issued
// Developer ID certificate of doremote's team and doremote's bundle identifier. Keeping the
// same designated requirement across versions is also what keeps Screen Recording and
// Accessibility grants across updates.
const updateRequirement = `anchor apple generic and identifier "dev.doremote.connector" and certificate leaf[subject.OU] = "8VSK2S4PQ7"`

func updateAssetKey() string { return "darwin" }

type darwinUpdate struct {
	dir    string // .doremote-update-* next to the installed bundle (same volume, so renames work)
	app    string // verified new bundle inside dir
	bundle string // installed bundle
}

// appBundle is the installed doremote.app this process runs from.
func appBundle() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return "", err
	}
	macos := filepath.Dir(exe)
	bundle := filepath.Dir(filepath.Dir(macos))
	if filepath.Base(macos) != "MacOS" || !strings.HasSuffix(bundle, ".app") {
		return "", errors.New("not running from an app bundle")
	}
	if strings.Contains(bundle, "/AppTranslocation/") {
		return "", errors.New("macOS is running doremote from a read-only copy; move doremote.app to Applications to get updates")
	}
	return bundle, nil
}

func stageUpdate(ctx context.Context, url string, sha []byte) (stagedUpdate, error) {
	bundle, err := appBundle()
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(filepath.Dir(bundle), ".doremote-update-")
	if err != nil {
		return nil, err
	}
	u := &darwinUpdate{dir: dir, app: filepath.Join(dir, "new", "doremote.app"), bundle: bundle}
	if err := u.stage(ctx, url, sha); err != nil {
		u.discard()
		return nil, err
	}
	return u, nil
}

func (u *darwinUpdate) stage(ctx context.Context, url string, sha []byte) error {
	zip := filepath.Join(u.dir, "update.zip")
	if err := downloadVerified(ctx, url, zip, sha); err != nil {
		return err
	}
	if out, err := exec.CommandContext(ctx, "ditto", "-x", "-k", zip, filepath.Join(u.dir, "new")).CombinedOutput(); err != nil {
		return fmt.Errorf("unzipping: %v: %s", err, out)
	}
	if out, err := exec.CommandContext(ctx, "codesign", "--verify", "--deep", "--strict",
		"-R="+updateRequirement, u.app).CombinedOutput(); err != nil {
		return fmt.Errorf("code signature of the download: %v: %s", err, out)
	}
	return nil
}

func (u *darwinUpdate) install(args []string) error {
	old := filepath.Join(u.dir, "old.app")
	if err := os.Rename(u.bundle, old); err != nil {
		return err
	}
	rollback := func(err error) error {
		os.RemoveAll(u.bundle)
		if rerr := os.Rename(old, u.bundle); rerr != nil {
			return fmt.Errorf("%w; restoring the previous version also failed: %v", err, rerr)
		}
		return err
	}
	if err := os.Rename(u.app, u.bundle); err != nil {
		return rollback(err)
	}
	// -n: this process is still running and LaunchServices would otherwise just activate it.
	launch := append([]string{"-n", u.bundle, "--args"}, args...)
	if out, err := exec.Command("open", launch...).CombinedOutput(); err != nil {
		return rollback(fmt.Errorf("launching: %v: %s", err, out))
	}
	return nil
}

func (u *darwinUpdate) discard() { os.RemoveAll(u.dir) }

// cleanupUpdate removes staging directories (and the previous version) left next to the bundle.
func cleanupUpdate() {
	bundle, err := appBundle()
	if err != nil {
		return
	}
	dirs, _ := filepath.Glob(filepath.Join(filepath.Dir(bundle), ".doremote-update-*"))
	for _, d := range dirs {
		os.RemoveAll(d)
	}
}
