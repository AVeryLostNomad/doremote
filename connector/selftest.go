package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/joey/doremote/connector/platform"
)

// selftest exercises the local half of a session without the portal: virtual display,
// isolation, capture, H.264 encoding and input injection. It writes the encoded frames to out
// as a raw Annex B stream (play it with `ffplay`, or convert with `ffmpeg -i out frame.png`).
func selftest(width, height int, isolate bool, out string) error {
	step := func(name string, start time.Time) {
		fmt.Printf("ok   %-34s %v\n", name, time.Since(start).Round(time.Millisecond))
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

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var stream []byte
	next := func(name string, wantKey bool) error {
		t := time.Now()
		f, err := sess.NextFrame(ctx)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if wantKey && !f.Key {
			return fmt.Errorf("%s: got a delta frame, want a keyframe", name)
		}
		if f.Width != w || f.Height != h {
			return fmt.Errorf("%s: frame is %dx%d, display is %dx%d", name, f.Width, f.Height, w, h)
		}
		if !hasStartCode(f.Data) {
			return fmt.Errorf("%s: frame is not Annex B H.264", name)
		}
		stream = append(stream, f.Data...)
		kind := "delta"
		if f.Key {
			kind = "key"
		}
		step(fmt.Sprintf("%s (%s, %d KiB)", name, kind, len(f.Data)/1024), t)
		return nil
	}

	if err := sess.MouseMove(w/2, h/2); err != nil {
		return fmt.Errorf("mouse move: %w", err)
	}
	if err := next("first frame", true); err != nil {
		return err
	}

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

	// The screen may be still now: a requested keyframe must arrive anyway.
	sess.SetBitrate(2_000_000)
	sess.RequestKeyframe()
	if err := next("requested keyframe", true); err != nil {
		return err
	}
	// Moving the pointer changes the picture (the cursor is part of it).
	if err := sess.MouseMove(w/4, h/4); err != nil {
		return fmt.Errorf("mouse move: %w", err)
	}
	if err := next("frame after pointer move", false); err != nil {
		return err
	}

	if err := os.WriteFile(out, stream, 0o644); err != nil {
		return err
	}
	fmt.Printf("     wrote %s\n", out)

	t = time.Now()
	if err := sess.Close(); err != nil {
		return fmt.Errorf("restore displays: %w", err)
	}
	step("restore displays", t)
	return nil
}

func hasStartCode(b []byte) bool {
	return len(b) > 4 && b[0] == 0 && b[1] == 0 && (b[2] == 1 || (b[2] == 0 && b[3] == 1))
}
