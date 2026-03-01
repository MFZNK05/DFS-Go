# DFS-Go: Complete Distributed Architecture Implementation Plan

## Sprint 2: Failure Detection & Recovery

**Goal:** Detect dead nodes reliably. Never lose a write because a target is temporarily down. Automatically re-replicate when a node dies.

**Depends on:** Sprint 1 (HashRing)
**Enables:** Sprints 3, 5

### Package: `Cluster/failure/`
**File:** `Cluster/failure/detector.go`

#### Structs

```go
// Config for the failure detector
type Config struct {
    HeartbeatInterval  time.Duration  // default 1s — how often to send pings
    SuspectThreshold   float64        // default 8.0 — phi value that marks suspect
    DeadTimeout        time.Duration  // default 30s — time after suspect before declared dead
    WindowSize         int            // default 200 — samples in sliding window
}

// PeerWindow tracks inter-arrival heartbeat times for one peer
type PeerWindow struct {
    mu          sync.Mutex
    intervals   []float64   // ring buffer of millisecond inter-arrival times
    head        int         // next write position
    count        int         // filled entries
    lastArrival time.Time   // wall time of last heartbeat
    capacity    int
}

// PhiAccrualDetector implements the Phi Accrual algorithm per peer
type PhiAccrualDetector struct {
    mu      sync.RWMutex
    windows map[string]*PeerWindow  // addr -> arrival window
    cfg     Config
}

// HeartbeatService sends periodic pings and drives the detector
type HeartbeatService struct {
    cfg       Config
    selfAddr  string
    detector  *PhiAccrualDetector
    getPeers  func() map[string]peer2peer.Peer  // closure over server peers map
    sendMsg   func(addr string, msg interface{}) error  // closure over server send
    onSuspect func(addr string)   // called when phi >= threshold
    onDead    func(addr string)   // called when peer is declared dead
    mu        sync.RWMutex
    states    map[string]peerState  // addr -> current state
    stopCh    chan struct{}
}

type peerState int
const (
    stateAlive   peerState = iota
    stateSuspect
    stateDead
)
```

#### Functions & Methods

```go
// NewConfig returns default config
func NewConfig() Config
  // returns Config{HeartbeatInterval:1s, SuspectThreshold:8.0, DeadTimeout:30s, WindowSize:200}

// newPeerWindow creates a ring buffer of given capacity
func newPeerWindow(capacity int) *PeerWindow
  // allocates intervals slice, sets capacity, lastArrival=now

// (w *PeerWindow) record(now time.Time)
  // 1. if lastArrival is zero, set lastArrival=now, return (first sample has no interval)
  // 2. interval = now.Sub(lastArrival).Seconds() * 1000  (milliseconds as float64)
  // 3. intervals[head] = interval; head = (head+1) % capacity; count = min(count+1, capacity)
  // 4. lastArrival = now

// (w *PeerWindow) phi(now time.Time) float64
  // 1. if count < 2, return 0.0 (not enough samples)
  // 2. compute mean = sum(intervals[:count]) / count
  // 3. compute stddev = sqrt(sum((x-mean)^2) / count)
  // 4. if stddev < 0.1, stddev = mean * 0.1 (prevent degenerate case)
  // 5. elapsed = now.Sub(lastArrival).Seconds() * 1000
  // 6. y = (elapsed - mean) / (stddev * sqrt(2))
  // 7. cdf = 0.5 * (1 + erf(y))   — use math.Erf
  // 8. if cdf >= 1.0, return 16.0  (cap at max phi)
  // 9. return -math.Log10(1.0 - cdf)

// NewPhiAccrualDetector(cfg Config) *PhiAccrualDetector
  // initialises empty windows map

// (d *PhiAccrualDetector) RecordHeartbeat(addr string, arrivedAt time.Time)
  // 1. lock d.mu
  // 2. if window not exists: create newPeerWindow(cfg.WindowSize)
  // 3. call window.record(arrivedAt)

// (d *PhiAccrualDetector) Phi(addr string) float64
  // 1. rlock d.mu; get window; if not found return 16.0 (presumed dead)
  // 2. return window.phi(time.Now())

// (d *PhiAccrualDetector) IsAlive(addr string) bool
  // return d.Phi(addr) < d.cfg.SuspectThreshold

// (d *PhiAccrualDetector) Remove(addr string)
  // delete(d.windows, addr)

// NewHeartbeatService(cfg Config, selfAddr string, getPeers func()map[string]Peer, sendMsg func(string,interface{})error) *HeartbeatService
  // creates service with empty states map, stopCh

// (hs *HeartbeatService) Start()   — launches goroutines
  // go hs.senderLoop()    — sends heartbeats
  // go hs.reaperLoop()    — checks phi and fires callbacks

// (hs *HeartbeatService) Stop()
  // close(hs.stopCh)

// (hs *HeartbeatService) RecordHeartbeat(from string)
  // hs.detector.RecordHeartbeat(from, time.Now())
  // if states[from] == stateSuspect or stateDead: reset to stateAlive, log recovery

// (hs *HeartbeatService) senderLoop()
  // ticker := time.NewTicker(cfg.HeartbeatInterval)
  // for { select { case <-ticker.C: send heartbeat to all current peers; case <-stopCh: return } }
  // send: iterate getPeers(), call sendMsg(addr, MessageHeartbeat{From: selfAddr, Timestamp: now.UnixNano()})

// (hs *HeartbeatService) reaperLoop()
  // ticker := time.NewTicker(cfg.HeartbeatInterval / 2)   — check at 2x frequency
  // for each peer in getPeers():
  //   phi := detector.Phi(peer)
  //   if phi >= cfg.SuspectThreshold && states[peer] == stateAlive:
  //     states[peer] = stateSuspect; log; if onSuspect != nil: go onSuspect(peer)
  //   if states[peer] == stateSuspect && timeSinceSuspect > cfg.DeadTimeout:
  //     states[peer] = stateDead; log; if onDead != nil: go onDead(peer)
```

**File:** `Cluster/failure/detector_test.go`
```
TestPhiIncreasesOverTime         — record heartbeats, stop, verify phi grows
TestPhiBelowThresholdWhileAlive  — regular heartbeats keep phi < 8.0
TestRecordHeartbeatReset         — phi high → receive heartbeat → phi drops
TestPhiNoSamples                 — no samples = phi 0.0
TestWindowRingBuffer             — window wraps correctly at capacity
BenchmarkPhiComputation          — phi on 200-sample window < 1µs
```

---

### Package: `Cluster/handoff/`
**File:** `Cluster/handoff/handoff.go`

#### Structs

```go
// Hint is a pending write that could not be delivered to its target node
type Hint struct {
    Key          string
    TargetAddr   string
    EncryptedKey string    // hex-encoded per-file key
    Data         []byte    // encrypted file bytes (already encrypted, safe to store)
    CreatedAt    time.Time
    Attempts     int
}

// Store persists hints to disk so they survive restarts
type Store struct {
    mu       sync.Mutex
    hints    map[string][]Hint  // targetAddr -> list of pending hints
    dir      string             // directory to persist hint files
    maxHints int                // per-target cap (default 1000)
    maxAge   time.Duration      // discard hints older than this (default 24h)
}

// HandoffService watches for peer reconnections and delivers hints
type HandoffService struct {
    store    *Store
    deliver  func(hint Hint) error  // closure: send hint data to target
    stopCh   chan struct{}
}
```

#### Functions & Methods

```go
// NewStore(dir string, maxHints int, maxAge time.Duration) (*Store, error)
  // 1. os.MkdirAll(dir, 0700)
  // 2. load existing hints from disk (json files in dir)
  // 3. purge expired hints (CreatedAt + maxAge < now)
  // 4. return store

// (s *Store) AddHint(hint Hint) error
  // 1. lock
  // 2. if len(hints[targetAddr]) >= maxHints: drop oldest hint, log warning
  // 3. append hint
  // 4. persist to disk: json.Marshal hints[targetAddr] -> write file targetAddr.hints.json
  // 5. unlock

// (s *Store) GetHints(targetAddr string) []Hint
  // lock; return copy of hints[targetAddr]; unlock

// (s *Store) DeleteHint(targetAddr string, key string)
  // lock; remove hint with matching Key from hints[targetAddr]; persist; unlock

// (s *Store) PurgeExpired()
  // iterate all targets; remove hints where CreatedAt + maxAge < now; persist each changed target

// NewHandoffService(store *Store, deliver func(Hint)error) *HandoffService

// (hs *HandoffService) OnPeerReconnect(addr string)
  // 1. hints := store.GetHints(addr)
  // 2. for each hint: go deliver(hint); on success: store.DeleteHint(addr, hint.Key)
  //    on failure: increment hint.Attempts, re-store (up to 5 attempts then discard)

// (hs *HandoffService) Start()
  // go hs.purgeLoop()  — every 1h: store.PurgeExpired()

// (hs *HandoffService) Stop()
  // close(hs.stopCh)
```

**File:** `Cluster/handoff/handoff_test.go`
```
TestAddAndRetrieveHint         — add hint, get it back
TestHintCapCausesEviction      — at maxHints, oldest is dropped
TestExpiredHintsRemoved        — purge removes old hints
TestDeliverOnReconnect         — reconnect triggers delivery, hint deleted on success
TestDeliverRetryOnFailure      — failed delivery increments attempts
TestPersistAndReload           — store survives process restart (write to tmpdir, reload)
```

