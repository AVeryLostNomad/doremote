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
	// displayCheckInterval paces verifying that the virtual display is still active (and alone,
	// when isolating). Windows can apply another layout mid-session, e.g. when a switched-off
	// monitor drops its hot-plug signal, and may give the virtual display a new GDI name.
	displayCheckInterval = 500 * time.Millisecond
	// displayLostTimeout ends a session whose virtual display can't be brought back.
	displayLostTimeout = 10 * time.Second
	// At most maxLayoutRestores layout restores per layoutRestoreWindow; beyond that the
	// session stops switching other screens off instead of reconfiguring them in a loop.
	maxLayoutRestores   = 3
	layoutRestoreWindow = time.Minute
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

	width, height    int // requested mode of the virtual display
	isolate          bool
	vddTarget        targetKey
	displayChecked   time.Time   // last verification of the display layout
	displayStale     bool        // verify the layout before the next capture
	displayLostSince time.Time   // first poll the virtual display was unusable; zero while usable
	restoredAt       time.Time   // last attempt to put the session's layout back
	restores         []time.Time // layout restores within layoutRestoreWindow

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
		width:       width,
		height:      height,
		isolate:     opts.Isolate,
		kick:        make(chan struct{}, 1),
		heldKeys:    make(map[string]scanKey),
		heldButtons: make(map[int]bool),
	}
	s.bitrate.Store(int64(max(opts.Bitrate, 0)))
	vdd.startKeepalive()

	if opts.Isolate {
		ensureGuard()
		// Isolation is saved to the display database so Windows keeps it when it re-evaluates
		// the layout (it re-applies saved layouts on hot-plug events). Windows keys saved
		// layouts by the set of connected displays, so this only replaces the entry for the
		// user's displays plus the virtual one; their own layout stays saved. Recover puts it
		// back if this process dies mid-session.
		if err := writeIsolationState(true); err != nil {
			vdd.close()
			return nil, fmt.Errorf("recording display layout: %w", err)
		}
	}

	if err := s.setup(baseline); err != nil {
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
	// The driver unplugs the virtual display once its owner stops pinging it. Until then the
	// saved layout for the connected displays is the isolated one.
	for deadline := time.Now().Add(2 * time.Second); vddActive() && time.Now().Before(deadline); {
		time.Sleep(vddPollInterval)
	}
	if err := setDisplayConfig(nil, nil, sdcApply|sdcUseDatabaseCurrent); err != nil {
		return fmt.Errorf("restore display configuration: %w", err)
	}
	clearIsolationState()
	return nil
}

// vddActive reports whether a Parsec virtual display is part of the active layout.
func vddActive() bool {
	cfg, err := queryDisplayConfig(qdcOnlyActivePaths)
	if err != nil {
		return false
	}
	for i := range cfg.paths {
		if isVDDTarget(&cfg.paths[i]) {
			return true
		}
	}
	return false
}

func (s *winSession) setup(baseline map[targetKey]bool) error {
	idx, err := s.vdd.addDisplay()
	if err != nil {
		return err
	}
	s.vddIndex, s.vddAdded = idx, true

	cfg, pathIdx, err := waitForVDDPath(baseline)
	if err != nil {
		return err
	}
	s.vddTarget = cfg.paths[pathIdx].targetKey()
	if s.isolate {
		if cfg, pathIdx, err = s.isolateVDD(cfg, pathIdx); err != nil {
			return err
		}
	} else if cfg, pathIdx, err = s.extendVDD(cfg, baseline); err != nil {
		return err
	}
	s.displayChecked = time.Now()
	return s.adoptPath(&cfg.paths[pathIdx])
}

// activePath returns the index of target's path in an active-paths configuration, or -1.
func activePath(cfg *displayConfig, target targetKey) int {
	for i := range cfg.paths {
		if cfg.paths[i].targetKey() == target {
			return i
		}
	}
	return -1
}

