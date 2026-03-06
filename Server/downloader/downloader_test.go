package downloader_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Faizan2005/DFS-Go/Cluster/selector"
	"github.com/Faizan2005/DFS-Go/Server/downloader"
	"github.com/Faizan2005/DFS-Go/Storage/chunker"
	"github.com/Faizan2005/DFS-Go/Storage/compression"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// buildManifest creates a test manifest from plaintext chunks.
// Returns the manifest and a map[storageKey]encData for the fake fetch func.
func buildManifest(t *testing.T, plaintexts [][]byte, compressed bool) (
	*chunker.ChunkManifest,
	map[string][]byte, // storageKey → encryptedBytes (here: plaintext, no real encryption)
) {
	t.Helper()
	store := make(map[string][]byte)
	infos := make([]chunker.ChunkInfo, len(plaintexts))

	for i, plain := range plaintexts {
		data := plain
		wasCompressed := false
		if compressed {
			c, wc, err := compression.CompressChunk(plain, compression.LevelFastest)
			if err != nil {
				t.Fatalf("compress chunk %d: %v", i, err)
			}
			if wc {
				data = c
				wasCompressed = true
			}
		}

		// "Encrypt" = identity (XOR with 0 — plaintext passes through).
		encData := data
		encHash := sha256.Sum256(encData)
		encHashHex := hex.EncodeToString(encHash[:])
		storageKey := chunker.ChunkStorageKey(encHashHex)
		store[storageKey] = encData

		plainHash := sha256.Sum256(plain)
		infos[i] = chunker.ChunkInfo{
			Index:      i,
			Hash:       hex.EncodeToString(plainHash[:]),
			Size:       int64(len(plain)),
			EncHash:    encHashHex,
			Compressed: wasCompressed,
		}
	}
	return chunker.BuildManifest("testfile", infos, time.Now().UnixNano()), store
}

