// Package gossip implements a push-pull epidemic dissemination protocol.
//
// Each round a node selects up to FanOut peers at random, sends them a compact
// digest of its cluster view, and they reply with full NodeInfo for any entries
// that appear newer in the digest.  This converges the whole cluster to a
// consistent view in O(log N) rounds.
//
// Wire integration: the gossip service does not manage TCP connections.  It
// works through two closures provided by the caller:
//   - getPeers() — returns all currently reachable peer addresses
//   - sendMsg(addr, msg) — delivers a message to a specific peer
//
// The server.go message dispatcher calls HandleDigest / HandleResponse when the
// corresponding gob-decoded messages arrive.
package gossip

import (
	"log"
	"math/rand"
	"sync"
	"time"

	"github.com/Faizan2005/DFS-Go/Cluster/membership"
)

// Config tunes the gossip protocol.
type Config struct {
	FanOut   int           // peers contacted per round, default 3
	Interval time.Duration // gossip round period, default 200ms
}

// DefaultConfig returns sensible defaults.
func DefaultConfig() Config {
	return Config{
		FanOut:   3,
		Interval: 200 * time.Millisecond,
	}
}

// MessageGossipDigest is sent as the first leg of a gossip exchange.
// The receiver compares the digests against its local state and responds with
// full NodeInfo for entries it believes are stale.
type MessageGossipDigest struct {
	From    string
	Digests []membership.GossipDigest
}

// MessageGossipResponse carries full NodeInfo entries requested by the receiver
// of a digest, along with the sender's own digest so both sides can update.
type MessageGossipResponse struct {
	From     string
	Full     []membership.NodeInfo     // full records the receiver wanted
	MyDigest []membership.GossipDigest // sender's current state
}

// GossipService drives the epidemic dissemination loop.
// Safe for concurrent use; Start/Stop control the background goroutine.
type GossipService struct {
	cfg      Config
	selfAddr string
	cluster  *membership.ClusterState

	getPeers func() []string                          // all known peer addresses
	sendMsg  func(addr string, msg interface{}) error // deliver a message to a peer

	// onNewPeer is called when gossip reveals a peer we have not dialled yet.
	// The caller should dial the new address so it appears in getPeers().
	onNewPeer func(addr string)

	stopCh chan struct{}
	once   sync.Once // guards Stop
}

// isConnected returns true if addr appears in the current getPeers() list.
func (gs *GossipService) isConnected(addr string) bool {
	for _, p := range gs.getPeers() {
		if p == addr {
			return true
		}
	}
	return false
}

// New creates a GossipService.
//
//   - cluster    — shared membership table (also written by failure detection)
//   - getPeers   — closure returning currently connected peer addresses
//   - sendMsg    — closure that encodes and delivers a message to addr
//   - onNewPeer  — called when gossip learns of a previously unknown peer (may be nil)
func New(
	cfg Config,
	selfAddr string,
	cluster *membership.ClusterState,
	getPeers func() []string,
	sendMsg func(addr string, msg interface{}) error,
	onNewPeer func(addr string),
) *GossipService {
	return &GossipService{
		cfg:       cfg,
		selfAddr:  selfAddr,
		cluster:   cluster,
		getPeers:  getPeers,
		sendMsg:   sendMsg,
		onNewPeer: onNewPeer,
		stopCh:    make(chan struct{}),
	}
}

// Start launches the background gossip loop.  Calling Start more than once is
// safe but only the first call has effect.
func (gs *GossipService) Start() {
	go gs.gossipLoop()
}

// Stop shuts down the gossip loop.  Safe to call multiple times.
func (gs *GossipService) Stop() {
	gs.once.Do(func() { close(gs.stopCh) })
}

// gossipLoop ticks at Config.Interval and performs one gossip round per tick.
func (gs *GossipService) gossipLoop() {
	ticker := time.NewTicker(gs.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			gs.doGossipRound()
		case <-gs.stopCh:
			return
		}
	}
}

