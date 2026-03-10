// Package downloader fetches all chunks of a file in parallel, decrypts and
// decompresses each, verifies integrity per-chunk (DC++ style), then writes
// them to the destination.
//
// Two download strategies:
//
//	DownloadToFile  — random-access io.WriterAt, zero HoL blocking, zero buffering
//	DownloadToStream — sequential io.Writer with sliding-window backpressure
//
// Both paths verify each chunk's SHA-256 hash ON-THE-FLY before writing.
// Bad peers are banned and chunks requeued automatically.
package downloader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Faizan2005/DFS-Go/Cluster/selector"
	"github.com/Faizan2005/DFS-Go/Storage/chunker"
	"github.com/Faizan2005/DFS-Go/Storage/compression"
)

// Config controls the parallel download behaviour.
type Config struct {
	MaxParallel  int           // max concurrent chunk fetches (default 4)
	ChunkTimeout time.Duration // per-chunk fetch timeout (default 30s)
	MaxRetries   int           // retries per chunk on failure (default 3)
}

// DefaultConfig returns sensible defaults for a college LAN.
func DefaultConfig() Config {
	return Config{
		MaxParallel:  4,
		ChunkTimeout: 30 * time.Second,
		MaxRetries:   3,
	}
}

// FetchFunc fetches the raw encrypted bytes for storageKey from a specific peer.
type FetchFunc func(storageKey string, peerAddr string) (encData []byte, err error)

// DecryptFunc decrypts encData using the provided DEK.
// When dek is nil, returns encData as-is (plaintext path).
type DecryptFunc func(storageKey string, encData []byte, dek []byte) (plaintext []byte, err error)

// GetPeersFunc returns the ordered list of peer addresses responsible for
// storing storageKey (from the consistent hash ring).
type GetPeersFunc func(storageKey string) []string

// ProgressFunc is called after each chunk completes. index is 0-based,
// total is the number of chunks in the manifest.
type ProgressFunc func(index, total int)

// Manager downloads chunked files in parallel.
type Manager struct {
	cfg      Config
	sel      *selector.Selector
	fetch    FetchFunc
	decrypt  DecryptFunc
	getPeers GetPeersFunc
}

// New creates a Manager. All function arguments are required.
func New(cfg Config, sel *selector.Selector, fetch FetchFunc, decrypt DecryptFunc, getPeers GetPeersFunc) *Manager {
	if cfg.MaxParallel <= 0 {
		cfg.MaxParallel = DefaultConfig().MaxParallel
	}
	if cfg.ChunkTimeout <= 0 {
		cfg.ChunkTimeout = DefaultConfig().ChunkTimeout
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = DefaultConfig().MaxRetries
	}
	return &Manager{cfg: cfg, sel: sel, fetch: fetch, decrypt: decrypt, getPeers: getPeers}
}

// ---------------------------------------------------------------------------
// Primary path: DownloadToFile (random-access, zero HoL blocking)
// ---------------------------------------------------------------------------

// DownloadToFile fetches all chunks in parallel and writes each directly to dst
// at its correct offset using random-access I/O. There is no sequential ordering
// constraint — chunks are written as soon as they are verified, eliminating
// head-of-line blocking entirely.
//
// Per-chunk integrity is verified BEFORE writing to disk. Bad peers are banned
// and the chunk is retried on a different peer.
//
// Memory: MaxParallel * ChunkSize (e.g., 4 * 4 MiB = 16 MiB).
// Concurrent WriteAt to different offsets on *os.File is safe on POSIX.
func (m *Manager) DownloadToFile(ctx context.Context, manifest *chunker.ChunkManifest, dst io.WriterAt, progress ProgressFunc, dek []byte) error {
	n := len(manifest.Chunks)
	if n == 0 {
		return nil
	}

	errs := make([]error, n)
	sem := make(chan struct{}, m.cfg.MaxParallel)
	var wg sync.WaitGroup
	var completed atomic.Int32

	for i, info := range manifest.Chunks {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{} // acquire slot

		go func(idx int, ci chunker.ChunkInfo) {
			defer wg.Done()
			defer func() { <-sem }() // release slot

			plain, err := m.fetchAndVerifyChunk(ci, dek)
			if err != nil {
				errs[idx] = err
				return
			}

			// Random-access write: chunk goes directly to its correct offset.
			offset := int64(idx) * int64(manifest.ChunkSize)
			if _, err := dst.WriteAt(plain, offset); err != nil {
				errs[idx] = fmt.Errorf("write chunk %d at offset %d: %w", idx, offset, err)
				return
			}

			completed.Add(1)
			if progress != nil {
				progress(int(completed.Load()), n)
			}
		}(i, info)
	}

	wg.Wait()

	// Surface first error.
	for i, err := range errs {
		if err != nil {
			return fmt.Errorf("downloader: chunk %d: %w", i, err)
		}
	}

	// End-to-end Merkle root verification.
	if err := chunker.VerifyMerkleRoot(manifest.Chunks, manifest.MerkleRoot); err != nil {
		return fmt.Errorf("downloader: %w", err)
	}

	return nil
}

