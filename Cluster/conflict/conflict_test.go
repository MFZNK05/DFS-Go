package conflict_test

import (
	"testing"

	"github.com/Faizan2005/DFS-Go/Cluster/conflict"
	"github.com/Faizan2005/DFS-Go/Cluster/vclock"
)

var resolver = conflict.NewLWWResolver()

// ---------------------------------------------------------------------------
// Causal ordering — causally later version must always win regardless of
// wall-clock timestamp.
// ---------------------------------------------------------------------------

// B wrote after seeing A's write → B is causally After A → B wins.
func TestCausallyLaterWins(t *testing.T) {
	vA := conflict.Version{
		NodeAddr:  "A",
		Clock:     vclock.VectorClock{"A": 1},
		Timestamp: 1000,
	}
	// B observed A's write (A:1) and then wrote — causally after.
	vB := conflict.Version{
		NodeAddr:  "B",
		Clock:     vclock.VectorClock{"A": 1, "B": 1},
		Timestamp: 900, // deliberately older wall-clock — should still win on causality
	}

	winner := resolver.Resolve([]conflict.Version{vA, vB})
	if winner.NodeAddr != "B" {
		t.Errorf("expected B (causally later) to win, got %s", winner.NodeAddr)
	}
}

// A is causally after B → A wins, even though B has a higher Timestamp.
func TestCausallyAfterWinsOverHigherTimestamp(t *testing.T) {
	vB := conflict.Version{
		NodeAddr:  "B",
		Clock:     vclock.VectorClock{"B": 1},
		Timestamp: 9999, // much higher wall clock
	}
	vA := conflict.Version{
		NodeAddr:  "A",
		Clock:     vclock.VectorClock{"A": 1, "B": 1}, // A saw B's write
		Timestamp: 1,
	}

	winner := resolver.Resolve([]conflict.Version{vB, vA})
	if winner.NodeAddr != "A" {
		t.Errorf("expected A (causally after) to win, got %s", winner.NodeAddr)
	}
}

// Linear history: v1 < v2 < v3 — v3 must win.
func TestLinearHistoryLatestWins(t *testing.T) {
	v1 := conflict.Version{NodeAddr: "n1", Clock: vclock.VectorClock{"n1": 1}, Timestamp: 100}
	v2 := conflict.Version{NodeAddr: "n2", Clock: vclock.VectorClock{"n1": 1, "n2": 1}, Timestamp: 200}
	v3 := conflict.Version{NodeAddr: "n3", Clock: vclock.VectorClock{"n1": 1, "n2": 1, "n3": 1}, Timestamp: 300}

	// Pass in reverse order to confirm it's not just taking the last element.
	winner := resolver.Resolve([]conflict.Version{v3, v1, v2})
	if winner.NodeAddr != "n3" {
		t.Errorf("expected n3 (latest in causal chain), got %s", winner.NodeAddr)
	}
}

// ---------------------------------------------------------------------------
// Concurrent writes — wall-clock Timestamp is the tiebreaker.
// ---------------------------------------------------------------------------

// Two nodes write independently (no causal relationship) — higher Timestamp wins.
func TestConcurrentHigherTimestampWins(t *testing.T) {
	vA := conflict.Version{
		NodeAddr:  "A",
		Clock:     vclock.VectorClock{"A": 1},
		Timestamp: 5000,
	}
	vB := conflict.Version{
		NodeAddr:  "B",
		Clock:     vclock.VectorClock{"B": 1},
		Timestamp: 7000, // higher — should win
	}

	winner := resolver.Resolve([]conflict.Version{vA, vB})
	if winner.NodeAddr != "B" {
		t.Errorf("expected B (higher timestamp among concurrent), got %s", winner.NodeAddr)
	}
}

// Symmetric: vB first in slice, vA has higher timestamp.
func TestConcurrentHigherTimestampWinsReversed(t *testing.T) {
	vA := conflict.Version{NodeAddr: "A", Clock: vclock.VectorClock{"A": 1}, Timestamp: 8000}
	vB := conflict.Version{NodeAddr: "B", Clock: vclock.VectorClock{"B": 1}, Timestamp: 2000}

	winner := resolver.Resolve([]conflict.Version{vB, vA})
	if winner.NodeAddr != "A" {
		t.Errorf("expected A (higher timestamp), got %s", winner.NodeAddr)
	}
}

// Three-way concurrent writes — the one with the highest timestamp wins.
func TestThreeWayConcurrentHighestTimestampWins(t *testing.T) {
	vA := conflict.Version{NodeAddr: "A", Clock: vclock.VectorClock{"A": 1}, Timestamp: 100}
	vB := conflict.Version{NodeAddr: "B", Clock: vclock.VectorClock{"B": 1}, Timestamp: 300}
	vC := conflict.Version{NodeAddr: "C", Clock: vclock.VectorClock{"C": 1}, Timestamp: 200}

	winner := resolver.Resolve([]conflict.Version{vA, vB, vC})
	if winner.NodeAddr != "B" {
		t.Errorf("expected B (highest timestamp=300), got %s", winner.NodeAddr)
	}
}

// ---------------------------------------------------------------------------
// Edge cases
// ---------------------------------------------------------------------------

// Single version — trivially returns it unchanged.
func TestSingleVersionReturnedAsIs(t *testing.T) {
	v := conflict.Version{NodeAddr: "only", Clock: vclock.VectorClock{"only": 3}, Timestamp: 42}
	winner := resolver.Resolve([]conflict.Version{v})
	if winner.NodeAddr != "only" {
		t.Errorf("expected only, got %s", winner.NodeAddr)
	}
}

// Identical clocks AND timestamps — first in slice wins (deterministic).
func TestIdenticalVersionsFirstWins(t *testing.T) {
	clock := vclock.VectorClock{"A": 2}
	v1 := conflict.Version{NodeAddr: "first", Clock: clock, Timestamp: 500}
	v2 := conflict.Version{NodeAddr: "second", Clock: clock, Timestamp: 500}

	winner := resolver.Resolve([]conflict.Version{v1, v2})
	if winner.NodeAddr != "first" {
		t.Errorf("expected first to win on exact tie, got %s", winner.NodeAddr)
	}
}

// Exact timestamp tie but concurrent clocks — first in slice still wins.
func TestConcurrentExactTimestampTieFirstWins(t *testing.T) {
	v1 := conflict.Version{NodeAddr: "X", Clock: vclock.VectorClock{"X": 1}, Timestamp: 1000}
	v2 := conflict.Version{NodeAddr: "Y", Clock: vclock.VectorClock{"Y": 1}, Timestamp: 1000}

	winner := resolver.Resolve([]conflict.Version{v1, v2})
	if winner.NodeAddr != "X" {
		t.Errorf("expected X (first on exact tie), got %s", winner.NodeAddr)
	}
}

// Verify that EncryptedKey is preserved on the winning version.
func TestEncryptedKeyPreservedOnWinner(t *testing.T) {
	loser := conflict.Version{
		NodeAddr:     "A",
		Clock:        vclock.VectorClock{"A": 1},
		Timestamp:    100,
		EncryptedKey: "loser-key",
	}
	winner := conflict.Version{
		NodeAddr:     "B",
		Clock:        vclock.VectorClock{"A": 1, "B": 1}, // causally after A
		Timestamp:    50,
		EncryptedKey: "winner-key",
	}

	result := resolver.Resolve([]conflict.Version{loser, winner})
	if result.EncryptedKey != "winner-key" {
		t.Errorf("expected winner's encrypted key, got %s", result.EncryptedKey)
	}
}
