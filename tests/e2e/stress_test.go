package e2e_test

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestStress_ManySmallFiles uploads 200 files of 10KB each and verifies
// every file round-trips correctly. Exercises quorum write concurrency.
func TestStress_ManySmallFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in short mode")
	}

	c := newCluster(t, 3, 3)

	const nFiles = 200
	const fileSize = 10 * 1024

	payloads := make([][]byte, nFiles)
	for i := range payloads {
		payloads[i] = makePayload("text", fileSize)
	}

	// Upload all files concurrently.
	var wg sync.WaitGroup
	errs := make([]error, nFiles)
	for i := 0; i < nFiles; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			key := fmt.Sprintf("stress-small-%d", idx)
			errs[idx] = c.nodes[idx%3].StoreData(key, readerOf(payloads[idx]))
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("upload[%d] failed: %v", i, err)
		}
	}

	// Allow replication to settle.
	time.Sleep(1 * time.Second)

	// Verify all files are retrievable.
	for i := 0; i < nFiles; i++ {
		key := fmt.Sprintf("stress-small-%d", i)
		got := mustGetData(t, c.nodes[0], key)
		if sha256hex(got) != sha256hex(payloads[i]) {
			t.Errorf("file[%d] sha256 mismatch", i)
		}
	}
}

// TestStress_50MBFile uploads a single 50MB file (13 chunks) and verifies the
// full round-trip with sha256 integrity check.
func TestStress_50MBFile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 50MB stress test in short mode")
	}

	c := newCluster(t, 3, 3)

	data := makePayload("text", 50*1024*1024)
	key := "stress-50mb-key"

	if err := c.nodes[0].StoreData(key, readerOf(data)); err != nil {
		t.Fatalf("StoreData(50MB): %v", err)
	}

	// Verify from all 3 nodes.
	wantHash := sha256hex(data)
	for i, nd := range c.nodes {
		got := mustGetData(t, nd, key)
		if sha256hex(got) != wantHash {
			t.Errorf("node[%d]: sha256 mismatch for 50MB file", i)
		}
	}
}

// TestStress_ParallelUploadDownload runs 10 upload goroutines and 10 download
// goroutines simultaneously on a 3-node cluster.
func TestStress_ParallelUploadDownload(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping parallel stress test in short mode")
	}

	c := newCluster(t, 3, 3)

	const nFiles = 10
	const fileSize = 2 * 1024 * 1024

	// Pre-populate keys so downloaders have something to fetch.
	payloads := make([][]byte, nFiles)
	for i := 0; i < nFiles; i++ {
		payloads[i] = makePayload("text", fileSize)
		key := fmt.Sprintf("par-preload-%d", i)
		if err := c.nodes[0].StoreData(key, readerOf(payloads[i])); err != nil {
			t.Fatalf("preload StoreData[%d]: %v", i, err)
		}
	}
	time.Sleep(500 * time.Millisecond)

	var wg sync.WaitGroup

	// Uploaders: 10 new files.
	uploadPayloads := make([][]byte, nFiles)
	for i := 0; i < nFiles; i++ {
		uploadPayloads[i] = makePayload("text", fileSize)
	}
	for i := 0; i < nFiles; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			key := fmt.Sprintf("par-upload-%d", idx)
			if err := c.nodes[idx%3].StoreData(key, readerOf(uploadPayloads[idx])); err != nil {
				t.Errorf("par-upload[%d]: %v", idx, err)
			}
		}(i)
	}

	// Downloaders: fetch the pre-populated files.
	for i := 0; i < nFiles; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			key := fmt.Sprintf("par-preload-%d", idx)
			got := mustGetData(t, c.nodes[idx%3], key)
			if sha256hex(got) != sha256hex(payloads[idx]) {
				t.Errorf("par-download[%d]: sha256 mismatch", idx)
			}
		}(i)
	}

	wg.Wait()
}

