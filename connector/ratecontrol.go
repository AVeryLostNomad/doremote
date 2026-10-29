package main

import (
	"sync"
	"time"
)

const (
	// rateInterval is how often the controller re-evaluates the bitrate.
	rateInterval = 500 * time.Millisecond
	// Average queueing delay (ack round trip above the link's base round trip) at which the
	// link counts as congested, and below which it counts as having headroom.
	congestedDelay = 80 * time.Millisecond
	clearDelay     = 30 * time.Millisecond
	decreaseFactor = 0.75
	increaseFactor = 1.10
	// The bitrate only grows while the stream actually uses this share of it: a still screen
	// says nothing about the link, and growing then would burst on the next motion.
	usedShare = 0.5
	// baseWindow is how long a base round-trip sample is remembered, so a route change that
	// raises the base round trip isn't mistaken for congestion forever.
	baseWindow = 10 * time.Second
)

type sentFrame struct {
	at    time.Time
	bytes int
	key   bool
}

// rateControl adapts the encoder bitrate to the path between connector and viewer from the
// viewer's per-frame acks: it lowers the bitrate when acks come back later than the path's
// base round trip (frames are queueing somewhere) and raises it while there is headroom.
type rateControl struct {
	mu       sync.Mutex
	bitrate  int
	min, max int

	inflight []sentFrame // sent, not yet acked; acks arrive in send order

	// Minimum round trip over the current and previous baseWindow.
	baseCur, basePrev time.Duration
	baseSince         time.Time

	windowStart time.Time
	delaySum    time.Duration
	delayN      int
	ackedBytes  int
}

func newRateControl(width, height int, now time.Time) *rateControl {
	pixelRate := float64(width) * float64(height) * 60
	rc := &rateControl{
		bitrate:     int(pixelRate * 0.05), // ≈6 Mbps at 1080p
		min:         max(int(pixelRate*0.005), 500_000),
		max:         int(pixelRate * 0.25),
		windowStart: now,
		baseSince:   now,
	}
	rc.bitrate = min(max(rc.bitrate, rc.min), rc.max)
	return rc
}

func (rc *rateControl) sent(bytes int, key bool, now time.Time) {
	rc.mu.Lock()
	rc.inflight = append(rc.inflight, sentFrame{at: now, bytes: bytes, key: key})
	rc.mu.Unlock()
}

// acked records the viewer's ack of the oldest unacked frame and returns the bitrate to use,
// with changed reporting whether it differs from the previous one.
func (rc *rateControl) acked(now time.Time) (bitrate int, changed bool) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if len(rc.inflight) == 0 {
		return rc.bitrate, false
	}
	f := rc.inflight[0]
	rc.inflight = append(rc.inflight[:0], rc.inflight[1:]...)
	rtt := now.Sub(f.at)

	if now.Sub(rc.baseSince) > baseWindow {
		rc.basePrev, rc.baseCur, rc.baseSince = rc.baseCur, 0, now
	}
	if rc.baseCur == 0 || rtt < rc.baseCur {
		rc.baseCur = rtt
	}
	base := rc.baseCur
	if rc.basePrev > 0 && rc.basePrev < base {
		base = rc.basePrev
	}

	rc.ackedBytes += f.bytes
	// A keyframe is several times larger than other frames, so its transmission time would
	// read as queueing.
	if !f.key {
		rc.delaySum += rtt - base
		rc.delayN++
	}

	elapsed := now.Sub(rc.windowStart)
	if elapsed < rateInterval {
		return rc.bitrate, false
	}
	old := rc.bitrate
	if rc.delayN > 0 {
		delay := rc.delaySum / time.Duration(rc.delayN)
		used := float64(rc.ackedBytes*8) / elapsed.Seconds()
		switch {
		case delay > congestedDelay:
			rc.bitrate = max(int(float64(rc.bitrate)*decreaseFactor), rc.min)
		case delay < clearDelay && used >= usedShare*float64(rc.bitrate):
			rc.bitrate = min(int(float64(rc.bitrate)*increaseFactor), rc.max)
		}
	}
	rc.windowStart, rc.delaySum, rc.delayN, rc.ackedBytes = now, 0, 0, 0
	return rc.bitrate, rc.bitrate != old
}
