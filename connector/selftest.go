package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/joey/doremote/connector/platform"
)

// selftest exercises the local half of a session without the portal: virtual display,
// isolation, capture, encoding and input injection. It writes the first captured frame to out.
func selftest(width, height int, isolate bool, out string) error {
	step := func(name string, start time.Time) {
		fmt.Printf("ok   %-28s %v\n", name, time.Since(start).Round(time.Millisecond))
	}

	t := time.Now()
	if err := platform.Preflight(); err != nil {
		fmt.Printf("warn preflight: %v\n", err)
	} else {
		step("preflight", t)
	}

	t = time.Now()
	sess, err := platform.Start(platform.Options{Width: width, Height: height, Isolate: isolate})
	if err != nil {
		return fmt.Errorf("start virtual display: %w", err)
	}
	defer sess.Close()
	w, h := sess.Size()
	step(fmt.Sprintf("virtual display %dx%d", w, h), t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	t = time.Now()
	if err := sess.MouseMove(w/2, h/2); err != nil {
		return fmt.Errorf("mouse move: %w", err)
	}
	f, err := sess.NextFrame(ctx)
	if err != nil {
		return fmt.Errorf("capture first frame: %w", err)
	}
	step("capture first frame", t)

	t = time.Now()
	var enc frameEncoder
	msg, err := enc.encode(f)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	step(fmt.Sprintf("encode (%d KiB)", len(msg)/1024), t)
	// A single full-frame rect: the JPEG starts after the 2-byte count and 12-byte rect header.
	if err := os.WriteFile(out, msg[14:], 0o644); err != nil {
		return err
	}
	fmt.Printf("     wrote %s\n", out)

	for _, step := range []struct {
		name string
		fn   func() error
	}{
		{"mouse click", func() error {
			if err := sess.MouseButton(0, true); err != nil {
				return err
			}
			return sess.MouseButton(0, false)
		}},
		{"scroll", func() error { return sess.Scroll(0, 120) }},
		{"key press", func() error {
			if err := sess.Key("ShiftLeft", true); err != nil {
				return err
			}
			return sess.Key("ShiftLeft", false)
		}},
	} {
		if err := step.fn(); err != nil {
			return fmt.Errorf("%s: %w", step.name, err)
		}
	}
	fmt.Println("ok   input injection")

	t = time.Now()
	if err := sess.Close(); err != nil {
		return fmt.Errorf("restore displays: %w", err)
	}
	step("restore displays", t)
	return nil
}
