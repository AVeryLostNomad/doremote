// Package platform creates a virtual display, captures it, encodes it as H.264 and injects
// input on the host OS.
//
// Each OS implements:
//
//	func Preflight() error                  // check/prompt permissions or driver presence
//	func Recover() error                    // restore displays left isolated by a crashed run
//	func Start(opts Options) (Session, error)
package platform

import (
	"context"
	"time"
)

const (
	// MaxFrameRate is the highest rate at which a session produces frames.
	MaxFrameRate = 60
	// RefineDelay is how long the screen must stay unchanged after a frame before NextFrame
	// re-encodes the same picture, so a still screen sharpens after motion blurred it.
	RefineDelay = 150 * time.Millisecond
	// RefineFrames is how many re-encodings of an unchanged picture NextFrame returns before
	// it blocks until the screen changes.
	RefineFrames = 2
)

// VideoFrame is one encoded H.264 access unit in Annex B form (start-code delimited NAL
// units), without B-frames. Keyframes are IDR frames carrying SPS and PPS in-band.
// Data is only valid until the next NextFrame call on the same Session.
type VideoFrame struct {
	Data          []byte
	Key           bool
	Width, Height int
}

// Options configures a remote session's virtual display.
type Options struct {
	Width, Height int
	// Isolate disables every other display while the session lives so new
	// and existing windows land on the virtual display.
	Isolate bool
	// Bitrate is the initial target average bitrate in bits per second.
	Bitrate int
}

// Session owns one virtual display plus its capture, encoder and input injection.
type Session interface {
	Size() (w, h int)
	// NextFrame blocks until the screen changed since the last returned frame (or a
	// refinement is due, see RefineDelay) and returns it encoded, or until ctx is done.
	// The first frame, and the first frame after RequestKeyframe, is a keyframe.
	NextFrame(ctx context.Context) (*VideoFrame, error)
	// RequestKeyframe makes the next NextFrame return a keyframe without waiting for the
	// screen to change (the current picture is re-encoded if it is unchanged). Safe to call
	// concurrently with NextFrame, including while it blocks: it wakes up and returns the
	// keyframe.
	RequestKeyframe()
	// SetBitrate changes the target average bitrate (bits/s) for later frames. Safe to call
	// concurrently with NextFrame.
	SetBitrate(bps int)
	MouseMove(x, y int) error
	// MouseButton uses DOM numbering: 0 left, 1 middle, 2 right.
	MouseButton(button int, down bool) error
	// Scroll takes DOM WheelEvent pixel deltas (positive dy scrolls down).
	Scroll(dx, dy float64) error
	// Key takes a DOM KeyboardEvent.code; unknown codes are ignored.
	Key(code string, down bool) error
	ReleaseAll() error
	// Close restores the original display layout and removes the virtual display. Idempotent.
	Close() error
}
