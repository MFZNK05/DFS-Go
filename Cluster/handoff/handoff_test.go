package handoff

import (
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "handoff-test-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(tempDir(t), 10, time.Hour)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

// TestAddAndRetrieveHint verifies basic add/get round-trip.
func TestAddAndRetrieveHint(t *testing.T) {
	s := newTestStore(t)

	h := Hint{Key: "file1", TargetAddr: "peer:3000", EncryptedKey: "aabbcc", Data: []byte("encrypted"), CreatedAt: time.Now()}
	if err := s.AddHint(h); err != nil {
		t.Fatalf("AddHint: %v", err)
	}

	hints := s.GetHints("peer:3000")
	if len(hints) != 1 {
		t.Fatalf("expected 1 hint, got %d", len(hints))
	}
	if hints[0].Key != "file1" {
		t.Errorf("wrong key: %s", hints[0].Key)
	}
}

// TestHintCapCausesEviction verifies oldest hint is dropped when cap is reached.
func TestHintCapCausesEviction(t *testing.T) {
	s, err := NewStore(tempDir(t), 3, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	addr := "peer:3001"
	for i := 0; i < 4; i++ {
		h := Hint{Key: string(rune('A' + i)), TargetAddr: addr, CreatedAt: time.Now()}
		_ = s.AddHint(h)
	}

	hints := s.GetHints(addr)
	if len(hints) != 3 {
		t.Fatalf("expected 3 hints after eviction, got %d", len(hints))
	}
	// First hint (key "A") should have been evicted.
	if hints[0].Key == "A" {
		t.Error("oldest hint 'A' should have been evicted")
	}
}

// TestExpiredHintsRemoved verifies PurgeExpired drops old hints.
func TestExpiredHintsRemoved(t *testing.T) {
	s, err := NewStore(tempDir(t), 100, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}

	addr := "peer:3002"
	h := Hint{Key: "old", TargetAddr: addr, CreatedAt: time.Now().Add(-200 * time.Millisecond)}
	_ = s.AddHint(h)

	s.PurgeExpired()

	if len(s.GetHints(addr)) != 0 {
		t.Error("expired hint should have been removed")
	}
}

// TestDeleteHint verifies DeleteHint removes exactly the specified hint.
func TestDeleteHint(t *testing.T) {
	s := newTestStore(t)
	addr := "peer:3003"

	for _, k := range []string{"k1", "k2", "k3"} {
		_ = s.AddHint(Hint{Key: k, TargetAddr: addr, CreatedAt: time.Now()})
	}

	if err := s.DeleteHint(addr, "k2"); err != nil {
		t.Fatalf("DeleteHint: %v", err)
	}

	hints := s.GetHints(addr)
	if len(hints) != 2 {
		t.Fatalf("expected 2 hints after delete, got %d", len(hints))
	}
	for _, h := range hints {
		if h.Key == "k2" {
			t.Error("k2 should have been deleted")
		}
	}
}

// TestPersistAndReload verifies hints survive a store restart.
func TestPersistAndReload(t *testing.T) {
	dir := tempDir(t)

	s1, err := NewStore(dir, 100, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	addr := "peer:3004"
	_ = s1.AddHint(Hint{Key: "persistent", TargetAddr: addr, EncryptedKey: "deadbeef", Data: []byte("data"), CreatedAt: time.Now()})

	// Reload from same dir.
	s2, err := NewStore(dir, 100, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	hints := s2.GetHints(addr)
	if len(hints) != 1 {
		t.Fatalf("expected 1 hint after reload, got %d", len(hints))
	}
	if hints[0].Key != "persistent" {
		t.Errorf("wrong key after reload: %s", hints[0].Key)
	}
}

// TestDeliverOnReconnect verifies delivery is triggered and hint deleted on success.
func TestDeliverOnReconnect(t *testing.T) {
	s := newTestStore(t)
	addr := "peer:3005"
	_ = s.AddHint(Hint{Key: "deliver-me", TargetAddr: addr, CreatedAt: time.Now(), Data: []byte("payload")})

	var delivered atomic.Int32
	svc := NewHandoffService(s, func(h Hint) error {
		delivered.Add(1)
		return nil
	})

	svc.OnPeerReconnect(addr)

	// Give the goroutine time to run.
	time.Sleep(100 * time.Millisecond)

	if delivered.Load() != 1 {
		t.Errorf("expected 1 delivery, got %d", delivered.Load())
	}
	if s.HasPending(addr) {
		t.Error("hint should have been deleted after successful delivery")
	}
}

// TestDeliverRetryOnFailure verifies failed deliveries increment Attempts and are re-stored.
func TestDeliverRetryOnFailure(t *testing.T) {
	s := newTestStore(t)
	addr := "peer:3006"
	_ = s.AddHint(Hint{Key: "fail-me", TargetAddr: addr, CreatedAt: time.Now(), Data: []byte("x")})

	calls := 0
	svc := NewHandoffService(s, func(h Hint) error {
		calls++
		return errors.New("network error")
	})

	// Call deliverPending directly (synchronously for test).
	svc.deliverPending(addr)

	hints := s.GetHints(addr)
	if len(hints) != 1 {
		t.Fatalf("hint should still exist after 1 failure, got %d hints", len(hints))
	}
	if hints[0].Attempts != 1 {
		t.Errorf("expected Attempts=1, got %d", hints[0].Attempts)
	}
}

// TestHintDiscardedAfterMaxAttempts verifies exhausted hints are removed.
func TestHintDiscardedAfterMaxAttempts(t *testing.T) {
	s := newTestStore(t)
	addr := "peer:3007"
	// Pre-set attempts to one below max so next failure discards it.
	h := Hint{Key: "expire", TargetAddr: addr, CreatedAt: time.Now(), Attempts: maxDeliveryAttempts - 1}
	_ = s.AddHint(h)

	svc := NewHandoffService(s, func(hint Hint) error {
		return errors.New("fail")
	})
	svc.deliverPending(addr)

	if s.HasPending(addr) {
		t.Error("hint should be discarded after max attempts")
	}
}

// TestHintFileCreated verifies a .hints.json file is written to disk.
func TestHintFileCreated(t *testing.T) {
	dir := tempDir(t)
	s, _ := NewStore(dir, 10, time.Hour)

	addr := "192.168.1.10:3000"
	_ = s.AddHint(Hint{Key: "k", TargetAddr: addr, CreatedAt: time.Now()})

	expected := filepath.Join(dir, sanitiseAddr(addr)+".hints.json")
	if _, err := os.Stat(expected); os.IsNotExist(err) {
		t.Errorf("hint file not created at %s", expected)
	}
}
