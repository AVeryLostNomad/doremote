//go:build windows

package platform

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

const (
	minFrameInterval = time.Second / MaxFrameRate
	// gdiPollInterval is how often the GDI fallback grabs and compares full frames.
	gdiPollInterval = time.Second / 30
	// ddaWaitSlice bounds each AcquireNextFrame wait, which can't be interrupted, so
	// RequestKeyframe, ctx cancellation and Close take effect within it.
	ddaWaitSlice = 20 * time.Millisecond
	// ddaRetryInterval paces recreating the duplication while the desktop is unavailable
	// (secure desktop, mode change, disconnected session).
	ddaRetryInterval = 100 * time.Millisecond
	// ddaFirstImageTimeout: a fresh duplication delivers the whole desktop at once; a driver
	// that never does is treated as unable to duplicate.
	ddaFirstImageTimeout = 2 * time.Second
	// ddaRecoverTimeout is how long duplication that worked earlier in the session may keep
	// failing with non-transient errors (device removed, driver update) before GDI takes over.
	ddaRecoverTimeout = 5 * time.Second
	vddAppearTimeout  = 6 * time.Second
	vddPollInterval   = 100 * time.Millisecond
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
	// display + capture + encoder state
	mu       sync.Mutex
	closed   atomic.Bool // set under mu by Close; read lock-free
	vdd      *vddDevice
	vddIndex int
	vddAdded bool
	saved    *displayConfig
	gdiName  string
	done     chan struct{} // closed by Close to wake NextFrame

	dda             *ddaCapturer
	ddaOff          bool // duplication unusable: GDI for the rest of the session
	ddaWorked       bool // a duplication delivered an image this session
	ddaCreated      time.Time
	ddaFailingSince time.Time

	gdi     *gdiCapturer
	gdiBufs [2][]byte
	gdiW    int // dimensions gdiBufs were allocated for
	gdiH    int
	gdiCur  int // index into gdiBufs of the last captured frame
	gdiHave bool

	img        []byte // last captured picture, BGRA top-down, stride 4*imgW
	imgW, imgH int
	lastSent   time.Time // when NextFrame last returned a frame
	refines    int       // re-encodes of img since it last changed

	enc        *h264Encoder
	encW, encH int
	encBitrate int
	encName    string

	keyReq  atomic.Bool
	kick    chan struct{} // wakes an idle NextFrame for a keyframe request
	bitrate atomic.Int64  // 0: default for the picture size

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
		done:        make(chan struct{}),
		kick:        make(chan struct{}, 1),
		heldKeys:    make(map[string]scanKey),
		heldButtons: make(map[int]bool),
	}
	s.bitrate.Store(int64(max(opts.Bitrate, 0)))
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

// NextFrame paces to MaxFrameRate, then waits for a changed picture (DDA update or, on the
// GDI fallback, a differing full frame), a keyframe request or a due refinement, and encodes it.
func (s *winSession) NextFrame(ctx context.Context) (*VideoFrame, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if s.closed.Load() {
			return nil, errClosed
		}
		s.mu.Lock()
		pace := time.Until(s.lastSent.Add(minFrameInterval))
		s.mu.Unlock()
		// Updates keep accumulating meanwhile; the poll below picks up the newest picture.
		if err := s.idle(ctx, pace, false); err != nil {
			return nil, err
		}
		f, wait, err := s.poll()
		if err != nil || f != nil {
			return f, err
		}
		if err := s.idle(ctx, wait, true); err != nil {
			return nil, err
		}
	}
}

// idle sleeps for d unless ctx ends, the session closes or (if kickable) a keyframe is requested.
func (s *winSession) idle(ctx context.Context, d time.Duration, kickable bool) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	var kick <-chan struct{}
	if kickable {
		kick = s.kick
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return errClosed
	case <-t.C:
	case <-kick:
	}
	return nil
}

