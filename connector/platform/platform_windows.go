//go:build windows

package platform

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const (
	frameInterval    = time.Second / 30
	vddAppearTimeout = 6 * time.Second
	vddPollInterval  = 100 * time.Millisecond
)

var errClosed = errors.New("session closed")

var dpiOnce sync.Once

// setDPIAwareness makes GDI, SendInput and display APIs use physical pixels.
func setDPIAwareness() {
	dpiOnce.Do(func() {
		if procSetProcessDpiAwarenessContext.Find() == nil {
			if r, _, _ := procSetProcessDpiAwarenessContext.Call(dpiAwarenessContextPerMonitorAwareV2); r != 0 {
				return
			}
		}
		if procSetProcessDPIAware.Find() == nil {
			procSetProcessDPIAware.Call()
		}
	})
}

// Preflight verifies the Parsec Virtual Display Driver is installed and responds.
func Preflight() error {
	d, err := openVDD()
	if err != nil {
		return err
	}
	defer d.close()
	if _, err := d.version(); err != nil {
		return fmt.Errorf("Parsec Virtual Display Driver is installed but not responding (%v); reinstall it or reboot. %s", err, vddInstallHint)
	}
	return nil
}

type winSession struct {
	// display + capture state
	mu       sync.Mutex
	closed   atomic.Bool // set under mu by Close; read lock-free
	vdd      *vddDevice
	vddIndex int
	vddAdded bool
	saved    *displayConfig
	gdiName  string
	capturer *gdiCapturer
	bufs     [2][]byte
	bufW     int // dimensions bufs were allocated for
	bufH     int
	cur      int // index into bufs of the last returned frame
	havePrev bool
	ticker   *time.Ticker
	done     chan struct{} // closed by Close to wake NextFrame
	first    bool

	geomMu     sync.RWMutex
	x, y, w, h int

	inputMu     sync.Mutex
	wheel       wheelAccumulator
	heldKeys    map[string]scanKey
	heldButtons map[int]bool

	closeOnce sync.Once
	closeErr  error
}

// Start plugs a Parsec virtual display, optionally isolates it as the only
// active display, and sizes it to opts.Width×opts.Height (or the closest mode
// the driver offers).
func Start(opts Options) (Session, error) {
	width, height := opts.Width, opts.Height
	if width <= 0 || height <= 0 {
		width, height = 1920, 1080
	}
	setDPIAwareness()

	saved, err := queryDisplayConfig(qdcOnlyActivePaths)
	if err != nil {
		return nil, fmt.Errorf("query display configuration: %w", err)
	}
	baseline := make(map[targetKey]bool, len(saved.paths))
	for i := range saved.paths {
		baseline[saved.paths[i].targetKey()] = true
	}

	writeVDDCustomResolution(width, height)

	vdd, err := openVDD()
	if err != nil {
		return nil, err
	}
	s := &winSession{
		vdd:         vdd,
		saved:       saved,
		first:       true,
		heldKeys:    make(map[string]scanKey),
		heldButtons: make(map[int]bool),
	}
	vdd.startKeepalive()

	if opts.Isolate {
		ensureGuard()
		// Isolation is applied without saving to the display database, so the database still
		// holds the user's layout; Recover re-applies it if this process dies mid-session.
		if err := writeIsolationState(true); err != nil {
			vdd.close()
			return nil, fmt.Errorf("recording display layout: %w", err)
		}
	}

	if err := s.setup(width, height, opts.Isolate, baseline); err != nil {
		if cerr := s.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("cleanup: %w", cerr))
		}
		return nil, err
	}
	s.ticker = time.NewTicker(frameInterval)
	s.done = make(chan struct{})
	return s, nil
}

// Recover restores the user's display layout if a previous connector died while a session had
// other displays switched off.
func Recover() error {
	var isolated bool
	found, err := readIsolationState(&isolated)
	if err != nil || !found {
		return err
	}
	if err := setDisplayConfig(nil, nil, sdcApply|sdcUseDatabaseCurrent); err != nil {
		return fmt.Errorf("restore display configuration: %w", err)
	}
	clearIsolationState()
	return nil
}

