package hashring

import (
	"fmt"
	"math"
	"testing"
)

func TestNewDefaults(t *testing.T) {
	hr := New(nil)
	if hr.virtualNodes != DefaultVirtualNodes {
		t.Errorf("expected %d virtual nodes, got %d", DefaultVirtualNodes, hr.virtualNodes)
	}
	if hr.replFactor != DefaultReplicationFactor {
		t.Errorf("expected replication factor %d, got %d", DefaultReplicationFactor, hr.replFactor)
	}
	if hr.Size() != 0 {
		t.Errorf("expected empty ring, got size %d", hr.Size())
	}
}

func TestNewCustomConfig(t *testing.T) {
	hr := New(&Config{VirtualNodes: 50, ReplicationFactor: 2})
	if hr.virtualNodes != 50 {
		t.Errorf("expected 50 virtual nodes, got %d", hr.virtualNodes)
	}
	if hr.replFactor != 2 {
		t.Errorf("expected replication factor 2, got %d", hr.replFactor)
	}
}

func TestAddRemoveNode(t *testing.T) {
	hr := New(nil)

	hr.AddNode(":3000")
	if hr.Size() != 1 {
		t.Fatalf("expected size 1, got %d", hr.Size())
	}
	if !hr.HasNode(":3000") {
		t.Fatal("expected :3000 to be on the ring")
	}

	// Adding the same node again is a no-op
	hr.AddNode(":3000")
	if hr.Size() != 1 {
		t.Fatalf("expected size still 1 after duplicate add, got %d", hr.Size())
	}

	hr.AddNode(":4000")
	if hr.Size() != 2 {
		t.Fatalf("expected size 2, got %d", hr.Size())
	}

	hr.RemoveNode(":3000")
	if hr.Size() != 1 {
		t.Fatalf("expected size 1 after remove, got %d", hr.Size())
	}
	if hr.HasNode(":3000") {
		t.Fatal("expected :3000 to be removed")
	}
	if !hr.HasNode(":4000") {
		t.Fatal("expected :4000 to still be on the ring")
	}

	// Removing non-existent node is a no-op
	hr.RemoveNode(":9999")
	if hr.Size() != 1 {
		t.Fatalf("expected size still 1 after removing non-existent, got %d", hr.Size())
	}
}

func TestGetNodesEmptyRing(t *testing.T) {
	hr := New(nil)
	nodes := hr.GetNodes("somekey", 3)
	if nodes != nil {
		t.Fatalf("expected nil for empty ring, got %v", nodes)
	}
}

func TestGetNodesSingleNode(t *testing.T) {
	hr := New(nil)
	hr.AddNode(":3000")

	nodes := hr.GetNodes("mykey", 3)
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node (only 1 exists), got %d: %v", len(nodes), nodes)
	}
	if nodes[0] != ":3000" {
		t.Fatalf("expected :3000, got %s", nodes[0])
	}
}

func TestGetNodesReturnsDistinctPhysicalNodes(t *testing.T) {
	hr := New(nil)
	hr.AddNode(":3000")
	hr.AddNode(":4000")
	hr.AddNode(":5000")

	nodes := hr.GetNodes("testkey", 3)
	if len(nodes) != 3 {
		t.Fatalf("expected 3 nodes, got %d: %v", len(nodes), nodes)
	}

	// Verify all distinct
	seen := make(map[string]bool)
	for _, n := range nodes {
		if seen[n] {
			t.Fatalf("duplicate node in result: %s", n)
		}
		seen[n] = true
	}
}

func TestGetNodesNeverExceedsPhysicalCount(t *testing.T) {
	hr := New(nil)
	hr.AddNode(":3000")
	hr.AddNode(":4000")

	// Request 5 but only 2 exist
	nodes := hr.GetNodes("key", 5)
	if len(nodes) != 2 {
		t.Fatalf("expected 2 nodes (only 2 physical), got %d: %v", len(nodes), nodes)
	}
}

func TestGetPrimaryNode(t *testing.T) {
	hr := New(nil)
	hr.AddNode(":3000")
	hr.AddNode(":4000")

	primary := hr.GetPrimaryNode("key1")
	if primary == "" {
		t.Fatal("expected a primary node, got empty")
	}

	// Primary should be consistent
	for i := 0; i < 100; i++ {
		if hr.GetPrimaryNode("key1") != primary {
			t.Fatal("primary node should be deterministic for the same key")
		}
	}
}