// extendVDD keeps the user's displays on beside the virtual display. Windows applies the
// layout saved for this set of displays when the virtual display appears, which an isolated
// session saved with them off; the extended layout replaces it.
func (s *winSession) extendVDD(cfg *displayConfig, baseline map[targetKey]bool) (*displayConfig, int, error) {
	on := make(map[targetKey]bool, len(baseline))
	for i := range cfg.paths {
		if k := cfg.paths[i].targetKey(); baseline[k] {
			on[k] = true
		}
	}
	idx := activePath(cfg, s.vddTarget)
	if len(on) == len(baseline) {
		return cfg, idx, nil
	}
	if err := setDisplayConfig(nil, nil, sdcApply|sdcTopologyExtend); err != nil {
		return nil, 0, fmt.Errorf("turn displays back on: %w", err)
	}
	cfg, err := queryDisplayConfig(qdcOnlyActivePaths)
	if err != nil {
		return nil, 0, fmt.Errorf("query display configuration: %w", err)
	}
	if err := setDisplayConfig(cfg.paths, cfg.modes, sdcApply|sdcUseSuppliedDisplayConfig|sdcSaveToDatabase|sdcAllowChanges); err != nil {
		log.Printf("display: save extended layout: %v", err)
	}
	if idx = activePath(cfg, s.vddTarget); idx < 0 {
		return nil, 0, errors.New("virtual display became inactive after turning the other displays on")
	}
	return cfg, idx, nil
}

// isolateVDD makes the virtual display at cfg.paths[idx] the only active display and returns
// the new active configuration with the display's path index in it. If Windows accepts the
// layout but keeps other displays on, isolation is dropped for the rest of the session:
// requesting it again would only reconfigure the screens over and over.
func (s *winSession) isolateVDD(cfg *displayConfig, idx int) (*displayConfig, int, error) {
	const flags = sdcApply | sdcUseSuppliedDisplayConfig | sdcSaveToDatabase | sdcAllowChanges
	paths, modes := isolatedConfig(cfg, idx, s.width, s.height, true)
	err := setDisplayConfig(paths, modes, flags)
	if err != nil {
		paths, modes = isolatedConfig(cfg, idx, s.width, s.height, false)
		if err2 := setDisplayConfig(paths, modes, flags); err2 != nil {
			return nil, 0, fmt.Errorf("isolate virtual display: %w", errors.Join(err, err2))
		}
	}
	// Re-resolve the path: indices shift after reconfiguration.
	cfg, err = queryDisplayConfig(qdcOnlyActivePaths)
	if err != nil {
		return nil, 0, fmt.Errorf("query display configuration: %w", err)
	}
	if idx = activePath(cfg, s.vddTarget); idx < 0 {
		return nil, 0, errors.New("virtual display became inactive after isolating it")
	}
	if n := len(cfg.paths) - 1; n > 0 {
		s.stopIsolating(fmt.Sprintf("Windows kept %d other display(s) on", n))
	}
	return cfg, idx, nil
}

// stopIsolating leaves the other displays as Windows has them for the rest of the session;
// the virtual display is still followed and streamed.
func (s *winSession) stopIsolating(reason string) {
	log.Printf("display: %s; leaving other screens on for the rest of the session", reason)
	s.isolate = false
}

// adoptPath captures the virtual display's source on path p from now on, switching it to the
// requested mode if needed.
func (s *winSession) adoptPath(p *pathInfo) error {
	name, err := getSourceGDIName(p)
	if err != nil {
		return err
	}
	x, y, w, h, err := displayRect(name)
	if err != nil {
		return err
	}
	if w != s.width || h != s.height {
		// Best effort: on failure we keep whatever mode the display has.
		_ = setDisplayResolution(name, s.width, s.height)
		if x, y, w, h, err = displayRect(name); err != nil {
			return err
		}
	}
	if w <= 0 || h <= 0 {
		return fmt.Errorf("virtual display %s reports invalid size %dx%d", name, w, h)
	}
	s.setGDIName(name)
	s.setGeom(x, y, w, h)
	return nil
}

// setGDIName switches capture to the GDI display name; a duplication of the previous name
// would keep showing whatever display now owns it.
func (s *winSession) setGDIName(name string) {
	if name == s.gdiName {
		return
	}
	if s.gdiName != "" {
		log.Printf("display: virtual display moved from %s to %s", s.gdiName, name)
	}
	s.gdiName = name
	s.resetDuplication()
}

// resetDuplication recreates desktop duplication on the next capture. Failures while the
// layout was wrong don't mean duplication can't work here, so a GDI fallback taken after
// duplication had worked is undone too.
func (s *winSession) resetDuplication() {
	if s.dda != nil {
		s.dda.close()
		s.dda = nil
	}
	s.ddaFailingSince = time.Time{}
	if s.ddaWorked {
		s.ddaOff = false
	}
}

