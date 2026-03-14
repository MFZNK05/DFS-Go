package failure

import (
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultConfig returns a production-ready failure detection configuration.
const (
	DefaultHeartbeatInterval = 5 * time.Second
	DefaultSuspectThreshold  = 8.0
	DefaultDeadTimeout       = 30 * time.Second
	DefaultWindowSize        = 200
)

// Config controls the behaviour of the failure detector.
type Config struct {
	HeartbeatInterval time.Duration // how often heartbeats are sent (default 1s)
	SuspectThreshold  float64       // phi value at which a peer is suspected (default 8.0)
	DeadTimeout       time.Duration // how long a suspect stays suspect before declared dead (default 30s)
	WindowSize        int           // samples kept in the sliding window per peer (default 200)
}

// DefaultConfig returns sensible defaults.
func DefaultConfig() Config {
	return Config{
		HeartbeatInterval: DefaultHeartbeatInterval,
		SuspectThreshold:  DefaultSuspectThreshold,
		DeadTimeout:       DefaultDeadTimeout,
		WindowSize:        DefaultWindowSize,
	}
}

// peerWindow is a fixed-capacity ring buffer of inter-arrival intervals (milliseconds).
// It is used by PhiAccrualDetector to compute the phi value for one peer.
type peerWindow struct {
	mu          sync.Mutex
	intervals   []float64 // ring buffer
	head        int       // next write position
	count       int       // number of filled slots (≤ capacity)
	lastArrival time.Time // wall time of last recorded heartbeat
	capacity    int
}

func newPeerWindow(capacity int) *peerWindow {
	return &peerWindow{
		intervals: make([]float64, capacity),
		capacity:  capacity,
	}
}

// record registers a new heartbeat arrival and appends the inter-arrival interval.
func (w *peerWindow) record(now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.lastArrival.IsZero() {
		// First sample — no interval to record yet.
		w.lastArrival = now
		return
	}

	interval := float64(now.Sub(w.lastArrival).Milliseconds())
	if interval < 0 {
		interval = 0
	}

	w.intervals[w.head] = interval
	w.head = (w.head + 1) % w.capacity
	if w.count < w.capacity {
		w.count++
	}
	w.lastArrival = now
}

// phi computes the Phi Accrual value at the given instant.
// Higher phi = higher probability the peer is dead.
// Returns 0 if insufficient samples, 16.0 if certainty of death.
func (w *peerWindow) phi(now time.Time) float64 {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.count < 2 {
		return 0.0
	}

	// Compute mean of recorded intervals.
	var sum float64
	for i := 0; i < w.count; i++ {
		sum += w.intervals[i]
	}
	mean := sum / float64(w.count)

	// Compute standard deviation.
	var variance float64
	for i := 0; i < w.count; i++ {
		d := w.intervals[i] - mean
		variance += d * d
	}
	variance /= float64(w.count)
	stddev := math.Sqrt(variance)

	// Guard against degenerate case (perfectly regular heartbeats).
	if stddev < mean*0.1 {
		stddev = mean * 0.1
	}
	if stddev < 1.0 {
		stddev = 1.0
	}

	// Time elapsed since last heartbeat.
	elapsed := float64(now.Sub(w.lastArrival).Milliseconds())

	// Normalised distance from the mean.
	y := (elapsed - mean) / (stddev * math.Sqrt2)

	// CDF of the normal distribution using erf.
	cdf := 0.5 * (1.0 + math.Erf(y))
	if cdf >= 1.0 {
		return 16.0 // hard cap — certain death
	}
	if cdf <= 0.0 {
		return 0.0
	}

	return -math.Log10(1.0 - cdf)
}

// PhiAccrualDetector tracks per-peer arrival windows and exposes phi values.
type PhiAccrualDetector struct {
	mu      sync.RWMutex
	windows map[string]*peerWindow
	cfg     Config
}

// NewPhiAccrualDetector creates a detector with the given configuration.
func NewPhiAccrualDetector(cfg Config) *PhiAccrualDetector {
	return &PhiAccrualDetector{
		windows: make(map[string]*peerWindow),
		cfg:     cfg,
	}
}

// RecordHeartbeat records a heartbeat arrival from addr at the given time.
func (d *PhiAccrualDetector) RecordHeartbeat(addr string, arrivedAt time.Time) {
	d.mu.Lock()
	w, ok := d.windows[addr]
	if !ok {
		w = newPeerWindow(d.cfg.WindowSize)
		d.windows[addr] = w
	}
	d.mu.Unlock()

	w.record(arrivedAt)
}

// Phi returns the current phi value for addr.
// A value ≥ SuspectThreshold indicates the peer is likely dead.
func (d *PhiAccrualDetector) Phi(addr string) float64 {
	d.mu.RLock()
	w, ok := d.windows[addr]
	d.mu.RUnlock()

	if !ok {
		return 0.0 // never seen — not suspicious yet
	}
	return w.phi(time.Now())
}

// IsAlive returns true if phi < SuspectThreshold.
func (d *PhiAccrualDetector) IsAlive(addr string) bool {
	return d.Phi(addr) < d.cfg.SuspectThreshold
}

// Remove deletes all state for addr (called when peer is explicitly removed).
func (d *PhiAccrualDetector) Remove(addr string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.windows, addr)
}

// -------------------------------------------------------------------
// peerState tracks the lifecycle state of a monitored peer.
// -------------------------------------------------------------------

