package e2e_test

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Faizan2005/DFS-Go/Storage/chunker"
)

// TestRoundtrip_Tiny verifies that tiny payloads (100B, 1KB) store and
// retrieve correctly with an exact SHA-256 match.
func TestRoundtrip_Tiny(t *testing.T) {
	c := newCluster(t, 3, 3)

	for _, size := range []int{100, 1024} {
		key := fmt.Sprintf("tiny-%d", size)
		data := makePayload("text", size)
		assertRoundtrip(t, c.nodes[0], c.nodes[0], key, data)
	}
}

// TestRoundtrip_SubChunk uploads a 1MB file (fits in one 4MB chunk).
func TestRoundtrip_SubChunk(t *testing.T) {
	c := newCluster(t, 3, 3)

	data := makePayload("text", 1*1024*1024)
	assertRoundtrip(t, c.nodes[0], c.nodes[0], "subchunk-key", data)
}

// TestRoundtrip_TwoChunks uploads a 5MB file (needs 2 chunks at 4MB boundary).
func TestRoundtrip_TwoChunks(t *testing.T) {
	c := newCluster(t, 3, 3)

	data := makePayload("text", 5*1024*1024)
	assertRoundtrip(t, c.nodes[0], c.nodes[0], "twochunk-key", data)
}

// TestRoundtrip_FiveChunks uploads a 20MB file (needs 5 chunks).
func TestRoundtrip_FiveChunks(t *testing.T) {
	c := newCluster(t, 3, 3)

	data := makePayload("text", 20*1024*1024)
	assertRoundtrip(t, c.nodes[0], c.nodes[0], "fivechunk-key", data)
}

// TestChunkManifestFields verifies that after a chunked upload the manifest
// has non-zero TotalSize, a positive ChunkCount, and a non-empty MerkleRoot.
func TestChunkManifestFields(t *testing.T) {
	c := newCluster(t, 3, 3)

	key := "manifest-fields-key"
	data := makePayload("text", 5*1024*1024) // 2 chunks

	if err := c.nodes[0].StoreData(key, readerOf(data)); err != nil {
		t.Fatalf("StoreData: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	manifest, ok := c.nodes[0].InspectManifest(key)
	if !ok {
		t.Fatalf("InspectManifest: not found after upload")
	}
	if manifest.TotalSize <= 0 {
		t.Errorf("TotalSize should be > 0, got %d", manifest.TotalSize)
	}
	if len(manifest.Chunks) == 0 {
		t.Errorf("Chunks slice should be non-empty")
	}
	if manifest.MerkleRoot == "" {
		t.Errorf("MerkleRoot should be non-empty")
	}
	if manifest.FileKey != key {
		t.Errorf("FileKey: got %q, want %q", manifest.FileKey, key)
	}
}

// TestDeduplication uploads the same bytes under two different keys and
// verifies that both keys are retrievable with identical data.
func TestDeduplication(t *testing.T) {
	c := newCluster(t, 1, 1) // single node is enough for dedup check

	data := makePayload("text", 512*1024)

	if err := c.nodes[0].StoreData("dedup-key-1", readerOf(data)); err != nil {
		t.Fatalf("StoreData(1): %v", err)
	}
	if err := c.nodes[0].StoreData("dedup-key-2", readerOf(data)); err != nil {
		t.Fatalf("StoreData(2): %v", err)
	}

	got1 := mustGetData(t, c.nodes[0], "dedup-key-1")
	got2 := mustGetData(t, c.nodes[0], "dedup-key-2")

	if sha256hex(got1) != sha256hex(data) {
		t.Errorf("dedup-key-1: data mismatch")
	}
	if sha256hex(got2) != sha256hex(data) {
		t.Errorf("dedup-key-2: data mismatch")
	}
}

// TestEmptyFile verifies that uploading 0 bytes round-trips to 0 bytes with
// no error.
func TestEmptyFile(t *testing.T) {
	c := newCluster(t, 1, 1)

	if err := c.nodes[0].StoreData("empty-key", bytes.NewReader(nil)); err != nil {
		t.Fatalf("StoreData(empty): %v", err)
	}

	got := mustGetData(t, c.nodes[0], "empty-key")
	if len(got) != 0 {
		t.Errorf("expected 0 bytes, got %d", len(got))
	}
}

// TestCrossNodeUploadDownload uploads to node[0] and downloads from node[2].
func TestCrossNodeUploadDownload(t *testing.T) {
	c := newCluster(t, 3, 3)

	key := "cross-upload-dl-key"
	data := makePayload("csv", 512*1024)
	assertRoundtrip(t, c.nodes[0], c.nodes[2], key, data)
}

// TestConcurrentUploads runs 20 goroutines each uploading a unique 1MB payload
// concurrently and verifies all succeed.
func TestConcurrentUploads(t *testing.T) {
	c := newCluster(t, 3, 3)

	const nFiles = 20
	payloads := make([][]byte, nFiles)
	for i := range payloads {
		payloads[i] = makePayload("text", 1*1024*1024)
	}

	var wg sync.WaitGroup
	errs := make([]error, nFiles)
	for i := 0; i < nFiles; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			key := fmt.Sprintf("concurrent-upload-%d", idx)
			errs[idx] = c.nodes[idx%3].StoreData(key, readerOf(payloads[idx]))
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("upload[%d] failed: %v", i, err)
		}
	}

	// Spot-check all files are retrievable.
	time.Sleep(500 * time.Millisecond)
	for i := 0; i < nFiles; i++ {
		key := fmt.Sprintf("concurrent-upload-%d", i)
		got := mustGetData(t, c.nodes[0], key)
		if sha256hex(got) != sha256hex(payloads[i]) {
			t.Errorf("concurrent-upload[%d]: sha256 mismatch", i)
		}
	}
}