// poll captures once and returns an encoded frame if the picture changed, a keyframe was
// requested or a refinement is due; otherwise how long to idle before polling again.
func (s *winSession) poll() (*VideoFrame, time.Duration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return nil, 0, errClosed
	}
	x, y, w, h, err := displayRect(s.gdiName)
	if err != nil {
		return nil, 0, fmt.Errorf("virtual display lost: %w", err)
	}
	if w <= 0 || h <= 0 {
		return nil, gdiPollInterval, nil
	}
	s.setGeom(x, y, w, h)

	// due is when an unchanged picture is re-encoded anyway: now for a requested keyframe,
	// RefineDelay after the last frame for a refinement.
	var due time.Time
	if s.img != nil {
		if s.keyReq.Load() {
			due = time.Now()
		} else if s.refines < RefineFrames {
			due = s.lastSent.Add(RefineDelay)
		}
	}
	budget := ddaWaitSlice
	if !due.IsZero() {
		budget = min(budget, max(time.Until(due), 0))
	}

	changed, wait, err := s.capture(x, y, w, h, budget)
	if err != nil {
		return nil, 0, err
	}
	if changed {
		s.refines = 0
		f, err := s.encode()
		return f, 0, err
	}
	if due.IsZero() {
		return nil, wait, nil
	}
	if left := time.Until(due); left > 0 {
		return nil, min(wait, left), nil
	}
	if !s.keyReq.Load() {
		s.refines++
	}
	f, err := s.encode()
	return f, 0, err
}

// capture refreshes s.img from desktop duplication, or GDI when duplication is unusable.
// DDA waits up to budget for an update itself; wait is the idle time before the next attempt.
func (s *winSession) capture(x, y, w, h int, budget time.Duration) (changed bool, wait time.Duration, err error) {
	if !s.ddaOff && s.dda == nil {
		c, err := newDDACapturer(s.gdiName)
		if err != nil {
			if !s.ddaGiveUp(err) {
				return false, ddaRetryInterval, nil
			}
		} else {
			s.dda, s.ddaCreated = c, time.Now()
		}
	}
	if s.dda != nil {
		changed, err := s.dda.next(budget)
		switch {
		case err != nil:
			s.dda.close()
			s.dda = nil
			if !s.ddaGiveUp(err) {
				return false, 0, nil // recreate on the next poll
			}
		case changed:
			if !s.ddaWorked {
				s.ddaWorked = true
				log.Printf("capture: DXGI desktop duplication of %s", s.gdiName)
			}
			s.ddaFailingSince = time.Time{}
			s.img, s.imgW, s.imgH = s.dda.img, s.dda.w, s.dda.h
			return true, 0, nil
		case !s.ddaWorked && !s.dda.haveImage && time.Since(s.ddaCreated) > ddaFirstImageTimeout:
			s.dda.close()
			s.dda = nil
			s.ddaFallback(fmt.Errorf("no desktop image within %s", ddaFirstImageTimeout))
		default:
			return false, 0, nil
		}
	}
	return s.captureGDI(x, y, w, h)
}

// ddaGiveUp records a duplication failure and reports whether GDI takes over for the rest of
// the session: at once if duplication never worked here, else once it kept failing for
// ddaRecoverTimeout. Transient failures behave like GDI on a secure desktop: no frames.
func (s *winSession) ddaGiveUp(err error) bool {
	if ddaTransient(err) {
		return false
	}
	if s.ddaWorked {
		if s.ddaFailingSince.IsZero() {
			s.ddaFailingSince = time.Now()
		}
		if time.Since(s.ddaFailingSince) < ddaRecoverTimeout {
			return false
		}
	}
	s.ddaFallback(err)
	return true
}

func (s *winSession) ddaFallback(err error) {
	log.Printf("capture: DXGI desktop duplication unavailable (%v); falling back to GDI", err)
	s.ddaOff = true
}

