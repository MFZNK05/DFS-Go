// Package swarm provides data structures for the hybrid swarm architecture.
//
// In swarm mode, the hash ring stores only metadata (manifests, provider lists).
// Payload chunks are served on-demand from the original file (Seed In Place)
// or from CAS cache (secondary seeders). Provider records track which nodes
// can serve chunks for each file.
package swarm

import "time"

// SeedSource describes how the local node can serve chunks for a file.
type SeedSource struct {
	// OriginalPath is set only on the uploading node. Points to the file
	// as it sits on disk — chunks are read directly from here on demand.
	OriginalPath string `json:"original_path,omitempty"`

	// CASBacked is true when this node has chunks in CAS storage
	// (i.e., it downloaded them and cached them for re-seeding).
	CASBacked bool `json:"cas_backed,omitempty"`

	// ChunkSize is the nominal chunk size (needed for seek offset calc).
	ChunkSize int `json:"chunk_size"`

	// TotalChunks is the total number of chunks in the file.
	TotalChunks int `json:"total_chunks"`

	// Bitfield tracks which chunks this node has. Bit-packed:
	// chunk i is available if byte[i/8] & (1 << (i%8)) != 0.
	// For the original seeder, all bits are set.
	Bitfield []byte `json:"bitfield"`
}

// ProviderRecord is stored by hash ring nodes to track who can serve a file.
type ProviderRecord struct {
	Addr         string `json:"addr"`
	ChunkCount   int    `json:"chunk_count"`
	TotalChunks  int    `json:"total_chunks"`
	RegisteredAt int64  `json:"registered_at"` // UnixNano
}

// ProviderTTL is how long a provider record stays valid without re-registration.
const ProviderTTL = 24 * time.Hour

// ReregistrationInterval is how often nodes re-register their seed sources.
const ReregistrationInterval = 10 * time.Minute

// ---------------------------------------------------------------------------
// Bitfield helpers
// ---------------------------------------------------------------------------

// NewBitfield allocates a zeroed bitfield for totalChunks.
func NewBitfield(totalChunks int) []byte {
	return make([]byte, (totalChunks+7)/8)
}

// NewFullBitfield allocates a bitfield with all bits set for totalChunks.
func NewFullBitfield(totalChunks int) []byte {
	bf := make([]byte, (totalChunks+7)/8)
	for i := range bf {
		bf[i] = 0xFF
	}
	// Clear trailing bits beyond totalChunks.
	if rem := totalChunks % 8; rem != 0 {
		bf[len(bf)-1] = (1 << rem) - 1
	}
	return bf
}

// SetBit marks chunk index as available.
func SetBit(bf []byte, index int) {
	if index < 0 || index/8 >= len(bf) {
		return
	}
	bf[index/8] |= 1 << (index % 8)
}

// ClearBit marks chunk index as unavailable.
func ClearBit(bf []byte, index int) {
	if index < 0 || index/8 >= len(bf) {
		return
	}
	bf[index/8] &^= 1 << (index % 8)
}

// HasBit returns true if chunk index is available.
func HasBit(bf []byte, index int) bool {
	if index < 0 || index/8 >= len(bf) {
		return false
	}
	return bf[index/8]&(1<<(index%8)) != 0
}

// CountBits returns the number of set bits (available chunks).
func CountBits(bf []byte) int {
	count := 0
	for _, b := range bf {
		count += popcount(b)
	}
	return count
}

// AllSet returns true if all bits up to totalChunks are set.
func AllSet(bf []byte, totalChunks int) bool {
	if len(bf) == 0 && totalChunks > 0 {
		return false
	}
	return CountBits(bf) >= totalChunks
}

// popcount returns the number of set bits in a byte.
func popcount(b byte) int {
	count := 0
	for b != 0 {
		count++
		b &= b - 1 // clear lowest set bit
	}
	return count
}
