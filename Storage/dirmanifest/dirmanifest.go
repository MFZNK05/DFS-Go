// Package dirmanifest manages directory-level manifests for multi-file uploads.
//
// A directory upload is N individual file uploads wrapped in a DirectoryManifest
// that records relative paths, sizes, and per-file manifest keys.  Each file
// gets its own ChunkManifest as usual; the DirectoryManifest is the table of
// contents that binds them together.
//
// Security: On download, relative paths are sanitized with filepath.Clean and
// bound-checked to prevent path traversal attacks (e.g., ../../etc/passwd).
package dirmanifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Faizan2005/DFS-Go/Storage/chunker"
)

// FileEntry records one file within a directory upload.
type FileEntry struct {
	RelativePath string      `json:"relative_path"` // slash-separated, rooted at dir
	Size         int64       `json:"size"`
	Mode         fs.FileMode `json:"mode"`
	ManifestKey  string      `json:"manifest_key"` // storage key for this file's ChunkManifest
	FileHash     string      `json:"file_hash"`    // SHA-256 of the entire plaintext file
}

// DirectoryManifest is the table of contents for a directory upload.
type DirectoryManifest struct {
	StorageKey string      `json:"storage_key"`
	TotalSize  int64       `json:"total_size"`
	FileCount  int         `json:"file_count"`
	Files      []FileEntry `json:"files"`
	MerkleRoot string      `json:"merkle_root"` // SHA-256 merkle over file hashes
	CreatedAt  int64       `json:"created_at"`
	IsDirectory bool       `json:"is_directory"` // always true

	// ECDH sharing fields (set when Encrypted=true).
	Encrypted     bool                  `json:"encrypted"`
	OwnerPubKey   string                `json:"owner_pub_key,omitempty"`
	OwnerEdPubKey string                `json:"owner_ed_pub_key,omitempty"`
	AccessList    []chunker.AccessEntry `json:"access_list,omitempty"`
	Signature     string                `json:"signature,omitempty"`
}

// Walk scans dirPath recursively and returns a sorted list of FileEntry.
// Symlinks are skipped. Empty directories are skipped.
func Walk(dirPath string) ([]FileEntry, error) {
	dirPath = filepath.Clean(dirPath)
	info, err := os.Stat(dirPath)
	if err != nil {
		return nil, fmt.Errorf("dirmanifest: stat: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("dirmanifest: %s is not a directory", dirPath)
	}

	var entries []FileEntry
	err = filepath.WalkDir(dirPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Skip symlinks entirely.
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		// Skip directories (we only record files).
		if d.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(dirPath, path)
		if err != nil {
			return err
		}
		// Normalize to slash-separated paths for cross-platform consistency.
		rel = filepath.ToSlash(rel)

		fi, err := d.Info()
		if err != nil {
			return err
		}

		entries = append(entries, FileEntry{
			RelativePath: rel,
			Size:         fi.Size(),
			Mode:         fi.Mode(),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("dirmanifest: walk: %w", err)
	}

	// Sort by relative path for deterministic ordering.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].RelativePath < entries[j].RelativePath
	})

	return entries, nil
}

// SafeOutputPath validates and returns a safe absolute path for a file entry
// within the output directory.  Returns an error if the path would escape
// outputDir (path traversal attack).
func SafeOutputPath(outputDir string, relativePath string) (string, error) {
	// Clean the relative path and reject absolute paths.
	cleaned := filepath.Clean(relativePath)
	if filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("dirmanifest: absolute path rejected: %s", relativePath)
	}
	// Reject paths that escape the output directory.
	if strings.HasPrefix(cleaned, "..") {
		return "", fmt.Errorf("dirmanifest: path traversal rejected: %s", relativePath)
	}

	full := filepath.Join(outputDir, filepath.FromSlash(cleaned))

	// Double-check: the result must be under outputDir.
	absOut, _ := filepath.Abs(outputDir)
	absFull, _ := filepath.Abs(full)
	if !strings.HasPrefix(absFull, absOut+string(filepath.Separator)) && absFull != absOut {
		return "", fmt.Errorf("dirmanifest: path traversal rejected: %s resolves to %s", relativePath, absFull)
	}

	return full, nil
}

// ComputeMerkleRoot builds a SHA-256 merkle root over the file hashes.
func ComputeMerkleRoot(files []FileEntry) string {
	if len(files) == 0 {
		var zero [32]byte
		return hex.EncodeToString(zero[:])
	}

	nodes := make([][32]byte, len(files))
	for i, f := range files {
		b, err := hex.DecodeString(f.FileHash)
		if err != nil || len(b) != 32 {
			nodes[i] = sha256.Sum256([]byte(f.FileHash))
		} else {
			copy(nodes[i][:], b)
		}
	}

	for len(nodes) > 1 {
		var next [][32]byte
		for i := 0; i < len(nodes); i += 2 {
			left := nodes[i]
			right := nodes[i]
			if i+1 < len(nodes) {
				right = nodes[i+1]
			}
			combined := make([]byte, 64)
			copy(combined[:32], left[:])
			copy(combined[32:], right[:])
			next = append(next, sha256.Sum256(combined))
		}
		nodes = next
	}

	return hex.EncodeToString(nodes[0][:])
}

// StorageKeyPrefix returns the metadata key prefix for directory manifests.
func StorageKeyPrefix() string {
	return "dirmanifest:"
}

// StorageKey returns the metadata key for a directory manifest.
func StorageKey(key string) string {
	return StorageKeyPrefix() + key
}

// Marshal serializes a DirectoryManifest to JSON.
func Marshal(dm *DirectoryManifest) ([]byte, error) {
	return json.Marshal(dm)
}

// Unmarshal deserializes a DirectoryManifest from JSON.
func Unmarshal(data []byte) (*DirectoryManifest, error) {
	var dm DirectoryManifest
	if err := json.Unmarshal(data, &dm); err != nil {
		return nil, err
	}
	return &dm, nil
}