// TestStress_DataTypeCrossProduct uploads all combinations of 4 payload types
// × 5 sizes (20 total) and verifies every sha256.
func TestStress_DataTypeCrossProduct(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cross-product stress test in short mode")
	}

	c := newCluster(t, 3, 3)

	types := []string{"text", "jpeg", "zip", "binary"}
	sizes := []int{64 * 1024, 256 * 1024, 512 * 1024, 1 * 1024 * 1024, 2 * 1024 * 1024}

	payloads := make(map[string][]byte)
	for _, typ := range types {
		for _, sz := range sizes {
			key := fmt.Sprintf("cross-%s-%d", typ, sz)
			payloads[key] = makePayload(typ, sz)
			if err := c.nodes[0].StoreData(key, readerOf(payloads[key])); err != nil {
				t.Fatalf("StoreData(%s, %d): %v", typ, sz, err)
			}
		}
	}

	time.Sleep(800 * time.Millisecond)

	for key, expected := range payloads {
		got := mustGetData(t, c.nodes[0], key)
		if sha256hex(got) != sha256hex(expected) {
			t.Errorf("key %q: sha256 mismatch", key)
		}
	}
}

// TestStress_RapidNodeChurn uploads 30 files to a 3-node cluster, then adds
// and removes a 4th node 3 times. All 30 files must remain accessible.
func TestStress_RapidNodeChurn(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping churn stress test in short mode")
	}

	c := newCluster(t, 3, 3)

	const nFiles = 30
	payloads := make(map[string][]byte, nFiles)
	for i := 0; i < nFiles; i++ {
		key := fmt.Sprintf("churn-file-%d", i)
		payloads[key] = makePayload("text", 128*1024)
		if err := c.nodes[0].StoreData(key, readerOf(payloads[key])); err != nil {
			t.Fatalf("StoreData(%q): %v", key, err)
		}
	}
	time.Sleep(400 * time.Millisecond)

	// Add and remove a transient 4th node twice.
	for round := 0; round < 2; round++ {
		node4 := c.addNode(c.addrs[0])
		waitFor(t, 8*time.Second, func() bool {
			return c.nodes[0].HashRing.Size() >= 4
		}, fmt.Sprintf("round %d: ring did not grow to 4", round))

		node4.GracefulShutdown()
		// Remove from tracking so teardown doesn't double-close.
		c.nodes = c.nodes[:len(c.nodes)-1]
		c.addrs = c.addrs[:len(c.addrs)-1]

		waitFor(t, 8*time.Second, func() bool {
			return c.nodes[0].HashRing.Size() <= 3
		}, fmt.Sprintf("round %d: ring did not shrink back to 3", round))

		time.Sleep(200 * time.Millisecond)
	}

	// All files must still be accessible.
	for key, expected := range payloads {
		got := mustGetData(t, c.nodes[0], key)
		if sha256hex(got) != sha256hex(expected) {
			t.Errorf("key %q: sha256 mismatch after churn", key)
		}
	}
}

// TestStress_LargeFiles uploads files at increasing sizes (1MB → 20MB) and
// checks that every file downloads correctly from all 3 nodes.
func TestStress_LargeFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping large file stress test in short mode")
	}

	c := newCluster(t, 3, 3)

	sizes := []int{
		1 * 1024 * 1024,
		4 * 1024 * 1024,
		8 * 1024 * 1024,
		16 * 1024 * 1024,
		20 * 1024 * 1024,
	}

	for _, sz := range sizes {
		key := fmt.Sprintf("large-%dMB", sz/(1024*1024))
		data := makePayload("text", sz)

		if err := c.nodes[0].StoreData(key, readerOf(data)); err != nil {
			t.Fatalf("StoreData(%s): %v", key, err)
		}

		wantHash := sha256hex(data)
		for i, nd := range c.nodes {
			got := mustGetData(t, nd, key)
			if sha256hex(got) != wantHash {
				t.Errorf("node[%d] key=%s: sha256 mismatch", i, key)
			}
		}
	}
}
