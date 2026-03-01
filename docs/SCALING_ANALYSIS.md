# DFS-Go Scaling Analysis
**Written:** 2026-03-01
**Current architecture:** Post-Sprint-8, all E2E tests passing (63 tests, 0 failures)
**Comfortable ceiling with current code:** ~50 nodes
**Target ceiling after fixes:** 300–500 nodes

---

## 1. What the System Does Well at Any Scale

These components are topology-independent and will not require changes:

| Component | Why it scales |
|-----------|--------------|
| **Consistent hashing** (`Cluster/hashring/`) | O(log n) ring lookups. 300 nodes × 150 virtual nodes = 45,000 ring entries — negligible. |
| **Chunking + CAS storage** (`Storage/chunker/`, `Storage/storage.go`) | Pure local I/O. Scales linearly with disk. Zero inter-node coupling. |
| **Gossip convergence** (`Cluster/gossip/`) | Fan-out=3 random peers per 200ms round. Converges in O(log N) rounds regardless of cluster size. At 300 nodes: ~8 rounds = ~1.6 seconds. Already correct. |
| **Replication fan-out** (`replicateChunk`, `replicateManifest`) | Both already route to **RF=3 ring-responsible nodes only**, not all peers. Correct. |
| **Parallel downloader** (`Server/downloader/`) | Semaphore-capped at 4 concurrent fetches per file. Peer selection via EWMA. No cluster-size dependency. |
| **Encryption + compression** (`Crypto/`, `Storage/compression/`) | Per-stream operations, completely local. |

---

## 2. The Four Bottlenecks — In Order of Severity

---

### Bottleneck 1: Bootstrap — Thundering Herd
**Severity: CRITICAL**
**Breaks at: ~50–80 nodes**
**File:** `Server/server.go:1614–1633`

#### What the code does
```go
func (s *Server) BootstrapNetwork() error {
    var wg sync.WaitGroup
    for _, addr := range s.serverOpts.bootstrapNodes {
        wg.Add(1)
        go func(addr string) {    // one goroutine per peer, no limit
            defer wg.Done()
            s.serverOpts.transport.Dial(addr)
        }(addr)
    }
    wg.Wait()
    return nil
}
```
Every node is given the **full list of all existing peers** at construction time (see `tests/e2e/helpers_test.go:57–61`) and dials all of them simultaneously on startup. At N=100 that is 99 simultaneous outbound TCP SYNs per node. Each successful dial spawns `handleConn` goroutines, triggers `OnPeer`, which adds to the ring and may re-trigger gossip dials back. This is a **thundering herd** — a startup spike that:

1. Floods the network with N(N-1)/2 simultaneous SYNs
2. Exhausts OS ephemeral port range on a single machine
3. Causes `OnPeer` to be called N-1 times concurrently per node — each under `peerLock.Lock()`

#### Why it matters
In production, a new node joining a 200-node cluster would be given 200 seed addresses and attempt 200 simultaneous dials. TCP SYN flood on the local network, plus goroutine explosion (200 goroutines × every joining node = thousands of goroutines per join event). The cluster becomes unstable during rolling restarts.

#### The fix
Seed with **3–5 well-known nodes only**. Let gossip's `onNewPeer` callback discover the rest organically. Gossip already has the correct deduplication logic (`dialingSet`).

```go
// CHANGE: In MakeServer/newCluster, only pass 1–3 seed addresses
// instead of all N-1 peers.

// CHANGE: BootstrapNetwork — add a semaphore to cap concurrent dials
func (s *Server) BootstrapNetwork() error {
    sem := make(chan struct{}, 5)  // max 5 concurrent dials
    var wg sync.WaitGroup
    for _, addr := range s.serverOpts.bootstrapNodes {
        wg.Add(1)
        sem <- struct{}{}
        go func(addr string) {
            defer wg.Done()
            defer func() { <-sem }()
            s.serverOpts.transport.Dial(addr)
        }(addr)
    }
    wg.Wait()
    return nil
}
```

Gossip will discover remaining peers via `onNewPeer` within O(log N) × 200ms = ~1.6s at 300 nodes. Total startup time increases by ~2s, but the thundering herd is eliminated entirely.