// ChunkRecordFunc is called after each chunk is written to disk. The downloader
// calls this so the server layer can update the resume sidecar. index is the
// chunk's manifest index, hash is the hex SHA-256 of the plaintext.
type ChunkRecordFunc func(index int, hash string)

// ---------------------------------------------------------------------------
// Resume path: DownloadToFileResumable (skip verified chunks)
// ---------------------------------------------------------------------------

// DownloadToFileResumable is like DownloadToFile but skips chunks in skipSet.
// The caller must have fast-verified the skip set against the actual .part file
// before calling — this method trusts the skip set completely.
//
// After each new chunk is written, onChunkDone is called so the caller can
// record it in the resume sidecar. onChunkDone may be nil.
func (m *Manager) DownloadToFileResumable(
	ctx context.Context,
	manifest *chunker.ChunkManifest,
	dst io.WriterAt,
	skipSet map[int]bool,
	progress ProgressFunc,
	onChunkDone ChunkRecordFunc,
	dek []byte,
) error {
	n := len(manifest.Chunks)
	if n == 0 {
		return nil
	}

	// Count how many we actually need to fetch.
	toFetch := 0
	for i := 0; i < n; i++ {
		if !skipSet[i] {
			toFetch++
		}
	}
	if toFetch == 0 {
		// All chunks already verified — just do Merkle check.
		return chunker.VerifyMerkleRoot(manifest.Chunks, manifest.MerkleRoot)
	}

	alreadyDone := int32(n - toFetch)
	errs := make([]error, n)
	sem := make(chan struct{}, m.cfg.MaxParallel)
	var wg sync.WaitGroup
	var completed atomic.Int32
	completed.Store(alreadyDone)

	// Report initial resume progress.
	if progress != nil {
		progress(int(alreadyDone), n)
	}

	for i, info := range manifest.Chunks {
		if ctx.Err() != nil {
			break
		}
		if skipSet[i] {
			continue // already verified on disk
		}

		wg.Add(1)
		sem <- struct{}{} // acquire slot

		go func(idx int, ci chunker.ChunkInfo) {
			defer wg.Done()
			defer func() { <-sem }() // release slot

			plain, err := m.fetchAndVerifyChunk(ci, dek)
			if err != nil {
				errs[idx] = err
				return
			}

			offset := int64(idx) * int64(manifest.ChunkSize)
			if _, err := dst.WriteAt(plain, offset); err != nil {
				errs[idx] = fmt.Errorf("write chunk %d at offset %d: %w", idx, offset, err)
				return
			}

			if onChunkDone != nil {
				onChunkDone(idx, ci.Hash)
			}

			completed.Add(1)
			if progress != nil {
				progress(int(completed.Load()), n)
			}
		}(i, info)
	}

	wg.Wait()

	for i, err := range errs {
		if err != nil {
			return fmt.Errorf("downloader: chunk %d: %w", i, err)
		}
	}

	if err := chunker.VerifyMerkleRoot(manifest.Chunks, manifest.MerkleRoot); err != nil {
		return fmt.Errorf("downloader: %w", err)
	}

	return nil
}

