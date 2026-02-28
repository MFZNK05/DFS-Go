package failure

import (
	"testing"
	"time"
)

// TestPhiBelowThresholdWhileAlive verifies that regular heartbeats keep phi < threshold.
func TestPhiBelowThresholdWhileAlive(t *testing.T) {
	cfg := DefaultConfig()
	d := NewPhiAccrualDetector(cfg)

	addr := "peer1"
	// Feed 50 regular 1s-interval heartbeats.
	now := time.Now()
	for i := 0; i < 50; i++ {
		d.RecordHeartbeat(addr, now)
		now = now.Add(time.Second)
	}

	// Immediately after last heartbeat phi should be very low.
	phi := d.windows[addr].phi(now)
	if phi >= cfg.SuspectThreshold {
		t.Errorf("expected phi < %.1f after regular heartbeats, got %.4f", cfg.SuspectThreshold, phi)
	}
}

// TestPhiIncreasesOverTime verifies that phi grows when heartbeats stop.
// Anchors elapsed time to lastArrival so wall-clock drift in the test loop
// doesn't pollute the computation.
func TestPhiIncreasesOverTime(t *testing.T) {
	cfg := DefaultConfig()
	d := NewPhiAccrualDetector(cfg)

	addr := "peer2"
	now := time.Now()
	// Feed 50 heartbeats at 1s intervals (mean=1000ms, stddev guard=100ms).
	for i := 0; i < 50; i++ {
		d.RecordHeartbeat(addr, now)
		now = now.Add(time.Second)
	}

	// Use lastArrival as base so real elapsed time between record calls is excluded.
	w := d.windows[addr]
	w.mu.Lock()
	base := w.lastArrival
	w.mu.Unlock()

	// 1100ms: y=(1100-1000)/(100*sqrt2)≈0.71 → phi≈0.80
	// 1200ms: y=(1200-1000)/(100*sqrt2)≈1.41 → phi≈1.17
	phiSoon := w.phi(base.Add(1100 * time.Millisecond))
	phiLater := w.phi(base.Add(1200 * time.Millisecond))

	if phiLater <= phiSoon {
		t.Errorf("expected phi to increase over time: phiSoon=%.4f phiLater=%.4f", phiSoon, phiLater)
	}
}

// TestPhiExceedsThresholdAfterSilence verifies threshold is crossed after sufficient silence.
func TestPhiExceedsThresholdAfterSilence(t *testing.T) {
	cfg := DefaultConfig()
	d := NewPhiAccrualDetector(cfg)

	addr := "peer3"
	now := time.Now()
	for i := 0; i < 50; i++ {
		d.RecordHeartbeat(addr, now)
		now = now.Add(time.Second)
	}

	// After 30s of silence (30x the interval), phi should be >> 8.0.
	phi := d.windows[addr].phi(now.Add(30 * time.Second))
	if phi < cfg.SuspectThreshold {
		t.Errorf("expected phi > %.1f after long silence, got %.4f", cfg.SuspectThreshold, phi)
	}
}

// TestPhiNoSamples verifies phi is 0 when no heartbeats have been recorded.
func TestPhiNoSamples(t *testing.T) {
	d := NewPhiAccrualDetector(DefaultConfig())
	phi := d.Phi("unknown-peer")
	if phi != 0.0 {
		t.Errorf("expected phi=0 for unknown peer, got %.4f", phi)
	}
}

// TestPhiOneSample verifies phi is 0 with only one sample (no interval yet).
func TestPhiOneSample(t *testing.T) {
	d := NewPhiAccrualDetector(DefaultConfig())
	d.RecordHeartbeat("p", time.Now())
	phi := d.Phi("p")
	if phi != 0.0 {
		t.Errorf("expected phi=0 with only 1 sample, got %.4f", phi)
	}
}

// TestWindowRingBuffer verifies the ring buffer wraps correctly.
func TestWindowRingBuffer(t *testing.T) {
	const cap = 5
	w := newPeerWindow(cap)

	now := time.Now()
	// Feed 10 heartbeats (2x capacity) — only last 5 intervals should remain.
	for i := 0; i < 11; i++ {
		w.record(now)
		now = now.Add(time.Second)
	}

	if w.count != cap {
		t.Errorf("expected count=%d after overflow, got %d", cap, w.count)
	}
}

// TestRecordHeartbeatResetsState verifies RecordHeartbeat resets suspect state.
func TestRecordHeartbeatResetsState(t *testing.T) {
	cfg := DefaultConfig()
	hs := NewHeartbeatService(cfg, "self",
		func() []string { return []string{"peer"} },
		func(string) error { return nil },
		nil,
		nil,
	)

	// Manually put peer into suspect state.
	hs.mu.Lock()
	e := &peerEntry{}
	e.state.Store(int32(peerSuspect))
	hs.states["peer"] = e
	hs.mu.Unlock()

	// Receive a heartbeat — should reset to alive.
	hs.RecordHeartbeat("peer")

	hs.mu.RLock()
	state := peerStateVal(hs.states["peer"].state.Load())
	hs.mu.RUnlock()

	if state != peerAlive {
		t.Errorf("expected peerAlive after heartbeat, got %d", state)
	}
}

// TestOnDeadCallbackFires verifies the onDead callback is invoked.
func TestOnDeadCallbackFires(t *testing.T) {
	cfg := Config{
		HeartbeatInterval: 50 * time.Millisecond,
		SuspectThreshold:  0.001, // near-zero threshold so first silence triggers suspect
		DeadTimeout:       100 * time.Millisecond,
		WindowSize:        10,
	}

	deadCh := make(chan string, 1)
	hs := NewHeartbeatService(cfg, "self",
		func() []string { return []string{"peer"} },
		func(string) error { return nil },
		nil,
		func(addr string) { deadCh <- addr },
	)

	// Feed enough heartbeats to build a window.
	now := time.Now()
	for i := 0; i < 15; i++ {
		hs.detector.RecordHeartbeat("peer", now)
		now = now.Add(50 * time.Millisecond)
	}

	// Start the service — reaper will fire shortly.
	hs.Start()
	defer hs.Stop()

	select {
	case addr := <-deadCh:
		if addr != "peer" {
			t.Errorf("expected dead addr=peer, got %s", addr)
		}
	case <-time.After(3 * time.Second):
		t.Error("onDead callback never fired")
	}
}

// TestIsAlive checks the IsAlive convenience method.
func TestIsAlive(t *testing.T) {
	d := NewPhiAccrualDetector(DefaultConfig())
	addr := "p"

	// Unknown peer — phi=0, considered alive.
	if !d.IsAlive(addr) {
		t.Error("unknown peer should be considered alive")
	}

	// After regular heartbeats it should still be alive.
	now := time.Now()
	for i := 0; i < 20; i++ {
		d.RecordHeartbeat(addr, now)
		now = now.Add(time.Second)
	}
	if !d.IsAlive(addr) {
		t.Error("peer should be alive immediately after heartbeats")
	}
}

// BenchmarkPhiComputation measures phi on a full 200-sample window.
func BenchmarkPhiComputation(b *testing.B) {
	w := newPeerWindow(200)
	now := time.Now()
	for i := 0; i < 200; i++ {
		w.record(now)
		now = now.Add(time.Second)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = w.phi(now.Add(time.Duration(i) * time.Millisecond))
	}
}
