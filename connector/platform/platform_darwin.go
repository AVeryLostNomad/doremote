package platform

/*
#cgo CFLAGS: -fobjc-arc
#cgo LDFLAGS: -framework Foundation -framework CoreGraphics -framework ApplicationServices -framework ScreenCaptureKit -framework CoreMedia -framework CoreVideo
#include <stdlib.h>
#include "darwin.h"
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"
)

// Preflight checks Screen Recording and Accessibility permissions, prompting for missing ones.
func Preflight() error {
	perms := int(C.dr_preflight(1))
	var missing []string
	if perms&C.DR_PERM_SCREEN == 0 {
		missing = append(missing, "Screen Recording")
	}
	if perms&C.DR_PERM_INPUT == 0 {
		missing = append(missing, "Accessibility")
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("grant %v permission to this app (or the terminal running it) in System Settings → Privacy & Security, then restart it", missing)
}

type darwinSession struct {
	// mu guards ptr: capture and input hold it shared (they touch disjoint C state), Close exclusively.
	mu       sync.RWMutex
	ptr      unsafe.Pointer
	isolated []uint32 // displays switched off for this session
	w, h     int
	buf      []byte
	frame    Frame
	wheelX   float64
	wheelY   float64
	held     map[string]bool
	btns     map[int]bool
}

// Start creates a CGVirtualDisplay, optionally switches other displays off, and starts capture.
func Start(opts Options) (Session, error) {
	if opts.Width <= 0 || opts.Height <= 0 {
		return nil, errors.New("invalid resolution")
	}
	errBuf := (*C.char)(C.malloc(512))
	defer C.free(unsafe.Pointer(errBuf))
	p := C.dr_start(C.int(opts.Width), C.int(opts.Height), errBuf, 512)
	if p == nil {
		return nil, errors.New(C.GoString(errBuf))
	}
	var w, h C.int
	C.dr_size(p, &w, &h)
	s := &darwinSession{ptr: p, w: int(w), h: int(h), held: map[string]bool{}, btns: map[int]bool{}}

	if opts.Isolate {
		if err := s.isolate(uint32(C.dr_display_id(p))); err != nil {
			s.Close()
			return nil, err
		}
	}
	if C.dr_start_capture(p, errBuf, 512) == 0 {
		err := errors.New(C.GoString(errBuf))
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *darwinSession) isolate(virtual uint32) error {
	var others []uint32
	for _, id := range onlineDisplays() {
		if id != virtual {
			others = append(others, id)
		}
	}
	if len(others) == 0 {
		return nil
	}
	ensureGuard()
	if err := writeIsolationState(others); err != nil {
		return fmt.Errorf("recording display layout: %w", err)
	}
	if err := runDisplayCtl("isolate", virtual, others); err != nil {
		clearIsolationState()
		return err
	}
	s.isolated = others
	// Let WindowServer migrate windows onto the virtual display before capture starts.
	time.Sleep(300 * time.Millisecond)
	return nil
}

func onlineDisplays() []uint32 {
	ids := make([]uint32, C.DR_MAX_DISPLAYS)
	n := C.dr_list_displays((*C.uint32_t)(unsafe.Pointer(&ids[0])), C.int(len(ids)))
	return ids[:n]
}

// Recover re-enables displays left switched off by a connector that died mid-session.
func Recover() error {
	var ids []uint32
	found, err := readIsolationState(&ids)
	if err != nil || !found {
		return err
	}
	if err := runDisplayCtl("restore", 0, ids); err != nil {
		return err
	}
	clearIsolationState()
	return nil
}

// runDisplayCtl performs a display configuration transaction in a helper copy of this
// executable. Doing it in-process freezes CoreGraphics' view of the display list for the rest
// of the process's life, so later virtual displays would never appear to come online.
func runDisplayCtl(op string, target uint32, ids []uint32) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{op, strconv.FormatUint(uint64(target), 10)}
	for _, id := range ids {
		args = append(args, strconv.FormatUint(uint64(id), 10))
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), helperEnv+"="+helperDisplayCtl, displayCtlEnv+"="+strings.Join(args, " "))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("display %s failed: %v: %s", op, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// displayCtlMain is the helper side of runDisplayCtl.
func displayCtlMain() int {
	f := strings.Fields(os.Getenv(displayCtlEnv))
	if len(f) < 2 {
		fmt.Fprintln(os.Stderr, "bad display control request")
		return 2
	}
	target, _ := strconv.ParseUint(f[1], 10, 32)
	ids := make([]C.uint32_t, 0, len(f)-2)
	for _, s := range f[2:] {
		id, err := strconv.ParseUint(s, 10, 32)
		if err != nil {
			fmt.Fprintln(os.Stderr, "bad display id", s)
			return 2
		}
		ids = append(ids, C.uint32_t(id))
	}
	if len(ids) == 0 {
		return 0
	}
	switch f[0] {
	case "isolate":
		if rc := C.dr_display_isolate(C.uint32_t(target), &ids[0], C.int(len(ids))); rc < 0 {
			fmt.Fprintf(os.Stderr, "could not disable or mirror other displays (CGError %d)\n", -rc)
			return 1
		}
	case "restore":
		// Undo both possible isolation modes; each is a no-op for displays it doesn't apply to.
		e1 := C.dr_display_restore(C.DR_ISOLATION_DISABLED, &ids[0], C.int(len(ids)))
		e2 := C.dr_display_restore(C.DR_ISOLATION_MIRRORED, &ids[0], C.int(len(ids)))
		if e1 != 0 && e2 != 0 {
			fmt.Fprintf(os.Stderr, "re-enabling displays failed (CGError %d/%d)\n", e1, e2)
			return 1
		}
	default:
		fmt.Fprintln(os.Stderr, "unknown display control op", f[0])
		return 2
	}
	return 0
}

func (s *darwinSession) Size() (int, int) { return s.w, s.h }

var errClosed = errors.New("session closed")

func (s *darwinSession) NextFrame(ctx context.Context) (*Frame, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		s.mu.RLock()
		if s.ptr == nil {
			s.mu.RUnlock()
			return nil, errClosed
		}
		if s.buf == nil {
			s.buf = make([]byte, s.w*s.h*4)
		}
		var w, h C.int
		rc := C.dr_next_frame(s.ptr, 100, (*C.uint8_t)(unsafe.Pointer(&s.buf[0])), C.int(len(s.buf)), &w, &h)
		s.mu.RUnlock()
		switch rc {
		case 1:
			s.frame = Frame{Width: int(w), Height: int(h), Stride: int(w) * 4, Pix: s.buf[:int(w)*int(h)*4]}
			return &s.frame, nil
		case 0:
			continue
		case -2:
			s.buf = make([]byte, int(w)*int(h)*4)
			continue
		default:
			return nil, errors.New("screen capture stopped")
		}
	}
}

func (s *darwinSession) with(fn func(p unsafe.Pointer)) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.ptr == nil {
		return errClosed
	}
	fn(s.ptr)
	return nil
}

func (s *darwinSession) MouseMove(x, y int) error {
	x = clamp(x, 0, s.w-1)
	y = clamp(y, 0, s.h-1)
	return s.with(func(p unsafe.Pointer) { C.dr_mouse_move(p, C.int(x), C.int(y)) })
}

func (s *darwinSession) MouseButton(button int, down bool) error {
	if button < 0 || button > 2 {
		return nil
	}
	if down {
		s.btns[button] = true
	} else {
		delete(s.btns, button)
	}
	return s.with(func(p unsafe.Pointer) { C.dr_mouse_button(p, C.int(button), cbool(down)) })
}

func (s *darwinSession) Scroll(dx, dy float64) error {
	// Accumulate sub-pixel remainders so trackpad deltas aren't lost.
	s.wheelX += dx
	s.wheelY += dy
	ix, iy := math.Trunc(s.wheelX), math.Trunc(s.wheelY)
	s.wheelX -= ix
	s.wheelY -= iy
	if ix == 0 && iy == 0 {
		return nil
	}
	return s.with(func(p unsafe.Pointer) { C.dr_scroll(p, C.int(ix), C.int(iy)) })
}

func (s *darwinSession) Key(code string, down bool) error {
	kc, ok := macKeycodes[code]
	if !ok {
		return nil
	}
	if down {
		s.held[code] = true
	} else {
		delete(s.held, code)
	}
	return s.with(func(p unsafe.Pointer) { C.dr_key(p, C.int(kc), cbool(down)) })
}

func (s *darwinSession) ReleaseAll() error {
	for code := range s.held {
		if code == "CapsLock" {
			delete(s.held, code)
			continue
		}
		if err := s.Key(code, false); err != nil {
			return err
		}
	}
	for b := range s.btns {
		if err := s.MouseButton(b, false); err != nil {
			return err
		}
	}
	return nil
}

func (s *darwinSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ptr == nil {
		return nil
	}
	var err error
	if len(s.isolated) > 0 {
		// Bring the original displays back before the virtual one goes away, so WindowServer is
		// never left without a display.
		if err = runDisplayCtl("restore", 0, s.isolated); err == nil {
			waitOnline(s.isolated, 3*time.Second)
			clearIsolationState()
		}
		s.isolated = nil
	}
	C.dr_stop(s.ptr)
	s.ptr = nil
	return err
}

func waitOnline(ids []uint32, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		online := map[uint32]bool{}
		for _, id := range onlineDisplays() {
			online[id] = true
		}
		all := true
		for _, id := range ids {
			all = all && online[id]
		}
		if all {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func cbool(b bool) C.int {
	if b {
		return 1
	}
	return 0
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
