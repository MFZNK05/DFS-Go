package downloader_test

import (
	"bytes"
	"context"
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

	fetch := func(_ context.Context, _ string, _ int, storageKey, _ string) ([]byte, error) {
		data, ok := store[storageKey]
		if !ok {
			return nil, fmt.Errorf("chunk not found: %s", storageKey)
		}
		return data, nil
	}

	// Identity decrypt (no real encryption in tests).
	decrypt := func(storageKey string, _ int, encData []byte, dek []byte) ([]byte, error) {
		return encData, nil
	}

	getPeers := func(_ string, _ int, _ string) []string { return peers }

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
	if err := dm.Download(context.Background(), "testfile", manifest, &buf, nil, nil); err != nil {
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
	if err := dm.Download(context.Background(), "testfile", manifest, &buf, nil, nil); err != nil {
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
	if err := dm.Download(context.Background(), "testfile", manifest, &buf, nil, nil); err != nil {
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
	if err := dm.Download(context.Background(), "testfile", manifest, &buf, progress, nil); err != nil {
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
	if err := dm.Download(context.Background(), "testfile", manifest, &buf, nil, nil); err != nil {
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
	err := dm.Download(context.Background(), "testfile", manifest, &buf, nil, nil)
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

	fetch := func(_ context.Context, _ string, _ int, storageKey, _ string) ([]byte, error) {
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
	decrypt := func(_ string, _ int, data []byte, _ []byte) ([]byte, error) { return data, nil }
	getPeers := func(_ string, _ int, _ string) []string { return []string{"peerA:3000", "peerB:3000"} }

	cfg := downloader.DefaultConfig()
	cfg.MaxRetries = 3
	dm := downloader.New(cfg, sel, fetch, decrypt, getPeers)

	var buf bytes.Buffer
	if err := dm.Download(context.Background(), "testfile", manifest, &buf, nil, nil); err != nil {
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
	if err := dm.Download(context.Background(), "testfile", manifest, &buf, nil, nil); err != nil {
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
	err := dm.Download(context.Background(), "testfile", manifest, &buf, nil, nil)
	if err == nil {
		t.Error("expected integrity error for corrupted chunk")
	}
}

// ---------------------------------------------------------------------------
// Directory download tests (global chunk work pool)
// ---------------------------------------------------------------------------

// memWriterAt is a fixed-size in-memory buffer implementing io.WriterAt.
type memWriterAt struct {
	data []byte
}

func newMemWriterAt(size int) *memWriterAt {
	return &memWriterAt{data: make([]byte, size)}
}

func (m *memWriterAt) WriteAt(p []byte, off int64) (int, error) {
	copy(m.data[off:], p)
	return len(p), nil
}

// buildFileContext creates a FileContext for testing from a manifest and in-memory writer.
func buildFileContext(t *testing.T, index int, relPath string, manifest *chunker.ChunkManifest) *downloader.FileContext {
	t.Helper()
	// Size the buffer to fit all chunks at their offset positions.
	// Last chunk may be smaller, but offsets are based on ChunkSize.
	bufSize := manifest.ChunkSize * len(manifest.Chunks)
	if int64(bufSize) < manifest.TotalSize {
		bufSize = int(manifest.TotalSize)
	}
	buf := newMemWriterAt(bufSize)

	fc := &downloader.FileContext{
		Index:        index,
		RelativePath: relPath,
		ChunkSize:    manifest.ChunkSize,
		Chunks:       manifest.Chunks,
		Writer:       buf,
		SkipSet:      make(map[int]bool),
		TotalChunks:  len(manifest.Chunks),
		OnChunkDone:  func(idx int, hash string) {},
		OnFinalize: func() error {
			return chunker.VerifyMerkleRoot(manifest.Chunks, manifest.MerkleRoot)
		},
		Cleanup: func() {},
	}
	fc.Remaining.Store(int32(len(manifest.Chunks)))
	fc.InitErrors(len(manifest.Chunks))
	return fc
}

func TestDownloadDirectorySingleFile(t *testing.T) {
	plaintexts := [][]byte{
		bytes.Repeat([]byte("chunk0 "), 200),
		bytes.Repeat([]byte("chunk1 "), 200),
		bytes.Repeat([]byte("chunk2 "), 200),
	}
	manifest, store := buildManifest(t, plaintexts, false)
	dm := newManager(downloader.DefaultConfig(), store, []string{"peer1:3000"})

	fc := buildFileContext(t, 0, "file1.txt", manifest)
	result := dm.DownloadDirectory(context.Background(), []*downloader.FileContext{fc}, 4, nil, nil)

	if result.Failed != 0 {
		t.Errorf("expected 0 failures, got %d: %v", result.Failed, result.FailedFiles)
	}
	if result.Succeeded != 1 {
		t.Errorf("expected 1 success, got %d", result.Succeeded)
	}

	// Verify each chunk is at the correct offset.
	buf := fc.Writer.(*memWriterAt)
	for i, p := range plaintexts {
		offset := i * manifest.ChunkSize
		if !bytes.Equal(buf.data[offset:offset+len(p)], p) {
			t.Errorf("chunk %d data mismatch at offset %d", i, offset)
		}
	}
}

func TestDownloadDirectoryMultiFile(t *testing.T) {
	// 3 files, each with different chunk counts.
	fileData := [][]byte{
		bytes.Repeat([]byte("fileA "), 300),
		bytes.Repeat([]byte("fileB "), 500),
		bytes.Repeat([]byte("fileC "), 100),
	}

	// Build a combined store and per-file manifests.
	store := make(map[string][]byte)
	var files []*downloader.FileContext

	for i, data := range fileData {
		// Split into 500-byte "chunks" for testing.
		var chunks [][]byte
		for off := 0; off < len(data); off += 500 {
			end := off + 500
			if end > len(data) {
				end = len(data)
			}
			chunks = append(chunks, data[off:end])
		}

		manifest, fileStore := buildManifest(t, chunks, false)
		for k, v := range fileStore {
			store[k] = v
		}

		fc := buildFileContext(t, i, fmt.Sprintf("file%d.bin", i), manifest)
		files = append(files, fc)
	}

	dm := newManager(downloader.DefaultConfig(), store, []string{"peer1:3000"})
	result := dm.DownloadDirectory(context.Background(), files, 4, nil, nil)

	if result.Failed != 0 {
		t.Errorf("expected 0 failures, got %d: %v", result.Failed, result.FailedFiles)
	}
	if result.Succeeded != 3 {
		t.Errorf("expected 3 successes, got %d", result.Succeeded)
	}

	// Verify each file: read chunk data back from its offset position.
	for fi, data := range fileData {
		buf := files[fi].Writer.(*memWriterAt)
		chunkSize := files[fi].ChunkSize
		var reconstructed []byte
		for ci := range files[fi].Chunks {
			offset := ci * chunkSize
			size := int(files[fi].Chunks[ci].Size)
			reconstructed = append(reconstructed, buf.data[offset:offset+size]...)
		}
		if !bytes.Equal(reconstructed, data) {
			t.Errorf("file %d reconstructed data mismatch: got %d bytes, want %d", fi, len(reconstructed), len(data))
		}
	}
}

func TestDownloadDirectoryResume(t *testing.T) {
	// 5 chunks, pre-populate 3 in the skip set.
	plaintexts := make([][]byte, 5)
	for i := range plaintexts {
		plaintexts[i] = bytes.Repeat([]byte{byte('a' + i)}, 200)
	}
	manifest, store := buildManifest(t, plaintexts, false)
	dm := newManager(downloader.DefaultConfig(), store, []string{"peer1:3000"})

	bufSize := manifest.ChunkSize * len(manifest.Chunks)
	buf := newMemWriterAt(bufSize)

	// Pre-populate chunks 0, 1, 2 in the buffer and skip set.
	skipSet := map[int]bool{0: true, 1: true, 2: true}
	for idx := range skipSet {
		offset := idx * manifest.ChunkSize
		copy(buf.data[offset:], plaintexts[idx])
	}

	var recordedChunks []int
	fc := &downloader.FileContext{
		Index:        0,
		RelativePath: "resume_test.bin",
		ChunkSize:    manifest.ChunkSize,
		Chunks:       manifest.Chunks,
		Writer:       buf,
		SkipSet:      skipSet,
		TotalChunks:  len(manifest.Chunks),
		OnChunkDone: func(idx int, hash string) {
			recordedChunks = append(recordedChunks, idx)
		},
		OnFinalize: func() error {
			return chunker.VerifyMerkleRoot(manifest.Chunks, manifest.MerkleRoot)
		},
		Cleanup: func() {},
	}
	fc.Remaining.Store(int32(2)) // only 2 chunks remaining
	fc.InitErrors(len(manifest.Chunks))

	result := dm.DownloadDirectory(context.Background(), []*downloader.FileContext{fc}, 4, nil, nil)
	if result.Failed != 0 {
		t.Errorf("expected 0 failures, got %d", result.Failed)
	}

	// Verify only chunks 3 and 4 were fetched.
	if len(recordedChunks) != 2 {
		t.Errorf("expected 2 chunks recorded, got %d", len(recordedChunks))
	}

	// Verify each chunk is at the correct offset.
	for i, p := range plaintexts {
		offset := i * manifest.ChunkSize
		if !bytes.Equal(buf.data[offset:offset+len(p)], p) {
			t.Errorf("resume: chunk %d data mismatch at offset %d", i, offset)
		}
	}
}

func TestDownloadDirectoryPartialFailure(t *testing.T) {
	// File 0: all chunks available. File 1: chunks missing from store.
	file0Data := [][]byte{bytes.Repeat([]byte("ok "), 200)}
	file1Data := [][]byte{bytes.Repeat([]byte("bad "), 200)}

	manifest0, store := buildManifest(t, file0Data, false)
	manifest1, _ := buildManifest(t, file1Data, false) // don't add to store

	dm := newManager(downloader.DefaultConfig(), store, []string{"peer1:3000"})

	fc0 := buildFileContext(t, 0, "good.bin", manifest0)
	fc1 := buildFileContext(t, 1, "bad.bin", manifest1)

	result := dm.DownloadDirectory(context.Background(), []*downloader.FileContext{fc0, fc1}, 4, nil, nil)
	if result.Succeeded != 1 {
		t.Errorf("expected 1 success, got %d", result.Succeeded)
	}
	if result.Failed != 1 {
		t.Errorf("expected 1 failure, got %d", result.Failed)
	}
	if len(result.FailedFiles) != 1 || result.FailedFiles[0] != "bad.bin" {
		t.Errorf("expected failed file 'bad.bin', got %v", result.FailedFiles)
	}

	// Good file should be intact (compare up to actual data size).
	buf0 := fc0.Writer.(*memWriterAt)
	if !bytes.Equal(buf0.data[:len(file0Data[0])], file0Data[0]) {
		t.Error("good file output mismatch")
	}
}

func TestDownloadDirectoryRetry(t *testing.T) {
	// All chunks fail on round 0 (transient), succeed on round 1.
	plaintexts := [][]byte{bytes.Repeat([]byte("retry "), 200)}
	manifest, store := buildManifest(t, plaintexts, false)

	var callCount atomic.Int32
	sel := selector.New()
	fetch := func(_ context.Context, _ string, _ int, storageKey, _ string) ([]byte, error) {
		n := callCount.Add(1)
		if n <= 1 { // fail on first attempt
			return nil, fmt.Errorf("transient error")
		}
		data, ok := store[storageKey]
		if !ok {
			return nil, fmt.Errorf("not found")
		}
		return data, nil
	}
	decrypt := func(_ string, _ int, data []byte, _ []byte) ([]byte, error) { return data, nil }
	getPeers := func(_ string, _ int, _ string) []string { return []string{"peerA:3000", "peerB:3000"} }

	cfg := downloader.DefaultConfig()
	cfg.MaxRetries = 3
	dm := downloader.New(cfg, sel, fetch, decrypt, getPeers)

	fc := buildFileContext(t, 0, "retry.bin", manifest)
	result := dm.DownloadDirectory(context.Background(), []*downloader.FileContext{fc}, 4, nil, nil)

	if result.Failed != 0 {
		t.Errorf("expected retry to succeed, got %d failures", result.Failed)
	}
	if result.Succeeded != 1 {
		t.Errorf("expected 1 success, got %d", result.Succeeded)
	}
}

func TestDownloadDirectoryProgress(t *testing.T) {
	// 2 files, 2 chunks each → expect progress calls.
	store := make(map[string][]byte)
	var files []*downloader.FileContext

	for i := 0; i < 2; i++ {
		data := [][]byte{
			bytes.Repeat([]byte{byte('a' + i)}, 200),
			bytes.Repeat([]byte{byte('c' + i)}, 200),
		}
		manifest, fStore := buildManifest(t, data, false)
		for k, v := range fStore {
			store[k] = v
		}
		fc := buildFileContext(t, i, fmt.Sprintf("prog%d.bin", i), manifest)
		files = append(files, fc)
	}

	dm := newManager(downloader.DefaultConfig(), store, []string{"peer1:3000"})

	var progressCalls atomic.Int32
	progress := func(fc, ft, cc, ct int) {
		progressCalls.Add(1)
	}

	result := dm.DownloadDirectory(context.Background(), files, 4, nil, progress)
	if result.Failed != 0 {
		t.Errorf("expected 0 failures, got %d", result.Failed)
	}

	// 4 chunks total → 4 per-chunk progress calls + 1 final progress call = 5.
	if got := progressCalls.Load(); got != 5 {
		t.Errorf("expected 5 progress calls, got %d", got)
	}
}
