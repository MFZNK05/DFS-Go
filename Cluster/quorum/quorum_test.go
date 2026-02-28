package quorum_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Faizan2005/DFS-Go/Cluster/quorum"
	"github.com/Faizan2005/DFS-Go/Cluster/vclock"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// makeCoordinator builds a Coordinator whose sendMsg records all messages sent
// and whose targets list is fixed.
func makeCoordinator(
	self string,
	targets []string,
	cfg quorum.Config,
	sendMsg func(string, interface{}) error,
	localWrite func(string, string, []byte, vclock.VectorClock) error,
	localRead func(string) (vclock.VectorClock, int64, string, bool),
) *quorum.Coordinator {
	return quorum.New(
		cfg,
		self,
		func(_ string) []string { return targets },
		sendMsg,
		localWrite,
		localRead,
	)
}

// ---------------------------------------------------------------------------
// Write quorum
// ---------------------------------------------------------------------------

// W=2, 3 targets (self + 2 peers): self acks immediately, then we inject one
// remote ack → quorum met → Write returns nil.
func TestWriteQuorumMetWith2Acks(t *testing.T) {
	cfg := quorum.Config{N: 3, W: 2, R: 2, Timeout: 2 * time.Second}
	targets := []string{"self", "peer1", "peer2"}

	var coord *quorum.Coordinator
	coord = makeCoordinator(
		"self", targets, cfg,
		func(addr string, msg interface{}) error {
			// Simulate peer ack arriving asynchronously.
			go func() {
				time.Sleep(10 * time.Millisecond)
				coord.HandleWriteAck(quorum.WriteAck{
					NodeAddr: addr,
					Key:      "notes.pdf",
					Success:  true,
				})
			}()
			return nil
		},
		// local write always succeeds immediately.
		func(key, encKey string, data []byte, clock vclock.VectorClock) error { return nil },
		nil,
	)

	clock := vclock.New().Increment("self")
	err := coord.Write("notes.pdf", "hexkey", []byte("data"), clock)
	if err != nil {
		t.Errorf("expected nil error when quorum met, got: %v", err)
	}
}

// W=2, only self acks and both peers are unreachable → timeout → error.
func TestWriteQuorumFailsOnTimeout(t *testing.T) {
	cfg := quorum.Config{N: 3, W: 2, R: 2, Timeout: 100 * time.Millisecond}
	targets := []string{"self", "peer1", "peer2"}

	coord := makeCoordinator(
		"self", targets, cfg,
		func(addr string, msg interface{}) error {
			return errors.New("connection refused") // peers unreachable
		},
		func(key, encKey string, data []byte, clock vclock.VectorClock) error { return nil },
		nil,
	)

	clock := vclock.New().Increment("self")
	err := coord.Write("notes.pdf", "hexkey", []byte("data"), clock)
	if err == nil {
		t.Error("expected error when quorum not met, got nil")
	}
}

// Local write failure counts as a failed ack.
func TestWriteLocalFailureCountsAgainstQuorum(t *testing.T) {
	cfg := quorum.Config{N: 1, W: 1, R: 1, Timeout: 200 * time.Millisecond}
	targets := []string{"self"}

	coord := makeCoordinator(
		"self", targets, cfg,
		func(addr string, msg interface{}) error { return nil },
		// local write fails
		func(key, encKey string, data []byte, clock vclock.VectorClock) error {
			return errors.New("disk full")
		},
		nil,
	)

	err := coord.Write("k", "e", []byte("d"), vclock.New())
	if err == nil {
		t.Error("expected error when local write fails")
	}
}

