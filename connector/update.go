package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

// Auto-update from GitHub Releases. Every release carries update.json (version and SHA-256 of
// each download) and update.json.sig (base64 Ed25519 signature over the exact bytes of
// update.json). The public key is compiled in; the private key exists only as a GitHub Actions
// secret of the release environment, so a tampered release asset, a compromised GitHub token
// without access to that secret, or anything on the network path cannot push code. Versions only
// move forward, so replaying an older signed manifest does nothing.
const (
	defaultUpdateFeed = "https://github.com/AVeryLostNomad/doremote/releases"
	updatePublicKey   = "30EW6ZscY0PRhznOCwFyABHDPB8vSORcWySZBlMdtL0="
	updateInterval    = time.Hour
	maxManifestSize   = 64 << 10
	maxAssetSize      = 256 << 20
)

type updateManifest struct {
	Version string                 `json:"version"`
	Assets  map[string]updateAsset `json:"assets"`
}

type updateAsset struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
}

// stagedUpdate is a downloaded, verified release sitting next to the installed copy.
type stagedUpdate interface {
	// install swaps the new version in and launches it with args; on error the installed copy
	// is left as it was.
	install(args []string) error
	discard()
}

type updater struct {
	feed    string
	current string
	busy    func() bool   // a remote session is running; installing would cut it off
	restart func()        // exits this process once the new version has been launched
	status  func(string)  // menu item text
	check   chan struct{} // "Check for updates" clicks
}

var updateHTTP = &http.Client{Timeout: 10 * time.Minute}

// updatesSupported reports whether this build can update itself: a released version on a
// platform with a release asset.
func updatesSupported() bool {
	_, err := parseVersion(version)
	return err == nil && updateAssetKey() != ""
}

func (u *updater) run(ctx context.Context) {
	u.status(fmt.Sprintf("Check for updates (v%s)", u.current))
	wait := time.Minute
	for first := true; ; first = false {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		case <-u.check:
		}
		wait = updateInterval
		if first {
			// By now the previous version has exited, so its leftovers can go.
			cleanupUpdate()
		}
		if err := u.once(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("update: %v", err)
			u.status("Update failed — check again")
		}
	}
}

func (u *updater) once(ctx context.Context) error {
	u.status("Checking for updates…")
	m, err := fetchManifest(ctx, u.feed)
	if err != nil {
		return err
	}
	newer, err := versionNewer(m.Version, u.current)
	if err != nil {
		return err
	}
	if !newer {
		u.status(fmt.Sprintf("Up to date (v%s)", u.current))
		return nil
	}
	asset, ok := m.Assets[updateAssetKey()]
	if !ok {
		return fmt.Errorf("v%s has no %s download", m.Version, updateAssetKey())
	}
	if asset.File == "" || path.Base(asset.File) != asset.File || strings.HasPrefix(asset.File, ".") {
		return fmt.Errorf("v%s: bad file name %q", m.Version, asset.File)
	}
	want, err := hex.DecodeString(asset.SHA256)
	if err != nil || len(want) != sha256.Size {
		return fmt.Errorf("v%s: bad sha256 for %s", m.Version, asset.File)
	}

	log.Printf("update: downloading v%s", m.Version)
	u.status(fmt.Sprintf("Downloading v%s…", m.Version))
	url := fmt.Sprintf("%s/download/v%s/%s", u.feed, m.Version, asset.File)
	staged, err := stageUpdate(ctx, url, want)
	if err != nil {
		return err
	}
	for u.busy() {
		u.status(fmt.Sprintf("v%s installs after this session", m.Version))
		select {
		case <-ctx.Done():
			staged.discard()
			return ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
	log.Printf("update: installing v%s", m.Version)
	u.status(fmt.Sprintf("Installing v%s…", m.Version))
	if err := staged.install(relaunchArgs()); err != nil {
		staged.discard()
		return fmt.Errorf("installing v%s: %w", m.Version, err)
	}
	u.restart()
	return nil
}

// fetchManifest downloads and authenticates the latest release's manifest.
func fetchManifest(ctx context.Context, feed string) (updateManifest, error) {
	var m updateManifest
	body, err := fetchSmall(ctx, feed+"/latest/download/update.json")
	if err != nil {
		return m, err
	}
	sig, err := fetchSmall(ctx, feed+"/latest/download/update.json.sig")
	if err != nil {
		return m, err
	}
	key, err := base64.StdEncoding.DecodeString(updatePublicKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return m, errors.New("update public key is malformed")
	}
	if err := verifyManifest(key, body, sig); err != nil {
		return m, err
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return m, fmt.Errorf("update.json: %w", err)
	}
	return m, nil
}

func verifyManifest(key ed25519.PublicKey, body, sigText []byte) error {
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigText)))
	if err != nil || !ed25519.Verify(key, body, sig) {
		return errors.New("update.json signature does not verify")
	}
	return nil
}

func fetchSmall(ctx context.Context, url string) ([]byte, error) {
	resp, err := get(ctx, url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxManifestSize {
		return nil, fmt.Errorf("%s is too large", url)
	}
	return b, nil
}

func get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "doremote/"+version)
	resp, err := updateHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return resp, nil
}

// downloadVerified writes url to dst and fails unless its SHA-256 is want.
func downloadVerified(ctx context.Context, url, dst string, want []byte) error {
	resp, err := get(ctx, url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxAssetSize+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > maxAssetSize {
		err = fmt.Errorf("%s is too large", url)
	}
	if err == nil && !bytes.Equal(h.Sum(nil), want) {
		err = fmt.Errorf("%s: SHA-256 mismatch", url)
	}
	if err != nil {
		os.Remove(dst)
	}
	return err
}

func equalBytes(a, b []byte) bool { return string(a) == string(b) }

// relaunchArgs are this process's flags for the new version, marked so it waits for the
// instance lock this process still holds.
func relaunchArgs() []string {
	args := []string{"-updated"}
	for _, a := range os.Args[1:] {
		if a != "-updated" && a != "--updated" {
			args = append(args, a)
		}
	}
	return args
}

func parseVersion(v string) ([3]int, error) {
	var out [3]int
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) != 3 {
		return out, fmt.Errorf("version %q is not MAJOR.MINOR.PATCH", v)
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, fmt.Errorf("version %q is not MAJOR.MINOR.PATCH", v)
		}
		out[i] = n
	}
	return out, nil
}

func versionNewer(candidate, current string) (bool, error) {
	a, err := parseVersion(candidate)
	if err != nil {
		return false, err
	}
	b, err := parseVersion(current)
	if err != nil {
		return false, err
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i], nil
		}
	}
	return false, nil
}