---

### Package: `Cluster/rebalance/`
**File:** `Cluster/rebalance/rebalance.go`

#### Structs

```go
// Rebalancer migrates data when the hash ring changes topology
type Rebalancer struct {
    selfAddr    string
    ring        *hashring.HashRing
    store       *storage.Store
    metaData    storage.MetadataStore
    sendFile    func(targetAddr, key string, encKey string, data []byte) error  // closure
    stopCh      chan struct{}
    mu          sync.Mutex
    inProgress  bool
}
```

#### Functions & Methods

```go
// New(selfAddr string, ring *hashring.HashRing, store *storage.Store,
//     meta storage.MetadataStore, sendFile func(...) error) *Rebalancer

// (r *Rebalancer) OnNodeJoined(newAddr string)
  // go r.migrateKeysToNewNode(newAddr)

// (r *Rebalancer) OnNodeLeft(deadAddr string)
  // go r.rereplicate(deadAddr)

// (r *Rebalancer) migrateKeysToNewNode(newAddr string)
  // 1. lock inProgress check (skip if already rebalancing)
  // 2. list all local keys: r.store.Keys()   [need to add Keys() to Store]
  // 3. for each key:
  //    a. responsible := r.ring.GetNodes(key, r.ring.ReplicationFactor())
  //    b. if newAddr IN responsible AND selfAddr IN responsible:
  //       — newAddr now co-owns this key
  //       — send copy to newAddr via sendFile
  //       — if selfAddr is NO LONGER in responsible: delete local copy
  // 4. log completion

// (r *Rebalancer) rereplicate(deadAddr string)
  // 1. list all local keys
  // 2. for each key:
  //    a. responsible := ring.GetNodes(key, N)
  //    b. if deadAddr WAS in responsible (determined by checking if local node has it
  //       and ring now returns fewer than N live nodes):
  //       — find next live node(s) not in current responsible set
  //       — send copy to them via sendFile
  // 3. log

// Keys() []string — add to storage.Store
  // walks root dir via filepath.WalkDir
  // for each leaf file: look up key from metadata reverse index
  // return all known keys
  // NOTE: MetaFile must add reverse index (path->key map) or iterate its store map
```

**Integration changes to `Storage/storage.go`:**
- Add `Keys() []string` to `Store` — iterates `MetaFile.store` map, returns all keys

**File:** `Cluster/rebalance/rebalance_test.go`
```
TestMigrateOnNodeJoin   — 3-node ring, 4th node joins, verify keys transferred
TestRereplOnNodeLeave   — 3-node ring, 1 dies, verify under-replicated keys re-sent
TestNoOpWhenNotOwner    — keys not owned by self not migrated
```

---

### Integration: Read Repair (in `Server/server.go`)

**Modify `GetData(key string)`:**
After a successful remote fetch (a peer responded), asynchronously:
```
go s.readRepair(key, respondingAddr)

func (s *Server) readRepair(key, sourceAddr string)
  // 1. targetNodes := s.HashRing.GetNodes(key, replFactor)
  // 2. for each node in targetNodes (excluding sourceAddr and self):
  //    a. send MessageGetFile{Key: key} — this is a probe
  //    b. if no response within 2s: that node is stale
  //    c. fetch data from sourceAddr, send MessageStoreFile to stale node
  // Note: do this in a goroutine, errors are logged only
```

---

### New Message Types (register in `init()` in `server.go`)

```go
type MessageHeartbeat struct {
    From      string
    Timestamp int64   // UnixNano
}

type MessageHeartbeatAck struct {
    From      string
    Timestamp int64   // echo back for RTT measurement
}
```

---

### Server.go Integration for Sprint 2

**New fields on `Server`:**
```go
HeartbeatSvc *failure.HeartbeatService
HandoffSvc   *handoff.HandoffService
Rebalancer   *rebalance.Rebalancer
```

**Modified `NewServer` / `MakeServer`:**
- Instantiate `failure.HeartbeatService` with callbacks:
  - `onSuspect`: log warning, note in cluster state
  - `onDead`: call `s.HashRing.RemoveNode(addr)`, call `s.Rebalancer.OnNodeLeft(addr)`
- Instantiate `HandoffService` with `deliver` closure that calls `s.sendEncryptedFile(hint)`
- Instantiate `Rebalancer` with `sendFile` closure

**Modified `Run()`:**
```
s.HeartbeatSvc.Start()
```

**Modified `Stop()`:**
```
s.HeartbeatSvc.Stop()
s.HandoffSvc.Stop()
```

**Modified `OnPeer(p Peer)`:**
```
// existing: add to peers, add to HashRing
s.HandoffSvc.OnPeerReconnect(addr)    // deliver pending hints
s.Rebalancer.OnNodeJoined(addr)       // migrate keys
```

**Modified `OnPeerDisconnect(p Peer)`:**
```
// existing: remove from peers, remove from HashRing
// HeartbeatSvc.reaperLoop already handles dead detection
```

**Modified `StoreData(key, reader)`:**
After collecting `targetPeers`, for any target not in `s.peers`:
```go
hint := handoff.Hint{
    Key: key, TargetAddr: nodeAddr, EncryptedKey: encKeyHex, Data: encryptedData,
    CreatedAt: time.Now(),
}
s.HandoffSvc.Store.AddHint(hint)
log.Printf("STORE_DATA: Node %s unreachable, stored hint", nodeAddr)
```

**Modified `handleMessage`:** add case for `*MessageHeartbeat`:
```go
case *MessageHeartbeat:
    s.HeartbeatSvc.RecordHeartbeat(m.From)
    // send ack
    ack := &Message{Payload: MessageHeartbeatAck{From: selfAddr, Timestamp: m.Timestamp}}
    s.sendToAddr(from, ack)
```

**New helper `sendToAddr(addr string, msg *Message) error`:**
```
// 1. s.peerLock.RLock(); peer, ok := s.peers[addr]; s.peerLock.RUnlock()
// 2. if !ok: return error
// 3. encode msg to gob buffer
// 4. peer.Send([]byte{IncomingMessage}); peer.Send(buf.Bytes())
```

**New CLI flag:** none for Sprint 2 (all configs use defaults)

---

## Sprint 3: Gossip Protocol & Cluster Membership

**Goal:** Replace static `--peer` bootstrap with epidemic gossip. Nodes discover each other automatically. Cluster membership is eventually consistent across all nodes.

**Depends on:** Sprint 2 (failure detection provides liveness events)
**Enables:** Sprint 8 (gossip carries public address info)

### Package: `Cluster/membership/`
**File:** `Cluster/membership/membership.go`

#### Structs

```go
type NodeState int
const (
    StateAlive   NodeState = iota  // 0 — responding normally
    StateSuspect                   // 1 — phi >= threshold, not yet confirmed dead
    StateDead                      // 2 — confirmed dead, ring removed
    StateLeft                      // 3 — voluntarily left (future: graceful shutdown msg)
)

type NodeInfo struct {
    Addr        string
    State       NodeState
    Generation  uint64            // monotonically increasing, bumped on each state change
    LastUpdated time.Time
    Metadata    map[string]string // e.g. "region":"us-east", "capacity":"500GB"
}

type ClusterState struct {
    mu        sync.RWMutex
    selfAddr  string
    nodes     map[string]*NodeInfo      // addr -> info
    onChange  []func(addr string, oldState, newState NodeState)
}
```

#### Functions & Methods

```go
// New(selfAddr string) *ClusterState
  // creates ClusterState, adds self as Alive with Generation=1

// (cs *ClusterState) AddNode(addr string, meta map[string]string)
  // lock; if not exists: nodes[addr] = &NodeInfo{Addr:addr, State:StateAlive, Generation:1, Metadata:meta}
  // fire onChange callbacks

// (cs *ClusterState) UpdateState(addr string, newState NodeState, generation uint64) bool
  // lock
  // existing := nodes[addr]; if not found: insert as new
  // if generation <= existing.Generation: return false (stale update, ignore)
  // existing.State = newState; existing.Generation = generation; existing.LastUpdated = now
  // fire onChange(addr, old, new)
  // return true

// (cs *ClusterState) GetNode(addr string) (NodeInfo, bool)
  // rlock; return copy

// (cs *ClusterState) AliveNodes() []string
  // rlock; return addrs where State == StateAlive

// (cs *ClusterState) AllNodes() []NodeInfo
  // rlock; return copy of all

// (cs *ClusterState) OnChange(fn func(addr string, old, new NodeState))
  // append to onChange slice

// (cs *ClusterState) Digest() []GossipDigest
  // rlock; for each node: append GossipDigest{Addr, State, Generation}; return

// (cs *ClusterState) Merge(digests []GossipDigest) (needFull []string)
  // for each digest:
  //   local := nodes[digest.Addr]
  //   if local == nil OR digest.Generation > local.Generation:
  //     needFull = append(needFull, digest.Addr)  — request full NodeInfo
  //   else if digest.Generation == local.Generation && digest.State != local.State:
  //     same generation conflict: take higher-numbered state (Alive < Suspect < Dead)
  // return needFull (caller requests full info for these)
```

