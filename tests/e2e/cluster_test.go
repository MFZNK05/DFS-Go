package e2e_test

import (
	"testing"
	"time"

	"github.com/Faizan2005/DFS-Go/Cluster/membership"
	server "github.com/Faizan2005/DFS-Go/Server"
)

// TestClusterForms3Nodes verifies that 3 nodes bootstrapping from each other
// all end up seeing 2 peers and a ring of size 3.
func TestClusterForms3Nodes(t *testing.T) {
	c := newCluster(t, 3, 3)

	for i, nd := range c.nodes {
		size := nd.HashRing.Size()
		if size != 3 {
			t.Errorf("node[%d] ring size = %d, want 3", i, size)
		}
		pc := peerCountHTTP(t, healthAddr(c.addrs[i]))
		if pc != 2 {
			t.Errorf("node[%d] peer_count = %d, want 2", i, pc)
		}
	}
}

// TestGossipPropagatesNewNode starts a 3-node cluster, then brings up a 4th
// node bootstrapped only to node[0]. Gossip should carry the new node's
// address to node[1] and node[2] within a few seconds.
func TestGossipPropagatesNewNode(t *testing.T) {
	c := newCluster(t, 3, 3)

	node4addr := freePort(t)
	node4 := server.MakeServer(node4addr, 3, c.addrs[0])
	go func() { _ = node4.Run() }()
	t.Cleanup(func() {
		node4.GracefulShutdown()
		cleanNodeDirs(node4addr)
	})

	// node[1] and node[2] should discover node4 via gossip within 5s.
	waitFor(t, 8*time.Second, func() bool {
		return c.nodes[1].HashRing.Size() >= 4 && c.nodes[2].HashRing.Size() >= 4
	}, "gossip did not propagate node4 to all peers")

	if s := node4.HashRing.Size(); s < 2 {
		t.Errorf("node4 ring size = %d, want >= 2 (knows at least node0 + self)", s)
	}
}

// TestNodeRejoinAfterGracefulLeave stops node[2] gracefully and verifies the
// ring shrinks, then restarts it and verifies the ring grows back.
func TestNodeRejoinAfterGracefulLeave(t *testing.T) {
	c := newCluster(t, 3, 3)

	// Take node[2] down.
	leaving := c.nodes[2]
	leavingAddr := c.addrs[2]
	leaving.GracefulShutdown()
	cleanNodeDirs(leavingAddr)

	waitFor(t, 6*time.Second, func() bool {
		return c.nodes[0].HashRing.Size() <= 2
	}, "ring did not shrink after node[2] left")

	// Restart node[2] with same address, bootstrap from node[0].
	rejoined := server.MakeServer(leavingAddr, 3, c.addrs[0])
	go func() { _ = rejoined.Run() }()
	t.Cleanup(func() {
		rejoined.GracefulShutdown()
		cleanNodeDirs(leavingAddr)
	})
	// Replace in cluster slice so teardown works.
	c.nodes[2] = rejoined

	waitFor(t, 8*time.Second, func() bool {
		return c.nodes[0].HashRing.Size() >= 3 && rejoined.HashRing.Size() >= 2
	}, "ring did not recover after node[2] rejoined")
}

// TestMembershipStateAfterLeave verifies that after a graceful shutdown the
// remaining nodes mark the departed node as StateSuspect or StateLeft in the
// cluster state (not still StateAlive).
func TestMembershipStateAfterLeave(t *testing.T) {
	c := newCluster(t, 3, 3)

	leavingAddr := c.addrs[2]
	c.nodes[2].GracefulShutdown()
	cleanNodeDirs(leavingAddr)
	// Remove from cleanup slice so double-shutdown is avoided.
	c.nodes[2] = nil

	waitFor(t, 6*time.Second, func() bool {
		info, ok := c.nodes[0].Cluster.GetNode(leavingAddr)
		if !ok {
			return false
		}
		return info.State != membership.StateAlive
	}, "cluster state did not mark departed node as non-alive")

	info, ok := c.nodes[0].Cluster.GetNode(leavingAddr)
	if !ok {
		t.Skip("departed node not tracked (acceptable if gossip hasn't seen it)")
	}
	if info.State == membership.StateAlive {
		t.Errorf("expected departed node state != Alive, got %v", info.State)
	}
}
