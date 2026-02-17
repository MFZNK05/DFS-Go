# DFS-Go Implementation Progress & Architecture Plan

## Project Context
College resource-sharing P2P distributed file storage system. Handles varied media: audio, video, PDFs, docs, PPTs of varied sizes (potentially GBs). Will become a web app.

---

## Current Project Structure (after Sprint 0 + Sprint 1)

```
DFS-Go/
├── main.go                          # Entry point → cmd.Execute()
├── go.mod                           # github.com/Faizan2005/DFS-Go (Go 1.24.1)
├── Makefile
├── .env                             # DFS_ENCRYPTION_KEY, DFS_ENABLE_TLS
│
├── cmd/                             # CLI layer (Cobra)
│   ├── root.go                      # Root "dfs" command
│   ├── start.go                     # "dfs start --port --peer --replication"
│   ├── daemon.go                    # StartDaemon: Unix socket + server
│   ├── socket.go                    # /tmp/dfs.sock path
│   ├── upload.go                    # "dfs upload --key --file"
│   ├── download.go                  # "dfs download --key --output"
│   ├── handle_client.go             # JSON over Unix socket → StoreData/GetData
│   ├── handle_upload.go             # CLI → base64 encode → Unix socket
│   └── handle_download.go           # Unix socket → file
│
├── Server/
│   └── server.go                    # Core server: StoreData, GetData, message handling, HashRing
│
├── Peer2Peer/
│   ├── transport.go                 # Peer + Transport interfaces
│   ├── message.go                   # RPC struct, IncomingMessage/IncomingStream constants
│   ├── encoding.go                  # GOBDecoder, DefaultDecoder
│   ├── handshake.go                 # NOPE, TLSVerify, FingerprintVerify handshakes
│   └── tcpTransport.go             # TCPPeer, TCPTransport (OnPeer, OnPeerDisconnect)
│
├── Storage/
│   ├── storage.go                   # Store (CAS), MetaFile, FileMeta, WriteStream/ReadStream
│   └── storage_test.go
│
├── Crypto/
│   ├── crypto.go                    # EncryptionService: AES-GCM streaming (4MB chunks), key wrapping
│   ├── crypto_test.go
│   ├── tls.go                       # CA gen, node certs, mTLS configs
│   └── tls_test.go
│
├── Cluster/
│   └── hashring/
│       ├── hashring.go              # ✅ HashRing: consistent hashing with virtual nodes
│       └── hashring_test.go         # ✅ 12 tests + benchmark
│
└── PROGRESS.md                      # This file
```

---

## Current Key Structs & Interfaces

```go
// Server/server.go
type ServerOpts struct {
    storageRoot       string
    pathTransform     storage.PathTransform
    tcpTransport      *peer2peer.TCPTransport    // pointer (was value — bug fix 0.6)
    metaData          storage.MetadataStore
    bootstrapNodes    []string
    Encryption        *crypto.EncryptionService
    ReplicationFactor int                         // NEW in Sprint 1
}

type Server struct {
    peerLock    sync.RWMutex                      // was sync.Mutex (bug fix 0.7)
    peers       map[string]peer2peer.Peer
    serverOpts  ServerOpts
    Store       *storage.Store
    HashRing    *hashring.HashRing                // NEW in Sprint 1
    quitch      chan struct{}
    pendingFile map[string]chan io.Reader
    mu          sync.Mutex
}

// Message types (gob-registered in init())
MessageStoreFile { Key string; Size int64; EncryptedKey string }  // EncryptedKey added in bug fix 0.2
MessageGetFile   { Key string }
MessageLocalFile { Key string; Size int64 }

// Peer2Peer/transport.go
type Peer interface { net.Conn; Send([]byte) error; CloseStream() }
type Transport interface { Addr() string; ListenAndAccept() error; Consume() <-chan RPC; Close() error; Dial(string) error }

// Peer2Peer/tcpTransport.go
type TCPTransportOpts struct {
    ListenAddr       string
    HandshakeFunc    HandshakeFunc
    Decoder          Decoder
    OnPeer           func(Peer) error
    OnPeerDisconnect func(Peer)          // NEW in bug fix 0.14
    TLSConfig        *tls.Config
}

// Storage/storage.go
type FileMeta struct { Path string; EncryptedKey string }
type MetadataStore interface { Get(key string) (FileMeta, bool); Set(key string, meta FileMeta) error }

// Cluster/hashring/hashring.go
type HashRing struct { sortedHashes []uint64; hashToNode map[uint64]string; nodeToHashes map[string][]uint64; virtualNodes int; replFactor int }
// Methods: New(cfg), AddNode, RemoveNode, GetNodes(key,n), GetPrimaryNode, HasNode, Members, Size, ReplicationFactor
```