// TestConcurrentDownloads downloads the same 5MB file from 10 goroutines
// simultaneously and verifies all get identical bytes.
func TestConcurrentDownloads(t *testing.T) {
	c := newCluster(t, 3, 3)

	key := "concurrent-dl-key"
	data := makePayload("text", 5*1024*1024)
	if err := c.nodes[0].StoreData(key, readerOf(data)); err != nil {
		t.Fatalf("StoreData: %v", err)
	}
	time.Sleep(400 * time.Millisecond)

	const nDownloads = 10
	results := make([][]byte, nDownloads)
	var wg sync.WaitGroup
	for i := 0; i < nDownloads; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx] = mustGetData(t, c.nodes[idx%3], key)
		}(i)
	}
	wg.Wait()

	wantHash := sha256hex(data)
	for i, got := range results {
		if sha256hex(got) != wantHash {
			t.Errorf("download[%d]: sha256 mismatch", i)
		}
	}
}

// TestChunkCountMatchesExpected checks that the manifest's chunk count is
// ceil(fileSize / DefaultChunkSize).
func TestChunkCountMatchesExpected(t *testing.T) {
	cases := []struct {
		name     string
		size     int
		wantChunks int
	}{
		{"1MB-1chunk", 1 * 1024 * 1024, 1},
		{"4MB-1chunk", 4 * 1024 * 1024, 1},
		{"5MB-2chunks", 5 * 1024 * 1024, 2},
		{"12MB-3chunks", 12 * 1024 * 1024, 3},
	}

	c := newCluster(t, 3, 3)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := "chunkcount-" + tc.name
			data := makePayload("text", tc.size)
			if err := c.nodes[0].StoreData(key, readerOf(data)); err != nil {
				t.Fatalf("StoreData: %v", err)
			}
			time.Sleep(200 * time.Millisecond)

			manifest, ok := c.nodes[0].InspectManifest(key)
			if !ok {
				t.Fatalf("InspectManifest: not found")
			}
			got := len(manifest.Chunks)
			want := tc.wantChunks
			if got != want {
				t.Errorf("chunk count: got %d, want %d (chunkSize=%d, fileSize=%d)",
					got, want, chunker.DefaultChunkSize, tc.size)
			}
		})
	}
}

// TestManyKeysRoundtrip stores and retrieves 50 distinct keys with varied
// payload types, checking every sha256.
func TestManyKeysRoundtrip(t *testing.T) {
	c := newCluster(t, 3, 3)

	types := []string{"text", "html", "csv", "binary"}
	payloads := make(map[string][]byte, 50)
	for i := 0; i < 50; i++ {
		typ := types[i%len(types)]
		key := fmt.Sprintf("many-key-%d-%s", i, typ)
		payloads[key] = makePayload(typ, 32*1024)
		if err := c.nodes[i%3].StoreData(key, readerOf(payloads[key])); err != nil {
			t.Fatalf("StoreData(%q): %v", key, err)
		}
	}

	time.Sleep(500 * time.Millisecond)

	for key, expected := range payloads {
		got := mustGetData(t, c.nodes[0], key)
		if sha256hex(got) != sha256hex(expected) {
			t.Errorf("key %q: sha256 mismatch", key)
		}
	}
}
