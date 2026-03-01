package e2e_test

import (
	"fmt"
	"io"
	"sync"
	"testing"
	"time"
)

// TestVectorClockIncrementedOnWrite verifies that after storing a file the
// metadata vector clock entry for the writing node is non-zero.
func TestVectorClockIncrementedOnWrite(t *testing.T) {
	c := newCluster(t, 3, 3)

	key := "vc-incr-key"
	data := makePayload("text", 64*1024)

	if err := c.nodes[0].StoreData(key, readerOf(data)); err != nil {
		t.Fatalf("StoreData: %v", err)
	}

	// Allow quorum write + metadata propagation.
	time.Sleep(400 * time.Millisecond)

	vc, ts, ok := c.nodes[0].InspectMeta(key)
	if !ok {
		t.Fatalf("InspectMeta: key not found on node[0] after write")
	}
	if ts == 0 {
		t.Errorf("Timestamp should be non-zero after write")
	}
	if len(vc) == 0 {
		t.Errorf("VectorClock should be non-empty after write")
	}
	// At least one node's clock entry must be >= 1.
	total := uint64(0)
	for _, v := range vc {
		total += v
	}
	if total == 0 {
		t.Errorf("VectorClock sum is 0; expected at least one increment")
	}
}

// TestSecondWriteHasHigherClock verifies that writing the same key twice
// produces a strictly increasing vector clock (sum of entries grows).
func TestSecondWriteHasHigherClock(t *testing.T) {
	c := newCluster(t, 3, 3)

	key := "vc-two-writes-key"
	data1 := makePayload("text", 32*1024)
	data2 := makePayload("text", 48*1024)

	if err := c.nodes[0].StoreData(key, readerOf(data1)); err != nil {
		t.Fatalf("first StoreData: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	vc1, ts1, ok := c.nodes[0].InspectMeta(key)
	if !ok {
		t.Fatalf("InspectMeta after first write: not found")
	}

	if err := c.nodes[0].StoreData(key, readerOf(data2)); err != nil {
		t.Fatalf("second StoreData: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	vc2, ts2, ok := c.nodes[0].InspectMeta(key)
	if !ok {
		t.Fatalf("InspectMeta after second write: not found")
	}

	// Timestamp should advance.
	if ts2 <= ts1 {
		t.Errorf("second write timestamp (%d) should be > first (%d)", ts2, ts1)
	}

	// VectorClock sum should be strictly larger.
	sum := func(vc map[string]uint64) uint64 {
		var s uint64
		for _, v := range vc {
			s += v
		}
		return s
	}
	if sum(vc2) <= sum(vc1) {
		t.Errorf("second write vclock sum (%d) should be > first (%d)", sum(vc2), sum(vc1))
	}
}

// TestConcurrentWritesConverge writes the same key from node[0] and node[1]
// concurrently then verifies each node returns one of the two valid payloads
// (no corruption). Full cross-node LWW convergence requires anti-entropy which
// operates on a longer timescale; this test only checks integrity.
func TestConcurrentWritesConverge(t *testing.T) {
	c := newCluster(t, 3, 3)

	key := "concurrent-key"
	data0 := makePayload("text", 50*1024)
	data1 := makePayload("html", 60*1024)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = c.nodes[0].StoreData(key, readerOf(data0))
	}()
	go func() {
		defer wg.Done()
		_ = c.nodes[1].StoreData(key, readerOf(data1))
	}()
	wg.Wait()

	// Allow replication to settle.
	time.Sleep(800 * time.Millisecond)

	h0, h1 := sha256hex(data0), sha256hex(data1)
	valid := map[string]bool{h0: true, h1: true}

	// Each node that has the manifest must return exactly one of the two payloads.
	for i, nd := range c.nodes {
		got, err := nd.GetData(key)
		if err != nil {
			// Node may not have received the manifest yet — acceptable.
			t.Logf("node[%d] GetData skipped: %v", i, err)
			continue
		}
		b, err := io.ReadAll(got)
		if err != nil {
			t.Errorf("node[%d] ReadAll: %v", i, err)
			continue
		}
		h := sha256hex(b)
		if !valid[h] {
			t.Errorf("node[%d] returned corrupt data (hash=%s, want one of %s or %s)", i, h, h0, h1)
		}
	}
}

// TestLWWPicksHigherTimestamp writes two payloads to the same key via the same
// node back-to-back. The second write must win since its wall-clock timestamp
// is strictly higher.
func TestLWWPicksHigherTimestamp(t *testing.T) {
	c := newCluster(t, 3, 3)

	key := "lww-ts-key"
	first := makePayload("text", 32*1024)
	second := makePayload("csv", 32*1024)

	if err := c.nodes[0].StoreData(key, readerOf(first)); err != nil {
		t.Fatalf("first write: %v", err)
	}
	// Small sleep ensures the system clock has advanced.
	time.Sleep(10 * time.Millisecond)

	if err := c.nodes[0].StoreData(key, readerOf(second)); err != nil {
		t.Fatalf("second write: %v", err)
	}
	time.Sleep(400 * time.Millisecond)

	// Retrieve from all nodes — each should return the second payload.
	wantHash := sha256hex(second)
	for i, nd := range c.nodes {
		got := mustGetData(t, nd, key)
		if sha256hex(got) != wantHash {
			t.Errorf("node[%d]: got hash %s, want %s (second write should win by LWW)",
				i, sha256hex(got), wantHash)
		}
	}
}

// TestVectorClockIndependentKeys verifies that VClocks for different keys
// are tracked independently (writing key A does not affect key B's clock).
func TestVectorClockIndependentKeys(t *testing.T) {
	c := newCluster(t, 3, 3)

	keyA := "vc-ind-key-a"
	keyB := "vc-ind-key-b"
	dataA := makePayload("text", 32*1024)
	dataB := makePayload("text", 32*1024)

	if err := c.nodes[0].StoreData(keyA, readerOf(dataA)); err != nil {
		t.Fatalf("StoreData(A): %v", err)
	}
	if err := c.nodes[0].StoreData(keyB, readerOf(dataB)); err != nil {
		t.Fatalf("StoreData(B): %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	vcA, _, okA := c.nodes[0].InspectMeta(keyA)
	vcB, _, okB := c.nodes[0].InspectMeta(keyB)
	if !okA || !okB {
		t.Fatalf("InspectMeta: okA=%v okB=%v", okA, okB)
	}

	// The two clocks are independent; writing A multiple times should not
	// change B's clock.
	for i := 0; i < 3; i++ {
		_ = c.nodes[0].StoreData(keyA, readerOf(dataA))
	}
	time.Sleep(200 * time.Millisecond)

	_, _, _ = vcA, vcB, fmt.Sprintf("%v", vcA) // suppress unused warning

	vcBAfter, _, okBAfter := c.nodes[0].InspectMeta(keyB)
	if !okBAfter {
		t.Fatal("InspectMeta(keyB) after writes to keyA: not found")
	}

	sumBefore := func(vc map[string]uint64) uint64 {
		var s uint64
		for _, v := range vc {
			s += v
		}
		return s
	}
	if sumBefore(vcBAfter) != sumBefore(vcB) {
		t.Errorf("keyB vclock changed after writing to keyA: before=%v after=%v", vcB, vcBAfter)
	}
}