---

## Current Data Flow

```
Upload:
  CLI → base64(file) → JSON → Unix socket → HandleClient
    → Server.StoreData(key, reader):
        1. EncryptStream(reader, tempFile) → encryptedKey
        2. metaData.Set(key, {EncryptedKey: hex(encryptedKey)})
        3. Store.WriteStream(key, tempFile) → CAS disk storage
        4. HashRing.GetNodes(key, N) → target N replicas
        5. For each target peer: send MessageStoreFile + stream encrypted data
    → Receiving peer: handleStoreMessage → WriteStream → save EncryptedKey to metadata

Download:
  CLI → JSON → Unix socket → HandleClient
    → Server.GetData(key):
        1. If local: ReadStream → DecryptStream → return plaintext
        2. If remote: HashRing.GetNodes(key, N) → send MessageGetFile to targets
           → Peer responds: handleGetMessage → ReadStream → send MessageLocalFile + stream
           → Requester: handleLocalMessage → WriteStream → DecryptStream → pendingFile channel
```

---

## COMPLETED SPRINTS

### Sprint 0: Bug Fixes ✅ (18 bugs fixed)

| Category | Bugs Fixed |
|----------|-----------|
| Data correctness | 0.1 local decrypt, 0.2 EncryptedKey propagation, 0.3 base64 encoding, 0.4 upload limit, 0.5 error handling |
| Crashes / races | 0.6 TCPTransport pointer, 0.7 RWMutex, 0.8 nil peer check, 0.9 server crash, 0.10 broadcast resilience |
| Transport | 0.11 export interface, 0.12 decoder limit, 0.13 stream RPC From, 0.14 peer disconnect |
| Hardening | 0.15 pendingFile cleanup, 0.16 file permissions, 0.17 bootstrap logging, 0.18 delete legacy file |

### Sprint 1: Consistent Hashing & Replication ✅

- **Package:** `Cluster/hashring/` — 150 virtual nodes, SHA-256, thread-safe
- **Server:** StoreData/GetData use `HashRing.GetNodes(key, N)` instead of broadcast-to-all
- **CLI:** `--replication` flag (default 3)
- **Tests:** 24.6% key movement on node add (ideal=25%), <10% distribution deviation

---

## REMAINING IMPLEMENTATION PLAN

### Target Package Structure (final)

```
Cluster/
├── hashring/        ✅ Consistent hashing ring
├── rebalance/       📋 Data migration on topology changes
├── failure/         📋 Heartbeat, Phi Accrual failure detection
├── handoff/         📋 Hinted handoff for temporarily failed nodes
├── membership/      📋 Cluster state machine (Alive/Suspect/Dead)
├── gossip/          📋 Epidemic gossip protocol
├── vclock/          📋 Vector clocks for versioning
├── quorum/          📋 Quorum reads/writes (R + W > N)
├── conflict/        📋 LWW conflict resolution
├── merkle/          📋 Merkle trees for anti-entropy sync
└── selector/        📋 Peer selection by latency/load

Observability/
├── logging/         📋 Structured logging (zerolog)
├── metrics/         📋 Prometheus metrics
├── health/          📋 /health + /metrics HTTP endpoint
└── tracing/         📋 OpenTelemetry + Jaeger

Storage/
├── storage.go       ✅ Existing CAS store + MetaFile
├── chunker/         📋 File chunking (4MB) + reassembly
└── compression/     📋 Zstandard compression

Server/
├── server.go        ✅ Core server
└── downloader/      📋 Parallel multi-source chunk downloads

Peer2Peer/
├── transport.go     ✅ Interfaces
├── tcpTransport.go  ✅ TCP implementation
├── quic/            📋 QUIC transport
├── factory/         📋 Transport factory (tcp/quic selector)
└── nat/             📋 NAT traversal (STUN/TURN)
```

---

### Sprint 2: Failure Detection & Recovery

**Why:** Currently if a node goes down, nobody knows. Data isn't re-replicated. Writes to dead nodes fail silently.

