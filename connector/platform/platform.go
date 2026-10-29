// Package platform creates a virtual display, captures it and injects input on the host OS.
//
// Each OS implements:
//
//	func Preflight() error                  // check/prompt permissions or driver presence
//	func Recover() error                    // restore displays left isolated by a crashed run
//	func Start(opts Options) (Session, error)
package platform

import "context"

// Frame is one captured image in BGRA byte order, rows top-down.
// Pix is only valid until the next NextFrame call on the same Session.
type Frame struct {
	Width, Height, Stride int
	Pix                   []byte
}

// Options configures a remote session's virtual display.
type Options struct {
	Width, Height int
	// Isolate disables every other display while the session lives so new
	// and existing windows land on the virtual display.
	Isolate bool
}

// Session owns one virtual display plus its capture and input injection.
type Session interface {
	Size() (w, h int)
	// NextFrame blocks until a frame different from the last returned one exists, or ctx is done.
	NextFrame(ctx context.Context) (*Frame, error)
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