**File:** `Cluster/membership/membership_test.go`
```
TestUpdateStateIgnoresStale    — lower generation update rejected
TestUpdateStateAcceptsNewer    — higher generation accepted, onChange fired
TestMergeReturnsMissingAddrs   — Merge identifies addrs not in local state
TestAliveNodesFilters          — Dead/Suspect nodes excluded
TestDigestContainsAllNodes     — Digest returns one entry per node
```

---

### Package: `Cluster/gossip/`
**File:** `Cluster/gossip/gossip.go`

#### Structs

```go
type Config struct {
    FanOut          int           // default 3 — peers per gossip round
    Interval        time.Duration // default 200ms
    SuspectTimeout  time.Duration // default 5s — after no heartbeat, suspect
    MaxGossipAge    time.Duration // default 24h — ignore digests older than this
}

type GossipDigest struct {
    Addr       string
    State      membership.NodeState
    Generation uint64
}

type GossipPush struct {
    Digests   []GossipDigest
    Full      []membership.NodeInfo  // nodes the receiver requested full info on
}

type GossipService struct {
    cfg       Config
    selfAddr  string
    cluster   *membership.ClusterState
    getPeers  func() []string             // returns all known peer addrs
    sendMsg   func(addr string, msg interface{}) error
    stopCh    chan struct{}
}
```

#### Functions & Methods

```go
// New(cfg Config, selfAddr string, cluster *membership.ClusterState,
//     getPeers func()[]string, sendMsg func(string,interface{})error) *GossipService

// (gs *GossipService) Start()
  // go gs.gossipLoop()

// (gs *GossipService) Stop()
  // close(gs.stopCh)

// (gs *GossipService) gossipLoop()
  // ticker := time.NewTicker(cfg.Interval)
  // for { select { case <-ticker.C: gs.doGossipRound(); case <-stopCh: return } }

// (gs *GossipService) doGossipRound()
  // 1. peers := getPeers()
  // 2. if len(peers) == 0: return
  // 3. shuffle(peers); pick min(cfg.FanOut, len(peers)) targets
  // 4. digest := cluster.Digest()
  // 5. for each target:
  //    msg := MessageGossipDigest{Digests: digest, From: selfAddr}
  //    sendMsg(target, msg)

// (gs *GossipService) HandleDigest(from string, msg MessageGossipDigest)
  // 1. needFull := cluster.Merge(msg.Digests)
  // 2. response := MessageGossipResponse{
  //        From: selfAddr,
  //        Full: cluster.GetNodes(needFull),   // full NodeInfo for requested addrs
  //        MyDigest: cluster.Digest(),          // our state so sender can update too
  //    }
  // 3. sendMsg(from, response)

// (gs *GossipService) HandleResponse(from string, msg MessageGossipResponse)
  // 1. for each NodeInfo in msg.Full:
  //    cluster.UpdateState(info.Addr, info.State, info.Generation)
  //    if info.State == StateAlive && info.Addr not in current peers:
  //       — fire "new node discovered" callback (used to dial new peer)
  // 2. cluster.Merge(msg.MyDigest)   — update from their perspective too
```

**New Message Types:**
```go
type MessageGossipDigest struct {
    From    string
    Digests []gossip.GossipDigest
}

type MessageGossipResponse struct {
    From     string
    Full     []membership.NodeInfo
    MyDigest []gossip.GossipDigest
}
```

**File:** `Cluster/gossip/gossip_test.go`
```
TestGossipConvergence3Nodes    — 3 nodes, 1 knows about new node, verify all 3 learn within 1s
TestFanOutLimitRespected       — gossip never contacts more than FanOut peers per round
TestHandleDigestRequestsFull   — digest with unknown addr triggers full info request
TestStaleDigestIgnored         — older generation in digest doesn't overwrite local state
TestNewNodeDiscovery           — node in gossip response not in peers triggers dial callback
```

**Server.go Integration for Sprint 3:**

**New fields on `Server`:**
```go
Cluster     *membership.ClusterState
GossipSvc   *gossip.GossipService
onNewPeer   func(addr string)   // callback: dial newly discovered peer
```

**Modified `MakeServer`:**
- Instantiate `membership.New(listenAddr)`
- Instantiate `gossip.New(cfg, listenAddr, cluster, getPeers, sendMsg)`
  - `getPeers` closure: return `s.HashRing.Members()` (all known addrs)
  - `sendMsg` closure: calls `sendToAddr`
- Wire `GossipSvc.onNewPeer = func(addr string) { s.serverOpts.tcpTransport.Dial(addr) }`

**Modified `Run()`:**
```
s.GossipSvc.Start()
```

**Modified `OnPeer(p Peer)`:**
```
s.Cluster.AddNode(addr, nil)
```

**Modified `OnPeerDisconnect(p Peer)`:**
```
s.Cluster.UpdateState(addr, membership.StateSuspect, s.Cluster.GetNode(addr).Generation+1)
```

**Modified `handleMessage`:**
```go
case *MessageGossipDigest:
    s.GossipSvc.HandleDigest(from, *m)
case *MessageGossipResponse:
    s.GossipSvc.HandleResponse(from, *m)
```

**New CLI flags** in `cmd/start.go`:
```go
--gossip-interval  duration  // default 200ms
--gossip-fanout    int       // default 3
```

**Modified Bootstrap:** `BootstrapNetwork` dials only initial seed nodes. After gossip starts, all other peers are discovered automatically. `--peer` flag becomes "seed nodes" not "all peers".

---

## Sprint 4: Consistency & Conflict Resolution

**Goal:** Give every write a version. Guarantee W-of-N writes succeed. Detect and resolve conflicts. Anti-entropy ensures all replicas converge.

**Depends on:** Sprint 3 (ClusterState needed to know live replicas)
**Enables:** Sprint 6 (quorum per chunk)

### Package: `Cluster/vclock/`
**File:** `Cluster/vclock/vclock.go`

#### Structs

```go
type VectorClock map[string]uint64  // nodeAddr -> logical timestamp

type Relation int
const (
    Before     Relation = iota  // this < other (causally before)
    After                       // this > other (causally after)
    Concurrent                  // neither dominates (conflict!)
    Equal                       // identical
)
```

#### Functions & Methods

```go
// New() VectorClock
  // return VectorClock{}

// (vc VectorClock) Increment(nodeAddr string) VectorClock
  // copy := vc.Copy(); copy[nodeAddr]++; return copy

// (vc VectorClock) Merge(other VectorClock) VectorClock
  // result := vc.Copy()
  // for k, v := range other: if v > result[k]: result[k] = v
  // return result

// (vc VectorClock) Compare(other VectorClock) Relation
  // thisGt := false; otherGt := false
  // allKeys := union(keys(vc), keys(other))
  // for each key:
  //   if vc[key] > other[key]: thisGt = true
  //   if other[key] > vc[key]: otherGt = true
  // if thisGt && otherGt: return Concurrent
  // if thisGt: return After
  // if otherGt: return Before
  // return Equal

// (vc VectorClock) Copy() VectorClock
  // allocate new map, copy all entries

// (vc VectorClock) Encode() ([]byte, error)   — gob encode
// Decode(data []byte) (VectorClock, error)    — gob decode
```

**File:** `Cluster/vclock/vclock_test.go`
```
TestIncrementDoesNotMutate      — original clock unchanged
TestMergeTakesMax               — per-key max from both clocks
TestCompareBeforeAfter          — A < B when B has strictly higher values
TestCompareConcurrent           — neither dominates → Concurrent
TestCompareEqual                — identical clocks
TestCopyIndependence            — modifying copy doesn't affect original
```

---

### Extend `Storage/storage.go`

**Modified `FileMeta`:**
```go
type FileMeta struct {
    Path         string
    EncryptedKey string
    VClock       vclock.VectorClock  // nil for old files (treated as empty clock)
    Timestamp    int64               // UnixNano wall clock, for LWW tiebreaker
    Version      uint64              // monotonic local version counter
}
```

This is a backwards-compatible extension (old metadata JSON files missing these fields will parse with zero values).

---

### Package: `Cluster/conflict/`
**File:** `Cluster/conflict/conflict.go`

#### Structs

```go
type VersionedValue struct {
    Key       string
    Meta      storage.FileMeta
    Data      []byte   // nil if only resolving metadata
}

type Resolver interface {
    Resolve(versions []VersionedValue) VersionedValue
}

type LWWResolver struct{}
```

#### Functions & Methods

```go
// (r LWWResolver) Resolve(versions []VersionedValue) VersionedValue
  // 1. if len(versions) == 0: panic
  // 2. if len(versions) == 1: return versions[0]
  // 3. best := versions[0]
  // 4. for i := 1; i < len(versions); i++:
  //    rel := best.Meta.VClock.Compare(versions[i].Meta.VClock)
  //    if rel == Before:  best = versions[i]   — versions[i] is causally later
  //    if rel == After:   continue              — best is already later
  //    if rel == Concurrent OR Equal:
  //       if versions[i].Meta.Timestamp > best.Meta.Timestamp:
  //          best = versions[i]   — wall-clock tiebreak (LWW)
  //       // else: keep best (existing writer wins on tie)
  // 5. return best

// NewLWWResolver() *LWWResolver
```