**Package: `Cluster/failure/`**
```go
type HeartbeatService struct {
    peers    map[string]*PeerHealth
    config   HeartbeatConfig         // Interval: 1s, Timeout: 5s
    onDead   func(addr string)
}
type PeerHealth struct { LastSeen time.Time; Alive bool }

type PhiAccrualDetector struct {
    intervals map[string]*slidingWindow  // per-peer arrival time intervals
    threshold float64                     // default 8.0
}
// Phi(addr) float64 — probability of failure
// IsAlive(addr) bool — phi < threshold
```
- New message: `MessageHeartbeat { From string; Timestamp int64 }`
- Server goroutine sends heartbeats every 1s; reaper goroutine checks for dead peers
- Dead peer → RemoveNode from HashRing → trigger re-replication

**Package: `Cluster/handoff/`**
```go
type HintedHandoff struct {
    hints    map[string][]Hint  // target addr → pending data
    maxHints int
}
type Hint struct { Key, TargetAddr, EncryptedKey string; Data []byte; CreatedAt time.Time }
```
- When StoreData target is unreachable → store Hint locally
- When peer reconnects (OnPeer) → deliver pending hints
- Ensures writes succeed even during partial failures

**Read Repair** (in `Server/server.go`):
- When GetData fetches from peers and some replicas are missing → async send to repair

**Depends on:** Sprint 1 (hash ring)
**Enables:** Automatic failure recovery, high availability during partial outages

---

### Sprint 3: Gossip & Cluster Membership

**Why:** Currently peer discovery is static (--peer flags). No way to discover new nodes or learn about cluster state.

**Package: `Cluster/membership/`**
```go
type NodeState int // Alive, Suspect, Dead, Left

type NodeInfo struct {
    Addr        string
    State       NodeState
    Generation  uint64      // monotonic, incremented on state change
    LastUpdated time.Time
    Metadata    map[string]string  // capacity, region, etc.
}

type ClusterState struct {
    self     string
    nodes    map[string]*NodeInfo
    onChange []func(addr string, old, new NodeState)
}
```

**Package: `Cluster/gossip/`**
```go
type GossipService struct {
    config    GossipConfig  // FanOut: 3, Interval: 200ms, SuspectTimeout: 5s
    cluster   *ClusterState
}
type GossipDigest struct { Addr string; State NodeState; Generation uint64 }
```
- Every 200ms: pick 3 random peers, send state digests
- Receiver compares, merges (highest generation wins), replies with updates
- Replaces heartbeat as primary health carrier (gossip carries liveness info)
- New messages: `MessageGossipDigest`, `MessageGossipResponse`

**Depends on:** Sprint 2 (failure detection provides health data for gossip)
**Enables:** Dynamic cluster membership, automatic node discovery

---

### Sprint 4: Consistency & Conflict Resolution

**Why:** Currently no versioning. If two nodes write the same key, data is silently lost. No way to detect or resolve conflicts.

**Package: `Cluster/vclock/`**
```go
type VectorClock map[string]uint64  // nodeAddr → logical timestamp
// Methods: Increment, Merge, Compare → Before/After/Concurrent/Equal, Copy
```
- **FileMeta extended:** `VClock map[string]uint64`, `Timestamp int64`
- Every write increments the local node's clock entry

**Package: `Cluster/quorum/`**
```go
type QuorumConfig struct { N, W, R int }  // R + W > N guaranteed
type QuorumCoordinator struct { config QuorumConfig; hashRing *HashRing; cluster *ClusterState }
// Write: send to N replicas, wait for W acks → success
// Read: query R replicas, return latest by vector clock
```
- Default: N=3, W=2, R=2 (strong consistency: 2+2 > 3)
- New messages: `MessageWriteAck`, `MessageReadResponse` (include VClock)

**Package: `Cluster/conflict/`**
```go
type ConflictResolver interface { Resolve(versions []VersionedValue) VersionedValue }
type LWWResolver struct{}  // vector clock comparison → if concurrent, wall-clock wins
```

**Package: `Cluster/merkle/`**
```go
type MerkleTree struct { root *MerkleNode; leaves []*MerkleNode }
type MerkleNode struct { Hash [32]byte; Left, Right *MerkleNode; KeyRange [2]string }
// BuildMerkleTree(keys, hashFunc) → *MerkleTree
// Diff(other) → []string (divergent keys)
```
- Background goroutine every 10 min: build tree of local keys, exchange root hashes with replicas, drill down to find and sync divergent keys
- New messages: `MessageMerkleSync`, `MessageMerkleDiff`