// captureGDI grabs one frame with GDI and reports whether it differs from the last one.
func (s *winSession) captureGDI(x, y, w, h int) (bool, time.Duration, error) {
	if s.gdi == nil || s.gdi.w != w || s.gdi.h != h {
		if s.gdi != nil {
			s.gdi.close()
			s.gdi = nil
		}
		c, err := newGDICapturer(w, h)
		if err != nil {
			return false, 0, err
		}
		s.gdi = c
	}
	if s.gdiW != w || s.gdiH != h {
		size := w * h * 4
		s.gdiBufs = [2][]byte{make([]byte, size), make([]byte, size)}
		s.gdiW, s.gdiH = w, h
		s.gdiHave = false
	}

	next := 1 - s.gdiCur
	if err := s.gdi.capture(x, y, s.gdiBufs[next]); err != nil {
		// Typically a secure desktop (UAC, lock screen) is showing; rebuild
		// the DCs and try again on the next poll.
		s.gdi.close()
		s.gdi = nil
		return false, gdiPollInterval, nil
	}
	if s.gdiHave && bytes.Equal(s.gdiBufs[next], s.gdiBufs[s.gdiCur]) {
		return false, gdiPollInterval, nil
	}
	s.gdiCur = next
	s.gdiHave = true
	s.img, s.imgW, s.imgH = s.gdiBufs[next], w, h
	return true, 0, nil
}

// encode encodes s.img, (re)creating the encoder for its size, and stamps lastSent.
func (s *winSession) encode() (*VideoFrame, error) {
	// 4:2:0 needs even dimensions; an odd mode loses its last column/row.
	w, h := s.imgW&^1, s.imgH&^1
	if s.enc != nil && (s.encW != w || s.encH != h) {
		s.enc.close()
		s.enc = nil
	}
	key := s.keyReq.Swap(false)
	for attempt := 0; ; attempt++ {
		if s.enc == nil {
			br := s.targetBitrate(w, h)
			e, err := newH264Encoder(w, h, MaxFrameRate, br)
			if err != nil {
				if key {
					s.keyReq.Store(true)
				}
				return nil, fmt.Errorf("create H.264 encoder: %w", err)
			}
			s.enc, s.encW, s.encH, s.encBitrate = e, w, h, br
			if n := e.name(); n != s.encName {
				s.encName = n
				log.Printf("video encoder: %s", n)
			}
		}
		if br := s.targetBitrate(w, h); br != s.encBitrate {
			s.enc.setBitrate(br)
			s.encBitrate = br
		}
		au, isKey, err := s.enc.encode(s.img, 4*s.imgW, key)
		if err == nil {
			s.lastSent = time.Now()
			return &VideoFrame{Data: au, Key: isKey, Width: w, Height: h}, nil
		}
		s.enc.close()
		s.enc = nil
		if attempt > 0 {
			if key {
				s.keyReq.Store(true)
			}
			return nil, fmt.Errorf("H.264 encode: %w", err)
		}
		// A GPU reset or driver update can break a hardware encoder; retry once on a fresh one.
	}
}

// targetBitrate is the requested bitrate, or 0.05 bits per pixel at MaxFrameRate by default.
func (s *winSession) targetBitrate(w, h int) int {
	if b := s.bitrate.Load(); b > 0 {
		return int(b)
	}
	return w * h * MaxFrameRate / 20
}

func (s *winSession) RequestKeyframe() {
	s.keyReq.Store(true)
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

func (s *winSession) SetBitrate(bps int) {
	if bps > 0 {
		s.bitrate.Store(int64(bps))
	}
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
		close(s.done)
		if s.dda != nil {
			s.dda.close()
			s.dda = nil
		}
		if s.gdi != nil {
			s.gdi.close()
			s.gdi = nil
		}
		if s.enc != nil {
			s.enc.close()
			s.enc = nil
		}
		s.gdiBufs = [2][]byte{}
		s.gdiW, s.gdiH = 0, 0
		s.img = nil
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