**Effort:** ~1 hour. Two changes: cap bootstrap seeds to 3–5 in caller, add semaphore to `BootstrapNetwork`.

---

### Bottleneck 2: Persistent All-to-All TCP Connections
**Severity: HIGH**
**Breaks at: ~970 nodes (hard FD limit), degrades from ~200**
**File:** `Server/server.go:70–78`, `Peer2Peer/tcpTransport.go`

#### What the code does
Every node maintains a **persistent TCP connection to every other node** in `s.peers map[string]peer2peer.Peer`. Each connection keeps:
- 1 goroutine for `handleConn` read loop
- 1 goroutine spawned per incoming stream
- 1 OS file descriptor

At N nodes per node: (N-1) connections × 2 goroutines = 2(N-1) goroutines just for connection management.

| N nodes | Connections per node | Goroutines per node | Cluster-wide FDs |
|---------|---------------------|--------------------|--------------------|
| 50      | 49                  | ~98                | 2,450              |
| 100     | 99                  | ~198               | 9,900              |
| 200     | 199                 | ~398               | 39,800             |
| 500     | 499                 | ~998               | 249,500            |
| 970     | 969                 | ~1,938             | **FD exhaustion**  |

Linux default `ulimit -n` = 1024. After reserving ~50 FDs for storage/health/stdio, the hard ceiling is ~**970 nodes**.

#### Why it matters
Beyond the hard FD limit, there is also a soft cost: each idle persistent connection consumes memory, contributes to `peerLock` contention (every replication/fetch operation takes `peerLock.RLock()`), and keeps goroutines alive doing nothing. At 200 nodes, ~400 goroutines per node are just blocking on network reads.

#### The fix
Switch to **on-demand connections** — dial when needed, close after a period of inactivity. Keep persistent connections only to ring neighbors (the nodes this node most frequently replicates to — typically 2 at RF=3).

```
Design:
- Keep permanent connections to RF-1 ring neighbors
- All other peers: dial on-demand, cache with a 30s idle TTL
- LRU connection pool capped at max_conns (e.g. 50)
- If connection not in pool: dial, use, return to pool or evict LRU
```

This is the largest engineering change of the four. It requires touching `fetchChunkFromPeer`, `replicateChunk`, `replicateManifest`, and `sendToAddr` to go through a connection pool instead of directly reading from `s.peers`.

**Effort:** ~1 day. New `connpool` package with `Get(addr) Peer` and `Put(addr, peer)`. All send paths updated to use pool.

---

### Bottleneck 3: All-to-All Heartbeat
**Severity: HIGH**
**Breaks at: ~200 nodes (bandwidth saturation on LAN)**
**File:** `Cluster/failure/detector.go:294–312`

#### What the code does
```go
func (hs *HeartbeatService) senderLoop() {
    ticker := time.NewTicker(hs.cfg.HeartbeatInterval)  // 1 second
    for {
        select {
        case <-ticker.C:
            for _, addr := range hs.getPeers() {
                go func(a string) { _ = hs.sendHeartbeat(a) }(addr)
            }
        }
    }
}
```
Every second, every node sends a heartbeat to **every other node**. This is O(N²) messages per second cluster-wide.

| N nodes | Messages/second (cluster-wide) | Notes |
|---------|-------------------------------|-------|
| 50      | 2,450/s                       | Fine |
| 100     | 9,900/s                       | Noticeable |
| 200     | 39,800/s                      | ~4MB/s on a 100-byte heartbeat payload |
| 300     | 89,700/s                      | ~9MB/s sustained |
| 500     | 249,500/s                     | ~25MB/s — saturates typical college LAN |

Additionally, `senderLoop` spawns **one goroutine per peer per second**. At 300 nodes: 299 goroutines created and destroyed every second per node. That's 89,700 goroutine create/destroy operations per second cluster-wide — measurable GC pressure.

#### Why it matters
The phi-accrual failure detector (`Cluster/failure/detector.go:138–170`) maintains a per-peer sliding window of 200 inter-arrival samples. At 300 nodes: 300 windows × 200 floats × 8 bytes = ~480KB of detector state per node. Not catastrophic, but the **message volume** is.