**Depends on:** Sprint 3 (cluster state needed to know which replicas to compare)
**Enables:** Strong consistency guarantees, automatic self-healing

---

### Sprint 5: Observability (parallelizable after Sprint 2)

**Why:** Current logging is unstructured `log.Println` everywhere. No metrics, no health checks, no tracing.

**Package: `Observability/logging/`**
- **Dep:** `github.com/rs/zerolog`
- Replace all `log.Println/Printf` with structured JSON logging
- Log levels: debug, info, warn, error

**Package: `Observability/metrics/`**
- **Dep:** `github.com/prometheus/client_golang`
- Counters: `dfs_store_ops_total{operation,status}`, `dfs_replication_ops_total`
- Histograms: `dfs_store_duration_seconds{operation}`, `dfs_gossip_round_duration_seconds`
- Gauges: `dfs_peer_count`, `dfs_ring_size`

**Package: `Observability/health/`**
- HTTP server (port = TCP port + 1000) serving `/health`, `/metrics`, `/debug/pprof`
- Health response: `{status, node, peers, uptime, ring_size}`

**Package: `Observability/tracing/`**
- **Dep:** `go.opentelemetry.io/otel` + Jaeger exporter
- Trace context propagated in RPC messages
- Spans: StoreData, GetData, replication, gossip rounds

**Depends on:** Nothing (can start after Sprint 2)
**Enables:** Production-ready debugging and monitoring

---

### Sprint 6: File Chunking & Integrity

**Why:** Large video/audio files (GBs) shouldn't be stored as single blobs. Chunking enables deduplication, parallel downloads, resumable transfers, and per-chunk integrity verification.

**Package: `Storage/chunker/`**
```go
const DefaultChunkSize = 4 * 1024 * 1024  // 4MB

type Chunk struct { Index int; Hash [32]byte; Size int64; Data []byte }
type ChunkManifest struct {
    FileKey    string
    TotalSize  int64
    ChunkSize  int
    Chunks     []ChunkInfo
    MerkleRoot [32]byte
    Compressed bool
}
type ChunkInfo struct { Index int; Hash string; Size int64; NodeAddr string }
```
- `ChunkFile(reader, chunkSize) → []Chunk`
- `ReassembleChunks(chunks, writer) → error`
- Each chunk stored via CAS (SHA-256 hash as key) → natural deduplication
- Reuses `Cluster/merkle/` for per-file integrity tree

**FileMeta extended:** `Chunked bool`, `Manifest *ChunkManifest`

**StoreData new flow:**
1. Chunk the file → []Chunk
2. For each chunk: encrypt → store locally → hash ring → replicate to N nodes
3. Build ChunkManifest with MerkleRoot → store as metadata

**GetData new flow:**
1. Fetch ChunkManifest from metadata
2. For each chunk: fetch from responsible node(s) → verify hash → collect
3. Reassemble chunks → decrypt → return

**Depends on:** Sprint 4 (quorum for chunk writes, merkle for integrity)
**Enables:** Multi-source downloads, dedup, resumable transfers

---

### Sprint 7: Multi-Source Downloads & Compression

**Why:** For a college platform with large lecture videos, downloading from a single node is slow. Parallel chunk fetching from multiple sources = faster downloads. Compression saves storage and bandwidth for text-heavy content (PDFs, docs).

**Package: `Server/downloader/`**
```go
type DownloadManager struct {
    hashRing    *hashring.HashRing
    cluster     *membership.ClusterState
    maxParallel int
    bandwidthLimit int64  // bytes/sec, 0 = unlimited
}
// Download(manifest, writer) → error
// DownloadWithProgress(manifest, writer, progressFn) → error
```
- Worker pool: `maxParallel` goroutines fetch chunks concurrently
- Priority queue reassembles chunks in order
- Resume: track completed chunks, only fetch missing ones

**Package: `Cluster/selector/`**
```go
type PeerSelector struct {
    latencies map[string]*ewma     // exponentially weighted moving average
    loads     map[string]int32     // active downloads per peer
}
// RecordLatency(addr, duration)
// BestPeer(candidates []string) string  // lowest latency × load score
```

