package e2e_test

import (
	"fmt"
	"io"
	"testing"
	"time"

	server "github.com/Faizan2005/DFS-Go/Server"
)

// TestGetDataWithOnePeerDown verifies that with RF=3 a GetData succeeds even
// when one of the three nodes is stopped.
func TestGetDataWithOnePeerDown(t *testing.T) {
	c := newCluster(t, 3, 3)

	key := "fault-one-down-key"
	data := makePayload("text", 256*1024)

	if err := c.nodes[0].StoreData(key, readerOf(data)); err != nil {
		t.Fatalf("StoreData: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	// Stop node[2] — simulates a crash or clean departure.
	c.nodes[2].GracefulShutdown()
	cleanNodeDirs(c.addrs[2])
	c.nodes[2] = nil

	// Wait for the ring to detect the departure.
	waitFor(t, 8*time.Second, func() bool {
		return c.nodes[0].HashRing.Size() <= 2
	}, "ring did not shrink after node stop")

	// node[0] and node[1] should still serve the file.
	for i, nd := range []*server.Server{c.nodes[0], c.nodes[1]} {
		got := mustGetData(t, nd, key)
		if sha256hex(got) != sha256hex(data) {
			t.Errorf("node[%d]: data mismatch after peer down", i)
		}
	}
}

// TestHintedHandoffStoresHintForDeadNode verifies that when a target node is
// unreachable during a write, data is still accessible on the surviving nodes.
func TestHintedHandoffStoresHintForDeadNode(t *testing.T) {
	c := newCluster(t, 3, 3)

	// Stop node[2] before the upload.
	deadAddr := c.addrs[2]
	c.nodes[2].GracefulShutdown()
	cleanNodeDirs(deadAddr)
	c.nodes[2] = nil

	waitFor(t, 8*time.Second, func() bool {
		return c.nodes[0].HashRing.Size() <= 2
	}, "ring did not shrink")

	key := "hint-handoff-key"
	data := makePayload("text", 64*1024)

	// Store while node[2] is down — should succeed via W=2 quorum.
	_ = c.nodes[0].StoreData(key, readerOf(data))

	// The data must be retrievable from the surviving nodes.
	for i, nd := range []*server.Server{c.nodes[0], c.nodes[1]} {
		got := mustGetData(t, nd, key)
		if sha256hex(got) != sha256hex(data) {
			t.Errorf("node[%d]: data not intact after hint store", i)
		}
	}
}

// TestHintDeliveredOnNodeReconnect uploads a file with one node down, then
// brings the node back up and verifies it receives the data.
func TestHintDeliveredOnNodeReconnect(t *testing.T) {
	c := newCluster(t, 3, 3)

	deadAddr := c.addrs[2]
	c.nodes[2].GracefulShutdown()
	cleanNodeDirs(deadAddr)
	c.nodes[2] = nil

	waitFor(t, 8*time.Second, func() bool {
		return c.nodes[0].HashRing.Size() <= 2
	}, "ring did not shrink after stopping node[2]")

	key := "hint-deliver-key"
	data := makePayload("text", 64*1024)

	if err := c.nodes[0].StoreData(key, readerOf(data)); err != nil {
		t.Fatalf("StoreData with node[2] down: %v", err)
	}

	// Restart on the same address, bootstrapped off node[0].
	newNode := server.MakeServer(deadAddr, 3, c.addrs[0])
	go func() { _ = newNode.Run() }()
	c.nodes = append(c.nodes, newNode)
	c.addrs = append(c.addrs, deadAddr)

	waitFor(t, 8*time.Second, func() bool {
		return peerCountHTTP(t, healthAddr(deadAddr)) >= 1
	}, "restarted node did not connect")

	// Wait for hint delivery / gossip replication to propagate.
	waitFor(t, 15*time.Second, func() bool {
		r, err := newNode.GetData(key)
		if err != nil {
			return false
		}
		got, err := io.ReadAll(r)
		if err != nil {
			return false
		}
		return sha256hex(got) == sha256hex(data)
	}, "hint was not delivered to restarted node")
}

// TestRebalanceOnNodeJoin verifies that after a 4th node joins the cluster
// the ring has size 4 and all previously uploaded keys are still fetchable.
func TestRebalanceOnNodeJoin(t *testing.T) {
	c := newCluster(t, 3, 3)

	const nKeys = 20
	payloads := make(map[string][]byte, nKeys)
	for i := 0; i < nKeys; i++ {
		key := fmt.Sprintf("rebalance-key-%d", i)
		payloads[key] = makePayload("text", 16*1024)
		if err := c.nodes[0].StoreData(key, readerOf(payloads[key])); err != nil {
			t.Fatalf("StoreData(%q): %v", key, err)
		}
	}
	time.Sleep(400 * time.Millisecond)

	// Add 4th node.
	node4 := c.addNode(c.addrs[0])
	_ = node4

	waitFor(t, 10*time.Second, func() bool {
		return c.nodes[0].HashRing.Size() >= 4
	}, "ring did not grow to size 4 after node join")

	// All keys must still be fetchable.
	for key, expected := range payloads {
		got := mustGetData(t, c.nodes[0], key)
		if sha256hex(got) != sha256hex(expected) {
			t.Errorf("key %q: data mismatch after rebalance", key)
		}
	}
}

// TestReplicateAfterCrash stores a file with RF=3, kills node[2],
// then verifies the remaining 2 nodes still serve the file.
func TestReplicateAfterCrash(t *testing.T) {
	c := newCluster(t, 3, 3)

	key := "crash-rereplicate-key"
	data := makePayload("binary", 128*1024)

	if err := c.nodes[0].StoreData(key, readerOf(data)); err != nil {
		t.Fatalf("StoreData: %v", err)
	}
	time.Sleep(400 * time.Millisecond)

	c.nodes[2].GracefulShutdown()
	cleanNodeDirs(c.addrs[2])
	c.nodes[2] = nil

	waitFor(t, 8*time.Second, func() bool {
		return c.nodes[0].HashRing.Size() <= 2
	}, "ring did not shrink after crash")

	for i, nd := range []*server.Server{c.nodes[0], c.nodes[1]} {
		got := mustGetData(t, nd, key)
		if sha256hex(got) != sha256hex(data) {
			t.Errorf("node[%d] data mismatch after crash", i)
		}
	}
}

// TestMultipleNodeFailures stores a 3-replica file, kills nodes 1 and 2,
// and verifies node 0 still has the data locally.
func TestMultipleNodeFailures(t *testing.T) {
	c := newCluster(t, 3, 3)

	key := "multi-fail-key"
	data := makePayload("text", 64*1024)

	if err := c.nodes[0].StoreData(key, readerOf(data)); err != nil {
		t.Fatalf("StoreData: %v", err)
	}
	time.Sleep(400 * time.Millisecond)

	for _, i := range []int{1, 2} {
		c.nodes[i].GracefulShutdown()
		cleanNodeDirs(c.addrs[i])
		c.nodes[i] = nil
	}

	got := mustGetData(t, c.nodes[0], key)
	if sha256hex(got) != sha256hex(data) {
		t.Errorf("node[0] lost data after two peer crashes")
	}
}

// TestRingRecoveryAfterSequentialLeave stores data, removes nodes one at a
// time, and confirms the last standing node still holds the data.
func TestRingRecoveryAfterSequentialLeave(t *testing.T) {
	c := newCluster(t, 3, 3)

	const nFiles = 5
	payloads := make(map[string][]byte, nFiles)
	for i := 0; i < nFiles; i++ {
		key := fmt.Sprintf("seqleave-key-%d", i)
		payloads[key] = makePayload("text", 32*1024)
		if err := c.nodes[0].StoreData(key, readerOf(payloads[key])); err != nil {
			t.Fatalf("StoreData(%q): %v", key, err)
		}
	}
	time.Sleep(400 * time.Millisecond)

	// Remove node[2], then node[1].
	for _, i := range []int{2, 1} {
		c.nodes[i].GracefulShutdown()
		cleanNodeDirs(c.addrs[i])
		c.nodes[i] = nil
		time.Sleep(200 * time.Millisecond)
	}

	// node[0] must still have all files.
	for key, expected := range payloads {
		got := mustGetData(t, c.nodes[0], key)
		if sha256hex(got) != sha256hex(expected) {
			t.Errorf("key %q: data mismatch after sequential leave", key)
		}
	}
}