#### The fix — SWIM-style partial monitoring
Instead of every node monitoring every peer, each node monitors only a **random subset of k peers** (e.g. k=8). Failure information propagates via gossip's existing `NodeInfo` dissemination. This is the SWIM protocol approach (Scalable Weakly-consistent Infection-style process group Membership).

```
Design:
- Each node randomly selects k=8 peers per heartbeat interval
- On suspicion, broadcast via gossip (already wired) not direct heartbeat
- Phi accrual maintained only for monitored subset
- Gossip's existing Alive/Suspect/Dead state already handles propagation
```

The gossip system already disseminates cluster state. The heartbeat only needs to detect failure of directly-monitored peers — gossip then propagates the failure belief to the rest.

```go
// CHANGE: In senderLoop, sample k random peers instead of all peers
func (hs *HeartbeatService) senderLoop() {
    ticker := time.NewTicker(hs.cfg.HeartbeatInterval)
    for {
        select {
        case <-ticker.C:
            peers := hs.getPeers()
            k := min(hs.cfg.MonitorSubset, len(peers))  // e.g. k=8
            // Fisher-Yates shuffle first k elements
            for i := 0; i < k; i++ {
                j := i + rand.Intn(len(peers)-i)
                peers[i], peers[j] = peers[j], peers[i]
            }
            for _, addr := range peers[:k] {
                go func(a string) { _ = hs.sendHeartbeat(a) }(addr)
            }
        }
    }
}
```

Traffic drops from O(N²) to O(N·k) = O(N) for constant k=8. At 300 nodes: 300 × 8 = 2,400 heartbeats/second — same as a 50-node all-to-all cluster today.

**Effort:** ~3 hours. Add `MonitorSubset int` to `Config`, modify `senderLoop` and `reaperLoop` to operate on the sampled subset only.

---

### Bottleneck 4: Serial `Broadcast` on `GracefulShutdown`
**Severity: MEDIUM**
**Breaks at: ~100+ nodes (causes slow graceful shutdowns)**
**File:** `Server/server.go:1071–1108`

#### What the code does
```go
func (s *Server) Broadcast(d Message) error {
    // ...encode once...
    for addr, peer := range peersCopy {
        peer.SendMsg(peer2peer.IncomingMessage, buf.Bytes())  // SERIAL, blocking
    }
    return nil
}
```
`Broadcast` iterates all peers **serially**. `SendMsg` is synchronous — it blocks until the message is written to the TCP socket buffer. At N=300 with typical LAN RTTs, this takes N × (socket write time) ≈ N × 0.1ms = 30ms at 300 nodes. Not catastrophic, but it blocks `GracefulShutdown` for 30ms+.

More importantly: `Broadcast` is used only for `MessageLeaving` in `GracefulShutdown`. This is correct behavior — `MessageLeaving` genuinely needs to reach all peers. The serial execution is the only issue.

#### The fix
Parallelize with a goroutine per peer, same pattern as `replicateChunk`/`replicateManifest`:

```go
func (s *Server) Broadcast(d Message) error {
    buf := new(bytes.Buffer)
    if err := gob.NewEncoder(buf).Encode(d); err != nil {
        return err
    }
    msgBytes := buf.Bytes()

    s.peerLock.RLock()
    peersCopy := make(map[string]peer2peer.Peer, len(s.peers))
    for addr, peer := range s.peers {
        peersCopy[addr] = peer
    }
    s.peerLock.RUnlock()

    var wg sync.WaitGroup
    for addr, peer := range peersCopy {
        wg.Add(1)
        go func(a string, p peer2peer.Peer) {
            defer wg.Done()
            _ = p.SendMsg(peer2peer.IncomingMessage, msgBytes)
        }(addr, peer)
    }
    wg.Wait()
    return nil
}
```

**Effort:** ~30 minutes. Pure mechanical change to `Broadcast`.

---

## 3. Node Count Ceilings — Summary

