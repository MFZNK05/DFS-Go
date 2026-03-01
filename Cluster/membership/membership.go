// Package membership tracks the liveness state of every node in the cluster.
// Each node maintains a local ClusterState — a map of NodeInfo entries keyed by
// address.  State transitions are guarded by a monotonically increasing
// generation counter so that stale updates arriving out of order are silently
// discarded (last-writer wins per generation).
package membership

import (
	"sync"
	"time"
)

// NodeState is the observed liveness of a cluster member.
type NodeState int32

const (
	StateAlive   NodeState = iota // 0 — responding to heartbeats
	StateSuspect                  // 1 — phi >= threshold, not yet confirmed dead
	StateDead                     // 2 — confirmed dead, removed from ring
	StateLeft                     // 3 — clean voluntary departure
)

func (s NodeState) String() string {
	switch s {
	case StateAlive:
		return "Alive"
	case StateSuspect:
		return "Suspect"
	case StateDead:
		return "Dead"
	case StateLeft:
		return "Left"
	default:
		return "Unknown"
	}
}

// NodeInfo is a snapshot of a single cluster member's state.
// It is safe to copy (no pointer receivers on the value fields that matter).
type NodeInfo struct {
	Addr        string
	State       NodeState
	Generation  uint64            // bumped on every state change; guards stale updates
	LastUpdated time.Time
	Metadata    map[string]string // e.g. "public_addr":"1.2.3.4:3000", "region":"us-east"
}

// GossipDigest is the compact summary of a NodeInfo sent during gossip rounds.
// Receivers compare the generation against their local copy and request the full
// NodeInfo only when the digest carries a higher generation.
type GossipDigest struct {
	Addr       string
	State      NodeState
	Generation uint64
}

// changeListener is a callback that fires whenever a node's state changes.
type changeListener func(addr string, oldState, newState NodeState)

// ClusterState is the eventually-consistent membership table for the local node.
// All public methods are safe for concurrent use.
type ClusterState struct {
	mu       sync.RWMutex
	selfAddr string
	nodes    map[string]*NodeInfo
	onChange []changeListener
}

// New creates a ClusterState that considers this node alive at generation 1.
func New(selfAddr string) *ClusterState {
	cs := &ClusterState{
		selfAddr: selfAddr,
		nodes:    make(map[string]*NodeInfo),
	}
	// Use Unix nanoseconds as initial generation so a restarted node always
	// advertises a generation strictly higher than any stale record on its peers,
	// even when the node leaves and rejoins within the same second.
	gen := uint64(time.Now().UnixNano())
	if gen < 2 {
		gen = 2 // safety floor
	}
	cs.nodes[selfAddr] = &NodeInfo{
		Addr:        selfAddr,
		State:       StateAlive,
		Generation:  gen,
		LastUpdated: time.Now(),
		Metadata:    make(map[string]string),
	}
	return cs
}

// OnChange registers a callback that is called (from the goroutine that triggers
// the change) every time a node's state transitions.
func (cs *ClusterState) OnChange(fn changeListener) {
	cs.mu.Lock()
	cs.onChange = append(cs.onChange, fn)
	cs.mu.Unlock()
}

// fireChange calls all registered listeners.  Must be called with cs.mu held
// for writing so listeners see consistent state when they read back.
// The callbacks are invoked with the lock still held; keep them non-blocking.
func (cs *ClusterState) fireChange(addr string, oldState, newState NodeState) {
	for _, fn := range cs.onChange {
		fn(addr, oldState, newState)
	}
}

// AddNode inserts a node as Alive at generation 1 if it is not already present.
// If the node exists the call is a no-op (use UpdateState to change state).
func (cs *ClusterState) AddNode(addr string, meta map[string]string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if _, exists := cs.nodes[addr]; exists {
		return
	}
	if meta == nil {
		meta = make(map[string]string)
	}
	cs.nodes[addr] = &NodeInfo{
		Addr:        addr,
		State:       StateAlive,
		Generation:  1,
		LastUpdated: time.Now(),
		Metadata:    meta,
	}
	cs.fireChange(addr, StateAlive, StateAlive) // newly discovered → Alive
}

