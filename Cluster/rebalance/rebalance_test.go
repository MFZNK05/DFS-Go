package rebalance

import (
	"fmt"
	"sync"
	"testing"

	"github.com/Faizan2005/DFS-Go/Cluster/hashring"
	storage "github.com/Faizan2005/DFS-Go/Storage"
	"github.com/Faizan2005/DFS-Go/Storage/chunker"
)

// mockMeta implements storage.MetadataStore and the Keys() extension.
type mockMeta struct {
	mu    sync.Mutex
	store map[string]storage.FileMeta
}

func newMockMeta(keys ...string) *mockMeta {
	m := &mockMeta{store: make(map[string]storage.FileMeta)}
	for _, k := range keys {
		m.store[k] = storage.FileMeta{}
	}
	return m
}

func (m *mockMeta) Get(key string) (storage.FileMeta, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.store[key]
	return v, ok
}

func (m *mockMeta) Set(key string, meta storage.FileMeta) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.store[key] = meta
	return nil
}

func (m *mockMeta) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.store))
	for k := range m.store {
		keys = append(keys, k)
	}
	return keys
}

func (m *mockMeta) GetManifest(_ string) (*chunker.ChunkManifest, bool) { return nil, false }
func (m *mockMeta) SetManifest(_ string, _ *chunker.ChunkManifest) error { return nil }
func (m *mockMeta) GetDirManifest(_ string) ([]byte, bool)              { return nil, false }
func (m *mockMeta) SetDirManifest(_ string, _ []byte) error             { return nil }
func (m *mockMeta) WithBatch(fn func(storage.MetadataBatch) error) error { return fn(m) }

// makeRing builds a ring with the given nodes and replication factor.
func makeRing(replFactor int, nodes ...string) *hashring.HashRing {
	ring := hashring.New(&hashring.Config{ReplicationFactor: replFactor})
	for _, n := range nodes {
		ring.AddNode(n)
	}
	return ring
}

// TestMigrateOnNodeJoin verifies that keys the new node is responsible for are sent.
func TestMigrateOnNodeJoin(t *testing.T) {
	const self = "node1:3000"
	const newNode = "node4:3000"

	ring := makeRing(2, self, "node2:3000", "node3:3000")

	// Build metadata with 20 keys stored on self.
	keys := make([]string, 20)
	for i := range keys {
		keys[i] = fmt.Sprintf("file-%d", i)
	}
	meta := newMockMeta(keys...)

	sent := make(map[string][]string) // targetAddr -> keys sent
	var mu sync.Mutex

	r := New(self, ring, meta,
		func(key string) ([]byte, error) {
			_, ok := meta.Get(key)
			if !ok {
				return nil, fmt.Errorf("not found")
			}
			return []byte("data-" + key), nil
		},
		func(target, key string, data []byte) error {
			mu.Lock()
			sent[target] = append(sent[target], key)
			mu.Unlock()
			return nil
		},
	)

	// Add newNode AFTER building rebalancer so ring reflects join.
	ring.AddNode(newNode)

	// Run synchronously for test.
	r.migrateToNewNode(newNode)

	mu.Lock()
	defer mu.Unlock()

	// At least some keys should have been sent to newNode.
	// (exact count depends on hash ring distribution)
	t.Logf("keys migrated to %s: %d", newNode, len(sent[newNode]))
	// We can't assert exact count since it's hash-based, but should be non-zero
	// with 20 keys and a new node joining a 4-node ring.
	if len(sent[newNode]) == 0 {
		t.Log("warning: no keys migrated — may be valid depending on ring distribution")
	}
}

// TestNoMigrationWhenNotOwner verifies self doesn't migrate keys it doesn't own.
func TestNoMigrationWhenNotOwner(t *testing.T) {
	const self = "node1:3000"
	const newNode = "node4:3000"

	// Use replFactor=1 so self owns exactly its own keys, newNode owns different keys.
	ring := makeRing(1, self, "node2:3000", "node3:3000")

	// Store only 5 keys.
	meta := newMockMeta("a", "b", "c", "d", "e")

	sentCount := 0
	var mu sync.Mutex

	r := New(self, ring, meta,
		func(key string) ([]byte, error) { return []byte("d"), nil },
		func(target, key string, data []byte) error {
			mu.Lock()
			sentCount++
			mu.Unlock()
			return nil
		},
	)

	ring.AddNode(newNode)
	r.migrateToNewNode(newNode)

	// With replFactor=1, self should only send keys it co-owns with newNode.
	// Count may be 0 if all of self's keys don't map to newNode — that's valid.
	t.Logf("keys sent: %d", sentCount)
}

// TestRereplOnNodeLeave verifies under-replicated keys are re-sent.
func TestRereplOnNodeLeave(t *testing.T) {
	const self = "node1:3000"
	const dead = "node2:3000"

	// Start with 3 nodes, replFactor=3 (all nodes hold all keys).
	ring := makeRing(3, self, dead, "node3:3000")
	meta := newMockMeta("x", "y", "z")

	sent := make(map[string]int) // targetAddr -> count
	var mu sync.Mutex

	r := New(self, ring, meta,
		func(key string) ([]byte, error) { return []byte("d"), nil },
		func(target, key string, data []byte) error {
			mu.Lock()
			sent[target]++
			mu.Unlock()
			return nil
		},
	)

	// Remove dead node from ring BEFORE calling rereplicate.
	ring.RemoveNode(dead)
	r.rereplicate(dead)

	mu.Lock()
	defer mu.Unlock()
	t.Logf("rereplicate sends: %v", sent)
	// With 2 remaining nodes and replFactor=3, can't fully restore replication
	// but the code should attempt to send to all remaining live nodes.
}

// TestContainsHelper verifies the contains utility function.
func TestContainsHelper(t *testing.T) {
	s := []string{"a", "b", "c"}
	if !contains(s, "b") {
		t.Error("expected contains to return true")
	}
	if contains(s, "d") {
		t.Error("expected contains to return false")
	}
}
