// Package merkle builds Merkle trees over key sets and runs background
// anti-entropy sync between replica pairs.
//
// # Merkle tree
//
// Keys are sorted lexicographically before building the tree so the same set
// of keys always produces the same root hash (determinism is critical —
// non-deterministic root hashes would trigger unnecessary syncs).
// Each leaf is SHA-256(key). Internal nodes are SHA-256(left.Hash || right.Hash).
// When the number of leaves is odd, the last leaf is duplicated so every
// internal node always has two children.
//
// # Anti-entropy service
//
// Every interval (default 10 minutes) the service:
//  1. Builds a Merkle tree over its own keys.
//  2. Sends MessageMerkleSync{RootHash} to each replica partner.
//  3. If the partner's root differs it responds with its full key list.
//  4. The initiator builds the partner's tree, diffs the two trees, and
//     repairs any keys that are missing on either side.
package merkle

import (
	"crypto/sha256"
	"log"
	"sort"
	"sync"
	"time"
)

// --------------------------------------------------------------------------
// Merkle tree
// --------------------------------------------------------------------------

// node is a single node in the Merkle tree.
type node struct {
	Hash  [32]byte
	Left  *node
	Right *node
	IsLeaf bool
	Key   string // non-empty only for leaf nodes
}

// Tree is the root of a Merkle tree built from a key set.
type Tree struct {
	Root   *node
	leaves []*node
}

// Build constructs a Merkle tree from the given keys.
// Keys are sorted before building so the same set always produces the same root.
// Returns an empty Tree (nil Root) when keys is empty.
func Build(keys []string) *Tree {
	if len(keys) == 0 {
		return &Tree{}
	}

	sorted := make([]string, len(keys))
	copy(sorted, keys)
	sort.Strings(sorted)

	// Build leaf layer.
	leaves := make([]*node, len(sorted))
	for i, k := range sorted {
		h := sha256.Sum256([]byte(k))
		leaves[i] = &node{Hash: h, IsLeaf: true, Key: k}
	}

	t := &Tree{leaves: leaves}
	t.Root = buildLevel(leaves)
	return t
}

// buildLevel recursively reduces a layer of nodes to a single root.
func buildLevel(layer []*node) *node {
	if len(layer) == 1 {
		return layer[0]
	}

	var next []*node
	for i := 0; i < len(layer); i += 2 {
		left := layer[i]
		right := left // duplicate last node if odd count
		if i+1 < len(layer) {
			right = layer[i+1]
		}
		parent := &node{Left: left, Right: right}
		combined := append(left.Hash[:], right.Hash[:]...)
		parent.Hash = sha256.Sum256(combined)
		next = append(next, parent)
	}
	return buildLevel(next)
}

// RootHash returns the root hash of the tree.
// Returns a zero [32]byte for an empty tree.
func (t *Tree) RootHash() [32]byte {
	if t == nil || t.Root == nil {
		return [32]byte{}
	}
	return t.Root.Hash
}