func (s *winSession) setup(width, height int, isolate bool, baseline map[targetKey]bool) error {
	idx, err := s.vdd.addDisplay()
	if err != nil {
		return err
	}
	s.vddIndex, s.vddAdded = idx, true

	cfg, pathIdx, err := waitForVDDPath(baseline)
	if err != nil {
		return err
	}
	vddTarget := cfg.paths[pathIdx].targetKey()

	if isolate {
		paths, modes := isolatedConfig(cfg, pathIdx, width, height, true)
		err := setDisplayConfig(paths, modes, sdcApply|sdcUseSuppliedDisplayConfig|sdcAllowChanges)
		if err != nil {
			paths, modes = isolatedConfig(cfg, pathIdx, width, height, false)
			if err2 := setDisplayConfig(paths, modes, sdcApply|sdcUseSuppliedDisplayConfig|sdcAllowChanges); err2 != nil {
				return fmt.Errorf("isolate virtual display: %w", errors.Join(err, err2))
			}
		}
		// Re-resolve the path: indices shift after reconfiguration.
		cfg, err = queryDisplayConfig(qdcOnlyActivePaths)
		if err != nil {
			return fmt.Errorf("query display configuration: %w", err)
		}
		pathIdx = -1
		for i := range cfg.paths {
			if cfg.paths[i].targetKey() == vddTarget {
				pathIdx = i
				break
			}
		}
		if pathIdx < 0 {
			return errors.New("virtual display became inactive after isolating it")
		}
	}

	s.gdiName, err = getSourceGDIName(&cfg.paths[pathIdx])
	if err != nil {
		return err
	}

	x, y, w, h, err := displayRect(s.gdiName)
	if err != nil {
		return err
	}
	if w != width || h != height {
		// Best effort: on failure we keep whatever mode the display has.
		_ = setDisplayResolution(s.gdiName, width, height)
		if x, y, w, h, err = displayRect(s.gdiName); err != nil {
			return err
		}
	}
	if w <= 0 || h <= 0 {
		return fmt.Errorf("virtual display %s reports invalid size %dx%d", s.gdiName, w, h)
	}
	s.setGeom(x, y, w, h)
	return nil
}

// waitForVDDPath polls the active display paths until the freshly plugged
// Parsec display shows up. If Windows doesn't activate it on its own, an
// extend topology is requested halfway through.
func waitForVDDPath(baseline map[targetKey]bool) (*displayConfig, int, error) {
	deadline := time.Now().Add(vddAppearTimeout)
	extendAt := time.Now().Add(vddAppearTimeout / 2)
	extended := false
	var lastErr error
	for {
		cfg, err := queryDisplayConfig(qdcOnlyActivePaths)
		if err == nil {
			if i := findNewVDDPath(cfg, baseline); i >= 0 {
				return cfg, i, nil
			}
		} else {
			lastErr = err
		}
		now := time.Now()
		if now.After(deadline) {
			if lastErr != nil {
				return nil, 0, fmt.Errorf("virtual display did not become active within %s: %w", vddAppearTimeout, lastErr)
			}
			return nil, 0, fmt.Errorf("virtual display did not become active within %s", vddAppearTimeout)
		}
		if !extended && now.After(extendAt) {
			extended = true
			_ = setDisplayConfig(nil, nil, sdcApply|sdcTopologyExtend)
		}
		time.Sleep(vddPollInterval)
	}
}

func (s *winSession) setGeom(x, y, w, h int) {
	s.geomMu.Lock()
	s.x, s.y, s.w, s.h = x, y, w, h
	s.geomMu.Unlock()
}

func (s *winSession) geom() (x, y, w, h int) {
	s.geomMu.RLock()
	defer s.geomMu.RUnlock()
	return s.x, s.y, s.w, s.h
}

func (s *winSession) Size() (int, int) {
	_, _, w, h := s.geom()
	return w, h
}

func (s *winSession) NextFrame(ctx context.Context) (*Frame, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if s.closed.Load() {
			return nil, errClosed
		}
		s.mu.Lock()
		first := s.first
		s.first = false
		ticker, done := s.ticker, s.done
		s.mu.Unlock()

		if !first {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-done:
				return nil, errClosed
			case <-ticker.C:
			}
		}

		f, err := s.captureChanged()
		if err != nil || f != nil {
			return f, err
		}
	}
}

