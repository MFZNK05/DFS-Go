// Package downloader fetches all chunks of a file in parallel, decrypts and
// decompresses each, verifies integrity, then reassembles them in order.
//
// Usage:
//
//	dm := downloader.New(cfg, sel, fetchFn, decryptFn)
//	err := dm.Download(manifest, dst, progressFn)
package downloader

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/Faizan2005/DFS-Go/Cluster/selector"
	"github.com/Faizan2005/DFS-Go/Storage/chunker"
	"github.com/Faizan2005/DFS-Go/Storage/compression"
)

// Config controls the parallel download behaviour.
type Config struct {
	MaxParallel int           // max concurrent chunk fetches (default 4)
	ChunkTimeout time.Duration // per-chunk fetch timeout (default 30s)
	MaxRetries  int           // retries per chunk on failure (default 3)
}

// DefaultConfig returns sensible defaults for a college LAN.
func DefaultConfig() Config {
	return Config{
		MaxParallel:  4,
		ChunkTimeout: 30 * time.Second,
		MaxRetries:   3,
	}
}

// FetchFunc fetches the raw encrypted bytes for storageKey from any available
// peer. It returns the encrypted bytes and the peer address that served them.
// The caller is responsible for choosing which peers to try — this function
// is called by the downloader with the best peer from the selector.
type FetchFunc func(storageKey string, peerAddr string) (encData []byte, err error)

// DecryptFunc decrypts encData using the per-chunk key identified by storageKey.
type DecryptFunc func(storageKey string, encData []byte) (plaintext []byte, err error)

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

// Download fetches all chunks in manifest in parallel, writes the reassembled
// plaintext to dst, and calls progress (if non-nil) after each chunk.
func (m *Manager) Download(manifest *chunker.ChunkManifest, dst io.Writer, progress ProgressFunc) error {
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

			c, err := m.fetchChunk(ci)
			chunks[idx] = c
			errs[idx] = err

			if progress != nil {
				progress(idx, n)
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

	return chunker.Reassemble(chunks, dst)
}

// fetchChunk fetches, decrypts, decompresses, and integrity-checks one chunk.
// It retries up to cfg.MaxRetries times on different peers.
func (m *Manager) fetchChunk(info chunker.ChunkInfo) (chunker.Chunk, error) {
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

		// Decrypt.
		plain, err := m.decrypt(storageKey, encData)
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

		// Integrity check.
		got := sha256.Sum256(plain)
		gotHex := hex.EncodeToString(got[:])
		if gotHex != info.Hash {
			lastErr = fmt.Errorf("integrity fail for chunk idx=%d (want %s got %s)",
				info.Index, info.Hash, gotHex)
			continue
		}

		return chunker.Chunk{
			Index: info.Index,
			Hash:  got,
			Size:  int64(len(plain)),
			Data:  plain,
		}, nil
	}

	if lastErr != nil {
		return chunker.Chunk{}, lastErr
	}
	return chunker.Chunk{}, fmt.Errorf("no peers available for chunk idx=%d key=%s",
		info.Index, storageKey)
}

// Stats returns aggregate latency information for all known peers.
// Useful for diagnostics and tests.
func (m *Manager) Stats() *selector.Selector {
	return m.sel
}
