package merkle_test

import (
	"sort"
	"testing"
	"time"

	"github.com/Faizan2005/DFS-Go/Cluster/merkle"
)

// ---------------------------------------------------------------------------
// Tree construction
// ---------------------------------------------------------------------------

// The same key set must always produce the same root hash, regardless of the
// order in which keys are provided.
func TestBuildDeterministic(t *testing.T) {
	keys1 := []string{"c.pdf", "a.pdf", "b.pdf"}
	keys2 := []string{"b.pdf", "c.pdf", "a.pdf"}

	if merkle.Build(keys1).RootHash() != merkle.Build(keys2).RootHash() {
		t.Error("same key set in different order produced different root hashes")
	}
}

// An empty key set must produce a zero root hash.
func TestBuildEmptyProducesZeroHash(t *testing.T) {
	tree := merkle.Build(nil)
	if tree.RootHash() != [32]byte{} {
		t.Error("empty tree should have zero root hash")
	}
}

// A single key must still produce a valid (non-zero) root hash.
func TestBuildSingleKey(t *testing.T) {
	tree := merkle.Build([]string{"only.pdf"})
	if tree.RootHash() == [32]byte{} {
		t.Error("single-key tree should have non-zero root hash")
	}
}

// Two different key sets must produce different root hashes.
func TestDifferentKeySetsDifferentRoots(t *testing.T) {
	t1 := merkle.Build([]string{"a.pdf", "b.pdf"})
	t2 := merkle.Build([]string{"a.pdf", "c.pdf"})
	if t1.RootHash() == t2.RootHash() {
		t.Error("different key sets should produce different root hashes")
	}
}

// ---------------------------------------------------------------------------
// Diff
// ---------------------------------------------------------------------------

// Identical trees must return nil (no diff needed).
func TestDiffIdentical(t *testing.T) {
	keys := []string{"a.pdf", "b.pdf", "c.pdf"}
	t1 := merkle.Build(keys)
	t2 := merkle.Build(keys)

	diff := t1.Diff(t2)
	if diff != nil {
		t.Errorf("expected nil diff for identical trees, got %v", diff)
	}
}

// Completely disjoint key sets must return all keys in the diff.
func TestDiffDisjoint(t *testing.T) {
	t1 := merkle.Build([]string{"a.pdf", "b.pdf"})
	t2 := merkle.Build([]string{"c.pdf", "d.pdf"})

	diff := t1.Diff(t2)
	if len(diff) == 0 {
		t.Fatal("expected non-empty diff for completely disjoint trees")
	}

	// Every key from both sets must appear in the diff.
	allKeys := map[string]bool{"a.pdf": true, "b.pdf": true, "c.pdf": true, "d.pdf": true}
	for _, k := range diff {
		delete(allKeys, k)
	}
	if len(allKeys) > 0 {
		t.Errorf("some keys missing from diff: %v", allKeys)
	}
}

// When one tree has a key the other doesn't, only that key should appear.
func TestDiffOneMissingKey(t *testing.T) {
	shared := []string{"a.pdf", "b.pdf", "c.pdf"}
	withExtra := append(shared, "d.pdf")

	t1 := merkle.Build(shared)
	t2 := merkle.Build(withExtra)

	diff := t1.Diff(t2)
	found := false
	for _, k := range diff {
		if k == "d.pdf" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected d.pdf in diff, got %v", diff)
	}
}

// Diff must be symmetric: t1.Diff(t2) and t2.Diff(t1) cover the same keys.
func TestDiffSymmetric(t *testing.T) {
	t1 := merkle.Build([]string{"a.pdf", "b.pdf"})
	t2 := merkle.Build([]string{"b.pdf", "c.pdf"})

	diff12 := t1.Diff(t2)
	diff21 := t2.Diff(t1)

	toSet := func(ss []string) map[string]bool {
		m := make(map[string]bool)
		for _, s := range ss {
			m[s] = true
		}
		return m
	}
	s12 := toSet(diff12)
	s21 := toSet(diff21)

	for k := range s12 {
		if !s21[k] {
			t.Errorf("key %s in diff(t1,t2) but not diff(t2,t1)", k)
		}
	}
	for k := range s21 {
		if !s12[k] {
			t.Errorf("key %s in diff(t2,t1) but not diff(t1,t2)", k)
		}
	}
}

// Diff against an empty tree must return all keys from the non-empty tree.
func TestDiffAgainstEmpty(t *testing.T) {
	keys := []string{"a.pdf", "b.pdf", "c.pdf"}
	t1 := merkle.Build(keys)
	t2 := merkle.Build(nil)

	diff := t1.Diff(t2)
	sort.Strings(diff)
	expected := []string{"a.pdf", "b.pdf", "c.pdf"}
	sort.Strings(expected)

	if len(diff) != len(expected) {
		t.Fatalf("expected %v, got %v", expected, diff)
	}
	for i := range diff {
		if diff[i] != expected[i] {
			t.Errorf("diff[%d]: expected %s, got %s", i, expected[i], diff[i])
		}
	}
}

