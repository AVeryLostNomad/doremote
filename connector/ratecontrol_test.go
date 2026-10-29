package main

import (
	"testing"
	"time"
)

// link models a bottleneck of capBps with a fixed base round trip: frames queue behind each
// other and each ack returns base after the frame finished crossing the link.
type link struct {
	capBps   float64
	base     time.Duration
	busyTill time.Time
}

func (l *link) ackTime(sent time.Time, bytes int) time.Time {
	start := sent
	if l.busyTill.After(start) {
		start = l.busyTill
	}
	l.busyTill = start.Add(time.Duration(float64(bytes*8) / l.capBps * float64(time.Second)))
	return l.busyTill.Add(l.base)
}

// run streams frames at fps for d through l, each sized to use `use` times the current
// bitrate (1 = a busy screen, small = refinements of a still one), and returns the mean
// bitrate over the last half of the run.
func run(rc *rateControl, l *link, now *time.Time, d time.Duration, fps int, use float64) int {
	interval := time.Second / time.Duration(fps)
	type pending struct {
		at time.Time
	}
	var acks []pending
	var sum, n int
	for t := time.Duration(0); t < d; t += interval {
		for len(acks) > 0 && !acks[0].at.After(*now) {
			rc.acked(acks[0].at)
			acks = acks[1:]
		}
		bytes := int(float64(rc.bitrate) * use / 8 / float64(fps))
		rc.sent(bytes, false, *now)
		acks = append(acks, pending{l.ackTime(*now, bytes)})
		if t >= d/2 {
			sum += rc.bitrate
			n++
		}
		*now = now.Add(interval)
	}
	for _, a := range acks {
		rc.acked(a.at)
		*now = a.at
	}
	return sum / n
}

func TestRateControlSettlesBelowLinkCapacity(t *testing.T) {
	now := time.Unix(0, 0)
	rc := newRateControl(1920, 1080, now)
	l := &link{capBps: 3e6, base: 40 * time.Millisecond}
	if rc.bitrate <= 3e6 {
		t.Fatalf("test needs a start above capacity, got %d", rc.bitrate)
	}
	mean := run(rc, l, &now, 30*time.Second, 60, 1)
	if mean > 3e6 || mean < 1.5e6 {
		t.Fatalf("mean bitrate %d on a 3 Mbps link, want 1.5–3 Mbps", mean)
	}
	if lag := l.busyTill.Sub(now); lag > 300*time.Millisecond {
		t.Fatalf("link still has %v queued at the end", lag)
	}
}

func TestRateControlFloorAndCeiling(t *testing.T) {
	now := time.Unix(0, 0)
	rc := newRateControl(1920, 1080, now)
	run(rc, &link{capBps: 100e3, base: 40 * time.Millisecond}, &now, 20*time.Second, 60, 1)
	if rc.bitrate != rc.min {
		t.Fatalf("link far below the floor: bitrate %d, want floor %d", rc.bitrate, rc.min)
	}

	now = time.Unix(0, 0)
	rc = newRateControl(1920, 1080, now)
	run(rc, &link{capBps: 1e9, base: 40 * time.Millisecond}, &now, 60*time.Second, 60, 1)
	if rc.bitrate != rc.max {
		t.Fatalf("fast link: bitrate %d, want cap %d", rc.bitrate, rc.max)
	}
}

func TestRateControlDoesNotGrowOnStillScreen(t *testing.T) {
	now := time.Unix(0, 0)
	rc := newRateControl(1920, 1080, now)
	start := rc.bitrate
	// Two refinement frames a second use a sliver of the bitrate and say nothing about the
	// link, so the next burst of motion must not start from an inflated bitrate.
	run(rc, &link{capBps: 1e9, base: 40 * time.Millisecond}, &now, 10*time.Second, 2, 0.05)
	if rc.bitrate != start {
		t.Fatalf("still screen changed bitrate %d -> %d", start, rc.bitrate)
	}
}

func TestRateControlIgnoresKeyframeTransmissionTime(t *testing.T) {
	now := time.Unix(0, 0)
	rc := newRateControl(1920, 1080, now)
	start := rc.bitrate
	// Every 10th frame is a keyframe that takes 400ms longer to arrive; the rest are clear.
	for i := range 60 {
		key := i%10 == 0
		rtt := 40 * time.Millisecond
		if key {
			rtt += 400 * time.Millisecond
		}
		rc.sent(rc.bitrate/8/30, key, now)
		rc.acked(now.Add(rtt))
		now = now.Add(time.Second / 30)
	}
	if rc.bitrate < start {
		t.Fatalf("keyframe delays lowered the bitrate %d -> %d", start, rc.bitrate)
	}
}

func TestRateControlForgetsOldBaseRoundTrip(t *testing.T) {
	now := time.Unix(0, 0)
	rc := newRateControl(1920, 1080, now)
	fast := &link{capBps: 1e9, base: 20 * time.Millisecond}
	run(rc, fast, &now, time.Second, 60, 1)
	// The route changes to a 150ms base round trip with plenty of capacity. Once the old
	// base has aged out the controller grows again instead of backing off forever.
	slow := &link{capBps: 1e9, base: 150 * time.Millisecond}
	run(rc, slow, &now, 2*baseWindow+time.Second, 60, 1)
	before := rc.bitrate
	run(rc, slow, &now, 2*time.Second, 60, 1)
	if rc.bitrate <= before && rc.bitrate != rc.max {
		t.Fatalf("after a route change the bitrate kept falling: %d -> %d", before, rc.bitrate)
	}
}
