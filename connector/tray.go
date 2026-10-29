package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"fyne.io/systray"
	"github.com/joey/doremote/connector/platform"
)

// runTray shows the menu-bar (macOS) / notification-area (Windows) UI and runs the connector
// until the user quits.
func runTray(a *app, statusUpdates <-chan string, updateFeed string) {
	ctx, cancel := context.WithCancel(context.Background())
	var done sync.WaitGroup

	systray.Run(func() {
		setTrayIcon()
		systray.SetTooltip("doremote")

		mStatus := systray.AddMenuItem("Starting…", "")
		mStatus.Disable()
		mSetup := systray.AddMenuItem("", "")
		mSetup.Hide()
		systray.AddSeparator()
		mOpen := systray.AddMenuItem("Open doremote", "Open the portal in your browser")
		mScreens := systray.AddMenuItemCheckbox("Turn off other screens during sessions",
			"Move all windows onto the remote session's virtual display", !a.keepScreensOn.Load())
		mRelink := systray.AddMenuItem("Sign in again…", "Link this computer to a GitHub account")
		mLog := systray.AddMenuItem("Show log", "")
		mUpdate := systray.AddMenuItem("", "Updates install automatically from GitHub Releases")
		mUpdate.Hide()
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("Quit doremote", "")

		// Missing OS permissions (macOS) or the virtual display driver (Windows) get a menu item
		// that takes the user straight to the fix.
		if err := platform.Preflight(); err != nil {
			log.Printf("preflight: %v", err)
			mSetup.SetTitle(setupTitle)
			mSetup.SetTooltip(err.Error())
			mSetup.Show()
			go func() {
				for range mSetup.ClickedCh {
					if err := fixSetup(); err != nil {
						log.Printf("setup: %v", err)
						continue
					}
					// Windows: the driver installer runs in the background; hide the item once
					// the driver answers. macOS permissions only apply after a restart.
					if runtime.GOOS == "windows" {
						go waitForSetup(mSetup)
					}
				}
			}()
		}

		// Logout/shutdown and `kill` should restore displays like Quit does.
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
		go func() {
			<-sig
			systray.Quit()
		}()

		go func() {
			for s := range statusUpdates {
				mStatus.SetTitle(s)
				systray.SetTooltip("doremote — " + s)
			}
		}()

		// quit restores displays (ending any session) before the menu disappears.
		quit := func() {
			cancel()
			done.Wait()
			systray.Quit()
		}

		go func() {
			for {
				select {
				case <-mOpen.ClickedCh:
					openURL(a.server)
				case <-mScreens.ClickedCh:
					if mScreens.Checked() {
						mScreens.Uncheck()
					} else {
						mScreens.Check()
					}
					a.setKeepScreensOn(!mScreens.Checked())
				case <-mRelink.ClickedCh:
					a.unlink()
				case <-mLog.ClickedCh:
					if p, err := logPath(); err == nil {
						openURL(p)
					}
				case <-mQuit.ClickedCh:
					mStatus.SetTitle("Quitting…")
					quit()
					return
				}
			}
		}()

		if updatesSupported() {
			check := make(chan struct{}, 1)
			u := &updater{feed: updateFeed, current: version, busy: a.inSession, check: check,
				status: mUpdate.SetTitle,
				restart: func() {
					mStatus.SetTitle("Restarting to update…")
					quit()
				}}
			mUpdate.Show()
			go func() {
				for range mUpdate.ClickedCh {
					select {
					case check <- struct{}{}:
					default:
					}
				}
			}()
			go u.run(ctx)
		}

		done.Add(1)
		go func() {
			defer done.Done()
			if err := a.run(ctx); err != nil && ctx.Err() == nil {
				log.Printf("connector stopped: %v", err)
				a.status("Stopped: " + err.Error())
			}
		}()
	}, func() {
		cancel()
		done.Wait()
	})
}

// waitForSetup hides the setup item once Preflight passes (polls for up to two minutes).
func waitForSetup(item *systray.MenuItem) {
	for range 60 {
		time.Sleep(2 * time.Second)
		if platform.Preflight() == nil {
			item.Hide()
			log.Print("setup complete")
			return
		}
	}
}

func logPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "doremote", "connector.log"), nil
}

// openLog sends log output to the log file, keeping it from growing without bound.
func openLog() (*os.File, error) {
	p, err := logPath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, err
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if st, err := os.Stat(p); err == nil && st.Size() > 5<<20 {
		flags |= os.O_TRUNC
	}
	return os.OpenFile(p, flags, 0o600)
}