// captureChanged grabs one frame and returns it if it differs from the last
// returned frame; (nil, nil) means unchanged or temporarily uncapturable.
func (s *winSession) captureChanged() (*Frame, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return nil, errClosed
	}
	x, y, w, h, err := displayRect(s.gdiName)
	if err != nil {
		return nil, fmt.Errorf("virtual display lost: %w", err)
	}
	if w <= 0 || h <= 0 {
		return nil, nil
	}
	s.setGeom(x, y, w, h)

	if s.capturer == nil || s.capturer.w != w || s.capturer.h != h {
		if s.capturer != nil {
			s.capturer.close()
			s.capturer = nil
		}
		c, err := newGDICapturer(w, h)
		if err != nil {
			return nil, err
		}
		s.capturer = c
	}
	if s.bufW != w || s.bufH != h {
		size := w * h * 4
		s.bufs = [2][]byte{make([]byte, size), make([]byte, size)}
		s.bufW, s.bufH = w, h
		s.havePrev = false
	}

	next := 1 - s.cur
	if err := s.capturer.capture(x, y, s.bufs[next]); err != nil {
		// Typically a secure desktop (UAC, lock screen) is showing; rebuild
		// the DCs and try again on the next tick.
		s.capturer.close()
		s.capturer = nil
		return nil, nil
	}
	if s.havePrev && bytes.Equal(s.bufs[next], s.bufs[s.cur]) {
		return nil, nil
	}
	s.cur = next
	s.havePrev = true
	return &Frame{Width: w, Height: h, Stride: 4 * w, Pix: s.bufs[next]}, nil
}

func (s *winSession) isClosed() bool { return s.closed.Load() }

func (s *winSession) MouseMove(x, y int) error {
	if s.isClosed() {
		return errClosed
	}
	ox, oy, w, h := s.geom()
	x = min(max(x, 0), max(w-1, 0))
	y = min(max(y, 0), max(h-1, 0))
	s.inputMu.Lock()
	defer s.inputMu.Unlock()
	return sendInputs([]input{absoluteMouse(ox+x, oy+y)})
}

func (s *winSession) MouseButton(button int, down bool) error {
	if s.isClosed() {
		return errClosed
	}
	in, ok := mouseButtonInput(button, down)
	if !ok {
		return nil
	}
	s.inputMu.Lock()
	defer s.inputMu.Unlock()
	if down {
		s.heldButtons[button] = true
	} else {
		delete(s.heldButtons, button)
	}
	return sendInputs([]input{in})
}

func (s *winSession) Scroll(dx, dy float64) error {
	if s.isClosed() {
		return errClosed
	}
	s.inputMu.Lock()
	defer s.inputMu.Unlock()
	return sendInputs(s.wheel.inputs(dx, dy))
}

func (s *winSession) Key(code string, down bool) error {
	if s.isClosed() {
		return errClosed
	}
	k, ok := winScancodes[code]
	if !ok {
		return nil
	}
	s.inputMu.Lock()
	defer s.inputMu.Unlock()
	if down {
		s.heldKeys[code] = k
	} else {
		delete(s.heldKeys, code)
	}
	return sendInputs([]input{keyInput(k, down)})
}

func (s *winSession) ReleaseAll() error {
	s.inputMu.Lock()
	defer s.inputMu.Unlock()
	return s.releaseAllLocked()
}

func (s *winSession) releaseAllLocked() error {
	ins := make([]input, 0, len(s.heldKeys)+len(s.heldButtons))
	for code, k := range s.heldKeys {
		ins = append(ins, keyInput(k, false))
		delete(s.heldKeys, code)
	}
	for b := range s.heldButtons {
		if in, ok := mouseButtonInput(b, false); ok {
			ins = append(ins, in)
		}
		delete(s.heldButtons, b)
	}
	s.wheel = wheelAccumulator{}
	return sendInputs(ins)
}

// Close releases held input, restores the original display layout and
// unplugs the virtual display. Safe to call more than once.
func (s *winSession) Close() error {
	s.closeOnce.Do(func() {
		var errs []error

		s.inputMu.Lock()
		if err := s.releaseAllLocked(); err != nil {
			errs = append(errs, err)
		}
		s.inputMu.Unlock()

		s.mu.Lock()
		s.closed.Store(true)
		if s.ticker != nil {
			s.ticker.Stop()
			close(s.done)
		}
		if s.capturer != nil {
			s.capturer.close()
			s.capturer = nil
		}
		s.bufs = [2][]byte{}
		s.bufW, s.bufH = 0, 0
		s.mu.Unlock()

		if s.vddAdded {
			if err := restoreDisplayConfig(s.saved); err != nil {
				errs = append(errs, fmt.Errorf("restore display configuration: %w", err))
			}
			if err := s.vdd.removeDisplay(s.vddIndex); err != nil {
				errs = append(errs, err)
			}
		}
		clearIsolationState()
		if s.vdd != nil {
			s.vdd.close()
		}
		s.closeErr = errors.Join(errs...)
	})
	return s.closeErr
}
