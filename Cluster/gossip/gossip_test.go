package gossip

import (
	"sync"
	"testing"
	"time"

	"github.com/Faizan2005/DFS-Go/Cluster/membership"
)

// makeCluster builds a ClusterState pre-populated with selfAddr.
func makeCluster(self string) *membership.ClusterState {
	return membership.New(self)
}

// noopSend is a sendMsg that always succeeds but does nothing.
func noopSend(_ string, _ interface{}) error { return nil }

// captureSend records every (addr, msg) pair sent.
type captureSend struct {
	mu   sync.Mutex
	sent []capturedMsg
}

type capturedMsg struct {
	addr string
	msg  interface{}
}

func (c *captureSend) send(addr string, msg interface{}) error {
	c.mu.Lock()
	c.sent = append(c.sent, capturedMsg{addr, msg})
	c.mu.Unlock()
	return nil
}

func (c *captureSend) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sent)
}

func (c *captureSend) addrs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	addrs := make([]string, len(c.sent))
	for i, m := range c.sent {
		addrs[i] = m.addr
	}
	return addrs
}

// ----------------------------------------------------------------------------
// TestGossipConvergence3Nodes
// A knows about C. B does not. After one HandleDigest + HandleResponse cycle
// driven by A→B, B should learn about C without any real timers.
// ----------------------------------------------------------------------------
func TestGossipConvergence3Nodes(t *testing.T) {
	csA := makeCluster("A")
	csB := makeCluster("B")

	// A already knows about C (e.g. C dialled A earlier).
	csA.AddNode("C", nil)
	csA.UpdateState("C", membership.StateAlive, 2)

	capture := &captureSend{}

	gsB := New(DefaultConfig(), "B", csB,
		func() []string { return []string{"A"} },
		capture.send,
		nil,
	)

	// Simulate: A sends its digest to B.
	digestFromA := csA.Digest()
	gsB.HandleDigest("A", &MessageGossipDigest{From: "A", Digests: digestFromA})

	// B should now know about C (applied from the response path — but HandleDigest
	// only merges and sends a response. The response carries full NodeInfo back to A,
	// not to B. So we also simulate A sending the full info back in a response.)

	// What HandleDigest does: merge A's digest → needFull may include "C" (B doesn't know it).
	// B then sends a MessageGossipResponse to A with B's full state.
	// Meanwhile A's response to B (second leg) carries full info for C.
	// Simulate that second leg: A sends MessageGossipResponse to B.

	fullFromA := csA.GetNodes([]string{"A", "C"})
	gsB.HandleResponse("A", &MessageGossipResponse{
		From:     "A",
		Full:     fullFromA,
		MyDigest: csA.Digest(),
	})

	// B should now know C is Alive.
	n, ok := csB.GetNode("C")
	if !ok {
		t.Fatal("B should know about C after gossip exchange, but does not")
	}
	if n.State != membership.StateAlive {
		t.Errorf("expected C to be Alive on B, got %s", n.State)
	}
}

// ----------------------------------------------------------------------------
// TestFanOutLimitRespected
// With 10 peers but FanOut=3, doGossipRound must contact exactly 3.
// ----------------------------------------------------------------------------
func TestFanOutLimitRespected(t *testing.T) {
	cs := makeCluster("self")

	peers := []string{"p1", "p2", "p3", "p4", "p5", "p6", "p7", "p8", "p9", "p10"}
	for _, p := range peers {
		cs.AddNode(p, nil)
	}

	capture := &captureSend{}
	cfg := Config{FanOut: 3, Interval: time.Hour} // long interval — we drive manually

	gs := New(cfg, "self", cs,
		func() []string { return peers },
		capture.send,
		nil,
	)

	gs.doGossipRound()

	if capture.count() != 3 {
		t.Errorf("expected exactly 3 messages sent (fanout=3), got %d", capture.count())
	}
}

// ----------------------------------------------------------------------------
// TestHandleDigestRequestsFull
// When a digest contains an addr unknown locally, HandleDigest must send a
// response that includes a request for full info (by including it in needFull,
// which causes the response Full slice to be populated from local state).
// We verify a MessageGossipResponse is sent back to the digest sender.
// ----------------------------------------------------------------------------
func TestHandleDigestRequestsFull(t *testing.T) {
	cs := makeCluster("self")
	// self does NOT know about "stranger"

	capture := &captureSend{}
	gs := New(DefaultConfig(), "self", cs,
		func() []string { return []string{} },
		capture.send,
		nil,
	)

	gs.HandleDigest("peer1", &MessageGossipDigest{
		From: "peer1",
		Digests: []membership.GossipDigest{
			{Addr: "stranger", State: membership.StateAlive, Generation: 1},
		},
	})

	// HandleDigest must send exactly one response back to "peer1".
	if capture.count() != 1 {
		t.Fatalf("expected 1 response sent, got %d", capture.count())
	}
	addrs := capture.addrs()
	if addrs[0] != "peer1" {
		t.Errorf("response should be addressed to peer1, got %s", addrs[0])
	}

	// The response must be a *MessageGossipResponse.
	capture.mu.Lock()
	_, ok := capture.sent[0].msg.(*MessageGossipResponse)
	capture.mu.Unlock()
	if !ok {
		t.Error("sent message should be *MessageGossipResponse")
	}
}