// Diff returns the keys that differ between t and other.
// A key appears in the result if it exists in one tree but not the other,
// or if it exists in both but the subtree hashes diverge (which in practice
// means the key content changed — not detected here at tree level since leaf
// hashes are keyed only by name, but the caller can use this as the set of
// keys to probe/repair).
// Returns nil when both trees are identical (same root hash).
// The returned slice has no duplicate keys.
func (t *Tree) Diff(other *Tree) []string {
	if t.RootHash() == other.RootHash() {
		return nil
	}
	raw := diffNodes(t.Root, other.Root)
	// Deduplicate — the odd-leaf duplication trick can surface the same key twice.
	seen := make(map[string]bool, len(raw))
	out := raw[:0]
	for _, k := range raw {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

func diffNodes(a, b *node) []string {
	if a == nil && b == nil {
		return nil
	}
	if a == nil {
		return collectKeys(b)
	}
	if b == nil {
		return collectKeys(a)
	}
	if a.Hash == b.Hash {
		return nil // identical subtree
	}
	if a.IsLeaf && b.IsLeaf {
		// Both are leaves with different hashes → both keys are "different"
		var out []string
		out = append(out, a.Key)
		if b.Key != a.Key {
			out = append(out, b.Key)
		}
		return out
	}
	if a.IsLeaf {
		return append([]string{a.Key}, collectKeys(b)...)
	}
	if b.IsLeaf {
		return append(collectKeys(a), b.Key)
	}
	return append(diffNodes(a.Left, b.Left), diffNodes(a.Right, b.Right)...)
}

// collectKeys returns all leaf keys under n.
func collectKeys(n *node) []string {
	if n == nil {
		return nil
	}
	if n.IsLeaf {
		return []string{n.Key}
	}
	return append(collectKeys(n.Left), collectKeys(n.Right)...)
}

// Keys returns all leaf keys stored in the tree (in sorted order).
func (t *Tree) Keys() []string {
	keys := make([]string, len(t.leaves))
	for i, l := range t.leaves {
		keys[i] = l.Key
	}
	return keys
}

// --------------------------------------------------------------------------
// Anti-entropy service
// --------------------------------------------------------------------------

// MessageMerkleSync is the first message in an anti-entropy exchange.
// The receiver compares RootHash against its own and responds with its full
// key list only when the hashes differ.
type MessageMerkleSync struct {
	From     string
	RootHash [32]byte
}

// MessageMerkleDiffResponse carries the full key list of the responding node.
// The initiator diffs the two key sets and transfers any missing keys.
type MessageMerkleDiffResponse struct {
	From    string
	AllKeys []string
}

// AntiEntropyService runs background Merkle sync between this node and its
// replica partners.
type AntiEntropyService struct {
	selfAddr string
	getKeys  func() []string                          // local keys
	getPeers func() []string                          // replica partner addresses
	sendMsg  func(addr string, msg interface{}) error // wire delivery
	onNeedKey func(addr, key string)                  // called when we are missing a key
	onSendKey func(addr, key string)                  // called when peer is missing a key

	interval time.Duration
	stopCh   chan struct{}
	once     sync.Once

	// pending diff responses: from-addr → channel
	pending sync.Map // map[string]chan []string
}

// NewAntiEntropyService creates a service with the given interval (default 10m
// if interval is 0).
func NewAntiEntropyService(
	selfAddr string,
	interval time.Duration,
	getKeys func() []string,
	getPeers func() []string,
	sendMsg func(addr string, msg interface{}) error,
	onNeedKey func(addr, key string),
	onSendKey func(addr, key string),
) *AntiEntropyService {
	if interval == 0 {
		interval = 10 * time.Minute
	}
	return &AntiEntropyService{
		selfAddr:  selfAddr,
		interval:  interval,
		getKeys:   getKeys,
		getPeers:  getPeers,
		sendMsg:   sendMsg,
		onNeedKey: onNeedKey,
		onSendKey: onSendKey,
		stopCh:    make(chan struct{}),
	}
}

// Start launches the background sync loop.
func (ae *AntiEntropyService) Start() {
	go ae.syncLoop()
}

// Stop shuts down the sync loop. Safe to call multiple times.
func (ae *AntiEntropyService) Stop() {
	ae.once.Do(func() { close(ae.stopCh) })
}

func (ae *AntiEntropyService) syncLoop() {
	ticker := time.NewTicker(ae.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			ae.doSync()
		case <-ae.stopCh:
			return
		}
	}
}

// doSync initiates one anti-entropy round with all current replica partners.
func (ae *AntiEntropyService) doSync() {
	myKeys := ae.getKeys()
	myTree := Build(myKeys)
	rootHash := myTree.RootHash()

	for _, peer := range ae.getPeers() {
		if peer == ae.selfAddr {
			continue
		}
		ch := make(chan []string, 1)
		ae.pending.Store(peer, ch)

		if err := ae.sendMsg(peer, &MessageMerkleSync{
			From:     ae.selfAddr,
			RootHash: rootHash,
		}); err != nil {
			log.Printf("[merkle] sync send to %s failed: %v", peer, err)
			ae.pending.Delete(peer)
			continue
		}

		// Wait up to 10s for the peer's key list.
		go func(p string, c chan []string, myK []string) {
			select {
			case theirKeys := <-c:
				ae.repair(p, myK, theirKeys)
			case <-time.After(10 * time.Second):
				log.Printf("[merkle] sync response timeout from %s", p)
			}
			ae.pending.Delete(p)
		}(peer, ch, myKeys)
	}
}

// HandleSync processes an incoming MessageMerkleSync from a peer.
// If our root hash matches we do nothing. Otherwise we send back our full key list.
func (ae *AntiEntropyService) HandleSync(from string, msg *MessageMerkleSync) {
	myKeys := ae.getKeys()
	myTree := Build(myKeys)

	if myTree.RootHash() == msg.RootHash {
		return // already in sync
	}

	if err := ae.sendMsg(from, &MessageMerkleDiffResponse{
		From:    ae.selfAddr,
		AllKeys: myKeys,
	}); err != nil {
		log.Printf("[merkle] failed to send diff response to %s: %v", from, err)
	}
}

// HandleDiffResponse processes the peer's key list and repairs divergence.
func (ae *AntiEntropyService) HandleDiffResponse(from string, msg *MessageMerkleDiffResponse) {
	if ch, ok := ae.pending.Load(from); ok {
		ch.(chan []string) <- msg.AllKeys
	}
	// If nobody is waiting (unsolicited response), run repair immediately.
	// This handles the case where the peer initiates a sync cycle to us.
	myKeys := ae.getKeys()
	ae.repair(from, myKeys, msg.AllKeys)
}

// repair diffs myKeys and theirKeys and calls the appropriate callbacks.
func (ae *AntiEntropyService) repair(peer string, myKeys, theirKeys []string) {
	myTree := Build(myKeys)
	theirTree := Build(theirKeys)

	diff := myTree.Diff(theirTree)
	if len(diff) == 0 {
		return
	}

	mySet := make(map[string]bool, len(myKeys))
	for _, k := range myKeys {
		mySet[k] = true
	}
	theirSet := make(map[string]bool, len(theirKeys))
	for _, k := range theirKeys {
		theirSet[k] = true
	}

	for _, key := range diff {
		switch {
		case mySet[key] && !theirSet[key]:
			// We have it, peer doesn't → send to peer.
			if ae.onSendKey != nil {
				ae.onSendKey(peer, key)
			}
		case !mySet[key] && theirSet[key]:
			// Peer has it, we don't → request from peer.
			if ae.onNeedKey != nil {
				ae.onNeedKey(peer, key)
			}
		// If both have it with different subtree hashes it's a version conflict;
		// the quorum/conflict layer handles resolution — we just log.
		default:
			log.Printf("[merkle] key %s exists on both sides but subtrees differ — conflict layer will resolve", key)
		}
	}
}
