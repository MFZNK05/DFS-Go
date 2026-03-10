package ratelimit

import (
	"bytes"
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// mockRTTSource returns a configurable SmoothedRTT value.
type mockRTTSource struct {
	rtt atomic.Int64 // stored as nanoseconds
}

func (m *mockRTTSource) SmoothedRTT() time.Duration {
	return time.Duration(m.rtt.Load())
}

func (m *mockRTTSource) setRTT(d time.Duration) {
	m.rtt.Store(int64(d))
}

func TestClampRate(t *testing.T) {
	tests := []struct {
		name string
		in   rate.Limit
		want rate.Limit
	}{
		{"below min", rate.Limit(100), rate.Limit(minRate)},
		{"at min", rate.Limit(minRate), rate.Limit(minRate)},
		{"normal", rate.Limit(50 << 20), rate.Limit(50 << 20)},
		{"at max", rate.Limit(maxRate), rate.Limit(maxRate)},
		{"above max", rate.Limit(300 << 20), rate.Limit(maxRate)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := clampRate(tt.in)
			if got != tt.want {
				t.Errorf("clampRate(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestTransferSemaphore(t *testing.T) {
	bm := New(Config{MaxTransfers: 2})

	// Should acquire 2 immediately.
	if err := bm.AcquireTransfer(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := bm.AcquireTransfer(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Third should block — use a timeout context.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := bm.AcquireTransfer(ctx); err == nil {
		t.Fatal("expected timeout, got nil")
	}

	// Release one, then acquire should succeed.
	bm.ReleaseTransfer()
	if err := bm.AcquireTransfer(context.Background()); err != nil {
		t.Fatal(err)
	}

	bm.ReleaseTransfer()
	bm.ReleaseTransfer()
}

func TestTransferSemaphoreUnlimited(t *testing.T) {
	bm := New(Config{MaxTransfers: 0})

	// Should never block.
	for i := 0; i < 100; i++ {
		if err := bm.AcquireTransfer(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	// ReleaseTransfer should be safe to call.
	bm.ReleaseTransfer()
}

func TestWrapWriter(t *testing.T) {
	bm := New(Config{})
	// Set a known rate so we can verify wrapping works.
	bm.uploadLimiter = rate.NewLimiter(rate.Limit(1<<30), 1<<20) // fast enough for test

	var buf bytes.Buffer
	w := bm.WrapWriter(&buf)
	data := []byte("hello world")
	n, err := w.Write(data)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(data) {
		t.Fatalf("wrote %d, want %d", n, len(data))
	}
	if buf.String() != "hello world" {
		t.Fatalf("got %q", buf.String())
	}
}

func TestWrapReader(t *testing.T) {
	bm := New(Config{})
	bm.downloadLimiter = rate.NewLimiter(rate.Limit(1<<30), 1<<20)

	src := bytes.NewReader([]byte("hello world"))
	r := bm.WrapReader(src)
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello world" {
		t.Fatalf("got %q", string(data))
	}
}

func TestRTTMonitorSlashesRate(t *testing.T) {
	bm := New(Config{})
	src := &mockRTTSource{}
	src.setRTT(50 * time.Millisecond) // above rttHigh

	initialUp := bm.uploadLimiter.Limit()

	bm.RegisterPeer("test-peer", src)
	// Wait for at least one probe cycle.
	time.Sleep(3 * time.Second)

	newUp := bm.uploadLimiter.Limit()
	if newUp >= initialUp {
		t.Errorf("expected rate to decrease from %v, got %v", initialUp, newUp)
	}

	bm.UnregisterPeer("test-peer")
}

func TestRTTMonitorGrowsRate(t *testing.T) {
	bm := New(Config{})
	// Start at a low rate so growth is visible.
	bm.uploadLimiter.SetLimit(rate.Limit(10 << 20)) // 10 MB/s
	bm.downloadLimiter.SetLimit(rate.Limit(10 << 20))

	src := &mockRTTSource{}
	src.setRTT(1 * time.Millisecond) // below rttLow

	initialUp := bm.uploadLimiter.Limit()

	bm.RegisterPeer("test-peer", src)
	time.Sleep(3 * time.Second)

	newUp := bm.uploadLimiter.Limit()
	if newUp <= initialUp {
		t.Errorf("expected rate to increase from %v, got %v", initialUp, newUp)
	}

	bm.UnregisterPeer("test-peer")
}

func TestStop(t *testing.T) {
	bm := New(Config{})
	src := &mockRTTSource{}
	src.setRTT(1 * time.Millisecond)
	bm.RegisterPeer("a", src)
	bm.RegisterPeer("b", src)

	bm.Stop()

	bm.mu.Lock()
	count := len(bm.monitors)
	bm.mu.Unlock()
	if count != 0 {
		t.Errorf("expected 0 monitors after Stop, got %d", count)
	}
}
