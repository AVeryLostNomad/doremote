package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
)

// app owns the connector lifecycle: pairing with the portal, keeping the control channel up,
// and the settings the tray menu changes while it runs.
type app struct {
	server string
	name   string
	status func(string)

	keepScreensOn atomic.Bool

	mu       sync.Mutex
	cancel   context.CancelFunc // cancels the current pairing/connection attempt
	unlinked bool
	retry    chan struct{} // wakes a failed pairing when the user asks to sign in again
}

func newApp(server, name string, status func(string)) (*app, error) {
	server = strings.TrimRight(server, "/")
	if name == "" {
		var err error
		if name, err = os.Hostname(); err != nil {
			name = "computer"
		}
		name = strings.TrimSuffix(name, ".local")
	}
	cfg, err := loadConfig()
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	a := &app{server: server, name: name, status: status, retry: make(chan struct{}, 1)}
	a.keepScreensOn.Store(cfg.KeepScreensOn)
	return a, nil
}

// run pairs (if needed) and stays connected until ctx ends.
func (a *app) run(ctx context.Context) error {
	for {
		cfg, err := loadConfig()
		if err != nil {
			return fmt.Errorf("reading config: %w", err)
		}
		attempt, cancel := context.WithCancel(ctx)
		a.mu.Lock()
		a.cancel = cancel
		a.mu.Unlock()

		if cfg.Token == "" || cfg.Server != a.server {
			select {
			case <-a.retry: // drop a stale request; this attempt already is the retry
			default:
			}
			a.status("Waiting for you to sign in from the browser…")
			token, mid, err := pair(attempt, a.server, a.name)
			if err == nil {
				cfg.Server, cfg.Token, cfg.MachineID = a.server, token, mid
				err = saveConfig(cfg)
			}
			cancel()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil && !a.takeUnlinked() {
				log.Printf("linking failed: %v", err)
				a.status("Not linked — choose “Sign in again” to retry")
				select {
				case <-a.retry:
					a.takeUnlinked()
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			continue
		}

		a.status("Connecting…")
		c := &client{server: a.server, token: cfg.Token, name: a.name, isolate: func() bool { return !a.keepScreensOn.Load() }, status: a.status}
		err = c.run(attempt)
		c.shutdown()
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		unlinked := a.takeUnlinked()
		if !unlinked && !errors.Is(err, errRevoked) {
			return err
		}
		if !unlinked {
			log.Printf("%v; linking again", err)
		}
		cfg.Token, cfg.MachineID = "", ""
		if err := saveConfig(cfg); err != nil {
			return err
		}
	}
}

// unlink forgets this machine's account link and starts pairing again.
func (a *app) unlink() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.unlinked = true
	if a.cancel != nil {
		a.cancel()
	}
	select {
	case a.retry <- struct{}{}:
	default:
	}
}

func (a *app) takeUnlinked() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	u := a.unlinked
	a.unlinked = false
	return u
}

func (a *app) setKeepScreensOn(on bool) {
	a.keepScreensOn.Store(on)
	cfg, err := loadConfig()
	if err == nil {
		cfg.KeepScreensOn = on
		err = saveConfig(cfg)
	}
	if err != nil {
		log.Printf("saving setting: %v", err)
	}
}