**Package: `Storage/compression/`**
```go
// Dep: github.com/klauspost/compress/zstd
func Compress(src io.Reader, dst io.Writer, level CompressionLevel) (int64, error)
func Decompress(src io.Reader, dst io.Writer) (int64, error)
func ShouldCompress(header []byte) bool  // entropy check on first 4KB
```
- Applied per-chunk BEFORE encryption (compress → encrypt → store)
- Skip for already-compressed formats (JPEG, MP4, ZIP)
- `ChunkManifest.Compressed` tracks whether compression was applied

**Depends on:** Sprint 6 (chunking)
**Enables:** Fast parallel downloads, storage savings

---

### Sprint 8: Advanced Networking (parallelizable after Sprint 3)

**Why:** TCP has head-of-line blocking. QUIC gives multiplexing, 0-RTT connections, and better mobile/lossy network performance. NAT traversal enables true peer-to-peer across campus networks.

**Package: `Peer2Peer/quic/`**
```go
// Dep: github.com/quic-go/quic-go
type QUICPeer struct { session quic.Connection; stream quic.Stream; outbound bool; Wg *sync.WaitGroup }
type QUICTransport struct { QUICTransportOpts; listener *quic.Listener; rpcch chan RPC }
// Implements Peer and Transport interfaces — drop-in replacement for TCP
```
- Each RPC gets its own QUIC stream → no head-of-line blocking
- Built-in TLS 1.3, 0-RTT reconnection
- Connection survives IP changes (WiFi ↔ mobile data)

**Package: `Peer2Peer/factory/`**
```go
type TransportType string  // "tcp" or "quic"
func NewTransport(typ TransportType, opts interface{}) (Transport, error)
```
- CLI: `--transport=quic|tcp`

**Package: `Peer2Peer/nat/`**
```go
type NATTraversal struct { stunServer, turnServer, publicAddr string }
// DiscoverPublicAddr() (string, error)
// PunchHole(targetAddr string) (net.Conn, error)
```
- STUN for public address discovery
- UDP hole punching for direct P2P
- TURN relay as fallback

**Depends on:** Sprint 3 (gossip carries public address info)
**Enables:** Lower latency, cross-network P2P, mobile support

---

## How Everything Orchestrates Together

### System Architecture (final state)

```
┌─────────────────────────────────────────────────────────┐
│                    CLI / Web API                         │
│   dfs start / upload / download                         │
└─────────────┬───────────────────────────────────────────┘
              │ Unix socket (JSON)
┌─────────────▼───────────────────────────────────────────┐
│                     Server                               │
│                                                          │
│  StoreData ─────────────────────── GetData               │
│     │                                  │                 │
│     ├─ Compress (skip if binary)       ├─ Fetch manifest │
│     ├─ Chunk (4MB pieces)              ├─ Parallel fetch │
│     ├─ Encrypt (per-chunk AES-GCM)     │   from N nodes  │
│     ├─ HashRing.GetNodes(key, N)       ├─ Verify Merkle  │
│     ├─ Quorum write (wait for W acks)  ├─ Reassemble     │
│     ├─ If target down → Hinted Handoff ├─ Decrypt        │
│     └─ Update VectorClock              ├─ Decompress     │
│                                        └─ Read repair    │
│                                          (async)         │
└────────┬────────────────────────────────────┬────────────┘
         │                                    │
┌────────▼────────┐                ┌──────────▼────────────┐
│   Cluster Layer  │                │   Storage Layer       │
│                  │                │                       │
│  HashRing        │                │  CAS Store (SHA-256   │
│  Quorum (R+W>N)  │                │    path + MD5 file)   │
│  VectorClocks    │                │  Chunker (4MB)        │
│  LWW Conflict    │                │  Compression (zstd)   │
│  Merkle Sync     │                │  MetaFile (JSON)      │
│  Gossip Protocol │                │    + VClock           │
│  Phi Accrual     │                │    + ChunkManifest    │
│  Hinted Handoff  │                │                       │
│  Peer Selector   │                │                       │
│  Rebalancer      │                │                       │
└────────┬────────┘                └───────────────────────┘
         │
┌────────▼────────────────────────────────────────────────┐
│                  Transport Layer                         │
│                                                          │
│  TCP (current) ──or── QUIC (future)                     │
│  + mTLS (optional)    + built-in TLS 1.3                │
│  + OnPeer/OnPeerDisconnect callbacks                    │
│  + NAT traversal (STUN/TURN)                            │
│                                                          │
│  Wire protocol: [control byte][gob message or stream]   │
└─────────────────────────────────────────────────────────┘
         │
┌────────▼────────────────────────────────────────────────┐
│                  Observability                           │
│                                                          │
│  Structured logs (zerolog) → JSON                       │
│  Prometheus metrics → /metrics endpoint                 │
│  Health check → /health endpoint                        │
│  Distributed tracing → Jaeger                           │
└─────────────────────────────────────────────────────────┘
```

