// Package chunker splits files into fixed-size chunks for distributed storage.
//
// Each chunk is content-addressed by its SHA-256 hash, enabling natural
// deduplication across the cluster. A ChunkManifest records the ordered list
// of chunks so the original file can be reassembled on download.
//
// Upload flow:
//
//	chunkCh, errCh := chunker.ChunkReader(r, DefaultChunkSize)
//	for chunk := range chunkCh:
//	    // encrypt chunk, store under ChunkStorageKey(encHash)
//	    chunkInfos = append(chunkInfos, ChunkInfo{...})
//	manifest := BuildManifest(fileKey, chunkInfos, false)
//
// Download flow:
//
//	for _, info := range manifest.Chunks:
//	    // fetch from local store or responsible peer by info.EncHash
//	    // decrypt, verify sha256(plaintext) == info.Hash
//	Reassemble(chunks, dst)
package chunker

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
)

// DefaultChunkSize is 4 MiB — a good trade-off between overhead and memory
// pressure for the college LAN use-case (PDFs, videos, PPTs).
const DefaultChunkSize = 4 * 1024 * 1024 // 4 MiB

// Chunk is a single piece of a file held in memory during the upload pipeline.
// Data is the plaintext bytes before encryption.
type Chunk struct {
	Index int
	Hash  [32]byte // SHA-256 of plaintext Data
	Size  int64
	Data  []byte
}

// ChunkInfo is the durable record of one chunk stored in a ChunkManifest.
// It contains no actual data — only the hashes needed to locate and verify.
type ChunkInfo struct {
	Index      int
	Hash       string // hex SHA-256 of plaintext chunk
	Size       int64
	EncHash    string // hex SHA-256 of *encrypted* chunk (used as CAS storage key)
	Compressed bool   // true if this chunk was zstd-compressed before encryption
}

// AccessEntry records one recipient's wrapped DEK in an ECDH-encrypted manifest.
type AccessEntry struct {
	RecipientPubKey string `json:"recipient_pub_key"` // hex X25519 pub
	WrappedDEK      string `json:"wrapped_dek"`       // hex [12B nonce][ciphertext+tag]
}

// ChunkManifest is the table of contents for a chunked file.
// Stored under "manifest:<fileKey>" in the metadata store.
type ChunkManifest struct {
	FileKey    string      // original file key passed to StoreData
	TotalSize  int64       // sum of all plaintext chunk sizes
	ChunkSize  int         // nominal chunk size used at upload time
	Chunks     []ChunkInfo // ordered list; Index field is authoritative
	MerkleRoot string      // hex SHA-256 over concatenated chunk plaintext hashes
	CreatedAt  int64       // UnixNano
	Encrypted  bool        // true if file was encrypted with ECDH sharing

	// ECDH sharing fields (set when Encrypted=true).
	OwnerPubKey   string        `json:"owner_pub_key,omitempty"`    // hex X25519 pub of uploader
	OwnerEdPubKey string        `json:"owner_ed_pub_key,omitempty"` // hex Ed25519 pub of uploader
	AccessList    []AccessEntry `json:"access_list,omitempty"`      // one entry per recipient
	Signature     string        `json:"signature,omitempty"`        // hex Ed25519 signature
}

// ChunkReader reads from r in chunkSize-byte increments and sends each chunk
// on the returned channel. It runs in a goroutine; the caller iterates the
// channel until it is closed. Any read error is forwarded on errCh.
//
// The caller must drain both channels (or select on them) to avoid goroutine
// leaks. The hash in each Chunk is computed over the plaintext bytes.
func ChunkReader(r io.Reader, chunkSize int) (<-chan Chunk, <-chan error) {
	return ChunkReaderWithPool(r, chunkSize, nil)
}

