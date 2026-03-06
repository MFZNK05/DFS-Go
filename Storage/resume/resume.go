// Package resume tracks partial download state via an append-only log sidecar.
//
// Architecture:
//
//	Output file:  /tmp/bigvid.mp4.part       (sparse, written via io.WriterAt)
//	Sidecar file: /tmp/bigvid.mp4.part.resume (append-only log)
//
// The sidecar is an append-only text file. The first line is a JSON header
// containing manifest metadata (storage key, merkle root, chunk count, etc).
// Subsequent lines are bare chunk indices written with O_APPEND after each
// chunk completes — O(1) per chunk, zero write amplification.
//
// On resume boot, the daemon:
//  1. Reads the header, checks it matches the current manifest.
//  2. Parses chunk indices from the log lines.
//  3. Fast-verifies each claimed chunk by reading it from the .part file and
//     hashing it against the manifest. Only verified chunks enter the skip set.
//  4. Fetches only missing/failed chunks from the network.
//
// On completion: Merkle root verified → .resume deleted → .part renamed to
// final path. Crash-proof at every step.
package resume

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// PartPath returns the incomplete download path for a given output file.
func PartPath(outputPath string) string {
	return outputPath + ".part"
}

// SidecarPath returns the resume sidecar path for a given output file.
func SidecarPath(outputPath string) string {
	return outputPath + ".part.resume"
}

// Header is the JSON metadata written as the first line of the sidecar.
type Header struct {
	StorageKey  string `json:"storage_key"`
	TotalChunks int    `json:"total_chunks"`
	TotalSize   int64  `json:"total_size"`
	ChunkSize   int    `json:"chunk_size"`
	MerkleRoot  string `json:"merkle_root"`
	CreatedAt   int64  `json:"created_at"`
}

// Sidecar manages the append-only resume log.
type Sidecar struct {
	header     Header
	outputPath string // the user's requested final path (without .part)
	file       *os.File
}

// Create initializes a new sidecar for a fresh download. Writes the header
// as the first line and leaves the file open for appending chunk indices.
func Create(outputPath, storageKey, merkleRoot string, totalChunks int, totalSize int64, chunkSize int) (*Sidecar, error) {
	path := SidecarPath(outputPath)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return nil, fmt.Errorf("create sidecar: %w", err)
	}

	hdr := Header{
		StorageKey:  storageKey,
		TotalChunks: totalChunks,
		TotalSize:   totalSize,
		ChunkSize:   chunkSize,
		MerkleRoot:  merkleRoot,
		CreatedAt:   time.Now().UnixNano(),
	}

	data, err := json.Marshal(hdr)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("marshal sidecar header: %w", err)
	}

	if _, err := fmt.Fprintf(f, "%s\n", data); err != nil {
		f.Close()
		return nil, fmt.Errorf("write sidecar header: %w", err)
	}

	return &Sidecar{header: hdr, outputPath: outputPath, file: f}, nil
}

// Open reads an existing sidecar, parses the header and all logged chunk
// indices. Returns the Sidecar (ready for appending) and the set of chunk
// indices that were previously recorded. The caller must fast-verify these
// against the actual .part file before trusting them.
//
// Returns nil, nil, nil if the sidecar does not exist.
func Open(outputPath string) (*Sidecar, map[int]bool, error) {
	path := SidecarPath(outputPath)

	// Read the entire file first to parse, then re-open for appending.
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("read sidecar: %w", err)
	}

	scanner := bufio.NewScanner(strings.NewReader(string(data)))

	// First line: JSON header.
	if !scanner.Scan() {
		return nil, nil, fmt.Errorf("sidecar is empty")
	}
	var hdr Header
	if err := json.Unmarshal([]byte(scanner.Text()), &hdr); err != nil {
		return nil, nil, fmt.Errorf("parse sidecar header: %w", err)
	}

	// Remaining lines: chunk indices (one per line).
	claimed := make(map[int]bool)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		idx, err := strconv.Atoi(line)
		if err != nil {
			continue // skip corrupted lines
		}
		if idx >= 0 && idx < hdr.TotalChunks {
			claimed[idx] = true
		}
	}

	// Re-open for appending new chunk indices.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, nil, fmt.Errorf("reopen sidecar for append: %w", err)
	}

	return &Sidecar{header: hdr, outputPath: outputPath, file: f}, claimed, nil
}

// Header returns the sidecar's metadata header.
func (s *Sidecar) Header() Header {
	return s.header
}

// Matches checks if the sidecar is compatible with the current manifest.
func (s *Sidecar) Matches(storageKey, merkleRoot string, totalChunks int, totalSize int64) bool {
	return s.header.StorageKey == storageKey &&
		s.header.MerkleRoot == merkleRoot &&
		s.header.TotalChunks == totalChunks &&
		s.header.TotalSize == totalSize
}

// RecordChunk appends a completed chunk index to the sidecar log.
// O(1) — single append write, no serialization of the full state.
func (s *Sidecar) RecordChunk(index int) error {
	_, err := fmt.Fprintf(s.file, "%d\n", index)
	return err
}

// Close closes the sidecar file handle.
func (s *Sidecar) Close() error {
	if s.file != nil {
		return s.file.Close()
	}
	return nil
}

// Cleanup removes the sidecar file. Called after successful completion.
func Cleanup(outputPath string) error {
	path := SidecarPath(outputPath)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove sidecar: %w", err)
	}
	return nil
}