### Lifecycle of a File Upload (final state)

```
1. User: dfs upload --key "lecture.mp4" --file ./lecture.mp4
2. CLI reads file, base64 encodes, sends JSON to Unix socket
3. Server.StoreData("lecture.mp4", reader):
   a. ShouldCompress(header)? → skip (MP4 already compressed)
   b. ChunkFile(reader, 4MB) → [chunk0, chunk1, ..., chunkN]
   c. For each chunk:
      - EncryptStream(chunkData, tempFile) → encryptedKey
      - Store.WriteStream(chunkHash, tempFile) → CAS disk
   d. BuildFileMerkleTree(chunks) → root hash
   e. Create ChunkManifest{chunks, merkleRoot, compressed=false}
   f. VectorClock.Increment(selfAddr)
   g. For each chunk:
      - HashRing.GetNodes(chunkHash, N=3) → [nodeA, nodeB, nodeC]
      - PeerSelector.BestPeer → order by latency
      - QuorumCoordinator.Write(chunkHash, data, W=2)
        → send to 3 nodes, wait for 2 acks
        → if nodeC is down: HintedHandoff.Store(hint)
   h. Store manifest in metadata with VClock
4. Response: "uploaded\n"
```

### Lifecycle of a File Download (final state)

```
1. User: dfs download --key "lecture.mp4" --output ./lecture.mp4
2. CLI sends JSON to Unix socket
3. Server.GetData("lecture.mp4"):
   a. Check local metadata for ChunkManifest
   b. If local and all chunks present:
      - Read chunks, verify Merkle hashes
      - Reassemble → Decrypt → Decompress → return
   c. If remote:
      - HashRing.GetNodes("lecture.mp4", N=3)
      - QuorumCoordinator.Read(R=2) → get manifest from 2 nodes
      - ConflictResolver.Resolve(versions) → pick latest by VClock/LWW
      - DownloadManager.DownloadWithProgress(manifest, writer, progressFn):
        For each chunk in manifest:
          - PeerSelector.BestPeer(chunk.candidates) → fastest node
          - Fetch chunk (up to maxParallel=4 concurrent)
          - Verify chunk hash against Merkle tree
          - If hash mismatch → retry from different node
      - Reassemble → Decrypt → Decompress → return
      - Read repair: async send missing chunks to stale replicas
4. Response: file bytes
```

---

## Dependency Graph & Execution Order

```
Sprint 0 (bugs)              ✅ DONE
    ↓
Sprint 1 (consistent hashing) ✅ DONE
    ↓
Sprint 2 (failure detection)  📋 NEXT
    │   ├── Cluster/failure/   — heartbeat + phi accrual
    │   ├── Cluster/handoff/   — hinted handoff
    │   └── Server/server.go   — read repair
    │
    ├──→ Sprint 5 (observability) 📋 CAN START IN PARALLEL
    │       ├── Observability/logging/
    │       ├── Observability/metrics/
    │       ├── Observability/health/
    │       └── Observability/tracing/
    ↓
Sprint 3 (gossip/membership)   📋
    │   ├── Cluster/membership/ — cluster state machine
    │   └── Cluster/gossip/     — epidemic gossip protocol
    │
    ├──→ Sprint 8 (networking) 📋 CAN START IN PARALLEL
    │       ├── Peer2Peer/quic/
    │       ├── Peer2Peer/factory/
    │       └── Peer2Peer/nat/
    ↓
Sprint 4 (consistency)         📋
    │   ├── Cluster/vclock/    — vector clocks
    │   ├── Cluster/quorum/    — quorum R/W
    │   ├── Cluster/conflict/  — LWW resolution
    │   └── Cluster/merkle/    — anti-entropy sync
    ↓
Sprint 6 (chunking)            📋
    │   ├── Storage/chunker/   — file chunking + reassembly
    │   └── Cluster/merkle/    — reused for per-file integrity
    ↓
Sprint 7 (downloads + compression) 📋
        ├── Server/downloader/ — parallel multi-source
        ├── Cluster/selector/  — peer selection
        └── Storage/compression/ — zstd compression
```

---

