package e2e_test

import (
	"bytes"
	"log"
	"testing"
	"time"
)

// TestUploadLatency_SmallFile measures end-to-end StoreData latency for a
// single-chunk file (<4 MiB) on a 2-node cluster. With the Fix 2 pipeline and
// Fix 3 sync.Pool the round-trip should complete well under 3 s even on a
// slow link; we use a generous 5 s limit to avoid spurious CI failures.
func TestUploadLatency_SmallFile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping latency test in short mode")
	}

	c := newCluster(t, 2, 2)

	// 100 KB of compressible text — fits in a single 4 MiB chunk.
	payload := makePayload("text", 100*1024)
	key := "latency-small"

	start := time.Now()
	if err := c.nodes[0].StoreData(key, bytes.NewReader(payload)); err != nil {
		t.Fatalf("StoreData: %v", err)
	}
	elapsed := time.Since(start)
	log.Printf("TestUploadLatency_SmallFile: StoreData took %v", elapsed)

	const limit = 5 * time.Second
	if elapsed > limit {
		t.Errorf("StoreData took %v, want < %v", elapsed, limit)
	}

	// Verify data integrity on the second node.
	got := mustGetData(t, c.nodes[1], key)
	if !bytes.Equal(got, payload) {
		t.Errorf("roundtrip mismatch: stored %d bytes, got %d bytes (sha256 want=%s got=%s)",
			len(payload), len(got), sha256hex(payload), sha256hex(got))
	}
}

// TestUploadLatency_MultiChunk uploads a file that spans multiple chunks and
// verifies that Stage 1 (compress+encrypt) and Stage 2 (store+replicate) run
// concurrently. We assert on overall elapsed time: a pipelined upload of
// ~9 MiB (≈3 chunks) should finish in under 10 s on loopback.
func TestUploadLatency_MultiChunk(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping latency test in short mode")
	}

	c := newCluster(t, 2, 2)

	// ~9 MiB of compressible text → 3 chunks of ~3 MiB each after compression.
	payload := makePayload("text", 9*1024*1024)
	key := "latency-multi"

	start := time.Now()
	if err := c.nodes[0].StoreData(key, bytes.NewReader(payload)); err != nil {
		t.Fatalf("StoreData: %v", err)
	}
	elapsed := time.Since(start)
	log.Printf("TestUploadLatency_MultiChunk: StoreData took %v", elapsed)

	const limit = 10 * time.Second
	if elapsed > limit {
		t.Errorf("StoreData took %v, want < %v", elapsed, limit)
	}

	// Verify data integrity on the second node.
	got := mustGetData(t, c.nodes[1], key)
	if !bytes.Equal(got, payload) {
		t.Errorf("roundtrip mismatch: stored %d bytes, got %d bytes (sha256 want=%s got=%s)",
			len(payload), len(got), sha256hex(payload), sha256hex(got))
	}
}
