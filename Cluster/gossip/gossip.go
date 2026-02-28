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
	Full     []membership.NodeInfo    // full records the receiver wanted
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
func (gs *GossipService) HandleDigest(from string, msg *MessageGossipDigest) {
	needFull := gs.cluster.Merge(msg.Digests)

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

	// Apply full NodeInfo updates.
	for _, info := range msg.Full {
		if info.Addr == gs.selfAddr {
			continue // never overwrite self
		}
		applied := gs.cluster.UpdateState(info.Addr, info.State, info.Generation)
		if applied && info.Metadata != nil {
			gs.cluster.SetMetadata(info.Addr, info.Metadata)
		}

		// If this is a node we hadn't seen before and it is Alive, notify.
		if !knownBefore[info.Addr] && info.State == membership.StateAlive && gs.onNewPeer != nil {
			log.Printf("[gossip] discovered new peer %s via gossip from %s", info.Addr, from)
			go gs.onNewPeer(info.Addr)
		}
	}

	// Also merge their digest in case they know something we missed.
	gs.cluster.Merge(msg.MyDigest)
}