// ---------------------------------------------------------------------------
// Fallback path: DownloadToStream (sliding window, sequential output)
// ---------------------------------------------------------------------------

// DownloadToStream fetches chunks in parallel but writes them to dst in strict
// index order, suitable for piping to stdout or an IPC socket.
//
// Uses a sliding window of size MaxParallel to bound memory. Workers are only
// allowed to fetch chunk N if N < nextExpected + MaxParallel. This provides
// backpressure: if the earliest chunk is slow, workers pause after filling the
// window, preventing unbounded memory growth.
//
// Per-chunk integrity is verified BEFORE placing in the window buffer.
// Memory: MaxParallel * ChunkSize (e.g., 4 * 4 MiB = 16 MiB).
func (m *Manager) DownloadToStream(manifest *chunker.ChunkManifest, dst io.Writer, progress ProgressFunc, dek []byte) error {
	n := len(manifest.Chunks)
	if n == 0 {
		return nil
	}

	type chunkResult struct {
		index int
		data  []byte
		err   error
	}

	windowSize := m.cfg.MaxParallel
	results := make(chan chunkResult, windowSize)

	// Track which chunks are dispatched/completed.
	var dispatchMu sync.Mutex
	nextDispatch := 0
	nextExpected := 0

	// dispatchChunks sends work to workers, respecting the sliding window.
	// Returns the number of chunks dispatched in this call.
	dispatchChunks := func(sem chan struct{}) int {
		dispatched := 0
		dispatchMu.Lock()
		for nextDispatch < n && nextDispatch < nextExpected+windowSize {
			idx := nextDispatch
			nextDispatch++
			dispatchMu.Unlock()

			sem <- struct{}{} // acquire worker slot
			go func(i int, ci chunker.ChunkInfo) {
				defer func() { <-sem }()
				plain, err := m.fetchAndVerifyChunk(ci, dek)
				results <- chunkResult{index: i, data: plain, err: err}
			}(idx, manifest.Chunks[idx])

			dispatched++
			dispatchMu.Lock()
		}
		dispatchMu.Unlock()
		return dispatched
	}

	sem := make(chan struct{}, windowSize)
	window := make(map[int][]byte) // out-of-order buffer (bounded by windowSize)

	// Initial dispatch: fill the window.
	dispatchChunks(sem)

	var completed int
	for nextExpected < n {
		// Wait for a result.
		r := <-results
		if r.err != nil {
			return fmt.Errorf("downloader: chunk %d: %w", r.index, r.err)
		}
		completed++

		if r.index == nextExpected {
			// Write directly — it's the chunk we need next.
			if _, err := dst.Write(r.data); err != nil {
				return fmt.Errorf("downloader: write chunk %d: %w", r.index, err)
			}
			nextExpected++

			// Flush any consecutive chunks already in the window.
			for {
				data, ok := window[nextExpected]
				if !ok {
					break
				}
				if _, err := dst.Write(data); err != nil {
					return fmt.Errorf("downloader: write chunk %d: %w", nextExpected, err)
				}
				delete(window, nextExpected)
				nextExpected++
			}

			// Window advanced — dispatch more chunks.
			dispatchMu.Lock()
			dispatchMu.Unlock()
			dispatchChunks(sem)
		} else {
			// Out of order — buffer it (within window bounds).
			window[r.index] = r.data
		}

		if progress != nil {
			progress(completed, n)
		}
	}

	// End-to-end Merkle root verification.
	if err := chunker.VerifyMerkleRoot(manifest.Chunks, manifest.MerkleRoot); err != nil {
		return fmt.Errorf("downloader: %w", err)
	}

	return nil
}

// ---------------------------------------------------------------------------
// Legacy path: Download (backwards compat, buffers all in memory)
// ---------------------------------------------------------------------------