// ----------------------------------------------------------------------------
// TestStaleDigestIgnored
// A digest with a lower generation than local should not downgrade local state.
// ----------------------------------------------------------------------------
func TestStaleDigestIgnored(t *testing.T) {
	cs := makeCluster("self")
	cs.AddNode("peer1", nil)
	cs.UpdateState("peer1", membership.StateDead, 10) // local gen=10

	gs := New(DefaultConfig(), "self", cs,
		func() []string { return []string{} },
		noopSend,
		nil,
	)

	// Incoming digest claims peer1 is Alive at gen=3 (stale).
	gs.HandleDigest("somenode", &MessageGossipDigest{
		From: "somenode",
		Digests: []membership.GossipDigest{
			{Addr: "peer1", State: membership.StateAlive, Generation: 3},
		},
	})

	n, _ := cs.GetNode("peer1")
	if n.State != membership.StateDead {
		t.Errorf("stale digest should not overwrite local state: expected Dead, got %s", n.State)
	}
	if n.Generation != 10 {
		t.Errorf("generation should remain 10, got %d", n.Generation)
	}
}

// ----------------------------------------------------------------------------
// TestNewNodeDiscovery
// A response carrying a NodeInfo for an unknown Alive node fires onNewPeer.
// ----------------------------------------------------------------------------
func TestNewNodeDiscovery(t *testing.T) {
	cs := makeCluster("self")

	newPeerCh := make(chan string, 1)

	gs := New(DefaultConfig(), "self", cs,
		func() []string { return []string{} },
		noopSend,
		func(addr string) { newPeerCh <- addr },
	)

	gs.HandleResponse("peer1", &MessageGossipResponse{
		From: "peer1",
		Full: []membership.NodeInfo{
			{Addr: "brand-new", State: membership.StateAlive, Generation: 1},
		},
		MyDigest: nil,
	})

	select {
	case addr := <-newPeerCh:
		if addr != "brand-new" {
			t.Errorf("expected onNewPeer(brand-new), got %s", addr)
		}
	case <-time.After(500 * time.Millisecond):
		t.Error("onNewPeer was not called within 500ms")
	}
}

// ----------------------------------------------------------------------------
// TestHandleResponseAppliesNewerState
// A response with a higher-generation NodeInfo should update local state.
// ----------------------------------------------------------------------------
func TestHandleResponseAppliesNewerState(t *testing.T) {
	cs := makeCluster("self")
	cs.AddNode("peer1", nil) // gen=1, Alive

	gs := New(DefaultConfig(), "self", cs,
		func() []string { return []string{} },
		noopSend,
		nil,
	)

	gs.HandleResponse("somenode", &MessageGossipResponse{
		From: "somenode",
		Full: []membership.NodeInfo{
			{Addr: "peer1", State: membership.StateDead, Generation: 5},
		},
		MyDigest: nil,
	})

	n, _ := cs.GetNode("peer1")
	if n.State != membership.StateDead {
		t.Errorf("expected peer1 state=Dead after response, got %s", n.State)
	}
	if n.Generation != 5 {
		t.Errorf("expected generation=5, got %d", n.Generation)
	}
}

// ----------------------------------------------------------------------------
// TestHandleResponseIgnoresSelf
// A response must never overwrite our own self entry.
// ----------------------------------------------------------------------------
func TestHandleResponseIgnoresSelf(t *testing.T) {
	cs := makeCluster("self")

	gs := New(DefaultConfig(), "self", cs,
		func() []string { return []string{} },
		noopSend,
		nil,
	)

	// A malicious/buggy peer claims "self" is Dead at gen=99.
	gs.HandleResponse("peer1", &MessageGossipResponse{
		From: "peer1",
		Full: []membership.NodeInfo{
			{Addr: "self", State: membership.StateDead, Generation: 99},
		},
		MyDigest: nil,
	})

	n, _ := cs.GetNode("self")
	if n.State != membership.StateAlive {
		t.Errorf("self entry must remain Alive, got %s", n.State)
	}
}

// ----------------------------------------------------------------------------
// TestStartStop — Start/Stop do not panic and Stop is idempotent.
// ----------------------------------------------------------------------------
func TestStartStop(t *testing.T) {
	cs := makeCluster("self")
	gs := New(DefaultConfig(), "self", cs,
		func() []string { return []string{} },
		noopSend,
		nil,
	)

	gs.Start()
	time.Sleep(10 * time.Millisecond) // let loop spin once
	gs.Stop()
	gs.Stop() // idempotent — must not panic
}
