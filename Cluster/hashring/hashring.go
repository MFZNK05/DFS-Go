package hashring

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"
	"sync"
)

const (
	DefaultVirtualNodes     = 150
	DefaultReplicationFactor = 3
)

// HashRing implements consistent hashing with virtual nodes.
// It maps keys to a set of N physical nodes on a ring of uint64 positions.
type HashRing struct {
	mu           sync.RWMutex
	sortedHashes []uint64            // sorted ring positions
	hashToNode   map[uint64]string   // ring position -> physical node address
	nodeToHashes map[string][]uint64 // physical node -> its virtual node positions
	virtualNodes int
	replFactor   int
}

// Config holds configuration for creating a new HashRing.
type Config struct {
	VirtualNodes      int
	ReplicationFactor int
}

// New creates a new HashRing with the given configuration.
// If cfg is nil, defaults are used (150 virtual nodes, replication factor 3).
func New(cfg *Config) *HashRing {
	vn := DefaultVirtualNodes
	rf := DefaultReplicationFactor

	if cfg != nil {
		if cfg.VirtualNodes > 0 {
			vn = cfg.VirtualNodes
		}
		if cfg.ReplicationFactor > 0 {
			rf = cfg.ReplicationFactor
		}
	}

	return &HashRing{
		sortedHashes: make([]uint64, 0),
		hashToNode:   make(map[uint64]string),
		nodeToHashes: make(map[string][]uint64),
		virtualNodes: vn,
		replFactor:   rf,
	}
}

// AddNode places a physical node on the ring with virtual node copies.
// Each virtual node is hashed as "<addr>:vnode:<i>" to spread load evenly.
func (hr *HashRing) AddNode(addr string) {
	hr.mu.Lock()
	defer hr.mu.Unlock()

	if _, exists := hr.nodeToHashes[addr]; exists {
		return // already on the ring
	}

	hashes := make([]uint64, 0, hr.virtualNodes)
	for i := 0; i < hr.virtualNodes; i++ {
		h := hashKey(fmt.Sprintf("%s:vnode:%d", addr, i))
		hr.sortedHashes = append(hr.sortedHashes, h)
		hr.hashToNode[h] = addr
		hashes = append(hashes, h)
	}
	hr.nodeToHashes[addr] = hashes

	sort.Slice(hr.sortedHashes, func(i, j int) bool {
		return hr.sortedHashes[i] < hr.sortedHashes[j]
	})
}

// RemoveNode removes a physical node and all its virtual nodes from the ring.
func (hr *HashRing) RemoveNode(addr string) {
	hr.mu.Lock()
	defer hr.mu.Unlock()

	hashes, exists := hr.nodeToHashes[addr]
	if !exists {
		return
	}

	// Build a set for O(1) lookup
	removeSet := make(map[uint64]struct{}, len(hashes))
	for _, h := range hashes {
		removeSet[h] = struct{}{}
		delete(hr.hashToNode, h)
	}
	delete(hr.nodeToHashes, addr)

	// Rebuild sorted hashes excluding removed ones
	newSorted := make([]uint64, 0, len(hr.sortedHashes)-len(removeSet))
	for _, h := range hr.sortedHashes {
		if _, removed := removeSet[h]; !removed {
			newSorted = append(newSorted, h)
		}
	}
	hr.sortedHashes = newSorted
}

// GetNodes returns up to n distinct physical nodes responsible for the given key.
// It walks clockwise from the key's hash position on the ring, skipping
// duplicate physical nodes (from virtual node overlap).
// Returns fewer than n nodes if fewer physical nodes exist on the ring.
func (hr *HashRing) GetNodes(key string, n int) []string {
	hr.mu.RLock()
	defer hr.mu.RUnlock()

	if len(hr.sortedHashes) == 0 {
		return nil
	}

	totalPhysical := len(hr.nodeToHashes)
	if n > totalPhysical {
		n = totalPhysical
	}

	keyHash := hashKey(key)
	startIdx := hr.search(keyHash)

	nodes := make([]string, 0, n)
	seen := make(map[string]struct{}, n)

	ringLen := len(hr.sortedHashes)
	for i := 0; i < ringLen && len(nodes) < n; i++ {
		idx := (startIdx + i) % ringLen
		addr := hr.hashToNode[hr.sortedHashes[idx]]

		if _, ok := seen[addr]; !ok {
			seen[addr] = struct{}{}
			nodes = append(nodes, addr)
		}
	}

	return nodes
}

// GetPrimaryNode returns the single primary node responsible for the key.
func (hr *HashRing) GetPrimaryNode(key string) string {
	nodes := hr.GetNodes(key, 1)
	if len(nodes) == 0 {
		return ""
	}
	return nodes[0]
}

// HasNode returns true if the given address is on the ring.
func (hr *HashRing) HasNode(addr string) bool {
	hr.mu.RLock()
	defer hr.mu.RUnlock()
	_, exists := hr.nodeToHashes[addr]
	return exists
}

// Members returns all physical node addresses currently on the ring.
func (hr *HashRing) Members() []string {
	hr.mu.RLock()
	defer hr.mu.RUnlock()

	members := make([]string, 0, len(hr.nodeToHashes))
	for addr := range hr.nodeToHashes {
		members = append(members, addr)
	}
	return members
}

// Size returns the number of physical nodes on the ring.
func (hr *HashRing) Size() int {
	hr.mu.RLock()
	defer hr.mu.RUnlock()
	return len(hr.nodeToHashes)
}

// ReplicationFactor returns the configured replication factor.
func (hr *HashRing) ReplicationFactor() int {
	return hr.replFactor
}

// search finds the first ring position >= the given hash using binary search.
// If no position is >= hash, it wraps around to index 0 (the ring is circular).
func (hr *HashRing) search(hash uint64) int {
	idx := sort.Search(len(hr.sortedHashes), func(i int) bool {
		return hr.sortedHashes[i] >= hash
	})
	if idx >= len(hr.sortedHashes) {
		return 0 // wrap around
	}
	return idx
}

// hashKey computes a uint64 position on the ring from a string key.
// Uses SHA-256 and takes the first 8 bytes as big-endian uint64.
func hashKey(key string) uint64 {
	h := sha256.Sum256([]byte(key))
	return binary.BigEndian.Uint64(h[:8])
}
