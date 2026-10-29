// Command doremote is the connector: it links this computer to a GitHub account on the
// doremote portal and serves remote sessions on a dedicated virtual display.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/joey/doremote/connector/platform"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	platform.RunHelperIfRequested()

	server := flag.String("server", "https://doremote.dev", "portal URL")
	name := flag.String("name", "", "computer name shown on the portal (default: hostname)")
	cli := flag.Bool("cli", false, "run in the terminal without a menu-bar/tray icon")
	logout := flag.Bool("logout", false, "forget this computer's link to your account and exit")
	runSelftest := flag.Bool("selftest", false, "create a virtual display, capture and inject input locally, then exit")
	selftestOut := flag.String("selftest-out", "doremote-selftest.jpg", "where -selftest writes the captured frame")
	noIsolate := flag.Bool("no-isolate", false, "with -selftest: keep other screens on")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	log.SetFlags(log.LstdFlags)

	switch {
	case *showVersion:
		fmt.Println("doremote", version)
		return
	case *logout:
		if err := saveConfig(config{}); err != nil {
			fatal(err)
		}
		return
	}

	// One connector per user: a second copy would fight over the account link, and macOS
	// ScreenCaptureKit stalls when two processes of the same executable capture.
	unlock, err := lockInstance()
	if err != nil {
		fatal(err)
	}
	defer unlock()

	if err := platform.Recover(); err != nil {
		log.Printf("warning: restoring displays from an interrupted session: %v", err)
	}

	if *runSelftest {
		if err := selftest(1280, 720, !*noIsolate, *selftestOut); err != nil {
			fatal(err)
		}
		fmt.Println("selftest passed")
		return
	}

	if *cli {
		a, err := newApp(*server, *name, func(s string) {})
		if err != nil {
			fatal(err)
		}
		if err := platform.Preflight(); err != nil {
			log.Printf("warning: %v", err)
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
		defer stop()
		if err := a.run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			fatal(err)
		}
		return
	}

	if f, err := openLog(); err == nil {
		log.SetOutput(f)
	}
	log.Printf("doremote %s starting", version)
	statusUpdates := make(chan string, 16)
	a, err := newApp(*server, *name, func(s string) {
		select {
		case statusUpdates <- s:
		default: // the menu only needs the latest state
		}
	})
	if err != nil {
		log.Print(err)
		os.Exit(1)
	}
	runTray(a, statusUpdates)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "doremote:", err)
	os.Exit(1)
}
