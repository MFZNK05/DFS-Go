// Package quorum implements W-of-N write confirmation and R-of-N read quorum
// for the DFS cluster.
//
// The Coordinator drives both operations:
//
//   - Write: sends MessageQuorumWrite to all N replicas, waits for W acks
//     within Timeout.  Returns nil only when quorum is met.
//   - Read:  sends MessageQuorumRead to all N replicas, waits for R metadata
//     responses, runs conflict.LWWResolver to pick the authoritative version.
//
// Pending operations are tracked in sync.Maps keyed by the file key so that
// inbound ack/response messages (delivered by server.go's handleMessage) can
// be routed back to the waiting goroutine without any shared mutex.
package quorum

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Faizan2005/DFS-Go/Cluster/conflict"
	"github.com/Faizan2005/DFS-Go/Cluster/vclock"
)

// Config holds quorum parameters.
type Config struct {
	N       int           // total replicas (usually == ReplicationFactor)
	W       int           // write quorum
	R       int           // read quorum
	Timeout time.Duration // per-operation deadline
}

// DefaultConfig returns sensible defaults (N=3, W=2, R=2, timeout=5s).
func DefaultConfig() Config {
	return Config{N: 3, W: 2, R: 2, Timeout: 5 * time.Second}
}

// WriteAck is the acknowledgement sent back by a replica after a successful write.
type WriteAck struct {
	NodeAddr string
	Key      string
	Success  bool
	ErrMsg   string
}

// ReadResponse is the metadata reply sent back by a replica for a read probe.
type ReadResponse struct {
	NodeAddr  string
	Key       string
	Found     bool
	Clock     vclock.VectorClock
	Timestamp int64
}

// MessageQuorumWrite is sent from the coordinator to each replica.
type MessageQuorumWrite struct {
	Key   string
	Data  []byte
	Clock map[string]uint64 // serialised VectorClock
}

// MessageQuorumWriteAck is sent from a replica back to the coordinator.
type MessageQuorumWriteAck struct {
	Key     string
	From    string
	Success bool
	ErrMsg  string
}

// MessageQuorumRead asks a replica whether it has metadata for key.
type MessageQuorumRead struct {
	Key string
}

// MessageQuorumReadResponse carries a replica's metadata for key.
type MessageQuorumReadResponse struct {
	Key       string
	From      string
	Found     bool
	Clock     map[string]uint64
	Timestamp int64
}

// Coordinator drives quorum operations. It is safe for concurrent use.
type Coordinator struct {
	cfg      Config
	selfAddr string

	// getTargets returns the N nodes responsible for key (from the hash ring).
	getTargets func(key string) []string

	// sendMsg delivers a message to a specific peer address.
	sendMsg func(addr string, msg interface{}) error

	// localWrite stores the write locally on self (bypasses network).
	localWrite func(key string, data []byte, clock vclock.VectorClock) error

	// localRead reads local metadata for key (nil clock if not found).
	localRead func(key string) (clock vclock.VectorClock, ts int64, found bool)

	writeAcks sync.Map // map[string]chan WriteAck
	readResps sync.Map // map[string]chan ReadResponse

	resolver conflict.Resolver
}

// New creates a Coordinator.
func New(
	cfg Config,
	selfAddr string,
	getTargets func(key string) []string,
	sendMsg func(addr string, msg interface{}) error,
	localWrite func(key string, data []byte, clock vclock.VectorClock) error,
	localRead func(key string) (vclock.VectorClock, int64, bool),
) *Coordinator {
	return &Coordinator{
		cfg:        cfg,
		selfAddr:   selfAddr,
		getTargets: getTargets,
		sendMsg:    sendMsg,
		localWrite: localWrite,
		localRead:  localRead,
		resolver:   conflict.NewLWWResolver(),
	}
}