**File:** `Cluster/conflict/conflict_test.go`
```
TestResolveLinearHistory    — A before B → B wins
TestResolveConcurrentLWW   — concurrent clocks → higher Timestamp wins
TestResolveAllEqual         — same clock, same time → first entry kept
TestResolveSingleVersion    — single version returned as-is
```

---

### Package: `Cluster/quorum/`
**File:** `Cluster/quorum/quorum.go`

#### Structs

```go
type Config struct {
    N       int           // total replicas (default 3, from replication factor)
    W       int           // write quorum (default 2)
    R       int           // read quorum (default 2)
    Timeout time.Duration // default 5s per operation
}

// WriteAck from a replica confirming write succeeded
type WriteAck struct {
    NodeAddr string
    Key      string
    Success  bool
    Err      string
}

// ReadResponse from a replica with file metadata
type ReadResponse struct {
    NodeAddr string
    Key      string
    Meta     storage.FileMeta
    Found    bool
}

// Coordinator drives quorum operations
type Coordinator struct {
    cfg       Config
    selfAddr  string
    ring      *hashring.HashRing
    cluster   *membership.ClusterState
    getPeer   func(addr string) (peer2peer.Peer, bool)
    sendMsg   func(addr string, msg interface{}) error
    // pending write ack channels: key -> chan WriteAck
    writeAcks sync.Map   // map[string]chan WriteAck
    // pending read response channels: key -> chan ReadResponse
    readResps sync.Map   // map[string]chan ReadResponse
}
```

#### Functions & Methods

```go
// New(cfg Config, selfAddr string, ring *hashring.HashRing,
//     cluster *membership.ClusterState, getPeer func(string)(Peer,bool),
//     sendMsg func(string,interface{})error) *Coordinator

// (c *Coordinator) Write(key string, encKey string, encData []byte, vclock vclock.VectorClock) error
  // 1. targets := ring.GetNodes(key, cfg.N)
  // 2. ch := make(chan WriteAck, cfg.N)
  // 3. writeAcks.Store(key, ch)
  // 4. defer writeAcks.Delete(key)
  // 5. for each target:
  //    if target == selfAddr: go func() { ch <- WriteAck{NodeAddr:selfAddr, Success:true} }()
  //    else: sendMsg(target, MessageQuorumWrite{Key:key, EncKey:encKey, Data:encData, VClock:vclock})
  // 6. acks := 0; timer := time.NewTimer(cfg.Timeout)
  // 7. loop:
  //    select:
  //      case ack := <-ch: if ack.Success: acks++; if acks >= cfg.W: return nil
  //      case <-timer.C: return fmt.Errorf("write quorum not met: got %d/%d acks", acks, cfg.W)
  // 8. if loop exhausted without quorum: return error

// (c *Coordinator) Read(key string) (storage.FileMeta, error)
  // 1. targets := ring.GetNodes(key, cfg.N)
  // 2. ch := make(chan ReadResponse, cfg.N)
  // 3. readResps.Store(key, ch)
  // 4. defer readResps.Delete(key)
  // 5. for each target (excluding self, which is handled by local Has check):
  //    sendMsg(target, MessageQuorumRead{Key: key})
  // 6. responses := []ReadResponse{}; timer := time.NewTimer(cfg.Timeout)
  // 7. loop until len(responses) >= cfg.R or timeout:
  //    select { case resp := <-ch: if resp.Found: responses = append(responses, resp) }
  // 8. if len(responses) < cfg.R: return error "read quorum not met"
  // 9. versions := []conflict.VersionedValue{...convert responses...}
  // 10. best := resolver.Resolve(versions)
  // 11. return best.Meta, nil

// (c *Coordinator) HandleWriteAck(ack WriteAck)
  // ch, ok := writeAcks.Load(ack.Key); if ok: ch.(chan WriteAck) <- ack

// (c *Coordinator) HandleReadResponse(resp ReadResponse)
  // ch, ok := readResps.Load(resp.Key); if ok: ch.(chan ReadResponse) <- resp
```

**New Message Types:**
```go
type MessageQuorumWrite struct {
    Key    string
    EncKey string
    Data   []byte
    VClock map[string]uint64
}
type MessageQuorumWriteAck struct {
    Key     string
    From    string
    Success bool
    ErrMsg  string
}
type MessageQuorumRead struct {
    Key string
}
type MessageQuorumReadResponse struct {
    Key      string
    From     string
    Found    bool
    Meta     storage.FileMeta  // includes VClock, Timestamp
}
```

**File:** `Cluster/quorum/quorum_test.go`
```
TestWriteQuorumMetWith2Acks    — W=2, 3 targets, 2 ack success → nil error
TestWriteQuorumFailsOnTimeout  — W=2, only 1 ack before timeout → error
TestReadQuorumPicksLatest      — 2 responses with different VClocks → later one wins
TestReadQuorumLWWTiebreak      — concurrent VClocks → higher Timestamp wins
TestReadQuorumTimeout          — fewer than R responses → error
```

---

### Package: `Cluster/merkle/`
**File:** `Cluster/merkle/merkle.go`

#### Structs

```go
type Node struct {
    Hash     [32]byte
    Left     *Node
    Right    *Node
    IsLeaf   bool
    Key      string    // only set for leaf nodes
}

type Tree struct {
    Root   *Node
    Leaves []*Node
}
```

#### Functions & Methods

```go
// Build(keys []string) *Tree
  // 1. if len(keys) == 0: return &Tree{} with empty root
  // 2. sort keys (deterministic tree shape)
  // 3. create leaf nodes: for each key: leaf.Hash = sha256(key); leaf.Key = key; leaf.IsLeaf = true
  // 4. build tree bottom-up:
  //    current_level := leaves
  //    while len(current_level) > 1:
  //      next_level := []
  //      for i := 0; i < len(current_level); i += 2:
  //        left := current_level[i]
  //        right := current_level[i+1] if exists, else left (duplicate last node)
  //        parent := &Node{Left:left, Right:right}
  //        parent.Hash = sha256(left.Hash || right.Hash)  — concatenate hashes
  //        next_level = append(next_level, parent)
  //      current_level = next_level
  // 5. root = current_level[0]

// (t *Tree) RootHash() [32]byte
  // if t.Root == nil: return zero hash; return t.Root.Hash

// (t *Tree) Diff(other *Tree) []string
  // returns keys in leaves of either tree that differ
  // 1. if t.RootHash() == other.RootHash(): return nil (identical)
  // 2. recursive diffNodes(t.Root, other.Root) -> collect differing leaf keys

// diffNodes(a, b *Node) []string  (internal)
  // if a == nil && b == nil: return nil
  // if a == nil: return all keys under b
  // if b == nil: return all keys under a
  // if a.Hash == b.Hash: return nil  (subtrees identical)
  // if a.IsLeaf && b.IsLeaf: return [a.Key, b.Key]
  // return append(diffNodes(a.Left, b.Left), diffNodes(a.Right, b.Right)...)

// collectLeafKeys(n *Node) []string  (internal)
  // if n == nil: return nil
  // if n.IsLeaf: return [n.Key]
  // return append(collectLeafKeys(n.Left), collectLeafKeys(n.Right)...)

// AntiEntropyService — runs background sync
type AntiEntropyService struct {
    selfAddr  string
    ring      *hashring.HashRing
    store     *storage.Store
    sendMsg   func(addr string, msg interface{}) error
    interval  time.Duration   // default 10 min
    stopCh    chan struct{}
    // pending diff response channels: addr -> chan MessageMerkleDiffResponse
    pending   sync.Map
}

// (ae *AntiEntropyService) Start() — go ae.syncLoop()
// (ae *AntiEntropyService) Stop()  — close(ae.stopCh)

// (ae *AntiEntropyService) syncLoop()
  // ticker := time.NewTicker(ae.interval)
  // for { select { case <-ticker.C: ae.doSync(); case <-stopCh: return } }

// (ae *AntiEntropyService) doSync()
  // 1. myKeys := store.Keys()
  // 2. myTree := merkle.Build(myKeys)
  // 3. replicas := ring.GetNodes(selfAddr, ring.ReplicationFactor())
  //    // use selfAddr as key to get this node's replica partners
  // 4. for each replica (not self):
  //    sendMsg(replica, MessageMerkleSync{From: selfAddr, RootHash: myTree.RootHash()})

// (ae *AntiEntropyService) HandleSync(from string, msg MessageMerkleSync)
  // 1. myKeys := store.Keys(); myTree := merkle.Build(myKeys)
  // 2. if myTree.RootHash() == msg.RootHash: return (already in sync)
  // 3. theirTree := rebuild from msg.AllKeys if provided, else request
  //    sendMsg(from, MessageMerkleDiffResponse{From:selfAddr, AllKeys: myKeys})

// (ae *AntiEntropyService) HandleDiffResponse(from string, msg MessageMerkleDiffResponse)
  // 1. myKeys := store.Keys(); myTree := merkle.Build(myKeys)
  // 2. theirTree := merkle.Build(msg.AllKeys)
  // 3. diff := myTree.Diff(theirTree)
  // 4. for each key in diff:
  //    if myTree has key but theirTree doesn't: send key to from (they're missing it)
  //    if theirTree has key but myTree doesn't: request key from from (we're missing it)
```