// ChunkReaderWithPool is like ChunkReader but uses pool (if non-nil) to obtain
// and return the temporary read buffer, eliminating one 4 MiB allocation per
// chunk. pool.New must return *[]byte of at least chunkSize bytes.
func ChunkReaderWithPool(r io.Reader, chunkSize int, pool *sync.Pool) (<-chan Chunk, <-chan error) {
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}
	out := make(chan Chunk, 4) // small buffer so reads pipeline with processing
	errCh := make(chan error, 1)

	go func() {
		defer close(out)
		defer close(errCh)

		// Borrow the read buffer from the pool (or allocate if no pool).
		var buf []byte
		if pool != nil {
			pb := pool.Get().(*[]byte)
			if len(*pb) < chunkSize {
				*pb = make([]byte, chunkSize)
			}
			buf = (*pb)[:chunkSize]
			defer pool.Put(pb)
		} else {
			buf = make([]byte, chunkSize)
		}

		index := 0
		for {
			n, err := io.ReadFull(r, buf)
			if n > 0 {
				// Make a copy — the caller owns this slice; buf is reused.
				data := make([]byte, n)
				copy(data, buf[:n])
				hash := sha256.Sum256(data)
				out <- Chunk{
					Index: index,
					Hash:  hash,
					Size:  int64(n),
					Data:  data,
				}
				index++
			}
			if err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
					// Normal end of file — ErrUnexpectedEOF just means the last
					// chunk was smaller than chunkSize (which is expected).
					return
				}
				errCh <- fmt.Errorf("chunker: read error at chunk %d: %w", index, err)
				return
			}
		}
	}()

	return out, errCh
}

// Reassemble writes chunks to dst in ascending Index order.
// Chunks may be provided in any order; they are sorted before writing.
// All chunks must be present — a missing index returns an error.
func Reassemble(chunks []Chunk, dst io.Writer) error {
	if len(chunks) == 0 {
		return nil
	}

	// Sort by index so out-of-order parallel downloads assemble correctly.
	sorted := make([]Chunk, len(chunks))
	copy(sorted, chunks)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Index < sorted[j].Index
	})

	// Verify there are no gaps.
	for i, c := range sorted {
		if c.Index != i {
			return fmt.Errorf("reassemble: missing chunk at index %d (got %d)", i, c.Index)
		}
	}

	for _, c := range sorted {
		if _, err := dst.Write(c.Data); err != nil {
			return fmt.Errorf("reassemble: write chunk %d: %w", c.Index, err)
		}
	}
	return nil
}

// VerifyChunk returns true iff sha256(chunk.Data) == chunk.Hash.
// Call this after decryption to detect bit-rot or tampering.
func VerifyChunk(c Chunk) bool {
	computed := sha256.Sum256(c.Data)
	return computed == c.Hash
}

// ChunkStorageKey returns the namespaced CAS key used to store an encrypted
// chunk in the distributed store. The encHash is the hex SHA-256 of the
// *encrypted* bytes (computed by the caller after encryption).
//
// The "chunk:" prefix prevents collision with regular file storage keys.
func ChunkStorageKey(encHash string) string {
	return "chunk:" + encHash
}

// ManifestStorageKey returns the metadata key under which a file's
// ChunkManifest is stored.
func ManifestStorageKey(fileKey string) string {
	return "manifest:" + fileKey
}

// BuildManifest constructs a ChunkManifest from the ordered ChunkInfo slice.
// It computes a Merkle-style root hash over the plaintext chunk hashes so the
// entire manifest can be verified in one comparison.
func BuildManifest(fileKey string, chunks []ChunkInfo, createdAt int64) *ChunkManifest {
	var totalSize int64
	for _, c := range chunks {
		totalSize += c.Size
	}

	root := computeMerkleRoot(chunks)

	return &ChunkManifest{
		FileKey:    fileKey,
		TotalSize:  totalSize,
		ChunkSize:  DefaultChunkSize,
		Chunks:     chunks,
		MerkleRoot: root,
		CreatedAt:  createdAt,
	}
}

// computeMerkleRoot builds a SHA-256 Merkle root over the ordered chunk
// plaintext hashes. Deterministic: same chunk set → same root.
func computeMerkleRoot(chunks []ChunkInfo) string {
	if len(chunks) == 0 {
		var zero [32]byte
		return hex.EncodeToString(zero[:])
	}

	// Leaf layer: decode hex hashes.
	nodes := make([][32]byte, len(chunks))
	for i, c := range chunks {
		b, err := hex.DecodeString(c.Hash)
		if err != nil || len(b) != 32 {
			// Fallback: hash the raw string.
			nodes[i] = sha256.Sum256([]byte(c.Hash))
		} else {
			copy(nodes[i][:], b)
		}
	}

	// Build tree bottom-up, duplicating the last node when the level is odd.
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