type peerStateVal int32

const (
	peerAlive   peerStateVal = 0
	peerSuspect peerStateVal = 1
	peerDead    peerStateVal = 2
)

type peerEntry struct {
	state        atomic.Int32 // peerStateVal stored atomically
	suspectSince time.Time
	mu           sync.Mutex // protects suspectSince
}

// HeartbeatService sends periodic heartbeats to all peers and drives the
// phi accrual reaper that fires onSuspect / onDead callbacks.
type HeartbeatService struct {
	cfg      Config
	selfAddr string
	detector *PhiAccrualDetector

	// getPeers returns the current set of connected peer addresses.
	// It is a closure over the server's peers map — called each tick.
	getPeers func() []string

	// sendHeartbeat sends a heartbeat message to the given peer address.
	// Implemented as a closure in server.go to avoid circular imports.
	sendHeartbeat func(addr string) error

	// Callbacks — run in a goroutine so they must not block.
	onSuspect func(addr string)
	onDead    func(addr string)

	mu     sync.RWMutex
	states map[string]*peerEntry

	stopCh   chan struct{}
	once     sync.Once
	stopOnce sync.Once
}

// NewHeartbeatService creates a new HeartbeatService.
// getPeers, sendHeartbeat, onSuspect, onDead are wired by server.go.
func NewHeartbeatService(
	cfg Config,
	selfAddr string,
	getPeers func() []string,
	sendHeartbeat func(addr string) error,
	onSuspect func(addr string),
	onDead func(addr string),
) *HeartbeatService {
	return &HeartbeatService{
		cfg:           cfg,
		selfAddr:      selfAddr,
		detector:      NewPhiAccrualDetector(cfg),
		getPeers:      getPeers,
		sendHeartbeat: sendHeartbeat,
		onSuspect:     onSuspect,
		onDead:        onDead,
		states:        make(map[string]*peerEntry),
		stopCh:        make(chan struct{}),
	}
}

// Start launches the sender and reaper goroutines. Safe to call only once.
func (hs *HeartbeatService) Start() {
	hs.once.Do(func() {
		go hs.senderLoop()
		go hs.reaperLoop()
	})
}

// Stop shuts down both goroutines. Safe to call multiple times.
func (hs *HeartbeatService) Stop() {
	hs.stopOnce.Do(func() { close(hs.stopCh) })
}

// RecordHeartbeat must be called whenever a heartbeat arrives from addr.
// It feeds the phi window and resets the peer to Alive if it was Suspect.
func (hs *HeartbeatService) RecordHeartbeat(from string) {
	hs.detector.RecordHeartbeat(from, time.Now())

	hs.mu.Lock()
	e, ok := hs.states[from]
	if !ok {
		e = &peerEntry{}
		hs.states[from] = e
	}
	hs.mu.Unlock()

	old := peerStateVal(e.state.Swap(int32(peerAlive)))
	if old == peerSuspect || old == peerDead {
		// Peer has recovered.
		_ = old // recovery logged by server via onSuspect / onDead inverse
	}
}

// RemovePeer cleans up state for a peer that has been explicitly disconnected.
func (hs *HeartbeatService) RemovePeer(addr string) {
	hs.detector.Remove(addr)
	hs.mu.Lock()
	delete(hs.states, addr)
	hs.mu.Unlock()
}

// senderLoop sends a heartbeat to every known peer on each tick.
func (hs *HeartbeatService) senderLoop() {
	ticker := time.NewTicker(hs.cfg.HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			for _, addr := range hs.getPeers() {
				if addr == hs.selfAddr {
					continue
				}
				// Fire-and-forget: errors are expected when peers are down.
				go func(a string) { _ = hs.sendHeartbeat(a) }(addr)
			}
		case <-hs.stopCh:
			return
		}
	}
}

// reaperLoop checks phi values at double the heartbeat frequency and fires
// state-transition callbacks when peers cross the suspect/dead thresholds.
func (hs *HeartbeatService) reaperLoop() {
	ticker := time.NewTicker(hs.cfg.HeartbeatInterval / 2)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			hs.reap()
		case <-hs.stopCh:
			return
		}
	}
}

func (hs *HeartbeatService) reap() {
	peers := hs.getPeers()

	for _, addr := range peers {
		if addr == hs.selfAddr {
			continue
		}

		phi := hs.detector.Phi(addr)

		hs.mu.Lock()
		e, ok := hs.states[addr]
		if !ok {
			e = &peerEntry{}
			hs.states[addr] = e
		}
		hs.mu.Unlock()

		current := peerStateVal(e.state.Load())

		switch {
		case phi >= hs.cfg.SuspectThreshold && current == peerAlive:
			// Alive → Suspect
			if e.state.CompareAndSwap(int32(peerAlive), int32(peerSuspect)) {
				e.mu.Lock()
				e.suspectSince = time.Now()
				e.mu.Unlock()
				if hs.onSuspect != nil {
					go hs.onSuspect(addr)
				}
			}

		case current == peerSuspect:
			e.mu.Lock()
			since := e.suspectSince
			e.mu.Unlock()

			if time.Since(since) >= hs.cfg.DeadTimeout {
				// Suspect → Dead
				if e.state.CompareAndSwap(int32(peerSuspect), int32(peerDead)) {
					if hs.onDead != nil {
						go hs.onDead(addr)
					}
				}
			}
		}
	}
}
