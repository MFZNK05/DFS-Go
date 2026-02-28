// Package selector tracks per-peer latency using an EWMA and picks the best
// peer for a given chunk download.
//
// Scoring:
//
//	score(peer) = latencyEWMA × (1 + 0.5 × activeDownloads)
//
// A peer with lower latency wins, but a currently-busy peer is penalised so
// load spreads across the cluster.
package selector

import (
	"math"
	"sync"
	"time"
)

const (
	// alpha is the EWMA smoothing factor (0 < α ≤ 1).
	// 0.2 means each new sample contributes 20% of the new value.
	alpha = 0.2

	// initialLatency is used when no sample has been recorded yet.
	// A high value ensures untested peers are tried before known-slow ones.
	initialLatency = 50 * time.Millisecond

	// busyPenaltyFactor is multiplied by activeDownloads in the score formula.
	busyPenaltyFactor = 0.5
)

// peerState holds the EWMA latency and active-download counter for one peer.
type peerState struct {
	ewma           float64 // nanoseconds
	activeDownloads int
}

// Selector maintains latency state for all known peers and picks the best one.
type Selector struct {
	mu    sync.Mutex
	peers map[string]*peerState
}

// New returns an initialised Selector.
func New() *Selector {
	return &Selector{peers: make(map[string]*peerState)}
}

// RecordLatency updates the EWMA for addr with the observed round-trip time.
// Safe to call from multiple goroutines.
func (s *Selector) RecordLatency(addr string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	st := s.getOrCreate(addr)
	sample := float64(d.Nanoseconds())
	st.ewma = alpha*sample + (1-alpha)*st.ewma
}

// BeginDownload increments the active-download counter for addr.
// Call this just before starting a fetch; pair with EndDownload.
func (s *Selector) BeginDownload(addr string) {
	s.mu.Lock()
	s.getOrCreate(addr).activeDownloads++
	s.mu.Unlock()
}

// EndDownload decrements the active-download counter for addr.
func (s *Selector) EndDownload(addr string) {
	s.mu.Lock()
	st := s.getOrCreate(addr)
	if st.activeDownloads > 0 {
		st.activeDownloads--
	}
	s.mu.Unlock()
}

// BestPeer returns the candidate address with the lowest score.
// Returns ("", false) when candidates is empty.
func (s *Selector) BestPeer(candidates []string) (string, bool) {
	if len(candidates) == 0 {
		return "", false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	best := ""
	bestScore := math.MaxFloat64

	for _, addr := range candidates {
		sc := s.score(addr)
		if sc < bestScore {
			bestScore = sc
			best = addr
		}
	}
	return best, best != ""
}

// Latency returns the current EWMA latency estimate for addr.
// Returns initialLatency if no samples have been recorded.
func (s *Selector) Latency(addr string) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Duration(s.getOrCreate(addr).ewma)
}

// ActiveDownloads returns the number of in-flight downloads to addr.
func (s *Selector) ActiveDownloads(addr string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getOrCreate(addr).activeDownloads
}

// score returns the routing score for addr (lower = better).
// Must be called with s.mu held.
func (s *Selector) score(addr string) float64 {
	st := s.getOrCreate(addr)
	penalty := 1.0 + busyPenaltyFactor*float64(st.activeDownloads)
	return st.ewma * penalty
}

// getOrCreate returns the peerState for addr, creating it with initialLatency
// if it does not exist. Must be called with s.mu held.
func (s *Selector) getOrCreate(addr string) *peerState {
	st, ok := s.peers[addr]
	if !ok {
		st = &peerState{ewma: float64(initialLatency.Nanoseconds())}
		s.peers[addr] = st
	}
	return st
}
