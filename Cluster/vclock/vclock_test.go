package vclock_test

import (
	"testing"

	"github.com/Faizan2005/DFS-Go/Cluster/vclock"
)

// ---------------------------------------------------------------------------
// Increment
// ---------------------------------------------------------------------------

// After A increments, A's counter must be 1 and all other entries must be 0.
func TestIncrementAddsCounter(t *testing.T) {
	vc := vclock.New()
	vc2 := vc.Increment("A")
	if vc2["A"] != 1 {
		t.Errorf("expected A=1, got %d", vc2["A"])
	}
}

// Incrementing must not mutate the original clock.
func TestIncrementDoesNotMutateOriginal(t *testing.T) {
	vc := vclock.VectorClock{"A": 3, "B": 2}
	_ = vc.Increment("A")
	if vc["A"] != 3 {
		t.Errorf("original clock mutated: expected A=3, got %d", vc["A"])
	}
}

// Successive increments by the same node must accumulate.
func TestIncrementAccumulates(t *testing.T) {
	vc := vclock.New()
	vc = vc.Increment("A")
	vc = vc.Increment("A")
	vc = vc.Increment("A")
	if vc["A"] != 3 {
		t.Errorf("expected A=3 after 3 increments, got %d", vc["A"])
	}
}

// ---------------------------------------------------------------------------
// Merge
// ---------------------------------------------------------------------------

// Merge must take the per-key maximum from both clocks.
func TestMergeTakesMax(t *testing.T) {
	a := vclock.VectorClock{"A": 3, "B": 1}
	b := vclock.VectorClock{"A": 1, "B": 4, "C": 2}
	m := a.Merge(b)

	if m["A"] != 3 {
		t.Errorf("A: expected 3, got %d", m["A"])
	}
	if m["B"] != 4 {
		t.Errorf("B: expected 4, got %d", m["B"])
	}
	if m["C"] != 2 {
		t.Errorf("C: expected 2, got %d", m["C"])
	}
}

// Merge must not mutate either input.
func TestMergeDoesNotMutateInputs(t *testing.T) {
	a := vclock.VectorClock{"A": 5}
	b := vclock.VectorClock{"A": 10}
	_ = a.Merge(b)
	if a["A"] != 5 {
		t.Error("a was mutated by Merge")
	}
	if b["A"] != 10 {
		t.Error("b was mutated by Merge")
	}
}

// Merging with an empty clock returns a copy of the original.
func TestMergeWithEmpty(t *testing.T) {
	a := vclock.VectorClock{"A": 7}
	m := a.Merge(vclock.New())
	if m["A"] != 7 {
		t.Errorf("expected A=7 after merging with empty, got %d", m["A"])
	}
}

// ---------------------------------------------------------------------------
// Compare — causal ordering
// ---------------------------------------------------------------------------

// {A:1} happened before {A:2}.
func TestCompareBeforeAfter(t *testing.T) {
	earlier := vclock.VectorClock{"A": 1}
	later := vclock.VectorClock{"A": 2}

	if earlier.Compare(later) != vclock.Before {
		t.Errorf("expected Before, got %s", earlier.Compare(later))
	}
	if later.Compare(earlier) != vclock.After {
		t.Errorf("expected After, got %s", later.Compare(earlier))
	}
}

// Two writes that each advanced a different node are concurrent — neither
// causally dominates the other.
func TestCompareConcurrent(t *testing.T) {
	a := vclock.VectorClock{"A": 1, "B": 0}
	b := vclock.VectorClock{"A": 0, "B": 1}

	if a.Compare(b) != vclock.Concurrent {
		t.Errorf("expected Concurrent, got %s", a.Compare(b))
	}
	if b.Compare(a) != vclock.Concurrent {
		t.Errorf("expected Concurrent (symmetric), got %s", b.Compare(a))
	}
}

// Identical clocks are Equal, not Before or After.
func TestCompareEqual(t *testing.T) {
	a := vclock.VectorClock{"A": 3, "B": 2}
	b := vclock.VectorClock{"A": 3, "B": 2}

	if a.Compare(b) != vclock.Equal {
		t.Errorf("expected Equal, got %s", a.Compare(b))
	}
}

// A node that wrote A=1,B=1 happened after a node that only wrote A=1.
// The latter has no entry for B so B defaults to 0.
func TestCompareMissingKeyTreatedAsZero(t *testing.T) {
	full := vclock.VectorClock{"A": 1, "B": 1}
	partial := vclock.VectorClock{"A": 1} // B implicitly 0

	if partial.Compare(full) != vclock.Before {
		t.Errorf("partial should be Before full, got %s", partial.Compare(full))
	}
	if full.Compare(partial) != vclock.After {
		t.Errorf("full should be After partial, got %s", full.Compare(partial))
	}
}

// A write that observed A=2 and then B wrote is After A=1 (A saw B's write).
// Concretely: {A:2,B:1} > {A:1,B:1} causally.
func TestCausalChain(t *testing.T) {
	// B wrote after seeing A=1: {A:1, B:1}
	// A then wrote after seeing B: {A:2, B:1}
	bWrote := vclock.VectorClock{"A": 1, "B": 1}
	aWroteAfterB := vclock.VectorClock{"A": 2, "B": 1}

	if bWrote.Compare(aWroteAfterB) != vclock.Before {
		t.Errorf("bWrote should be Before aWroteAfterB, got %s", bWrote.Compare(aWroteAfterB))
	}
}

// ---------------------------------------------------------------------------
// Copy
// ---------------------------------------------------------------------------

// Modifying the copy must not affect the original.
func TestCopyIsIndependent(t *testing.T) {
	orig := vclock.VectorClock{"A": 1, "B": 2}
	cp := orig.Copy()
	cp["A"] = 99
	if orig["A"] != 1 {
		t.Error("modifying copy affected original")
	}
}

// ---------------------------------------------------------------------------
// Encode / Decode round-trip
// ---------------------------------------------------------------------------

func TestEncodeDecodeRoundTrip(t *testing.T) {
	orig := vclock.VectorClock{"node1": 5, "node2": 3, "node3": 0}
	data, err := orig.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	decoded, err := vclock.Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if decoded.Compare(orig) != vclock.Equal {
		t.Errorf("decoded clock does not equal original: %v vs %v", decoded, orig)
	}
}

func TestDecodeEmptyClock(t *testing.T) {
	orig := vclock.New()
	data, err := orig.Encode()
	if err != nil {
		t.Fatalf("Encode empty clock: %v", err)
	}
	decoded, err := vclock.Decode(data)
	if err != nil {
		t.Fatalf("Decode empty clock: %v", err)
	}
	if decoded.Compare(orig) != vclock.Equal {
		t.Error("empty clock did not survive encode/decode")
	}
}
