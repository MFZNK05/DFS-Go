package membership

import (
	"testing"
)

// TestUpdateStateIgnoresStale verifies that a lower-generation update is rejected.
func TestUpdateStateIgnoresStale(t *testing.T) {
	cs := New("self")
	cs.AddNode("peer1", nil)

	// Advance to gen 3.
	cs.UpdateState("peer1", StateSuspect, 2)
	cs.UpdateState("peer1", StateDead, 3)

	// Try to apply an old gen=2 update.
	applied := cs.UpdateState("peer1", StateAlive, 2)
	if applied {
		t.Fatal("stale update (gen=2 < current gen=3) should have been rejected")
	}

	n, _ := cs.GetNode("peer1")
	if n.State != StateDead {
		t.Errorf("expected state=Dead after stale update ignored, got %s", n.State)
	}
}

// TestUpdateStateAcceptsNewer verifies that a higher-generation update is accepted
// and the onChange callback fires.
func TestUpdateStateAcceptsNewer(t *testing.T) {
	cs := New("self")
	cs.AddNode("peer1", nil)

	fired := false
	cs.OnChange(func(addr string, oldState, newState NodeState) {
		if addr == "peer1" && newState == StateSuspect {
			fired = true
		}
	})

	applied := cs.UpdateState("peer1", StateSuspect, 2)
	if !applied {
		t.Fatal("newer update (gen=2 > gen=1) should have been applied")
	}

	n, _ := cs.GetNode("peer1")
	if n.State != StateSuspect {
		t.Errorf("expected StateSuspect, got %s", n.State)
	}
	if !fired {
		t.Error("onChange callback should have fired")
	}
}

// TestMergeReturnsMissingAddrs verifies that Merge identifies addresses
// not present locally or with lower generations.
func TestMergeReturnsMissingAddrs(t *testing.T) {
	cs := New("self")
	cs.AddNode("known", nil)

	digests := []GossipDigest{
		{Addr: "known", State: StateSuspect, Generation: 5}, // newer — should request
		{Addr: "unknown", State: StateAlive, Generation: 1}, // not in local state
	}

	needFull := cs.Merge(digests)
	if len(needFull) != 2 {
		t.Fatalf("expected 2 addrs needing full info, got %d: %v", len(needFull), needFull)
	}
}

// TestAliveNodesFilters verifies that Dead/Suspect nodes are excluded.
func TestAliveNodesFilters(t *testing.T) {
	cs := New("self")
	cs.AddNode("alive1", nil)
	cs.AddNode("suspect1", nil)
	cs.AddNode("dead1", nil)

	cs.UpdateState("suspect1", StateSuspect, 2)
	cs.UpdateState("dead1", StateDead, 2)

	alive := cs.AliveNodes()

	// Should contain "self" and "alive1" only.
	aliveSet := make(map[string]bool)
	for _, a := range alive {
		aliveSet[a] = true
	}

	if !aliveSet["self"] {
		t.Error("self should be in AliveNodes")
	}
	if !aliveSet["alive1"] {
		t.Error("alive1 should be in AliveNodes")
	}
	if aliveSet["suspect1"] {
		t.Error("suspect1 should not be in AliveNodes")
	}
	if aliveSet["dead1"] {
		t.Error("dead1 should not be in AliveNodes")
	}
}

// TestDigestContainsAllNodes verifies that Digest returns one entry per node.
func TestDigestContainsAllNodes(t *testing.T) {
	cs := New("self")
	cs.AddNode("p1", nil)
	cs.AddNode("p2", nil)
	cs.AddNode("p3", nil)

	digests := cs.Digest()
	if len(digests) != 4 { // self + 3 peers
		t.Errorf("expected 4 digests, got %d", len(digests))
	}
}

// TestMergeSameGenerationHigherState verifies that when generations match
// but the incoming state is higher (e.g. Suspect > Alive), local state is promoted.
func TestMergeSameGenerationHigherState(t *testing.T) {
	cs := New("self")
	cs.AddNode("peer1", nil)

	// Local: peer1 at gen=1, Alive.  Incoming: peer1 at gen=1, Suspect.
	digests := []GossipDigest{
		{Addr: "peer1", State: StateSuspect, Generation: 1},
	}
	cs.Merge(digests)

	n, _ := cs.GetNode("peer1")
	if n.State != StateSuspect {
		t.Errorf("expected state promoted to Suspect (same gen, higher state), got %s", n.State)
	}
}

// TestAddNodeIsIdempotent verifies that adding the same node twice is a no-op.
func TestAddNodeIsIdempotent(t *testing.T) {
	cs := New("self")
	cs.AddNode("peer1", nil)
	cs.AddNode("peer1", nil) // should not panic or duplicate

	all := cs.AllNodes()
	count := 0
	for _, n := range all {
		if n.Addr == "peer1" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 entry for peer1, got %d", count)
	}
}

// TestNextGeneration verifies generation helper returns current+1.
func TestNextGeneration(t *testing.T) {
	cs := New("self")
	cs.AddNode("peer1", nil)
	cs.UpdateState("peer1", StateSuspect, 5)

	next := cs.NextGeneration("peer1")
	if next != 6 {
		t.Errorf("expected NextGeneration=6, got %d", next)
	}

	// Unknown node → 2.
	if g := cs.NextGeneration("nobody"); g != 2 {
		t.Errorf("expected NextGeneration=2 for unknown, got %d", g)
	}
}