// checkDisplay verifies that the virtual display is active (and the only active display when
// isolating), restoring that layout if Windows changed it, and tracks its GDI name. It
// reports whether the display can be captured now.
func (s *winSession) checkDisplay() bool {
	cfg, err := queryDisplayConfig(qdcOnlyActivePaths)
	if err != nil {
		return false
	}
	idx := activePath(cfg, s.vddTarget)
	if idx >= 0 && (!s.isolate || len(cfg.paths) == 1) {
		return s.followPath(&cfg.paths[idx])
	}
	now := time.Now()
	if now.Sub(s.restoredAt) < displayCheckInterval {
		return false
	}
	// Restores within the last layoutRestoreWindow; past maxLayoutRestores, Windows (or a
	// monitor that keeps reconnecting) is undoing them faster than they help.
	recent := s.restores[:0]
	for _, t := range s.restores {
		if now.Sub(t) < layoutRestoreWindow {
			recent = append(recent, t)
		}
	}
	s.restores = recent
	if len(s.restores) >= maxLayoutRestores {
		if !s.isolate {
			return false // the virtual display keeps going away; displayLostTimeout ends the session
		}
		s.stopIsolating(fmt.Sprintf("Windows changed the layout %d times within %s", len(s.restores), layoutRestoreWindow))
		if idx >= 0 {
			return s.followPath(&cfg.paths[idx])
		}
	}
	s.restoredAt = now
	s.restores = append(s.restores, now)
	log.Printf("display: layout changed (virtual display active: %v, %d active displays); restoring it", idx >= 0, len(cfg.paths))
	if idx < 0 {
		// The extend topology activates every connected display, the virtual one included.
		if err := setDisplayConfig(nil, nil, sdcApply|sdcTopologyExtend); err != nil {
			log.Printf("display: reactivate virtual display: %v", err)
			return false
		}
		if cfg, err = queryDisplayConfig(qdcOnlyActivePaths); err != nil {
			return false
		}
		if idx = activePath(cfg, s.vddTarget); idx < 0 {
			log.Printf("display: virtual display is still inactive")
			return false
		}
	}
	if s.isolate {
		icfg, iidx, err := s.isolateVDD(cfg, idx)
		if err != nil {
			s.stopIsolating(err.Error())
			// The failed attempt may still have changed the layout.
			if cfg, err = queryDisplayConfig(qdcOnlyActivePaths); err != nil {
				return false
			}
			if idx = activePath(cfg, s.vddTarget); idx < 0 {
				return false
			}
		} else {
			cfg, idx = icfg, iidx
		}
	}
	if err := s.adoptPath(&cfg.paths[idx]); err != nil {
		log.Printf("display: %v", err)
		return false
	}
	s.resetDuplication()
	log.Printf("display: restored virtual display %s", s.gdiName)
	return true
}

// followPath captures the virtual display's source on path p without changing the layout.
func (s *winSession) followPath(p *pathInfo) bool {
	name, err := getSourceGDIName(p)
	if err != nil {
		return false
	}
	s.setGDIName(name)
	return true
}

// display returns the virtual display's desktop rectangle, verifying the layout every
// displayCheckInterval or when capture failed. w == 0 means nothing can be captured now: the
// session shows its last picture rather than another display.
func (s *winSession) display() (x, y, w, h int, err error) {
	now := time.Now()
	ok := true
	if s.displayStale || now.Sub(s.displayChecked) >= displayCheckInterval {
		ok = s.checkDisplay()
		s.displayChecked = now
	}
	if ok {
		x, y, w, h, err = displayRect(s.gdiName)
		ok = err == nil && w > 0 && h > 0
	}
	if ok {
		s.displayStale = false
		s.displayLostSince = time.Time{}
		s.setGeom(x, y, w, h)
		return x, y, w, h, nil
	}
	s.displayStale = true
	if s.displayLostSince.IsZero() {
		s.displayLostSince = now
	} else if now.Sub(s.displayLostSince) > displayLostTimeout {
		return 0, 0, 0, 0, fmt.Errorf("virtual display lost for %s", displayLostTimeout)
	}
	return 0, 0, 0, 0, nil
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
	x, y, w, h, err := s.display()
	if err != nil {
		return nil, 0, err
	}
	if w == 0 {
		return nil, displayCheckInterval, nil
	}

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
			s.displayStale = true // e.g. the display's GDI name changed
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
			s.displayStale = true
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
