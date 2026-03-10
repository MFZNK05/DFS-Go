// Package ratelimit provides invisible, automatic bandwidth management using
// LEDBAT-lite adaptive throttling. It monitors QUIC SmoothedRTT per peer
// connection and dynamically adjusts shared upload/download rate limiters.
//
// Users never configure rate limits — the system automatically yields to
// interactive traffic by backing off when RTT rises (bufferbloat) and
// increasing throughput when RTT is low (network clear).
//
// The only user-facing knob is MaxTransfers (concurrent transfer semaphore).
package ratelimit

import (
	"context"
	"io"
	"log"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// RTT thresholds for adaptive rate adjustment.
const (
	rttLow  = 5 * time.Millisecond  // below this: network clear, grow rate
	rttHigh = 25 * time.Millisecond  // above this: bufferbloat, slash rate

	initialRate = 50 << 20  // 50 MB/s starting point
	minRate     = 1 << 20   // 1 MB/s floor
	maxRate     = 200 << 20 // 200 MB/s ceiling

	burstSize     = 64 << 10 // 64 KiB burst (above 32 KiB io.CopyBuffer slice)
	probeInterval = 2 * time.Second
)

// RTTSource abstracts a QUIC connection's stats for RTT monitoring.
// This avoids a circular dependency on the quic transport package.
type RTTSource interface {
	SmoothedRTT() time.Duration
}

// Config holds bandwidth manager configuration.
type Config struct {
	MaxTransfers int // 0 = unlimited; only user-facing knob
}

// BandwidthManager provides adaptive rate limiting and transfer semaphore.
type BandwidthManager struct {
	uploadLimiter   *rate.Limiter
	downloadLimiter *rate.Limiter
	transferSem     chan struct{} // nil if MaxTransfers == 0

	mu       sync.Mutex
	monitors map[string]context.CancelFunc
	stopCh   chan struct{}
}

// New creates a BandwidthManager. Always created — adaptive throttling is
// always-on. Transfer semaphore is only active when cfg.MaxTransfers > 0.
func New(cfg Config) *BandwidthManager {
	bm := &BandwidthManager{
		uploadLimiter:   rate.NewLimiter(rate.Limit(initialRate), burstSize),
		downloadLimiter: rate.NewLimiter(rate.Limit(initialRate), burstSize),
		monitors:        make(map[string]context.CancelFunc),
		stopCh:          make(chan struct{}),
	}
	if cfg.MaxTransfers > 0 {
		bm.transferSem = make(chan struct{}, cfg.MaxTransfers)
	}
	return bm
}

// RegisterPeer starts an RTT monitor goroutine for the given peer.
func (bm *BandwidthManager) RegisterPeer(addr string, src RTTSource) {
	bm.mu.Lock()
	defer bm.mu.Unlock()

	// Cancel existing monitor if any (reconnect scenario).
	if cancel, ok := bm.monitors[addr]; ok {
		cancel()
	}

	ctx, cancel := context.WithCancel(context.Background())
	bm.monitors[addr] = cancel
	go bm.monitorRTT(ctx, addr, src)
}

// UnregisterPeer stops the RTT monitor for the given peer.
func (bm *BandwidthManager) UnregisterPeer(addr string) {
	bm.mu.Lock()
	defer bm.mu.Unlock()
	if cancel, ok := bm.monitors[addr]; ok {
		cancel()
		delete(bm.monitors, addr)
	}
}

// Stop cancels all RTT monitors.
func (bm *BandwidthManager) Stop() {
	bm.mu.Lock()
	defer bm.mu.Unlock()
	for addr, cancel := range bm.monitors {
		cancel()
		delete(bm.monitors, addr)
	}
	select {
	case <-bm.stopCh:
	default:
		close(bm.stopCh)
	}
}

// WrapWriter returns an io.Writer that throttles writes through the upload
// limiter. Writes are blocked until tokens are available, causing QUIC
// flow-control backpressure on the remote peer.
func (bm *BandwidthManager) WrapWriter(w io.Writer) io.Writer {
	return &rateLimitedWriter{writer: w, limiter: bm.uploadLimiter}
}

// WrapReader returns an io.Reader that throttles reads through the download
// limiter. Slow reads cause the QUIC receive buffer to fill, which stops
// MAX_STREAM_DATA frames, physically throttling the remote sender.
func (bm *BandwidthManager) WrapReader(r io.Reader) io.Reader {
	return &rateLimitedReader{reader: r, limiter: bm.downloadLimiter}
}

// AcquireTransfer blocks until a transfer slot is available. Returns nil
// immediately if no semaphore is configured (MaxTransfers == 0).
func (bm *BandwidthManager) AcquireTransfer(ctx context.Context) error {
	if bm.transferSem == nil {
		return nil
	}
	select {
	case bm.transferSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ReleaseTransfer returns a transfer slot to the semaphore.
func (bm *BandwidthManager) ReleaseTransfer() {
	if bm.transferSem == nil {
		return
	}
	<-bm.transferSem
}

// monitorRTT samples SmoothedRTT every probeInterval and adjusts the shared
// rate limiters. Multiple monitors (one per peer) share the same limiters —
// the worst-congested link drives the rate down (conservative, correct).
func (bm *BandwidthManager) monitorRTT(ctx context.Context, addr string, src RTTSource) {
	ticker := time.NewTicker(probeInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-bm.stopCh:
			return
		case <-ticker.C:
			rtt := src.SmoothedRTT()
			if rtt == 0 {
				continue // no samples yet
			}

			bm.mu.Lock()
			currentUp := float64(bm.uploadLimiter.Limit())
			currentDown := float64(bm.downloadLimiter.Limit())

			switch {
			case rtt < rttLow:
				// Network clear — grow 20%
				bm.uploadLimiter.SetLimit(clampRate(rate.Limit(currentUp * 1.2)))
				bm.downloadLimiter.SetLimit(clampRate(rate.Limit(currentDown * 1.2)))
			case rtt > rttHigh:
				// Bufferbloat — slash 40%
				newUp := clampRate(rate.Limit(currentUp * 0.6))
				newDown := clampRate(rate.Limit(currentDown * 0.6))
				bm.uploadLimiter.SetLimit(newUp)
				bm.downloadLimiter.SetLimit(newDown)
				log.Printf("[ratelimit] RTT %v from %s — throttling to upload=%.0f B/s download=%.0f B/s",
					rtt, addr, float64(newUp), float64(newDown))
			}
			bm.mu.Unlock()
		}
	}
}

func clampRate(r rate.Limit) rate.Limit {
	if r < rate.Limit(minRate) {
		return rate.Limit(minRate)
	}
	if r > rate.Limit(maxRate) {
		return rate.Limit(maxRate)
	}
	return r
}

// rateLimitedWriter throttles writes through a token bucket limiter.
// Blocks before writing until tokens are available.
type rateLimitedWriter struct {
	writer  io.Writer
	limiter *rate.Limiter
}

func (w *rateLimitedWriter) Write(p []byte) (int, error) {
	burst := w.limiter.Burst()
	written := 0
	for written < len(p) {
		chunk := len(p) - written
		if chunk > burst {
			chunk = burst
		}
		if err := w.limiter.WaitN(context.Background(), chunk); err != nil {
			return written, err
		}
		n, err := w.writer.Write(p[written : written+chunk])
		written += n
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

// rateLimitedReader throttles reads through a token bucket limiter.
// Drains tokens after reading to match actual bytes received from wire.
type rateLimitedReader struct {
	reader  io.Reader
	limiter *rate.Limiter
}

func (r *rateLimitedReader) Read(p []byte) (int, error) {
	// Limit the read size to the limiter's burst to avoid WaitN exceeding burst.
	burst := r.limiter.Burst()
	if len(p) > burst {
		p = p[:burst]
	}
	n, err := r.reader.Read(p)
	if n > 0 {
		if waitErr := r.limiter.WaitN(context.Background(), n); waitErr != nil {
			return n, waitErr
		}
	}
	return n, err
}
