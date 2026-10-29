package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
)

const (
	setupTitle = "⚠ Install the virtual display driver…"
	// Parsec's Virtual Display Driver (see https://github.com/nomi-san/parsec-vdd).
	vddInstallerURL = "https://builds.parsec.app/vdd/parsec-vdd-0.45.0.0.exe"
)

// fixSetup downloads the virtual display driver installer and runs it silently with
// elevation; Windows shows the UAC prompt.
func fixSetup() error {
	resp, err := http.Get(vddInstallerURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading driver: %s", resp.Status)
	}
	path := filepath.Join(os.TempDir(), fmt.Sprintf("parsec-vdd-setup-%d.exe", time.Now().Unix()))
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	verb, _ := windows.UTF16PtrFromString("runas")
	file, _ := windows.UTF16PtrFromString(path)
	args, _ := windows.UTF16PtrFromString("/S")
	return windows.ShellExecute(0, verb, file, args, nil, windows.SW_HIDE)
}
