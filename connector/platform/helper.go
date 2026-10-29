package platform

import (
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
)

// Helper processes are copies of the connector executable started with helperEnv set:
//
//   - guard: waits for its parent to exit, then runs Recover. If the connector is killed or
//     crashes mid-session, the guard puts the machine's screens back immediately instead of
//     leaving them off until the next connector start.
//   - displayctl: performs one display configuration transaction and exits (macOS; see
//     runDisplayCtl).
const (
	helperEnv        = "DOREMOTE_HELPER"
	helperGuard      = "guard"
	helperDisplayCtl = "displayctl"
	displayCtlEnv    = "DOREMOTE_DISPLAYCTL"
)

var (
	guardOnce sync.Once
	guardPipe io.WriteCloser // held open for the parent's lifetime; EOF signals its exit
)

// RunHelperIfRequested must be called first thing in main. In a helper process it does the
// helper's job and exits; otherwise it returns immediately.
func RunHelperIfRequested() {
	switch os.Getenv(helperEnv) {
	case helperGuard:
		// Terminal signals reach the whole process group; the parent handles them and restores
		// displays itself, and the guard must outlive it to cover crashes.
		signal.Ignore(os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
		io.Copy(io.Discard, os.Stdin)
		if err := Recover(); err != nil {
			log.Printf("display guard: %v", err)
			os.Exit(1)
		}
		os.Exit(0)
	case helperDisplayCtl:
		os.Exit(displayCtlMain())
	}
}

// ensureGuard starts the guard once per connector process, before any display is isolated.
func ensureGuard() {
	guardOnce.Do(func() {
		exe, err := os.Executable()
		if err != nil {
			log.Printf("display guard unavailable: %v", err)
			return
		}
		cmd := exec.Command(exe)
		cmd.Env = append(os.Environ(), helperEnv+"="+helperGuard)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		w, err := cmd.StdinPipe()
		if err != nil {
			log.Printf("display guard unavailable: %v", err)
			return
		}
		if err := cmd.Start(); err != nil {
			log.Printf("display guard unavailable: %v", err)
			return
		}
		guardPipe = w
		go cmd.Wait()
	})
}