// Download fetches all chunks in manifest in parallel, writes the reassembled
// plaintext to dst, and calls progress (if non-nil) after each chunk.
// dek is the decryption key — nil for plaintext files.
//
// Deprecated: Use DownloadToFile for file output or DownloadToStream for pipes.
// This method buffers all chunks in memory before writing.
func (m *Manager) Download(manifest *chunker.ChunkManifest, dst io.Writer, progress ProgressFunc, dek []byte) error {
	n := len(manifest.Chunks)
	if n == 0 {
		return nil
	}

	chunks := make([]chunker.Chunk, n)
	errs := make([]error, n)

	sem := make(chan struct{}, m.cfg.MaxParallel)
	var wg sync.WaitGroup

	for i, info := range manifest.Chunks {
		wg.Add(1)
		sem <- struct{}{} // acquire slot

		go func(idx int, ci chunker.ChunkInfo) {
			defer wg.Done()
			defer func() { <-sem }() // release slot

			plain, err := m.fetchAndVerifyChunk(ci, dek)
			if err != nil {
				errs[idx] = err
			} else {
				h := sha256.Sum256(plain)
				chunks[idx] = chunker.Chunk{
					Index: ci.Index,
					Hash:  h,
					Size:  int64(len(plain)),
					Data:  plain,
				}
			}

			if progress != nil {
				progress(idx, n)
			}
		}(i, info)
	}

	wg.Wait()

	for i, err := range errs {
		if err != nil {
			return fmt.Errorf("downloader: chunk %d: %w", i, err)
		}
	}

	return chunker.Reassemble(chunks, dst)
}

// ---------------------------------------------------------------------------
// Core: fetchAndVerifyChunk (shared by all paths)
// ---------------------------------------------------------------------------

// fetchAndVerifyChunk fetches, decrypts, decompresses, and integrity-checks one
// chunk. It retries up to cfg.MaxRetries times on different peers. Returns the
// verified plaintext bytes.
//
// On integrity failure the peer is penalised via a large latency record in the
// selector, effectively banning it for subsequent chunk picks.
func (m *Manager) fetchAndVerifyChunk(info chunker.ChunkInfo, dek []byte) ([]byte, error) {
	storageKey := chunker.ChunkStorageKey(info.EncHash)
	candidates := m.getPeers(storageKey)

	var lastErr error
	tried := make(map[string]bool)

	for attempt := 0; attempt <= m.cfg.MaxRetries; attempt++ {
		// Pick best untried peer.
		var available []string
		for _, p := range candidates {
			if !tried[p] {
				available = append(available, p)
			}
		}
		if len(available) == 0 {
			break
		}

		peer, ok := m.sel.BestPeer(available)
		if !ok {
			break
		}
		tried[peer] = true

		m.sel.BeginDownload(peer)
		t0 := time.Now()

		encData, err := m.fetch(storageKey, peer)
		elapsed := time.Since(t0)

		m.sel.EndDownload(peer)
		m.sel.RecordLatency(peer, elapsed)

		if err != nil {
			lastErr = fmt.Errorf("fetch from %s: %w", peer, err)
			continue
		}

		// Decrypt (no-op when dek is nil).
		plain, err := m.decrypt(storageKey, encData, dek)
		if err != nil {
			lastErr = fmt.Errorf("decrypt from %s: %w", peer, err)
			continue
		}

		// Decompress if the chunk was compressed at upload time.
		if info.Compressed {
			plain, err = compression.DecompressChunk(plain)
			if err != nil {
				lastErr = fmt.Errorf("decompress from %s: %w", peer, err)
				continue
			}
		}

		// ON-THE-FLY integrity check (DC++ TTH-style).
		got := sha256.Sum256(plain)
		gotHex := hex.EncodeToString(got[:])
		if gotHex != info.Hash {
			// Penalise this peer heavily — effectively ban it for future picks.
			m.sel.RecordLatency(peer, 10*time.Minute)
			lastErr = fmt.Errorf("integrity fail for chunk idx=%d from %s (want %s got %s)",
				info.Index, peer, info.Hash, gotHex)
			continue
		}

		return plain, nil
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("no peers available for chunk idx=%d key=%s",
		info.Index, storageKey)
}

// Stats returns aggregate latency information for all known peers.
func (m *Manager) Stats() *selector.Selector {
	return m.sel
}