// UpdateState transitions addr to newState if generation > current generation.
// Returns true when the update was applied, false when discarded as stale.
// If addr is unknown it is inserted as a new node.
func (cs *ClusterState) UpdateState(addr string, newState NodeState, generation uint64) bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	existing, ok := cs.nodes[addr]
	if !ok {
		// Unknown node — insert it.
		cs.nodes[addr] = &NodeInfo{
			Addr:        addr,
			State:       newState,
			Generation:  generation,
			LastUpdated: time.Now(),
			Metadata:    make(map[string]string),
		}
		cs.fireChange(addr, StateAlive, newState)
		return true
	}

	if generation <= existing.Generation {
		return false // stale — discard
	}

	oldState := existing.State
	existing.State = newState
	existing.Generation = generation
	existing.LastUpdated = time.Now()

	cs.fireChange(addr, oldState, newState)
	return true
}

// SetMetadata updates the metadata map for a node.  It does not bump the
// generation; metadata is merged (new keys win, existing keys are overwritten).
func (cs *ClusterState) SetMetadata(addr string, meta map[string]string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	node, ok := cs.nodes[addr]
	if !ok {
		return
	}
	if node.Metadata == nil {
		node.Metadata = make(map[string]string)
	}
	for k, v := range meta {
		node.Metadata[k] = v
	}
}

// GetNode returns a copy of the NodeInfo for addr and whether it was found.
func (cs *ClusterState) GetNode(addr string) (NodeInfo, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	n, ok := cs.nodes[addr]
	if !ok {
		return NodeInfo{}, false
	}
	return *n, true
}

// GetNodes returns the full NodeInfo for each address in addrs that is known.
func (cs *ClusterState) GetNodes(addrs []string) []NodeInfo {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	result := make([]NodeInfo, 0, len(addrs))
	for _, addr := range addrs {
		if n, ok := cs.nodes[addr]; ok {
			result = append(result, *n)
		}
	}
	return result
}

// AliveNodes returns the addresses of all nodes currently in StateAlive.
func (cs *ClusterState) AliveNodes() []string {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	var alive []string
	for addr, n := range cs.nodes {
		if n.State == StateAlive {
			alive = append(alive, addr)
		}
	}
	return alive
}

// AllNodes returns a copy of every NodeInfo in the cluster table.
func (cs *ClusterState) AllNodes() []NodeInfo {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	result := make([]NodeInfo, 0, len(cs.nodes))
	for _, n := range cs.nodes {
		result = append(result, *n)
	}
	return result
}

// Digest returns a compact GossipDigest for every known node.  The digest is
// used in the first leg of a gossip exchange to let the recipient decide which
// nodes it needs full info for.
func (cs *ClusterState) Digest() []GossipDigest {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	digests := make([]GossipDigest, 0, len(cs.nodes))
	for _, n := range cs.nodes {
		digests = append(digests, GossipDigest{
			Addr:       n.Addr,
			State:      n.State,
			Generation: n.Generation,
		})
	}
	return digests
}

// Merge compares incoming digests against local state and returns the list of
// addresses whose full NodeInfo should be requested from the sender because the
// sender appears to have newer information.
func (cs *ClusterState) Merge(digests []GossipDigest) (needFull []string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	for _, d := range digests {
		local, ok := cs.nodes[d.Addr]
		if !ok {
			// Unknown node — we want its full info.
			needFull = append(needFull, d.Addr)
			continue
		}

		if d.Generation > local.Generation {
			// Sender has newer info — request full NodeInfo.
			needFull = append(needFull, d.Addr)
			continue
		}

		// Same generation but different state: take the higher-numbered state
		// (Alive=0 < Suspect=1 < Dead=2 < Left=3 — monotone progression).
		if d.Generation == local.Generation && d.State > local.State {
			oldState := local.State
			local.State = d.State
			local.LastUpdated = time.Now()
			cs.fireChange(d.Addr, oldState, d.State)
		}
	}
	return needFull
}

// NextGeneration returns generation+1 for addr, or 2 if addr is unknown.
// Callers use this when they need to construct a state-change update.
func (cs *ClusterState) NextGeneration(addr string) uint64 {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	if n, ok := cs.nodes[addr]; ok {
		return n.Generation + 1
	}
	return 2
}

// SelfAddr returns the address this node identifies itself with.
func (cs *ClusterState) SelfAddr() string {
	return cs.selfAddr
}