## New External Dependencies (to be added)

| Sprint | Package | Why |
|--------|---------|-----|
| 5 | `github.com/rs/zerolog` | Structured logging, zero-allocation |
| 5 | `github.com/prometheus/client_golang` | Metrics (industry standard) |
| 5 | `go.opentelemetry.io/otel` + Jaeger | Distributed tracing |
| 7 | `github.com/klauspost/compress/zstd` | Best Go zstd implementation |
| 8 | `github.com/quic-go/quic-go` | QUIC transport |
| 8 | `github.com/hashicorp/yamux` (optional) | TCP multiplexing fallback |

---

## Files Summary

### Created (Sprint 0+1)
| File | Purpose |
|------|---------|
| `Cluster/hashring/hashring.go` | Consistent hash ring with virtual nodes |
| `Cluster/hashring/hashring_test.go` | 12 tests + benchmark |

### Modified (Sprint 0+1)
| File | Key Changes |
|------|-------------|
| `Server/server.go` | HashRing integration, targeted replication, RWMutex, peer disconnect, local decrypt, metadata propagation, error resilience |
| `Peer2Peer/transport.go` | Exported `ListenAndAccept()` |
| `Peer2Peer/tcpTransport.go` | `OnPeerDisconnect` callback, stream RPC From field |
| `Peer2Peer/encoding.go` | 4096-byte decoder buffer |
| `Storage/storage.go` | File permissions 0600 |
| `cmd/handle_client.go` | io.ReadAll, base64 decode, error handling |
| `cmd/handle_upload.go` | base64 encode |
| `cmd/start.go` | `--replication` flag |
| `cmd/daemon.go` | Pass replication factor |

### Deleted (Sprint 0)
| File | Reason |
|------|--------|
| `metadata.go` | Legacy unused code, superseded by `Storage.MetaFile` |

### Planned (Sprint 2-8): ~25 new files across 15 new packages

---

## What's Missing? (Cross-check)

Everything from the original Phase 2+3 plan is accounted for:

| Feature | Sprint | Status |
|---------|--------|--------|
| Consistent hashing + virtual nodes | 1 | ✅ Done |
| Replication factor (configurable) | 1 | ✅ Done |
| Node join/leave rebalancing | 2 | 📋 Planned (Cluster/rebalance/) |
| Heartbeat + Phi Accrual detector | 2 | 📋 Planned |
| Hinted handoff | 2 | 📋 Planned |
| Read repair | 2 | 📋 Planned |
| Gossip protocol | 3 | 📋 Planned |
| Cluster membership (state machine) | 3 | 📋 Planned |
| Vector clocks | 4 | 📋 Planned |
| Quorum reads/writes (R+W>N) | 4 | 📋 Planned |
| LWW conflict resolution | 4 | 📋 Planned |
| Anti-entropy (Merkle tree sync) | 4 | 📋 Planned |
| Structured logging | 5 | 📋 Planned |
| Prometheus metrics | 5 | 📋 Planned |
| Health check endpoint | 5 | 📋 Planned |
| Distributed tracing | 5 | 📋 Planned |
| File chunking (4MB) | 6 | 📋 Planned |
| Per-file Merkle tree | 6 | 📋 Planned |
| Content deduplication | 6 | 📋 Planned (natural via CAS) |
| Multi-source parallel downloads | 7 | 📋 Planned |
| Resume partial downloads | 7 | 📋 Planned |
| Bandwidth throttling | 7 | 📋 Planned |
| Progress reporting | 7 | 📋 Planned |
| Zstandard compression | 7 | 📋 Planned |
| Adaptive compression | 7 | 📋 Planned |
| QUIC transport | 8 | 📋 Planned |
| Connection multiplexing | 8 | 📋 Planned |
| NAT traversal (STUN/TURN) | 8 | 📋 Planned |

**Additional items NOT in original plan but worth considering:**
- **Rebalancer** (Sprint 2): data migration when topology changes — listed above
- **Raft consensus** (optional): for metadata coordination if needed — deferred, gossip + quorum sufficient for file storage
- **libp2p integration** (optional): deferred — custom transport is more educational and lighter
- **Rate limiting / QoS**: important for a college network to prevent single user hogging bandwidth — could add to Sprint 7
- **Access control / user auth**: needed before web app — separate concern, not part of distributed systems core
- **File metadata search / indexing**: users need to find resources — separate concern, likely Elasticsearch or SQLite index