// newManager creates a Manager with a trivial (identity) decrypt function.
func newManager(cfg downloader.Config, store map[string][]byte, peers []string) *downloader.Manager {
	sel := selector.New()

	fetch := func(storageKey, _ string) ([]byte, error) {
		data, ok := store[storageKey]
		if !ok {
			return nil, fmt.Errorf("chunk not found: %s", storageKey)
		}
		return data, nil
	}

	// Identity decrypt (no real encryption in tests).
	decrypt := func(storageKey string, encData []byte, dek []byte) ([]byte, error) {
		return encData, nil
	}

	getPeers := func(_ string) []string { return peers }

	return downloader.New(cfg, sel, fetch, decrypt, getPeers)
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestDownloadSingleChunk(t *testing.T) {
	plain := bytes.Repeat([]byte("hello "), 100)
	manifest, store := buildManifest(t, [][]byte{plain}, false)

	dm := newManager(downloader.DefaultConfig(), store, []string{"peer1:3000"})

	var buf bytes.Buffer
	if err := dm.Download(manifest, &buf, nil, nil); err != nil {
		t.Fatalf("download error: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), plain) {
		t.Errorf("output mismatch: got %d bytes, want %d", buf.Len(), len(plain))
	}
}

func TestDownloadMultipleChunks(t *testing.T) {
	plaintexts := [][]byte{
		bytes.Repeat([]byte("chunk0 "), 200),
		bytes.Repeat([]byte("chunk1 "), 200),
		bytes.Repeat([]byte("chunk2 "), 200),
	}
	manifest, store := buildManifest(t, plaintexts, false)

	dm := newManager(downloader.DefaultConfig(), store, []string{"peer1:3000"})

	var buf bytes.Buffer
	if err := dm.Download(manifest, &buf, nil, nil); err != nil {
		t.Fatalf("download error: %v", err)
	}

	// Verify reassembled output matches concatenation of plaintexts.
	var expected bytes.Buffer
	for _, p := range plaintexts {
		expected.Write(p)
	}
	if !bytes.Equal(buf.Bytes(), expected.Bytes()) {
		t.Errorf("reassembled output mismatch")
	}
}

func TestDownloadEmptyManifest(t *testing.T) {
	manifest := chunker.BuildManifest("empty", nil, 0)
	dm := newManager(downloader.DefaultConfig(), nil, nil)

	var buf bytes.Buffer
	if err := dm.Download(manifest, &buf, nil, nil); err != nil {
		t.Errorf("expected no error for empty manifest, got %v", err)
	}
}

func TestProgressCallback(t *testing.T) {
	plaintexts := [][]byte{
		bytes.Repeat([]byte("a"), 100),
		bytes.Repeat([]byte("b"), 100),
		bytes.Repeat([]byte("c"), 100),
	}
	manifest, store := buildManifest(t, plaintexts, false)
	dm := newManager(downloader.DefaultConfig(), store, []string{"peer1:3000"})

	var calls int32
	progress := func(_, _ int) { atomic.AddInt32(&calls, 1) }

	var buf bytes.Buffer
	if err := dm.Download(manifest, &buf, progress, nil); err != nil {
		t.Fatalf("download error: %v", err)
	}
	if atomic.LoadInt32(&calls) != int32(len(plaintexts)) {
		t.Errorf("expected %d progress calls, got %d", len(plaintexts), calls)
	}
}

func TestDownloadWithCompression(t *testing.T) {
	// Use data that compresses well.
	plaintexts := [][]byte{
		bytes.Repeat([]byte("compressible text for testing "), 300),
		bytes.Repeat([]byte("another compressible chunk "), 300),
	}
	manifest, store := buildManifest(t, plaintexts, true)
	dm := newManager(downloader.DefaultConfig(), store, []string{"peer1:3000"})

	var buf bytes.Buffer
	if err := dm.Download(manifest, &buf, nil, nil); err != nil {
		t.Fatalf("download error: %v", err)
	}

	var expected bytes.Buffer
	for _, p := range plaintexts {
		expected.Write(p)
	}
	if !bytes.Equal(buf.Bytes(), expected.Bytes()) {
		t.Error("compressed roundtrip mismatch")
	}
}

func TestDownloadMissingChunkError(t *testing.T) {
	plain := bytes.Repeat([]byte("x"), 100)
	manifest, _ := buildManifest(t, [][]byte{plain}, false)

	// Empty store — fetch will fail.
	dm := newManager(downloader.DefaultConfig(), map[string][]byte{}, []string{"peer1:3000"})

	var buf bytes.Buffer
	err := dm.Download(manifest, &buf, nil, nil)
	if err == nil {
		t.Error("expected error when chunk is missing from store")
	}
}

func TestDownloadRetriesOnFailure(t *testing.T) {
	plain := bytes.Repeat([]byte("retry data "), 100)
	manifest, store := buildManifest(t, [][]byte{plain}, false)

	// First call fails, second succeeds.
	var callCount int32
	sel := selector.New()

	fetch := func(storageKey, _ string) ([]byte, error) {
		n := atomic.AddInt32(&callCount, 1)
		if n == 1 {
			return nil, fmt.Errorf("transient error")
		}
		data, ok := store[storageKey]
		if !ok {
			return nil, fmt.Errorf("not found")
		}
		return data, nil
	}
	decrypt := func(_ string, data []byte, _ []byte) ([]byte, error) { return data, nil }
	getPeers := func(_ string) []string { return []string{"peerA:3000", "peerB:3000"} }

	cfg := downloader.DefaultConfig()
	cfg.MaxRetries = 3
	dm := downloader.New(cfg, sel, fetch, decrypt, getPeers)

	var buf bytes.Buffer
	if err := dm.Download(manifest, &buf, nil, nil); err != nil {
		t.Fatalf("expected retry to succeed, got: %v", err)
	}
}

func TestDownloadParallelism(t *testing.T) {
	// 8 chunks, maxParallel=4 — verify all arrive and reassemble correctly.
	plaintexts := make([][]byte, 8)
	for i := range plaintexts {
		plaintexts[i] = bytes.Repeat([]byte{byte('a' + i)}, 500)
	}
	manifest, store := buildManifest(t, plaintexts, false)

	cfg := downloader.DefaultConfig()
	cfg.MaxParallel = 4
	dm := newManager(cfg, store, []string{"peer1:3000", "peer2:3000"})

	var buf bytes.Buffer
	if err := dm.Download(manifest, &buf, nil, nil); err != nil {
		t.Fatalf("parallel download error: %v", err)
	}

	var expected bytes.Buffer
	for _, p := range plaintexts {
		expected.Write(p)
	}
	if !bytes.Equal(buf.Bytes(), expected.Bytes()) {
		t.Error("parallel download reassembly mismatch")
	}
}

func TestDownloadIntegrityCheckFails(t *testing.T) {
	plain := bytes.Repeat([]byte("integrity test "), 100)
	manifest, store := buildManifest(t, [][]byte{plain}, false)

	// Corrupt the stored data.
	for k := range store {
		store[k] = bytes.Repeat([]byte{0xFF}, len(store[k]))
	}

	dm := newManager(downloader.DefaultConfig(), store, []string{"peer1:3000"})

	var buf bytes.Buffer
	err := dm.Download(manifest, &buf, nil, nil)
	if err == nil {
		t.Error("expected integrity error for corrupted chunk")
	}
}