// Write replicates (key, data, clock) to all N targets and waits for
// at least W successful acks within cfg.Timeout. Returns nil on success.
func (c *Coordinator) Write(key string, data []byte, clock vclock.VectorClock) error {
	targets := c.getTargets(key)

	ackCh := make(chan WriteAck, len(targets)+1)
	c.writeAcks.Store(key, ackCh)
	defer c.writeAcks.Delete(key)

	for _, addr := range targets {
		if addr == c.selfAddr {
			// Local write — handled directly.
			go func() {
				var err error
				if c.localWrite != nil {
					err = c.localWrite(key, data, clock)
				}
				if err != nil {
					ackCh <- WriteAck{NodeAddr: addr, Key: key, Success: false, ErrMsg: err.Error()}
				} else {
					ackCh <- WriteAck{NodeAddr: addr, Key: key, Success: true}
				}
			}()
		} else {
			go func(target string) {
				msg := &MessageQuorumWrite{
					Key:   key,
					Data:  data,
					Clock: map[string]uint64(clock),
				}
				if err := c.sendMsg(target, msg); err != nil {
					log.Printf("[quorum] write send to %s failed: %v", target, err)
					ackCh <- WriteAck{NodeAddr: target, Key: key, Success: false, ErrMsg: err.Error()}
				}
				// Successful ack arrives via HandleWriteAck; nothing to do here.
			}(addr)
		}
	}

	timer := time.NewTimer(c.cfg.Timeout)
	defer timer.Stop()

	acks := 0
	for {
		select {
		case ack := <-ackCh:
			if ack.Success {
				acks++
				if acks >= c.cfg.W {
					return nil
				}
			}
		case <-timer.C:
			return fmt.Errorf("quorum write: timeout — got %d/%d acks for key %q", acks, c.cfg.W, key)
		}
	}
}

// Read asks all N targets for their metadata for key, waits for R responses,
// and returns the authoritative version according to LWW conflict resolution.
// Returns (clock, error).
func (c *Coordinator) Read(key string) (vclock.VectorClock, error) {
	targets := c.getTargets(key)

	respCh := make(chan ReadResponse, len(targets)+1)
	c.readResps.Store(key, respCh)
	defer c.readResps.Delete(key)

	for _, addr := range targets {
		if addr == c.selfAddr {
			go func() {
				if c.localRead != nil {
					clock, ts, found := c.localRead(key)
					respCh <- ReadResponse{
						NodeAddr: addr, Key: key, Found: found,
						Clock: clock, Timestamp: ts,
					}
				}
			}()
		} else {
			go func(target string) {
				if err := c.sendMsg(target, &MessageQuorumRead{Key: key}); err != nil {
					log.Printf("[quorum] read probe to %s failed: %v", target, err)
				}
				// Response arrives via HandleReadResponse.
			}(addr)
		}
	}

	timer := time.NewTimer(c.cfg.Timeout)
	defer timer.Stop()

	var responses []ReadResponse
	for {
		select {
		case resp := <-respCh:
			if resp.Found {
				responses = append(responses, resp)
			}
			if len(responses) >= c.cfg.R {
				return c.resolveRead(responses)
			}
		case <-timer.C:
			if len(responses) == 0 {
				return nil, fmt.Errorf("quorum read: timeout — no responses for key %q", key)
			}
			// Partial quorum — resolve what we have (degraded mode).
			log.Printf("[quorum] read partial quorum for %q: %d/%d responses", key, len(responses), c.cfg.R)
			return c.resolveRead(responses)
		}
	}
}

func (c *Coordinator) resolveRead(responses []ReadResponse) (vclock.VectorClock, error) {
	versions := make([]conflict.Version, 0, len(responses))
	for _, r := range responses {
		versions = append(versions, conflict.Version{
			NodeAddr:  r.NodeAddr,
			Clock:     vclock.VectorClock(r.Clock),
			Timestamp: r.Timestamp,
		})
	}
	winner := c.resolver.Resolve(versions)
	return winner.Clock, nil
}

// HandleWriteAck routes an incoming ack to the waiting Write call.
func (c *Coordinator) HandleWriteAck(ack WriteAck) {
	if ch, ok := c.writeAcks.Load(ack.Key); ok {
		select {
		case ch.(chan WriteAck) <- ack:
		default:
		}
	}
}

// HandleReadResponse routes an incoming read response to the waiting Read call.
func (c *Coordinator) HandleReadResponse(resp ReadResponse) {
	if ch, ok := c.readResps.Load(resp.Key); ok {
		select {
		case ch.(chan ReadResponse) <- resp:
		default:
		}
	}
}
