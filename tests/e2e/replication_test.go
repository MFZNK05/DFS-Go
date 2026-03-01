package e2e_test

import (
	"fmt"
	"testing"
	"time"
)

// TestQuorumWriteReplicatesToAllNodes stores a file on node[0] and verifies
// that node[1] and node[2] can both retrieve identical bytes.
func TestQuorumWriteReplicatesToAllNodes(t *testing.T) {
	c := newCluster(t, 3, 3)

	key := "quorum-replicate-key"
	data := makePayload("text", 512*1024)

	if err := c.nodes[0].StoreData(key, readerOf(data)); err != nil {
		t.Fatalf("StoreData: %v", err)
	}
	time.Sleep(400 * time.Millisecond)

	for i := 1; i < 3; i++ {
		got := mustGetData(t, c.nodes[i], key)
		if sha256hex(got) != sha256hex(data) {
			t.Errorf("node[%d] returned wrong data (sha256 mismatch)", i)
		}
	}
}

// TestGetDataFromNonPrimary uploads to node[0] and retrieves from node[2],
// verifying cross-node reads work.
func TestGetDataFromNonPrimary(t *testing.T) {
	c := newCluster(t, 3, 3)

	key := "cross-node-key"
	data := makePayload("csv", 200*1024)

	assertRoundtrip(t, c.nodes[0], c.nodes[2], key, data)
}

// TestReadRepairFillsMissingReplica manually removes a file from node[2]'s store
// then triggers a GetData which should read-repair node[2] from another replica.
func TestReadRepairFillsMissingReplica(t *testing.T) {
	c := newCluster(t, 3, 3)

	key := "repair-key"
	data := makePayload("text", 256*1024)

	if err := c.nodes[0].StoreData(key, readerOf(data)); err != nil {
		t.Fatalf("StoreData: %v", err)
	}
	time.Sleep(400 * time.Millisecond)

	// Remove the key from node[2]'s local store directly.
	_ = c.nodes[2].Store.Remove(key)

	// GetData from node[0] should succeed (reads from node[1] or itself)
	// and asynchronously repair node[2].
	got := mustGetData(t, c.nodes[0], key)
	if sha256hex(got) != sha256hex(data) {
		t.Fatalf("data mismatch after removal: sha256 mismatch")
	}

	// Wait for read repair to propagate to node[2].
	waitFor(t, 5*time.Second, func() bool {
		r, err := c.nodes[2].GetData(key)
		if err != nil {
			return false
		}
		b := make([]byte, 1)
		_, err = r.Read(b)
		return err == nil
	}, "read repair did not restore data on node[2]")
}

// TestAllReplicasReturnIdenticalBytes uploads 10 files and verifies all 3 nodes
// return identical bytes for every key.
func TestAllReplicasReturnIdenticalBytes(t *testing.T) {
	c := newCluster(t, 3, 3)

	const nFiles = 10
	files := make(map[string][]byte, nFiles)
	for i := 0; i < nFiles; i++ {
		key := fmt.Sprintf("identity-key-%d", i)
		files[key] = makePayload("text", 64*1024)
		if err := c.nodes[0].StoreData(key, readerOf(files[key])); err != nil {
			t.Fatalf("StoreData(%q): %v", key, err)
		}
	}

	time.Sleep(600 * time.Millisecond)

	for key, expected := range files {
		h := sha256hex(expected)
		for i, nd := range c.nodes {
			got := mustGetData(t, nd, key)
			if sha256hex(got) != h {
				t.Errorf("node[%d] key=%q: sha256 mismatch", i, key)
			}
		}
	}
}