| Node count | Status | What limits it |
|-----------|--------|----------------|
| **1–20**  | Completely comfortable | Nothing — design headroom |
| **20–50** | Works correctly | Minor write latency from manifest broadcast |
| **50–80** | Bootstrap becomes unreliable | Thundering herd on new node join |
| **80–150** | Unstable joins; heartbeat noisy | Bootstrap + O(N²) heartbeat traffic |
| **150–200** | LAN bandwidth saturation | ~40k heartbeat msg/s |
| **200–970** | FD limit approached | Persistent connection count |
| **970+**  | Hard crash | OS file descriptor exhaustion |

**Comfortable ceiling with current architecture: ~50 nodes.**
**After all four fixes: 300–500 nodes comfortably.**

---

## 4. Fix Priority and Effort

| Priority | Fix | Effort | Impact | File(s) |
|----------|-----|--------|--------|---------|
| 1 (do first) | Bootstrap: seed 3–5 peers + semaphore cap | ~1 hour | Eliminates thundering herd; unblocks >50 node joins | `Server/server.go:1614` |
| 2 | Heartbeat: SWIM-style k=8 subset monitoring | ~3 hours | O(N²)→O(N) heartbeat traffic; enables >200 nodes | `Cluster/failure/detector.go:294` |
| 3 | Broadcast: parallelize with goroutines | ~30 min | Faster graceful shutdown at large N | `Server/server.go:1071` |
| 4 (last) | Connection pool: on-demand + LRU eviction | ~1 day | Removes FD ceiling; enables >200 nodes | `Server/server.go`, new `connpool/` package |

Fixes 1 + 2 + 3 together get you to **~200 nodes** in roughly a half-day of work.
Fix 4 additionally gets you to **300–500 nodes**.

---

## 5. What Does NOT Need to Change

These are sometimes mistakenly targeted for scaling work but are already correct:

- **`replicateChunk`** — already RF=3 only, already parallel with WaitGroup. No change needed.
- **`replicateManifest`** — already RF=3 only, already parallel with WaitGroup. No change needed.
- **`fetchChunkFromPeer` / `fetchManifestFromPeers`** — already ring-scoped. First-response pattern is correct.
- **Gossip fan-out** — already O(log N). FanOut=3 is correct for epidemic broadcast.
- **Consistent hash ring** — correct implementation, O(log n) lookups, no changes needed.
- **Chunker / CAS / Encryption** — local operations, topology-independent.

---

## 6. Implementation Notes for When You Fix These

### Bootstrap fix: important interaction with gossip
When you change bootstrap to seed only 3–5 nodes, the cluster forms fully via gossip's `onNewPeer` → dial path. That path already uses `dialingSet` to prevent duplicate dials. However, you need to ensure the **gossip interval is short enough** that a new 300-node cluster converges before any timeout fires. At 200ms/round and FanOut=3: convergence in ~8 rounds = ~1.6s. All existing timeouts (10s for chunk fetch, 8s for cluster formation in tests) are well above this. No timeout changes needed.

### Heartbeat fix: interaction with phi-accrual
The phi-accrual detector (`detector.go`) maintains a **per-peer** sliding window. When you switch to subset monitoring, a node only monitors k=8 peers directly. For the other N-8 peers, phi cannot be computed locally. Failure of non-monitored peers is still detected — via gossip propagating `StateSuspect`/`StateDead` from the nodes that *do* monitor them. The `onDead` callback (which removes from ring and triggers rebalancer) must therefore accept failures from **both** local phi and gossip-propagated state. This is already partially wired — the gossip `HandleDigest`/`HandleResponse` path updates `ClusterState`, and the failure detector's `reap()` already reads from `ClusterState`. Verify that `onDead` fires from the gossip path as well as the phi path when implementing this fix.

### Connection pool fix: `peerLock` replacement
Currently `s.peers map[string]peer2peer.Peer` is the central connection registry, protected by `peerLock RWMutex`. When you switch to a connection pool, `s.peers` becomes the **pool of active/cached connections** rather than all connections. The `OnPeer` callback, `OnPeerDisconnect`, `handleLeaving`, and all send paths currently use `s.peers` directly — they will need to route through the pool's `Get(addr)` / `Put(addr, peer)` interface. Keep `s.peers` for ring-neighbor permanent connections and introduce `s.pool` for on-demand connections. This separation makes the change incremental and testable.

---

*Document written for future reference before web app layer development. Revisit before scaling beyond 50 concurrent nodes.*
