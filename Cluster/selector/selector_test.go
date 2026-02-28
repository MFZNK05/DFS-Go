package selector_test

import (
	"testing"
	"time"

	"github.com/Faizan2005/DFS-Go/Cluster/selector"
)

func TestInitialLatency(t *testing.T) {
	s := selector.New()
	lat := s.Latency("peer1:3000")
	if lat <= 0 {
		t.Errorf("expected positive initial latency, got %v", lat)
	}
}

func TestRecordLatencyDecreases(t *testing.T) {
	s := selector.New()
	// Feed many fast samples — EWMA should converge downwards.
	for i := 0; i < 30; i++ {
		s.RecordLatency("fast:3000", 1*time.Millisecond)
	}
	lat := s.Latency("fast:3000")
	if lat > 10*time.Millisecond {
		t.Errorf("EWMA should have converged to ~1ms, got %v", lat)
	}
}

func TestRecordLatencyIncreases(t *testing.T) {
	s := selector.New()
	// First make peer fast.
	for i := 0; i < 20; i++ {
		s.RecordLatency("peer:3000", 1*time.Millisecond)
	}
	// Then feed slow samples.
	for i := 0; i < 20; i++ {
		s.RecordLatency("peer:3000", 500*time.Millisecond)
	}
	lat := s.Latency("peer:3000")
	if lat < 100*time.Millisecond {
		t.Errorf("EWMA should have risen towards 500ms, got %v", lat)
	}
}

func TestBestPeerPicksFastest(t *testing.T) {
	s := selector.New()

	// Make peer A slow, peer B fast.
	for i := 0; i < 20; i++ {
		s.RecordLatency("slow:3000", 200*time.Millisecond)
		s.RecordLatency("fast:3000", 5*time.Millisecond)
	}

	best, ok := s.BestPeer([]string{"slow:3000", "fast:3000"})
	if !ok {
		t.Fatal("BestPeer returned no result")
	}
	if best != "fast:3000" {
		t.Errorf("expected fast:3000, got %s", best)
	}
}

func TestBestPeerEmptyCandidates(t *testing.T) {
	s := selector.New()
	_, ok := s.BestPeer(nil)
	if ok {
		t.Error("expected BestPeer to return false for empty candidates")
	}
}

func TestBestPeerSingleCandidate(t *testing.T) {
	s := selector.New()
	best, ok := s.BestPeer([]string{"only:3000"})
	if !ok {
		t.Fatal("BestPeer returned no result for single candidate")
	}
	if best != "only:3000" {
		t.Errorf("expected only:3000, got %s", best)
	}
}

func TestBusyPenaltyShiftsChoice(t *testing.T) {
	s := selector.New()

	// Both peers equal latency.
	for i := 0; i < 20; i++ {
		s.RecordLatency("peerA:3000", 10*time.Millisecond)
		s.RecordLatency("peerB:3000", 10*time.Millisecond)
	}

	// Mark peerA as busy with 4 active downloads.
	s.BeginDownload("peerA:3000")
	s.BeginDownload("peerA:3000")
	s.BeginDownload("peerA:3000")
	s.BeginDownload("peerA:3000")

	best, ok := s.BestPeer([]string{"peerA:3000", "peerB:3000"})
	if !ok {
		t.Fatal("no best peer")
	}
	if best != "peerB:3000" {
		t.Errorf("expected peerB (less busy), got %s", best)
	}
}

func TestActiveDownloadsCounter(t *testing.T) {
	s := selector.New()
	s.BeginDownload("p:3000")
	s.BeginDownload("p:3000")
	if n := s.ActiveDownloads("p:3000"); n != 2 {
		t.Errorf("expected 2 active, got %d", n)
	}
	s.EndDownload("p:3000")
	if n := s.ActiveDownloads("p:3000"); n != 1 {
		t.Errorf("expected 1 active after EndDownload, got %d", n)
	}
}

func TestEndDownloadFloorAtZero(t *testing.T) {
	s := selector.New()
	// Call EndDownload without a matching Begin — should not go negative.
	s.EndDownload("p:3000")
	if n := s.ActiveDownloads("p:3000"); n != 0 {
		t.Errorf("expected 0, got %d", n)
	}
}

func TestConcurrentRecordLatency(t *testing.T) {
	s := selector.New()
	done := make(chan struct{})
	for i := 0; i < 50; i++ {
		go func(i int) {
			s.RecordLatency("peer:3000", time.Duration(i)*time.Millisecond)
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < 50; i++ {
		<-done
	}
	// Just verify no panic and a positive latency.
	if s.Latency("peer:3000") <= 0 {
		t.Error("expected positive latency after concurrent writes")
	}
}
