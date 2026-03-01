package e2e_test

import (
	"fmt"
	"testing"
	"time"
)

// TestKeyRoutesToReplFactor3Nodes verifies that for 50 distinct keys each key
// maps to exactly 3 distinct responsible nodes on a 3-node cluster with RF=3.
func TestKeyRoutesToReplFactor3Nodes(t *testing.T) {
	c := newCluster(t, 3, 3)

	for i := 0; i < 50; i++ {
		key := fmt.Sprintf("routing-key-%d", i)
		nodes := c.nodes[0].HashRing.GetNodes(key, 3)
		if len(nodes) != 3 {
			t.Errorf("key %q: got %d responsible nodes, want 3", key, len(nodes))
		}
		// All returned nodes must be distinct.
		seen := map[string]bool{}
		for _, n := range nodes {
			if seen[n] {
				t.Errorf("key %q: duplicate node %q in result", key, n)
			}
			seen[n] = true
		}
	}
}

// TestAllNodesRepresentedInDistribution verifies that across 50 keys all 3
// physical nodes appear as primary responsible node at least once.
func TestAllNodesRepresentedInDistribution(t *testing.T) {
	c := newCluster(t, 3, 3)

	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		key := fmt.Sprintf("dist-key-%d", i)
		nodes := c.nodes[0].HashRing.GetNodes(key, 1)
		if len(nodes) > 0 {
			seen[nodes[0]] = true
		}
	}
	for _, addr := range c.addrs {
		if !seen[addr] {
			t.Errorf("node %s never appeared as primary for any of 50 keys", addr)
		}
	}
}

// TestReplicationFactorEnforced uploads a file and verifies that all 3 nodes
// hold the chunk in their local store (ring RF=3 guarantees every node stores it).
func TestReplicationFactorEnforced(t *testing.T) {
	c := newCluster(t, 3, 3)

	key := "rf-enforced-key"
	data := makePayload("text", 256*1024) // 256KB — single chunk
	if err := c.nodes[0].StoreData(key, readerOf(data)); err != nil {
		t.Fatalf("StoreData: %v", err)
	}

	// Allow replication to complete.
	time.Sleep(600 * time.Millisecond)

	// Each node should be able to retrieve the data.
	for i, nd := range c.nodes {
		got := mustGetData(t, nd, key)
		if sha256hex(got) != sha256hex(data) {
			t.Errorf("node[%d] returned wrong data (sha256 mismatch)", i)
		}
	}
}

// TestKeyMovementOnNodeJoin verifies that when a 4th node joins a 3-node cluster
// that already has 30 files, fewer than 50% of the keys move (consistent hashing
// guarantees ~25% movement for a 3→4 node transition).
func TestKeyMovementOnNodeJoin(t *testing.T) {
	c := newCluster(t, 3, 3)

	const nKeys = 30
	// Record primary node for each key before 4th node joins.
	before := make(map[string]string, nKeys)
	for i := 0; i < nKeys; i++ {
		key := fmt.Sprintf("churn-key-%d", i)
		nodes := c.nodes[0].HashRing.GetNodes(key, 1)
		if len(nodes) > 0 {
			before[key] = nodes[0]
		}
		// Upload each key so data is present.
		if err := c.nodes[0].StoreData(key, readerOf([]byte(key))); err != nil {
			t.Fatalf("StoreData(%q): %v", key, err)
		}
	}

	// Add 4th node.
	_ = c.addNode(c.addrs[0])
	time.Sleep(500 * time.Millisecond)

	// Count how many keys moved.
	moved := 0
	for i := 0; i < nKeys; i++ {
		key := fmt.Sprintf("churn-key-%d", i)
		nodes := c.nodes[0].HashRing.GetNodes(key, 1)
		if len(nodes) > 0 && nodes[0] != before[key] {
			moved++
		}
	}

	pct := float64(moved) / float64(nKeys) * 100
	t.Logf("keys moved after node join: %d/%d (%.1f%%)", moved, nKeys, pct)
	if pct > 50 {
		t.Errorf("too many keys moved (%.1f%%), consistent hashing should move ≤50%%", pct)
	}
}

// TestKeyMovementOnNodeLeave verifies that after a node leaves all 30 files
// are still retrievable from the remaining nodes.
func TestKeyMovementOnNodeLeave(t *testing.T) {
	c := newCluster(t, 3, 3)

	const nKeys = 30
	data := make(map[string][]byte, nKeys)
	for i := 0; i < nKeys; i++ {
		key := fmt.Sprintf("leave-key-%d", i)
		data[key] = []byte(fmt.Sprintf("payload-%d", i))
		if err := c.nodes[0].StoreData(key, readerOf(data[key])); err != nil {
			t.Fatalf("StoreData(%q): %v", key, err)
		}
	}

	time.Sleep(400 * time.Millisecond)

	// Remove node[2] gracefully.
	c.nodes[2].GracefulShutdown()
	cleanNodeDirs(c.addrs[2])
	c.nodes[2] = nil

	waitFor(t, 5*time.Second, func() bool {
		return c.nodes[0].HashRing.Size() <= 2
	}, "ring did not shrink after node leave")

	// All keys should still be fetchable from node[0] or node[1].
	for key, expected := range data {
		got := mustGetData(t, c.nodes[0], key)
		if sha256hex(got) != sha256hex(expected) {
			t.Errorf("key %q: data mismatch after node leave", key)
		}
	}
}