// ---------------------------------------------------------------------------
// Anti-entropy service
// ---------------------------------------------------------------------------

// When root hashes match, HandleSync must not send any response.
func TestHandleSyncNoResponseWhenInSync(t *testing.T) {
	keys := []string{"a.pdf", "b.pdf"}
	tree := merkle.Build(keys)

	sent := 0
	ae := merkle.NewAntiEntropyService(
		"self",
		time.Hour,
		func() []string { return keys },
		func() []string { return []string{} },
		func(addr string, msg interface{}) error { sent++; return nil },
		nil, nil,
	)

	ae.HandleSync("peer", &merkle.MessageMerkleSync{
		From:     "peer",
		RootHash: tree.RootHash(),
	})

	if sent != 0 {
		t.Errorf("expected 0 messages sent when in sync, got %d", sent)
	}
}

// When root hashes differ, HandleSync must send a MessageMerkleDiffResponse.
func TestHandleSyncSendsResponseWhenDifferent(t *testing.T) {
	myKeys := []string{"a.pdf", "b.pdf"}
	theirKeys := []string{"c.pdf", "d.pdf"}
	theirTree := merkle.Build(theirKeys)

	var sentMsg interface{}
	ae := merkle.NewAntiEntropyService(
		"self",
		time.Hour,
		func() []string { return myKeys },
		func() []string { return []string{} },
		func(addr string, msg interface{}) error { sentMsg = msg; return nil },
		nil, nil,
	)

	ae.HandleSync("peer", &merkle.MessageMerkleSync{
		From:     "peer",
		RootHash: theirTree.RootHash(),
	})

	resp, ok := sentMsg.(*merkle.MessageMerkleDiffResponse)
	if !ok {
		t.Fatalf("expected *MessageMerkleDiffResponse, got %T", sentMsg)
	}
	if len(resp.AllKeys) != 2 {
		t.Errorf("expected 2 keys in response, got %d", len(resp.AllKeys))
	}
}

// HandleDiffResponse with keys we're missing must call onNeedKey for each.
func TestHandleDiffResponseCallsOnNeedKey(t *testing.T) {
	myKeys := []string{"a.pdf"}
	theirKeys := []string{"a.pdf", "b.pdf", "c.pdf"}

	needed := []string{}
	ae := merkle.NewAntiEntropyService(
		"self",
		time.Hour,
		func() []string { return myKeys },
		func() []string { return []string{} },
		func(addr string, msg interface{}) error { return nil },
		func(addr, key string) { needed = append(needed, key) },
		nil,
	)

	ae.HandleDiffResponse("peer", &merkle.MessageMerkleDiffResponse{
		From:    "peer",
		AllKeys: theirKeys,
	})

	sort.Strings(needed)
	expected := []string{"b.pdf", "c.pdf"}
	if len(needed) != len(expected) {
		t.Fatalf("expected onNeedKey for %v, got %v", expected, needed)
	}
	for i := range needed {
		if needed[i] != expected[i] {
			t.Errorf("needed[%d]: expected %s, got %s", i, expected[i], needed[i])
		}
	}
}

// HandleDiffResponse for keys the peer is missing must call onSendKey.
func TestHandleDiffResponseCallsOnSendKey(t *testing.T) {
	myKeys := []string{"a.pdf", "b.pdf", "c.pdf"}
	theirKeys := []string{"a.pdf"}

	sent := []string{}
	ae := merkle.NewAntiEntropyService(
		"self",
		time.Hour,
		func() []string { return myKeys },
		func() []string { return []string{} },
		func(addr string, msg interface{}) error { return nil },
		nil,
		func(addr, key string) { sent = append(sent, key) },
	)

	ae.HandleDiffResponse("peer", &merkle.MessageMerkleDiffResponse{
		From:    "peer",
		AllKeys: theirKeys,
	})

	sort.Strings(sent)
	expected := []string{"b.pdf", "c.pdf"}
	if len(sent) != len(expected) {
		t.Fatalf("expected onSendKey for %v, got %v", expected, sent)
	}
	for i := range sent {
		if sent[i] != expected[i] {
			t.Errorf("sent[%d]: expected %s, got %s", i, expected[i], sent[i])
		}
	}
}

// Start/Stop must not panic and Stop must be idempotent.
func TestAntiEntropyStartStop(t *testing.T) {
	ae := merkle.NewAntiEntropyService(
		"self",
		time.Hour,
		func() []string { return nil },
		func() []string { return nil },
		func(string, interface{}) error { return nil },
		nil, nil,
	)
	ae.Start()
	time.Sleep(5 * time.Millisecond)
	ae.Stop()
	ae.Stop() // idempotent
}