**New Message Types:**
```go
type MessageMerkleSync struct {
    From     string
    RootHash [32]byte
}
type MessageMerkleDiffResponse struct {
    From    string
    AllKeys []string   // all keys this node holds
}
```

**File:** `Cluster/merkle/merkle_test.go`
```
TestBuildDeterministic    — same keys always produce same root hash
TestDiffIdentical         — identical key sets → empty diff
TestDiffDisjoint          — completely different sets → all keys in diff
TestDiffOverlap           — partial overlap → only missing keys in diff
TestBuildEmpty            — empty key list produces zero root hash
TestDiffSingleKey         — one key missing is detected
BenchmarkBuild1000Keys    — build tree of 1000 keys, < 5ms
```

**Server.go Integration for Sprint 4:**

**New fields on `Server`:**
```go
Quorum       *quorum.Coordinator
AntiEntropy  *merkle.AntiEntropyService
Resolver     conflict.Resolver
```

**Modified `StoreData(key, reader)`:**
```
// After local store:
1. vc := s.loadVClock(key).Increment(selfAddr)
2. err := s.Quorum.Write(key, encKeyHex, encryptedData, vc)
3. if err: return err   (quorum not met = write failed)
4. update local metadata: meta.VClock = vc; meta.Timestamp = now.UnixNano()
// Remove old goroutine-based fan-out (Quorum.Write handles delivery)
```

**Modified `GetData(key)`:**
```
// Local check stays same
// Remote: use Quorum.Read(key) → returns best FileMeta
// Then fetch actual data from the node that has the best version
```

**Modified `handleMessage`:**
```go
case *MessageQuorumWrite:
    // store file (same as handleStoreMessage), then send ack
    err := s.storeFromBytes(m.Key, m.EncKey, m.Data)
    ack := MessageQuorumWriteAck{Key: m.Key, From: selfAddr, Success: err==nil}
    s.sendToAddr(from, &Message{Payload: ack})

case *MessageQuorumWriteAck:
    s.Quorum.HandleWriteAck(quorum.WriteAck{NodeAddr: m.From, Key: m.Key, Success: m.Success})

case *MessageQuorumRead:
    meta, ok := s.serverOpts.metaData.Get(m.Key)
    resp := MessageQuorumReadResponse{Key: m.Key, From: selfAddr, Found: ok, Meta: meta}
    s.sendToAddr(from, &Message{Payload: resp})

case *MessageQuorumReadResponse:
    s.Quorum.HandleReadResponse(quorum.ReadResponse{NodeAddr: m.From, Key: m.Key, Found: m.Found, Meta: m.Meta})

case *MessageMerkleSync:
    s.AntiEntropy.HandleSync(from, *m)

case *MessageMerkleDiffResponse:
    s.AntiEntropy.HandleDiffResponse(from, *m)
```

**New CLI flags:**
```go
--quorum-write int  // default 2
--quorum-read  int  // default 2
```

---

## Sprint 5: Observability

**Goal:** Replace unstructured `log.Println` with structured JSON logs. Add Prometheus metrics, HTTP health endpoint, distributed tracing.

**Depends on:** Sprint 2 (can run in parallel after Sprint 2 is stable)
**Can be done in any order internally**

### Package: `Observability/logging/`
**File:** `Observability/logging/logger.go`

```go
// Dep: github.com/rs/zerolog

type Level int
const (
    LevelDebug Level = iota
    LevelInfo
    LevelWarn
    LevelError
)

type Logger struct {
    zl zerolog.Logger
}

// New(component string, level Level) *Logger
  // zerolog.New(os.Stdout).With().Timestamp().Str("component", component).Logger()
  // set global level

// (l *Logger) Info(msg string, fields ...interface{})   — key-value pairs
// (l *Logger) Warn(msg string, fields ...interface{})
// (l *Logger) Error(msg string, err error, fields ...interface{})
// (l *Logger) Debug(msg string, fields ...interface{})
// (l *Logger) With(key string, value interface{}) *Logger  — returns child logger

// Global package-level logger (for easy adoption):
var Global *Logger
func Init(component string, level Level)  // sets Global
```

**Integration:** Replace all `log.Printf/Println` calls across `server.go`, `tcpTransport.go`, `storage.go` with `logging.Global.Info(...)` / `.Error(...)` etc. This is a mechanical find-and-replace pass.

---

### Package: `Observability/metrics/`
**File:** `Observability/metrics/metrics.go`

```go
// Dep: github.com/prometheus/client_golang/prometheus

// Counters
var (
    StoreOpsTotal      *prometheus.CounterVec   // labels: operation={store,replicate}, status={ok,err}
    GetOpsTotal        *prometheus.CounterVec   // labels: status={local,remote,miss,err}
    ReplicationTotal   *prometheus.CounterVec   // labels: status={ok,hint,skip}
    GossipRoundsTotal  prometheus.Counter
    HeartbeatsTotal    *prometheus.CounterVec   // labels: direction={sent,received}
)

// Histograms
var (
    StoreDuration   *prometheus.HistogramVec  // labels: operation
    GetDuration     *prometheus.HistogramVec  // labels: source={local,remote}
    GossipDuration  prometheus.Histogram
    QuorumDuration  *prometheus.HistogramVec  // labels: type={read,write}
)

// Gauges
var (
    PeerCount   prometheus.Gauge
    RingSize    prometheus.Gauge
    HintsPending prometheus.Gauge
)

// Init() — registers all metrics with prometheus.DefaultRegisterer
// Must be called once at startup from MakeServer or main

// RecordStore(op string, err error, duration time.Duration)
// RecordGet(source string, err error, duration time.Duration)
// RecordReplication(status string)
// etc. — thin wrappers so callers don't import prometheus directly
```

---

### Package: `Observability/health/`
**File:** `Observability/health/health.go`

```go
// HTTP server serving /health and /metrics

type HealthServer struct {
    httpServer *http.Server
    getStatus  func() Status   // closure returning current cluster health
}

type Status struct {
    Status    string            // "ok" or "degraded"
    NodeAddr  string
    PeerCount int
    RingSize  int
    Uptime    string
}

// New(listenAddr string, getStatus func() Status) *HealthServer
  // mux := http.NewServeMux()
  // mux.HandleFunc("/health", serveHealth)
  // mux.HandleFunc("/metrics", promhttp.Handler())
  // mux.HandleFunc("/debug/pprof/", pprof.Index)   — import net/http/pprof
  // httpServer = &http.Server{Addr: listenAddr, Handler: mux}

// (hs *HealthServer) Start() error
  // go hs.httpServer.ListenAndServe()

// (hs *HealthServer) Stop(ctx context.Context) error
  // hs.httpServer.Shutdown(ctx)

// serveHealth(w http.ResponseWriter, r *http.Request)
  // status := getStatus()
  // w.Header().Set("Content-Type", "application/json")
  // if status.Status != "ok": w.WriteHeader(http.StatusServiceUnavailable)
  // json.NewEncoder(w).Encode(status)
```

**Integration in `MakeServer`:**
```go
healthPort := fmt.Sprintf(":%d", parsePort(listenAddr) + 1000)
hs := health.New(healthPort, func() health.Status { return s.healthStatus() })
hs.Start()
```

**New `Server` method:**
```go
func (s *Server) healthStatus() health.Status
  // return Status{Status:"ok", NodeAddr:selfAddr, PeerCount:len(peers), RingSize:ring.Size(), Uptime:...}
```

**New CLI flags:**
```go
--health-port int  // default: tcp_port + 1000
```

---

### Package: `Observability/tracing/`
**File:** `Observability/tracing/tracing.go`

```go
// Dep: go.opentelemetry.io/otel, go.opentelemetry.io/otel/exporters/jaeger

// Init(serviceName string, jaegerEndpoint string) (func(), error)
  // sets up OTLP/Jaeger exporter
  // returns shutdown function

// StartSpan(ctx context.Context, name string) (context.Context, trace.Span)
  // wrapper: otel.Tracer("dfs").Start(ctx, name)

// SpanFromContext(ctx context.Context) trace.Span

// InjectToMap(ctx context.Context, carrier map[string]string)  — for propagating over RPC
// ExtractFromMap(ctx context.Context, carrier map[string]string) context.Context
```

**Integration:** Pass `context.Context` through `StoreData(ctx, key, reader)` and `GetData(ctx, key)`. Create spans at top of each public method. Thread context through to peer sends (add `TraceContext map[string]string` to messages).

**New CLI flags:**
```go
--jaeger-endpoint string  // default ""  (tracing disabled if empty)
```

---

## Sprint 6: File Chunking & Integrity

**Goal:** Split files into 4MB chunks. Store each chunk via CAS. Enable natural deduplication. Prevent OOM on large files.

**Depends on:** Sprint 4 (quorum write per chunk, Merkle for integrity)

### Package: `Storage/chunker/`
**File:** `Storage/chunker/chunker.go`

#### Structs

