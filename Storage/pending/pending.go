// Package pending manages intermediate upload state for crash-safe resumable
// uploads.  It uses the same append-only log sidecar pattern as the download
// resume package: a JSON header line followed by bare chunk indices.
//
// Flow:
//  1. StoreData calls Create() at the start of an upload.
//  2. After each chunk is stored + replicated, RecordChunk() appends its index
//     and ChunkInfo as a single JSON line.
//  3. On crash / resume, Load() reads the sidecar, returns the set of already-
//     completed chunk indices and their ChunkInfos.
//  4. StoreData skips completed chunks and tells the CLI to seek past them.
//  5. Once all chunks finish, Finalize() deletes the sidecar.
package pending

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/Faizan2005/DFS-Go/Storage/chunker"
)

// Header is the first line of the sidecar file.
type Header struct {
	StorageKey  string `json:"storage_key"`
	TotalSize   int64  `json:"total_size"`
	ChunkSize   int    `json:"chunk_size"`
	MerkleRoot  string `json:"merkle_root,omitempty"` // empty until finalized
	CreatedAt   int64  `json:"created_at"`
}

// ChunkRecord is one line in the sidecar (after the header).
type ChunkRecord struct {
	Index      int    `json:"i"`
	Hash       string `json:"h"`
	Size       int64  `json:"s"`
	EncHash    string `json:"e"`
	Compressed bool   `json:"c,omitempty"`
}

// Sidecar manages the append-only log for a single in-progress upload.
type Sidecar struct {
	mu     sync.Mutex
	header Header
	file   *os.File
}

// Dir returns the directory where sidecar files are stored.
func Dir(storageRoot string) string {
	return filepath.Join(storageRoot, ".pending_uploads")
}

// sidecarPath returns the path for a given storage key's sidecar.
func sidecarPath(storageRoot, storageKey string) string {
	// Use a safe filename derived from the key.
	safe := sanitizeKey(storageKey)
	return filepath.Join(Dir(storageRoot), safe+".pending")
}

// sanitizeKey replaces path separators with underscores for safe filenames.
func sanitizeKey(key string) string {
	out := make([]byte, len(key))
	for i := range key {
		switch key[i] {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			out[i] = '_'
		default:
			out[i] = key[i]
		}
	}
	return string(out)
}

// Create initializes a new sidecar for the given upload.
func Create(storageRoot string, h Header) (*Sidecar, error) {
	dir := Dir(storageRoot)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("pending: mkdir: %w", err)
	}
	path := sidecarPath(storageRoot, h.StorageKey)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("pending: create: %w", err)
	}

	enc := json.NewEncoder(f)
	if err := enc.Encode(h); err != nil {
		f.Close()
		return nil, fmt.Errorf("pending: write header: %w", err)
	}

	return &Sidecar{header: h, file: f}, nil
}

// RecordChunk appends a completed chunk record to the sidecar.
func (s *Sidecar) RecordChunk(info chunker.ChunkInfo) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec := ChunkRecord{
		Index:      info.Index,
		Hash:       info.Hash,
		Size:       info.Size,
		EncHash:    info.EncHash,
		Compressed: info.Compressed,
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = s.file.Write(data)
	return err
}

// Close closes the sidecar file handle without deleting it.
func (s *Sidecar) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file != nil {
		return s.file.Close()
	}
	return nil
}

// LoadResult is returned by Load with the parsed sidecar state.
type LoadResult struct {
	Header     Header
	Completed  map[int]bool
	ChunkInfos []chunker.ChunkInfo
}

// Load reads an existing sidecar and returns the set of completed chunks.
// Returns nil, nil if no sidecar exists.
func Load(storageRoot, storageKey string) (*LoadResult, error) {
	path := sidecarPath(storageRoot, storageKey)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("pending: open: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// First line = header.
	if !scanner.Scan() {
		return nil, fmt.Errorf("pending: empty sidecar")
	}
	var h Header
	if err := json.Unmarshal(scanner.Bytes(), &h); err != nil {
		return nil, fmt.Errorf("pending: parse header: %w", err)
	}

	completed := make(map[int]bool)
	var infos []chunker.ChunkInfo
	for scanner.Scan() {
		var rec ChunkRecord
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			continue // skip corrupt lines
		}
		if completed[rec.Index] {
			continue // duplicate
		}
		completed[rec.Index] = true
		infos = append(infos, chunker.ChunkInfo{
			Index:      rec.Index,
			Hash:       rec.Hash,
			Size:       rec.Size,
			EncHash:    rec.EncHash,
			Compressed: rec.Compressed,
		})
	}

	return &LoadResult{
		Header:     h,
		Completed:  completed,
		ChunkInfos: infos,
	}, nil
}

// Finalize deletes the sidecar file after a successful upload.
func Finalize(storageRoot, storageKey string) error {
	path := sidecarPath(storageRoot, storageKey)
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("pending: finalize: %w", err)
	}
	return nil
}

// Exists returns true if a sidecar exists for the given key.
func Exists(storageRoot, storageKey string) bool {
	path := sidecarPath(storageRoot, storageKey)
	_, err := os.Stat(path)
	return err == nil
}