// W=1 with only self as target → immediate success.
func TestWriteSingleNodeQuorum(t *testing.T) {
	cfg := quorum.Config{N: 1, W: 1, R: 1, Timeout: time.Second}
	targets := []string{"self"}

	coord := makeCoordinator("self", targets, cfg,
		func(string, interface{}) error { return nil },
		func(string, string, []byte, vclock.VectorClock) error { return nil },
		nil,
	)

	err := coord.Write("k", "e", []byte("d"), vclock.New())
	if err != nil {
		t.Errorf("single-node quorum should succeed immediately, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Read quorum
// ---------------------------------------------------------------------------

// R=2: two responses arrive, the one with the causally later clock wins.
func TestReadQuorumPicksCausallyLater(t *testing.T) {
	cfg := quorum.Config{N: 3, W: 2, R: 2, Timeout: 2 * time.Second}
	targets := []string{"self", "peer1", "peer2"}

	// Older version on self.
	olderClock := vclock.VectorClock{"self": 1}
	// Newer version on peer1 (saw self's write then wrote).
	newerClock := vclock.VectorClock{"self": 1, "peer1": 1}

	var coord *quorum.Coordinator
	var mu sync.Mutex
	_ = mu

	coord = makeCoordinator(
		"self", targets, cfg,
		func(addr string, msg interface{}) error {
			go func() {
				time.Sleep(5 * time.Millisecond)
				coord.HandleReadResponse(quorum.ReadResponse{
					NodeAddr:     addr,
					Key:          "notes.pdf",
					Found:        true,
					Clock:        newerClock,
					Timestamp:    2000,
					EncryptedKey: "newer-key",
				})
			}()
			return nil
		},
		nil,
		// local read returns the older version.
		func(key string) (vclock.VectorClock, int64, string, bool) {
			return olderClock, 1000, "older-key", true
		},
	)

	encKey, clock, err := coord.Read("notes.pdf")
	if err != nil {
		t.Fatalf("unexpected read error: %v", err)
	}
	if encKey != "newer-key" {
		t.Errorf("expected newer-key to win, got %s", encKey)
	}
	if clock.Compare(newerClock) != vclock.Equal {
		t.Errorf("expected newerClock to be returned, got %v", clock)
	}
}

// Concurrent clocks → LWW: higher Timestamp wins.
func TestReadQuorumLWWTiebreak(t *testing.T) {
	cfg := quorum.Config{N: 2, W: 2, R: 2, Timeout: 2 * time.Second}
	targets := []string{"self", "peer1"}

	clockA := vclock.VectorClock{"self": 1}  // concurrent with clockB
	clockB := vclock.VectorClock{"peer1": 1} // concurrent with clockA

	var coord *quorum.Coordinator
	coord = makeCoordinator(
		"self", targets, cfg,
		func(addr string, msg interface{}) error {
			go func() {
				time.Sleep(5 * time.Millisecond)
				coord.HandleReadResponse(quorum.ReadResponse{
					NodeAddr:     addr,
					Key:          "f.pdf",
					Found:        true,
					Clock:        clockB,
					Timestamp:    9000, // higher → should win
					EncryptedKey: "key-B",
				})
			}()
			return nil
		},
		nil,
		func(key string) (vclock.VectorClock, int64, string, bool) {
			return clockA, 1000, "key-A", true // lower timestamp
		},
	)

	encKey, _, err := coord.Read("f.pdf")
	if err != nil {
		t.Fatalf("unexpected read error: %v", err)
	}
	if encKey != "key-B" {
		t.Errorf("expected key-B (higher timestamp) to win LWW, got %s", encKey)
	}
}

// No responses within timeout → error.
func TestReadQuorumTimeout(t *testing.T) {
	cfg := quorum.Config{N: 3, W: 2, R: 2, Timeout: 80 * time.Millisecond}
	targets := []string{"self", "peer1", "peer2"}

	coord := makeCoordinator(
		"self", targets, cfg,
		func(addr string, msg interface{}) error { return errors.New("unreachable") },
		nil,
		// self also has no data
		func(key string) (vclock.VectorClock, int64, string, bool) { return nil, 0, "", false },
	)

	_, _, err := coord.Read("missing.pdf")
	if err == nil {
		t.Error("expected timeout error when no responses, got nil")
	}
}

// Partial quorum: R=2 but only 1 response arrives → degraded mode returns what it has.
func TestReadPartialQuorumDegradedMode(t *testing.T) {
	cfg := quorum.Config{N: 3, W: 2, R: 2, Timeout: 100 * time.Millisecond}
	targets := []string{"self", "peer1", "peer2"}

	coord := makeCoordinator(
		"self", targets, cfg,
		// peers don't respond
		func(addr string, msg interface{}) error { return nil },
		nil,
		// only self has data
		func(key string) (vclock.VectorClock, int64, string, bool) {
			return vclock.VectorClock{"self": 1}, 500, "my-key", true
		},
	)

	encKey, _, err := coord.Read("partial.pdf")
	if err != nil {
		t.Fatalf("partial quorum should degrade gracefully, got error: %v", err)
	}
	if encKey != "my-key" {
		t.Errorf("expected my-key from partial quorum, got %s", encKey)
	}
}

// ---------------------------------------------------------------------------
// HandleWriteAck / HandleReadResponse routing
// ---------------------------------------------------------------------------

// Acks that arrive after the operation times out must be silently dropped.
func TestLateAckDroppedGracefully(t *testing.T) {
	cfg := quorum.Config{N: 2, W: 2, R: 2, Timeout: 50 * time.Millisecond}
	targets := []string{"self", "peer1"}

	coord := makeCoordinator("self", targets, cfg,
		func(addr string, msg interface{}) error { return nil },
		func(string, string, []byte, vclock.VectorClock) error { return nil },
		nil,
	)

	// Let write time out (peer1 never acks).
	clock := vclock.New()
	_ = coord.Write("late.pdf", "k", []byte("d"), clock)

	// Inject a late ack after the operation is already done — must not panic.
	coord.HandleWriteAck(quorum.WriteAck{NodeAddr: "peer1", Key: "late.pdf", Success: true})
}