```go
const DefaultChunkSize = 4 * 1024 * 1024  // 4MB

type Chunk struct {
    Index  int
    Hash   [32]byte   // SHA-256 of plaintext chunk data
    Size   int64
    Data   []byte     // plaintext data (not stored persistently, only used during pipeline)
}

type ChunkInfo struct {
    Index      int
    Hash       string   // hex-encoded SHA-256
    Size       int64
    EncHash    string   // SHA-256 of *encrypted* chunk (used as CAS storage key)
}

type ChunkManifest struct {
    FileKey     string        // original file key
    TotalSize   int64
    ChunkSize   int
    Chunks      []ChunkInfo
    MerkleRoot  [32]byte      // SHA-256 Merkle root over chunk plaintext hashes
    Compressed  bool          // true if compression was applied per chunk
    CreatedAt   int64         // UnixNano
}
```

#### Functions & Methods

```go
// ChunkReader(r io.Reader, chunkSize int) (<-chan Chunk, <-chan error)
  // starts goroutine: reads chunkSize bytes at a time from r
  // for each chunk:
  //   data := read up to chunkSize bytes
  //   hash := sha256.Sum256(data)
  //   send Chunk{Index: i, Hash: hash, Size: len(data), Data: data}
  // on EOF: close channel; on error: send to errCh

// Reassemble(chunks []Chunk, dst io.Writer) error
  // sort chunks by Index (in case received out of order)
  // for each chunk in order: dst.Write(chunk.Data)

// BuildManifest(fileKey string, chunks []ChunkInfo, compressed bool) *ChunkManifest
  // computes MerkleRoot over chunk hashes using merkle.Build

// VerifyChunk(chunk Chunk) bool
  // recompute sha256.Sum256(chunk.Data) == chunk.Hash

// ChunkStorageKey(encHash string) string
  // returns "chunk:" + encHash  — namespaced CAS key to avoid collision with file keys

// SplitManifestKey(fileKey string) string
  // returns "manifest:" + fileKey  — storage key for ChunkManifest JSON
```

**File:** `Storage/chunker/chunker_test.go`
```
TestChunkSmallFile      — file < 4MB produces 1 chunk
TestChunkExactBoundary  — file exactly 4MB produces 1 chunk with no empty tail
TestChunkMultiple       — 10MB file → 3 chunks (4+4+2)
TestReassembleInOrder   — reassemble sorted chunks equals original
TestReassembleOutOfOrder — unsorted chunks reassemble correctly
TestVerifyChunkOk       — valid chunk passes
TestVerifyChunkCorrupt  — flipped bit fails verification
TestManifestMerkleRoot  — same chunks always produce same root
BenchmarkChunk100MB     — chunk 100MB reader, < 2s
```

---

**Modify `Storage/storage.go`:**

Add to `FileMeta`:
```go
Chunked   bool
Manifest  *chunker.ChunkManifest  // nil for non-chunked files
```

Add to `MetadataStore` interface:
```go
GetManifest(fileKey string) (*chunker.ChunkManifest, bool)
SetManifest(fileKey string, manifest *chunker.ChunkManifest) error
```

Implement `GetManifest`/`SetManifest` in `MetaFile` by storing manifest JSON under key `"manifest:"+fileKey` in the same JSON store.

---

**Modify `Server/server.go` — `StoreData(key, reader)`:**

```
New flow:
1. chunkCh, errCh := chunker.ChunkReader(reader, DefaultChunkSize)
2. var chunkInfos []chunker.ChunkInfo
3. for chunk := range chunkCh:
   a. optionally: if shouldCompress(chunk.Data): compress → compressedData, compressed=true
   b. encData, encKey := Encryption.EncryptStream(bytes.NewReader(chunkData), tempFile)
   c. encHash := sha256(encData)
   d. storageKey := chunker.ChunkStorageKey(hex(encHash))
   e. if !store.Has(storageKey): store.WriteStream(storageKey, encData)  // dedup
   f. meta.Set(storageKey, FileMeta{EncryptedKey: hex(encKey)})
   g. Quorum.Write(storageKey, encKey, encData, vclock)
   h. chunkInfos = append(chunkInfos, ChunkInfo{Index:i, Hash:hex(chunk.Hash), Size:chunk.Size, EncHash:hex(encHash)})
4. manifest := chunker.BuildManifest(key, chunkInfos, compressed)
5. meta.SetManifest(key, manifest)
6. update FileMeta for key: {Chunked:true, VClock:vc, Timestamp:now}
```

**Modify `Server/server.go` — `GetData(key)`:**

```
New flow:
1. manifest, ok := meta.GetManifest(key)
2. if !ok: fall back to old single-blob logic (backwards compat)
3. chunks := make([]chunker.Chunk, len(manifest.Chunks))
4. for each chunkInfo in manifest.Chunks:
   a. storageKey := chunker.ChunkStorageKey(chunkInfo.EncHash)
   b. if store.Has(storageKey): read locally
   c. else: fetch from ring-responsible node (MessageGetFile with storageKey)
   d. decrypt chunk → plaintext
   e. verify: sha256(plaintext) == chunkInfo.Hash  → if mismatch: error (data corrupted)
   f. chunks[i] = Chunk{Index:i, Data:plaintext}
5. reassemble: chunker.Reassemble(chunks, outputWriter)
6. return outputWriter as reader
```

---

## Sprint 7: Multi-Source Downloads & Compression

**Goal:** Fetch chunks from multiple peers in parallel. Select fastest peer per chunk. Compress text-heavy content.

**Depends on:** Sprint 6 (chunking)

### Package: `Storage/compression/`
**File:** `Storage/compression/compression.go`

```go
// Dep: github.com/klauspost/compress/zstd

type Level int
const (
    LevelFastest Level = iota
    LevelDefault
    LevelBest
)

// Compress(src io.Reader, dst io.Writer, level Level) (int64, error)
  // encoder, _ := zstd.NewWriter(dst, zstd.WithEncoderLevel(zstd.EncoderLevel(level)))
  // n, err := io.Copy(encoder, src)
  // encoder.Close()
  // return n, err

// Decompress(src io.Reader, dst io.Writer) (int64, error)
  // decoder, _ := zstd.NewReader(src)
  // defer decoder.Close()
  // return io.Copy(dst, decoder)

// ShouldCompress(header []byte) bool
  // 1. check magic bytes for known incompressible formats:
  //    JPEG: 0xFF 0xD8; MP4: 0x00 0x00 0x00 0x?? 0x66 0x74 0x79 0x70
  //    ZIP: 0x50 0x4B 0x03 0x04; PNG: 0x89 0x50 0x4E 0x47
  //    if match: return false
  // 2. entropy check on header (first 4KB):
  //    compute byte frequency histogram
  //    shannon entropy = -sum(p * log2(p))
  //    if entropy > 7.5: return false  (already high entropy, won't compress well)
  // 3. return true

// CompressChunk(data []byte, level Level) ([]byte, bool, error)
  // 1. if !ShouldCompress(data[:min(4096,len(data))]): return data, false, nil
  // 2. var buf bytes.Buffer; Compress(bytes.NewReader(data), &buf, level)
  // 3. if buf.Len() >= len(data): return data, false, nil  (don't use if bigger)
  // 4. return buf.Bytes(), true, nil

// DecompressChunk(data []byte, wasCompressed bool) ([]byte, error)
  // if !wasCompressed: return data, nil
  // var buf bytes.Buffer; Decompress(bytes.NewReader(data), &buf)
  // return buf.Bytes(), nil
```

**File:** `Storage/compression/compression_test.go`
```
TestCompressDecompressRoundtrip  — random data → compress → decompress → equal
TestShouldCompressDetectsJPEG   — JPEG magic bytes → false
TestShouldCompressText          — low-entropy text → true
TestShouldCompressHighEntropy   — random bytes → false
TestCompressChunkSkipsIfBigger  — if compressed > original → return original, false
```

---

### Package: `Cluster/selector/`
**File:** `Cluster/selector/selector.go`

```go
// EWMA = Exponentially Weighted Moving Average

type PeerStats struct {
    mu          sync.Mutex
    latencyEWMA float64   // milliseconds
    activeLoad  int32     // concurrent active downloads (atomic)
    alpha       float64   // EWMA smoothing factor, default 0.2
}

type Selector struct {
    mu    sync.RWMutex
    peers map[string]*PeerStats   // addr -> stats
}

// New() *Selector

// (sel *Selector) RecordLatency(addr string, d time.Duration)
  // stats := sel.getOrCreate(addr)
  // ms := float64(d.Milliseconds())
  // stats.mu.Lock(); stats.latencyEWMA = alpha*ms + (1-alpha)*stats.latencyEWMA; stats.mu.Unlock()

// (sel *Selector) IncrLoad(addr string)   — call when starting download from peer
  // atomic.AddInt32(&sel.peers[addr].activeLoad, 1)

// (sel *Selector) DecrLoad(addr string)   — call when download completes
  // atomic.AddInt32(&sel.peers[addr].activeLoad, -1)

// (sel *Selector) BestPeer(candidates []string) string
  // if len(candidates) == 0: return ""
  // score(addr) = latencyEWMA * (1 + 0.5 * activeLoad)   // lower is better
  // return addr with minimum score
  // if no stats for addr: assign score 100ms (neutral default)

// (sel *Selector) Remove(addr string)
  // sel.mu.Lock(); delete(sel.peers, addr); sel.mu.Unlock()
```