// doGossipRound picks up to FanOut random peers and sends them a digest.
func (gs *GossipService) doGossipRound() {
	peers := gs.getPeers()
	if len(peers) == 0 {
		return
	}

	// Shuffle in place (Go 1.20+ rand.Shuffle is safe without explicit source).
	rand.Shuffle(len(peers), func(i, j int) { peers[i], peers[j] = peers[j], peers[i] })

	fanOut := gs.cfg.FanOut
	if fanOut > len(peers) {
		fanOut = len(peers)
	}
	targets := peers[:fanOut]

	digest := gs.cluster.Digest()
	msg := &MessageGossipDigest{
		From:    gs.selfAddr,
		Digests: digest,
	}

	for _, target := range targets {
		if err := gs.sendMsg(target, msg); err != nil {
			log.Printf("[gossip] failed to send digest to %s: %v", target, err)
		}
	}
}

// HandleDigest processes an incoming gossip digest from a peer.
// It merges the digest into the local cluster state, then sends back:
//  1. Full NodeInfo for any entries the peer claims are newer than local.
//  2. Our own digest so the sender can update its view too.
//
// If the digest contains nodes we have never seen, we also call onNewPeer so
// the server can open an outbound connection to them.
func (gs *GossipService) HandleDigest(from string, msg *MessageGossipDigest) {
	// Record which addrs we already know BEFORE the merge so we can detect
	// newly discovered nodes afterwards.
	knownBefore := make(map[string]bool)
	for _, n := range gs.cluster.AllNodes() {
		knownBefore[n.Addr] = true
	}

	needFull := gs.cluster.Merge(msg.Digests)

	// Build a digest map for quick state lookup.
	digestMap := make(map[string]membership.GossipDigest, len(msg.Digests))
	for _, d := range msg.Digests {
		digestMap[d.Addr] = d
	}

	log.Printf("[TRACE][HandleDigest] self=%s from=%s needFull=%v knownBefore_count=%d digest_count=%d",
		gs.selfAddr, from, needFull, len(knownBefore), len(msg.Digests))

	// Collect addresses to dial — use a set to avoid dialling the same addr twice
	// (both the needFull loop and the digest_scan loop might flag the same addr).
	toDialSet := make(map[string]struct{})

	if gs.onNewPeer != nil {
		for _, addr := range needFull {
			if addr == gs.selfAddr {
				continue
			}
			connected := gs.isConnected(addr)
			d := digestMap[addr]
			log.Printf("[TRACE][HandleDigest] needFull: self=%s addr=%s connected=%v knownBefore=%v d.State=%v d.Gen=%d",
				gs.selfAddr, addr, connected, knownBefore[addr], d.State, d.Generation)
			if !connected {
				if !knownBefore[addr] {
					log.Printf("[TRACE][HandleDigest] NEW peer %s via digest from %s → queued", addr, from)
					toDialSet[addr] = struct{}{}
				} else if d.State == membership.StateAlive {
					// Previously known but was StateLeft/StateDead — now alive again (rejoin).
					local, ok := gs.cluster.GetNode(addr)
					log.Printf("[TRACE][HandleDigest] KNOWN addr=%s local.State=%v local.Gen=%d ok=%v d.Gen=%d", addr, local.State, local.Generation, ok, d.Generation)
					if ok && local.State != membership.StateAlive {
						log.Printf("[TRACE][HandleDigest] REJOIN via needFull: addr=%s was=%v → queued", addr, local.State)
						toDialSet[addr] = struct{}{}
					}
				}
			}
		}
		// Also check every digest entry — the sender might be new or rejoined.
		for _, d := range msg.Digests {
			if d.Addr == gs.selfAddr {
				continue
			}
			connected := gs.isConnected(d.Addr)
			log.Printf("[TRACE][HandleDigest] digest_scan: self=%s addr=%s d.State=%v d.Gen=%d connected=%v knownBefore=%v",
				gs.selfAddr, d.Addr, d.State, d.Generation, connected, knownBefore[d.Addr])
			if d.State == membership.StateAlive && !connected {
				if !knownBefore[d.Addr] {
					// Ensure the node is registered in our membership table.
					gs.cluster.AddNode(d.Addr, nil)
					gs.cluster.UpdateState(d.Addr, d.State, d.Generation)
					log.Printf("[TRACE][HandleDigest] digest_scan NEW: addr=%s from=%s → queued", d.Addr, from)
					toDialSet[d.Addr] = struct{}{}
				} else {
					// Known node — check if it was non-alive and is now advertising Alive with higher gen.
					local, ok := gs.cluster.GetNode(d.Addr)
					log.Printf("[TRACE][HandleDigest] digest_scan KNOWN: addr=%s local.State=%v local.Gen=%d d.Gen=%d ok=%v",
						d.Addr, local.State, local.Generation, d.Generation, ok)
					if ok && local.State != membership.StateAlive && d.Generation > local.Generation {
						log.Printf("[TRACE][HandleDigest] REJOIN via scan: addr=%s was=%v gen=%d>%d → queued", d.Addr, local.State, d.Generation, local.Generation)
						toDialSet[d.Addr] = struct{}{}
					}
				}
			}
		}

		// Dial all queued addresses exactly once.
		for addr := range toDialSet {
			log.Printf("[TRACE][HandleDigest] DIALLING addr=%s from self=%s", addr, gs.selfAddr)
			go gs.onNewPeer(addr)
		}
	}

	full := gs.cluster.GetNodes(needFull)

	resp := &MessageGossipResponse{
		From:     gs.selfAddr,
		Full:     full,
		MyDigest: gs.cluster.Digest(),
	}

	if err := gs.sendMsg(from, resp); err != nil {
		log.Printf("[gossip] failed to send response to %s: %v", from, err)
	}
}