func TestConsistencyOnNodeAdd(t *testing.T) {
	hr := New(&Config{VirtualNodes: 150, ReplicationFactor: 1})

	hr.AddNode(":3000")
	hr.AddNode(":4000")
	hr.AddNode(":5000")

	// Record primary for 1000 keys
	numKeys := 1000
	before := make(map[string]string, numKeys)
	for i := 0; i < numKeys; i++ {
		key := fmt.Sprintf("key:%d", i)
		before[key] = hr.GetPrimaryNode(key)
	}

	// Add a 4th node
	hr.AddNode(":6000")

	// Count how many keys changed primary
	changed := 0
	for i := 0; i < numKeys; i++ {
		key := fmt.Sprintf("key:%d", i)
		if hr.GetPrimaryNode(key) != before[key] {
			changed++
		}
	}

	// With consistent hashing, roughly 1/N of keys should move (N=4, ~25%)
	// Allow generous margin: should be less than 40%
	changeRate := float64(changed) / float64(numKeys)
	if changeRate > 0.40 {
		t.Errorf("too many keys moved on node add: %.1f%% (expected ~25%%)", changeRate*100)
	}
	t.Logf("Keys moved on adding 4th node: %d/%d (%.1f%%)", changed, numKeys, changeRate*100)
}

func TestDistributionUniformity(t *testing.T) {
	hr := New(&Config{VirtualNodes: 150, ReplicationFactor: 1})

	nodes := []string{":3000", ":4000", ":5000", ":6000", ":7000"}
	for _, n := range nodes {
		hr.AddNode(n)
	}

	// Distribute 10K keys
	numKeys := 10000
	counts := make(map[string]int)
	for i := 0; i < numKeys; i++ {
		key := fmt.Sprintf("key:%d", i)
		primary := hr.GetPrimaryNode(key)
		counts[primary]++
	}

	// Expected: 2000 per node (10000 / 5)
	expected := float64(numKeys) / float64(len(nodes))

	for _, n := range nodes {
		count := counts[n]
		deviation := math.Abs(float64(count)-expected) / expected * 100
		t.Logf("Node %s: %d keys (%.1f%% deviation from ideal)", n, count, deviation)

		// Allow up to 30% deviation (150 vnodes gives good distribution)
		if deviation > 30 {
			t.Errorf("Node %s has too much deviation: %.1f%%", n, deviation)
		}
	}
}

func TestMembers(t *testing.T) {
	hr := New(nil)
	hr.AddNode(":3000")
	hr.AddNode(":4000")
	hr.AddNode(":5000")

	members := hr.Members()
	if len(members) != 3 {
		t.Fatalf("expected 3 members, got %d", len(members))
	}

	memberSet := make(map[string]bool)
	for _, m := range members {
		memberSet[m] = true
	}
	for _, expected := range []string{":3000", ":4000", ":5000"} {
		if !memberSet[expected] {
			t.Errorf("expected %s in members", expected)
		}
	}
}

func TestReplicationFactor(t *testing.T) {
	hr := New(&Config{ReplicationFactor: 5})
	if hr.ReplicationFactor() != 5 {
		t.Errorf("expected replication factor 5, got %d", hr.ReplicationFactor())
	}
}

func TestGetNodesDeterministic(t *testing.T) {
	hr := New(nil)
	hr.AddNode(":3000")
	hr.AddNode(":4000")
	hr.AddNode(":5000")

	// Same key should always return same nodes in same order
	first := hr.GetNodes("stable-key", 3)
	for i := 0; i < 50; i++ {
		result := hr.GetNodes("stable-key", 3)
		for j := range first {
			if result[j] != first[j] {
				t.Fatalf("non-deterministic: call %d returned %v, expected %v", i, result, first)
			}
		}
	}
}

func BenchmarkGetNodes(b *testing.B) {
	hr := New(nil)
	for i := 0; i < 10; i++ {
		hr.AddNode(fmt.Sprintf(":%d", 3000+i))
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		hr.GetNodes(fmt.Sprintf("key:%d", i), 3)
	}
}