**File:** `Cluster/selector/selector_test.go`
```
TestBestPeerByLatency       — lower EWMA latency wins
TestBestPeerPenalizesLoad   — same latency, higher load → worse score
TestBestPeerNoStats         — unknown peer gets neutral score
TestRecordLatencyEWMA       — EWMA converges toward true mean
TestDecrLoadBelowZero       — load never goes below 0 (guard against underflow)
```

---

### Package: `Server/downloader/`
**File:** `Server/downloader/downloader.go`

#### Structs

```go
type Config struct {
    MaxParallel    int           // max concurrent chunk fetches, default 4
    ChunkTimeout   time.Duration // per-chunk timeout, default 30s
    MaxRetries     int           // retries per chunk on failure, default 3
}

type Progress struct {
    TotalChunks     int
    CompletedChunks int
    TotalBytes      int64
    ReceivedBytes   int64
}

type ProgressFunc func(p Progress)

// chunkResult carries a fetched+verified chunk
type chunkResult struct {
    chunk chunker.Chunk
    err   error
}

type DownloadManager struct {
    cfg      Config
    ring     *hashring.HashRing
    selector *selector.Selector
    getPeer  func(addr string) (peer2peer.Peer, bool)
    fetchChunk func(addr, storageKey, encKey string) ([]byte, error)  // fetch encrypted chunk from peer
    decrypt  func(encData []byte, encKey string) ([]byte, error)
}
```

#### Functions & Methods

```go
// New(cfg Config, ring *hashring.HashRing, sel *selector.Selector,
//     getPeer func(string)(Peer,bool), fetchChunk func(string,string,string)([]byte,error),
//     decrypt func([]byte,string)([]byte,error)) *DownloadManager

// (dm *DownloadManager) Download(manifest *chunker.ChunkManifest, dst io.Writer,
//                                 progressFn ProgressFunc) error
  // 1. results := make([]chunker.Chunk, len(manifest.Chunks))
  // 2. errCh := make(chan error, 1)
  // 3. sem := make(chan struct{}, cfg.MaxParallel)   — semaphore for parallelism
  // 4. var wg sync.WaitGroup
  // 5. for each chunkInfo in manifest.Chunks:
  //    wg.Add(1)
  //    go dm.fetchOne(chunkInfo, manifest, &results[i], sem, &wg, progressFn, errCh)
  // 6. wg.Wait()
  // 7. select { case err := <-errCh: return err; default: }
  // 8. chunker.Reassemble(results, dst)

// (dm *DownloadManager) fetchOne(info ChunkInfo, manifest *ChunkManifest,
//                                 result *Chunk, sem chan struct{},
//                                 wg *WaitGroup, progressFn ProgressFunc, errCh chan error)
  // defer wg.Done()
  // sem <- struct{}{}; defer func() { <-sem }()
  // storageKey := chunker.ChunkStorageKey(info.EncHash)
  // encKey from manifest's per-chunk metadata (looked up by storageKey from MetaStore)
  // candidates := ring.GetNodes(storageKey, ring.ReplicationFactor())
  // for attempt := 0; attempt < cfg.MaxRetries; attempt++:
  //   best := selector.BestPeer(candidates)
  //   selector.IncrLoad(best)
  //   t0 := time.Now()
  //   encData, err := fetchChunk(best, storageKey, encKey)
  //   selector.DecrLoad(best)
  //   selector.RecordLatency(best, time.Since(t0))
  //   if err: candidates = remove(candidates, best); continue  — try next
  //   plainData, err := decrypt(encData, encKey)
  //   if err: continue
  //   verify: sha256(plainData) == decode(info.Hash)
  //   if mismatch: continue
  //   *result = Chunk{Index:info.Index, Data:plainData}
  //   if progressFn != nil: progressFn(...)
  //   return
  // errCh <- fmt.Errorf("chunk %d: all retries failed", info.Index)
```

**File:** `Server/downloader/downloader_test.go`
```
TestDownloadAllChunksParallel  — 3 chunks, maxParallel=3, all fetched concurrently
TestDownloadRetryOnFailure     — first peer fails, retries on second peer succeeds
TestDownloadFailsAfterRetries  — all peers fail → error returned
TestDownloadProgress           — progressFn called once per chunk completion
TestDownloadReassembleOrder    — out-of-order chunk completion → correct file output
BenchmarkDownload100MB         — 100MB file over mock network, verify throughput
```

**Server.go Integration for Sprint 7:**

**New fields on `Server`:**
```go
Downloader *downloader.DownloadManager
Selector   *selector.Selector
```

**Modified `GetData(key)`:**
```
// After building manifest:
err := s.Downloader.Download(manifest, &outputBuf, func(p downloader.Progress) {
    log.Printf("Download progress: %d/%d chunks", p.CompletedChunks, p.TotalChunks)
})
```

**Modified `StoreData(key, reader)` — compression:**
```
// Before encryption of each chunk:
compressed, wasCompressed, err := compression.CompressChunk(chunk.Data, compression.LevelDefault)
// store wasCompressed flag per chunk in ChunkInfo
// decompress on download before hash verification
```

**New CLI flags:**
```go
--download-parallel int   // default 4
--compress            bool // default true
```

---

## Sprint 8: Advanced Networking

**Goal:** QUIC transport for multiplexing & lower latency. Transport factory for pluggability. NAT traversal for cross-network P2P.

**Depends on:** Sprint 3 (gossip carries public address info needed for NAT)

### Package: `Peer2Peer/quic/`
**File:** `Peer2Peer/quic/quic.go`

```go
// Dep: github.com/quic-go/quic-go

// QUICPeer implements peer2peer.Peer
type QUICPeer struct {
    conn     quic.Connection
    stream   quic.Stream
    outbound bool
    Wg       sync.WaitGroup
}

// Implement net.Conn interface via quic.Stream:
// (p *QUICPeer) Read(b []byte) (int, error)     -> p.stream.Read(b)
// (p *QUICPeer) Write(b []byte) (int, error)    -> p.stream.Write(b)
// (p *QUICPeer) Close() error                   -> p.conn.CloseWithError(0, "close")
// (p *QUICPeer) LocalAddr() net.Addr            -> p.conn.LocalAddr()
// (p *QUICPeer) RemoteAddr() net.Addr           -> p.conn.RemoteAddr()
// (p *QUICPeer) SetDeadline(t time.Time) error         -> p.stream.SetDeadline(t)
// (p *QUICPeer) SetReadDeadline(t time.Time) error     -> p.stream.SetReadDeadline(t)
// (p *QUICPeer) SetWriteDeadline(t time.Time) error    -> p.stream.SetWriteDeadline(t)
// (p *QUICPeer) Send(b []byte) error            -> p.stream.Write(b) (ignore n)
// (p *QUICPeer) CloseStream()                   -> p.Wg.Done()

// QUICTransportOpts
type QUICTransportOpts struct {
    ListenAddr       string
    TLSConfig        *tls.Config         // REQUIRED for QUIC (always TLS 1.3)
    HandshakeFunc    peer2peer.HandshakeFunc
    OnPeer           func(peer2peer.Peer) error
    OnPeerDisconnect func(peer2peer.Peer)
    QUICConfig       *quic.Config        // optional, nil = defaults
}

// QUICTransport implements peer2peer.Transport
type QUICTransport struct {
    opts     QUICTransportOpts
    listener *quic.Listener
    rpcch    chan peer2peer.RPC
}

// NewQUICTransport(opts QUICTransportOpts) *QUICTransport

// (t *QUICTransport) ListenAndAccept() error
  // t.listener = quic.ListenAddr(opts.ListenAddr, opts.TLSConfig, opts.QUICConfig)
  // go t.acceptLoop()

// (t *QUICTransport) acceptLoop()
  // for:
  //   conn := t.listener.Accept(ctx)
  //   go t.handleConn(conn, false)

// (t *QUICTransport) handleConn(conn quic.Connection, outbound bool)
  // defer: conn.CloseWithError(0,""); opts.OnPeerDisconnect(peer)
  // stream := conn.OpenStreamSync(ctx)
  // peer := &QUICPeer{conn:conn, stream:stream, outbound:outbound}
  // opts.HandshakeFunc(peer)
  // opts.OnPeer(peer)
  // read loop: same control byte logic as TCP handleConn
  //   read 1 byte: 0x1 → decode GOB → send to rpcch; 0x2 → stream signal

// (t *QUICTransport) Dial(addr string) error
  // conn := quic.DialAddr(ctx, addr, opts.TLSConfig, opts.QUICConfig)
  // go t.handleConn(conn, true)

// (t *QUICTransport) Addr() string    -> opts.ListenAddr
// (t *QUICTransport) Consume() <-chan RPC  -> t.rpcch
// (t *QUICTransport) Close() error    -> t.listener.Close()
```