// HandleResponse processes the full NodeInfo entries returned by a peer that
// received our digest.  For each entry we apply the state update if the
// generation is higher than local.  If a node is newly discovered we call the
// onNewPeer callback so the server can dial it.
func (gs *GossipService) HandleResponse(from string, msg *MessageGossipResponse) {
	// Collect which addrs we already know before merging.
	knownBefore := make(map[string]bool)
	for _, n := range gs.cluster.AllNodes() {
		knownBefore[n.Addr] = true
	}

	log.Printf("[TRACE][HandleResponse] self=%s from=%s full_count=%d my_digest_count=%d knownBefore=%d",
		gs.selfAddr, from, len(msg.Full), len(msg.MyDigest), len(knownBefore))

	// Apply full NodeInfo updates.
	for _, info := range msg.Full {
		if info.Addr == gs.selfAddr {
			continue // never overwrite self
		}
		applied := gs.cluster.UpdateState(info.Addr, info.State, info.Generation)
		if applied && info.Metadata != nil {
			gs.cluster.SetMetadata(info.Addr, info.Metadata)
		}
		connected := gs.isConnected(info.Addr)
		log.Printf("[TRACE][HandleResponse] self=%s info.Addr=%s info.State=%v info.Gen=%d applied=%v connected=%v knownBefore=%v",
			gs.selfAddr, info.Addr, info.State, info.Generation, applied, connected, knownBefore[info.Addr])

		// If this is a node we hadn't seen before and it is Alive, notify.
		if !knownBefore[info.Addr] && info.State == membership.StateAlive && gs.onNewPeer != nil {
			log.Printf("[TRACE][HandleResponse] NEW peer %s via response from %s → dialling", info.Addr, from)
			go gs.onNewPeer(info.Addr)
		} else if knownBefore[info.Addr] && info.State == membership.StateAlive && !connected && gs.onNewPeer != nil {
			// Known node that was non-Alive and is now Alive — rejoin case from Response path.
			localBefore, lokOk := gs.cluster.GetNode(info.Addr)
			log.Printf("[TRACE][HandleResponse] KNOWN alive addr=%s lokOk=%v local.State=%v local.Gen=%d info.Gen=%d",
				info.Addr, lokOk, localBefore.State, localBefore.Generation, info.Generation)
		}
	}

	// Also merge their digest in case they know something we missed.
	gs.cluster.Merge(msg.MyDigest)
}