**File:** `Peer2Peer/quic/quic_test.go`
```
TestQUICPeerSendReceive         — send bytes from one peer, receive on other
TestQUICTransportDial           — dial creates connected peer, OnPeer called
TestQUICTransportHandleConn     — message sent via 0x1 control byte appears in Consume()
TestQUICImplementsTransport     — compile-time interface check: var _ Transport = &QUICTransport{}
TestQUICImplementsPeer          — compile-time: var _ Peer = &QUICPeer{}
```

---

### Package: `Peer2Peer/factory/`
**File:** `Peer2Peer/factory/factory.go`

```go
type TransportType string
const (
    TransportTCP  TransportType = "tcp"
    TransportQUIC TransportType = "quic"
)

type Options struct {
    Type             TransportType
    ListenAddr       string
    TLSConfig        *tls.Config
    HandshakeFunc    peer2peer.HandshakeFunc
    Decoder          peer2peer.Decoder
    OnPeer           func(peer2peer.Peer) error
    OnPeerDisconnect func(peer2peer.Peer)
}

// New(opts Options) (peer2peer.Transport, error)
  // switch opts.Type:
  //   case TransportTCP:
  //     return peer2peer.NewTCPTransport(peer2peer.TCPTransportOpts{...}), nil
  //   case TransportQUIC:
  //     if opts.TLSConfig == nil: return nil, error("QUIC requires TLS config")
  //     return quic.NewQUICTransport(quic.QUICTransportOpts{...}), nil
  //   default: return nil, fmt.Errorf("unknown transport type: %s", opts.Type)
```

**Modify `MakeServer`:**
```go
// Replace direct NewTCPTransport call with factory:
transport, err := factory.New(factory.Options{
    Type:       factory.TransportType(transportFlag),
    ListenAddr: listenAddr,
    TLSConfig:  tlsConfig,
    ...
})
```

**New CLI flags:**
```go
--transport string   // default "tcp", values: "tcp" | "quic"
```

---

### Package: `Peer2Peer/nat/`
**File:** `Peer2Peer/nat/nat.go`

```go
// STUN: discover public IP:port behind NAT
// UDP hole punching: establish direct P2P through NAT

type Config struct {
    STUNServers []string      // default: ["stun:stun.l.google.com:19302"]
    TURNServer  string        // optional TURN relay fallback
    TURNUser    string
    TURNPass    string
}

type NATTraversal struct {
    cfg        Config
    publicAddr string   // discovered public addr (cached)
    mu         sync.RWMutex
}

// New(cfg Config) *NATTraversal

// (n *NATTraversal) DiscoverPublicAddr() (string, error)
  // 1. for each STUN server:
  //    a. conn := net.DialUDP("udp", nil, stunServer)
  //    b. send STUN Binding Request (RFC 5389 format):
  //       [2B type=0x0001][2B length=0][4B magic=0x2112A442][12B transaction ID]
  //    c. read response, parse XOR-MAPPED-ADDRESS attribute
  //    d. return "ip:port" string
  //    e. on error: try next STUN server
  // 2. cache in n.publicAddr; return

// (n *NATTraversal) PublicAddr() string
  // n.mu.RLock(); return n.publicAddr

// (n *NATTraversal) PunchHole(targetPublicAddr string) (net.Conn, error)
  // UDP hole punching:
  // 1. localConn := net.ListenUDP("udp", localAddr)
  // 2. send 3 UDP packets to targetPublicAddr (opens NAT hole)
  // 3. signal target via gossip/signaling (targetPublicAddr knows our public addr)
  // 4. listen for incoming UDP from target (with 5s timeout)
  // 5. if connected: return net.Conn wrapper around UDP conn
  // 6. else: dial TURN relay (if configured), return TURN-relayed conn

// (n *NATTraversal) AnnotateGossip() map[string]string
  // returns map{"public_addr": n.PublicAddr()} for inclusion in NodeInfo.Metadata
```

**Integration:**
- On startup: `nat.DiscoverPublicAddr()` — store in `NodeInfo.Metadata["public_addr"]`
- Gossip propagates `public_addr` to all peers via `NodeInfo.Metadata`
- When `MakeServer` sees a peer's `public_addr` differs from `RemoteAddr()` (they're behind NAT): use `PunchHole` to establish direct connection

**New CLI flags:**
```go
--stun-server string  // default "stun:stun.l.google.com:19302"
--turn-server string  // default "" (disabled)
```

**File:** `Peer2Peer/nat/nat_test.go`
```
TestDiscoverPublicAddrMock   — mock STUN server, verify parsing of XOR-MAPPED-ADDRESS
TestAnnotateGossipIncludesAddr — AnnotateGossip returns public_addr key
TestPunchHoleMock            — mock peer, verify UDP packets sent
```

---

## Dependency Graph (Final)

```
Sprint 0  ──► Sprint 1  ──► Sprint 2  ──► Sprint 3  ──► Sprint 4  ──► Sprint 6  ──► Sprint 7
(bugs)         (hashing)    (failure)    (gossip)      (quorum)      (chunking)    (parallel DL)
                                │                           │
                                └──► Sprint 5 (observe)    └──► Sprint 4 reuses Sprint 3 ClusterState
                                           Sprint 3 ──► Sprint 8 (QUIC/NAT)
```

## Package Import Map (no circular deps)

```
Cluster/hashring    ← no internal imports
Cluster/vclock      ← no internal imports
Cluster/merkle      ← no internal imports
Cluster/failure     ← hashring (indirect, via server closure)
Cluster/membership  ← vclock (for generation)
Cluster/gossip      ← membership
Cluster/handoff     ← no internal imports
Cluster/rebalance   ← hashring, storage
Cluster/quorum      ← hashring, membership, vclock, conflict
Cluster/conflict    ← vclock, storage
Cluster/selector    ← no internal imports
Storage/chunker     ← merkle (for manifest root)
Storage/compression ← no internal imports
Server/downloader   ← hashring, selector, chunker, compression
Observability/*     ← no internal imports (only stdlib + 3rd party)
Peer2Peer/quic      ← Peer2Peer (interfaces only)
Peer2Peer/factory   ← Peer2Peer, Peer2Peer/quic
Peer2Peer/nat       ← no internal imports
Server/server.go    ← everything above
```

---

## New External Dependencies (go get these per sprint)

| Sprint | Command |
|--------|---------|
| 5 | `go get github.com/rs/zerolog` |
| 5 | `go get github.com/prometheus/client_golang/prometheus` |
| 5 | `go get go.opentelemetry.io/otel go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc` |
| 7 | `go get github.com/klauspost/compress/zstd` |
| 8 | `go get github.com/quic-go/quic-go` |

---

## Modified Files Summary

| File | Sprint(s) | What changes |
|------|-----------|--------------|
| `Server/server.go` | 2,3,4,6,7 | New fields, modified StoreData/GetData, new message cases, new goroutines |
| `Storage/storage.go` | 4,6 | FileMeta extended (VClock,Timestamp,Chunked,Manifest), Keys() added, GetManifest/SetManifest |
| `Peer2Peer/transport.go` | 8 | No change needed (interfaces already correct) |
| `Peer2Peer/tcpTransport.go` | 8 (factory only) | Factory wraps it, no direct changes |
| `cmd/start.go` | 3,7,8 | New CLI flags: --gossip-*, --download-parallel, --compress, --transport, --stun-server |
| `cmd/daemon.go` | 3,7,8 | Pass new flags to MakeServer |
| `go.mod` | 5,7,8 | Add dependencies |

---

## Per-Sprint Test Gates

| Sprint | Gate |
|--------|------|
| 2 | `go test ./Cluster/failure/... ./Cluster/handoff/... ./Cluster/rebalance/...` + kill node mid-write → verify hint stored → restart → verify hint delivered |
| 3 | `go test ./Cluster/membership/... ./Cluster/gossip/...` + 5 nodes, 1 seed, all discover each other within 2s |
| 4 | `go test ./Cluster/vclock/... ./Cluster/quorum/... ./Cluster/conflict/... ./Cluster/merkle/...` + concurrent writes to same key → LWW wins → corrupted replica repaired within 10min |
| 5 | `curl localhost:4000/health` → JSON response; `curl localhost:4000/metrics` → Prometheus text |
| 6 | `go test ./Storage/chunker/...` + upload 50MB file → download → sha256 of output == sha256 of input |
| 7 | `go test ./Storage/compression/... ./Cluster/selector/... ./Server/downloader/...` + download shows parallel chunk fetching in logs |
| 8 | `go test ./Peer2Peer/quic/... ./Peer2Peer/factory/... ./Peer2Peer/nat/...` + `dfs start --transport=quic` connects two nodes |

---

## After Sprint 8: Web App Layer

Once all sprints complete, the system exposes:
- Unix socket IPC (current: upload/download commands)
- Health HTTP endpoint per node

**Next phase** adds:
- REST API server (Gin or Chi): `POST /files`, `GET /files/:key`, `DELETE /files/:key`, `GET /files` (list)
- JWT auth (user accounts, college email verification)
- React/Next.js frontend: file browser, upload with progress, preview (PDF.js, video player)
- PostgreSQL metadata index (search by name, subject, uploader, tags)
- Nginx reverse proxy in front of REST API

The distributed core (Sprints 0–8) is the storage engine. The web layer treats the running daemon as a library: import `Server` package, call `server.StoreData()` / `server.GetData()` directly — no Unix socket needed when embedded.
