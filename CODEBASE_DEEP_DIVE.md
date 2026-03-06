# DFS-Go — Complete Codebase Deep Dive

> A distributed file system written in Go, targeting college-LAN deployments.
> This document traces every subsystem, data flow, and design decision in the codebase.

---

## Table of Contents

1. [Architecture Overview](#1-architecture-overview)
2. [Entry Point & CLI Layer](#2-entry-point--cli-layer)
3. [Daemon Bootstrap & Server Construction](#3-daemon-bootstrap--server-construction)
4. [Peer-to-Peer Transport Layer](#4-peer-to-peer-transport-layer)
5. [Storage Engine](#5-storage-engine)
6. [File Chunking](#6-file-chunking)
7. [Compression](#7-compression)
8. [Encryption & Cryptographic Identity](#8-encryption--cryptographic-identity)
9. [File Upload Flow (End-to-End)](#9-file-upload-flow-end-to-end)
10. [File Download Flow (End-to-End)](#10-file-download-flow-end-to-end)
11. [Consistent Hashing & Data Placement](#11-consistent-hashing--data-placement)
12. [Replication](#12-replication)
13. [Cluster Membership & Gossip](#13-cluster-membership--gossip)
14. [Failure Detection](#14-failure-detection)
15. [Hinted Handoff](#15-hinted-handoff)
16. [Quorum Reads & Writes](#16-quorum-reads--writes)
17. [Conflict Resolution & Vector Clocks](#17-conflict-resolution--vector-clocks)
18. [Anti-Entropy (Merkle Tree Sync)](#18-anti-entropy-merkle-tree-sync)
19. [Rebalancing](#19-rebalancing)
20. [Parallel Downloads & Peer Selection](#20-parallel-downloads--peer-selection)
21. [Observability](#21-observability)
22. [TLS & Mutual Authentication](#22-tls--mutual-authentication)
23. [NAT Traversal](#23-nat-traversal)
24. [IPC Protocol (CLI ↔ Daemon)](#24-ipc-protocol-cli--daemon)
25. [ECDH File Sharing Flow](#25-ecdh-file-sharing-flow)
26. [Server Message Loop & Wire Protocol](#26-server-message-loop--wire-protocol)
27. [Graceful Shutdown](#27-graceful-shutdown)
28. [Package Dependency Graph](#28-package-dependency-graph)

---

## 1. Architecture Overview

DFS-Go is structured as a **single-binary daemon** (`dfs start`) that participates in a peer-to-peer cluster, combined with a CLI client (`dfs upload`, `dfs download`) that talks to the local daemon over a **Unix domain socket**.

```
┌──────────────────────────────────────────────────────────────┐
│  CLI (dfs upload/download/identity)                         │
│     ↕  Unix socket IPC (/tmp/dfs-<port>.sock)               │
├──────────────────────────────────────────────────────────────┤
│  Daemon  (Server)                                           │
│  ┌────────────┐ ┌──────────┐ ┌──────────┐ ┌──────────────┐  │
│  │  Storage    │ │  Crypto  │ │  Chunker │ │ Compression  │  │
│  │  (CAS +    │ │  (AES +  │ │  (4 MiB  │ │  (zstd)      │  │
│  │   BoltDB)  │ │  ECDH)   │ │  splits) │ │              │  │
│  └────────────┘ └──────────┘ └──────────┘ └──────────────┘  │
│  ┌────────────────────────────────────────────────────────┐  │
│  │ Cluster Layer                                          │  │
│  │  HashRing · Gossip · Membership · Failure Detection    │  │
│  │  Quorum · Handoff · Rebalance · Merkle Anti-Entropy    │  │
│  │  Vector Clocks · Conflict Resolution · Selector        │  │
│  └────────────────────────────────────────────────────────┘  │
│  ┌────────────────────────────────────────────────────────┐  │
│  │ Transport: QUIC (default) or TCP — both with mTLS      │  │
│  └────────────────────────────────────────────────────────┘  │
│  ┌────────────────────────────────────────────────────────┐  │
│  │ Observability: Prometheus · Health HTTP · OTEL Tracing │  │
│  └────────────────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────────────────┘
```

**Key design principles:**

- **Closure-based dependency injection** — no circular imports. `Server` creates all subsystems and wires them together via Go function closures.
- **Content-addressed storage (CAS)** — files are addressed by their hash, enabling natural deduplication.
- **Streaming pipeline** — data flows through chunk → compress → encrypt → store → replicate in a streaming fashion, minimising memory pressure.
- **Quorum consistency** — configurable W/R quorum levels with majority-based defaults.

---

## 2. Entry Point & CLI Layer

### `main.go`

A single line: `cmd.Execute()` — dispatches to the Cobra CLI framework.

### `cmd/root.go`

Registers the root `dfs` command. All subcommands are added in their respective files.

### `cmd/start.go` — `dfs start`

Launches the daemon with flags:

| Flag            | Default  | Description                          |
|-----------------|----------|--------------------------------------|
| `--port`        | `:3000`  | Listen address for peer connections  |
| `--peer`        | (none)   | Comma-separated bootstrap peer addrs |
| `--replication` | `3`      | Replication factor (N)               |

Calls `StartDaemon(port, peers, replicationFactor)`.

### `cmd/identity.go` — `dfs identity`

- `dfs identity init --alias <name>` — generates an Ed25519 signing key + X25519 key-agreement key, saves to `~/.dfs/identity.json`.
- `dfs identity show` — prints alias, fingerprint (first 16 hex chars of SHA-256 of Ed25519 pub), and public keys.

### `cmd/upload.go` — `dfs upload`

```
dfs upload --name <key> --file <path> [--share-with <alias1,alias2>] [--share-with-key <pubkey>]
```

**Plaintext path** (no `--share-with`):
1. Loads identity to derive the storage namespace: `<fingerprint>/<name>`.
2. Connects to the daemon's Unix socket.
3. Sends IPC opcode `0x01` (Upload) with key + file size + raw bytes.

**ECDH encrypted path** (`--share-with` specified):
1. Generates a random 32-byte DEK (Data Encryption Key).
2. Wraps DEK for self via `envelope.WrapDEKForRecipient(senderPriv, selfPub, dek)`.
3. For each `--share-with` alias: resolves to X25519 public key via IPC opcode `0x06` (Resolve Alias), then wraps DEK for that recipient.
4. Signs the manifest fields (canonical JSON payload) with Ed25519 via `envelope.SignManifest`.
5. Sends IPC opcode `0x03` (ECDH Upload) with the full envelope: DEK, owner pub keys, access list, signature, then file bytes.

### `cmd/download.go` — `dfs download`

```
dfs download --name <key> [--from <alias>] [--from-key <fingerprint>] [--output <path>]
```

1. Determines the owner fingerprint — own identity if not specified, or resolved via `--from` alias.
2. Constructs storage key: `<ownerFingerprint>/<name>`.
3. Fetches manifest metadata via IPC opcode `0x05` (Get Manifest) to check if the file is ECDH-encrypted.
4. **If encrypted:** Finds own `AccessEntry` in the manifest's access list, calls `envelope.UnwrapDEK` with own X25519 private key and the owner's X25519 pub to recover the DEK, then sends IPC opcode `0x04` (ECDH Download) with the raw DEK.
5. **If plaintext:** Sends IPC opcode `0x02` (Download) with just the key.
6. Streams received data to the output file (or stdout).

---

## 3. Daemon Bootstrap & Server Construction

### `cmd/daemon.go` — `StartDaemon()`

Sequential bootstrap:

1. **Structured logging** — `logging.Init("server", logging.LevelInfo)` (zerolog-based JSON logs).
2. **Distributed tracing** — `tracing.Init("dfs", endpoint)` with OTLP/HTTP exporter (only if `OTEL_EXPORTER_OTLP_ENDPOINT` is set).
3. **Unix socket creation** — at `/tmp/dfs-<port>.sock` for IPC.
4. **Identity loading** — reads `~/.dfs/identity.json` (or `DFS_IDENTITY_PATH` env var). Identity provides:
   - Ed25519 keypair (manifest signing, fingerprint identity)
   - X25519 keypair (ECDH key agreement for DEK wrapping)
   - Alias (human-readable name, gossip-searchable)
5. **Server construction** — `Server.MakeServer(port, replicationFactor, identityMeta, peers...)`.
6. **Signal handling** — intercepts SIGTERM/SIGINT → `server.GracefulShutdown()`.
7. **Server start** — `server.Start()` binds the transport port and begins accepting connections.
8. **IPC loop** — accepts Unix socket connections, dispatches each to `HandleClient`.

### `Server/server.go` — `MakeServer()`

Constructs and wires **all** subsystems:

```
MakeServer(listenAddr, replFactor, makeOpts, bootstrapNodes...)
  │
  ├─ logging.Init() + metrics.Init()
  ├─ godotenv.Load() — loads .env for configuration
  │
  ├─ TLS setup (if DFS_ENABLE_TLS=true):
  │   ├─ Crypto.LoadOrGenerateCA() — CA cert at .certs/ca.crt
  │   ├─ Crypto.LoadOrGenerateNodeCert() — node cert signed by CA
  │   └─ Crypto.LoadMTLSConfig() — full mTLS config
  │
  ├─ factory.NewFromEnv() — creates QUIC or TCP transport
  │   (controlled by DFS_TRANSPORT env var, default QUIC)
  │
  ├─ storage.NewBoltMetaStore() — bbolt-backed metadata DB
  ├─ NewServer(opts) — creates Server struct with:
  │   ├─ storage.NewStore(CASPathTransformFunc)
  │   └─ hashring.New(config)
  │
  ├─ failure.NewHeartbeatService() — phi accrual failure detector
  ├─ handoff.NewHandoffService() — hinted handoff + disk store
  ├─ rebalance.New() — data migration engine
  ├─ quorum.New() — W/R quorum coordinator
  ├─ merkle.NewAntiEntropyService() — periodic Merkle sync
  ├─ selector.New() — EWMA latency tracker
  ├─ downloader.New() — parallel chunk fetcher
  ├─ health.New() — HTTP health + metrics + pprof server
  ├─ membership.New() — cluster state machine
  └─ gossip.New() — epidemic dissemination
```

Each subsystem receives its dependencies as **closures** that capture the `Server` pointer, avoiding circular imports.

---

## 4. Peer-to-Peer Transport Layer

### Transport Interface (`Peer2Peer/transport.go`)

```go
type Peer interface {
    net.Conn
    Send([]byte) error
    SendMsg(controlByte byte, payload []byte) error
    SendStream(msgPayload []byte, streamData []byte) error
    CloseStream()
    Outbound() bool
}

type Transport interface {
    Addr() string
    ListenAndAccept() error
    Consume() <-chan RPC
    Close() error
    Dial(string) error
}
```

### Message Types (`Peer2Peer/message.go`)

Three control byte values define the wire protocol:

| Control Byte | Value  | Description                                          |
|-------------|--------|------------------------------------------------------|
| `IncomingMessage`           | `0x01` | Regular gob-encoded message                         |
| `IncomingStream`            | `0x02` | Raw byte stream (legacy)                            |
| `IncomingMessageWithStream` | `0x03` | Framed message followed by raw stream data          |

**RPC struct:**
```go
type RPC struct {
    From         net.Addr
    Peer         Peer
    Payload      []byte
    Stream       bool
    StreamWg     *sync.WaitGroup  // call Done() when finished reading stream
    StreamReader io.Reader        // source for raw stream bytes
}
```

### Framing (`Peer2Peer/encoding.go`)

`DefaultDecoder` reads length-prefixed frames:

```
[4 bytes big-endian length][payload bytes]
```

A 64 MiB sanity cap prevents accidental OOM on malformed frames.

### TCP Transport (`Peer2Peer/tcpTransport.go`)

- **Listener:** optional TLS via `tls.Listen` or plain `net.Listen`.
- **Connection handling:** `handleConn` → handshake → OnPeer callback → read loop.
- **Read loop:** reads 1-byte control, then dispatches:
  - `0x01` → decode framed message → send on `rpcch`
  - `0x02` → standalone stream signal, StreamReader = conn
  - `0x03` → decode framed message + stream, StreamReader = conn
- **Send serialization:** `sendMu` mutex + `net.Buffers` writev syscall prevents interleaving between concurrent senders on the same TCP connection.
- **SendStream:** atomically writes `[0x03][4B len][msgPayload][streamData]` in one locked section.

### QUIC Transport (`Peer2Peer/quic/transport.go`)

Drop-in replacement for TCP with significant advantages:

- **Stream multiplexing:** each `SendMsg`/`SendStream` opens a new QUIC stream. No head-of-line blocking between parallel messages.
- **No send mutex needed:** `quic.Conn.OpenStreamSync` is inherently safe for concurrent use.
- **Built-in TLS 1.3:** generates a self-signed cert if none provided.
- **Flow-control tuning:** windows sized for 4 MiB chunk transfers:
  - `InitialStreamReceiveWindow` = 8 MiB
  - `InitialConnectionReceiveWindow` = 32 MiB

**QUIC stream lifecycle:** Open → write all bytes in single Write call → Close. Flattening to a single write prevents partial-read splits in the decoder.

### Transport Factory (`factory/factory.go`)

Controlled by `DFS_TRANSPORT` env var:

| Value    | Transport Created | Notes |
|----------|------------------|-------|
| `quic`   | `quictransport.Transport` | Default. Better for cross-subnet. |
| `tcp`    | `TCPTransport` with NOP handshake | Fallback for debugging / compat. |

### Handshake (`Peer2Peer/handshake.go`)

Three modes:

1. **`NOPEHandshakeFunc`** — always succeeds (development only).
2. **`TLSVerifyHandshakeFunc`** — verifies peer presented a valid TLS certificate. Logs subject, issuer, fingerprint, TLS version, cipher suite.
3. **`MakeFingerprintVerifyHandshake`** — verifies peer cert SHA-256 fingerprint against an allowlist (certificate pinning without a CA).

---

## 5. Storage Engine

### `Storage/storage.go`

#### Content-Addressed Storage (CAS)

Files are stored on disk using their hash as the filename:

```go
func CASPathTransformFunc(key string) PathKey {
    hash := sha256.Sum256([]byte(key))
    hashStr := hex.EncodeToString(hash[:])
    // Split hash into 5-char directory segments for filesystem efficiency:
    // "abc12/def34/..."
    paths := make([]string, sliceLen)
    for i := 0; i < sliceLen; i++ {
        paths[i] = hashStr[from:to]
    }
    return PathKey{pathname: strings.Join(paths, "/"), filename: hashStr}
}
```

#### WriteStream — Atomic Write

1. Creates directory tree under `<root>/<CAS path>`.
2. Creates a temporary file in the target directory.
3. Streams data through `io.TeeReader`: writes to both the temp file and an MD5 hasher simultaneously.
4. After all bytes are written, computes MD5 hash → filename.
5. Atomically `os.Rename`s temp file to final content-addressed path.
6. Updates metadata with the final path.

This ensures no partial files are visible to readers.

#### ReadStream

Looks up the file path from metadata, opens it with `os.Open`, returns `(size, io.ReadCloser, error)`. **Caller must close the reader.**

#### Metadata Stores

Two implementations of the `MetadataStore` interface:

**`MetaFile`** — JSON file-backed (legacy):
- In-memory `map[string]FileMeta` + `sync.Mutex`.
- Every `Set()` rewrites the entire JSON file (O(N)).
- Fine for small deployments / tests.

**`BoltMetaStore`** — bbolt (etcd's embedded DB):
- Two buckets: `"filemeta"` and `"manifests"`.
- Each `Set()` writes only the affected key (O(1)).
- WAL-based crash safety.
- `Keys()` method for rebalancer enumeration.

**`FileMeta` struct:**
```go
type FileMeta struct {
    Path      string                   // disk path to the file
    VClock    map[string]uint64        // vector clock at time of write
    Timestamp int64                    // UnixNano wall clock (LWW tiebreaker)
    Chunked   bool                     // true when file was split into chunks
    Manifest  *chunker.ChunkManifest   // non-nil iff Chunked == true
}
```

---

## 6. File Chunking

### `Storage/chunker/chunker.go`

Files are split into 4 MiB chunks for distributed storage:

```
const DefaultChunkSize = 4 * 1024 * 1024  // 4 MiB
```

#### ChunkReader — Streaming Producer

Runs in a goroutine, reads from `io.Reader` in `chunkSize`-byte increments:

```go
func ChunkReaderWithPool(r io.Reader, chunkSize int, pool *sync.Pool) (<-chan Chunk, <-chan error)
```

- Each `Chunk` has `Index`, `Hash` (SHA-256 of plaintext), `Size`, `Data`.
- Uses `io.ReadFull` — the last chunk is smaller (EOF / ErrUnexpectedEOF is normal).
- `pool` parameter eliminates a 4 MiB allocation per chunk (reuses read buffer).
- Output channel buffered to 4 for producer-consumer pipelining.

#### Reassemble

Sorts chunks by `Index`, verifies no gaps, writes sequentially to `dst io.Writer`.

#### Content Addressing

Each chunk gets two hashes:
- **Plaintext hash** (`ChunkInfo.Hash`) — SHA-256 of raw data. Used for integrity verification after decryption.
- **Encrypted hash** (`ChunkInfo.EncHash`) — SHA-256 of the encrypted bytes. Used as the CAS storage key via `ChunkStorageKey("chunk:" + encHash)`.

#### ChunkManifest

The table of contents for a chunked file:

```go
type ChunkManifest struct {
    FileKey    string        // original file key
    TotalSize  int64         // sum of all plaintext chunk sizes
    ChunkSize  int           // nominal chunk size (4 MiB)
    Chunks     []ChunkInfo   // ordered list of chunks
    MerkleRoot string        // SHA-256 Merkle root over plaintext chunk hashes
    CreatedAt  int64         // UnixNano
    Encrypted  bool          // true if ECDH-encrypted

    // ECDH sharing fields:
    OwnerPubKey   string        // hex X25519 pub of uploader
    OwnerEdPubKey string        // hex Ed25519 pub of uploader
    AccessList    []AccessEntry // one entry per recipient (wrapped DEKs)
    Signature     string        // hex Ed25519 signature
}
```

Stored under `"manifest:<fileKey>"` in the metadata store.

#### Merkle Root

`BuildManifest` computes a Merkle root over the ordered plaintext chunk hashes:

1. Leaf layer: decode hex hashes into `[32]byte`.
2. Build tree bottom-up: each pair of nodes → `SHA-256(left || right)`.
3. Odd leaf count → duplicate the last node.
4. Single root hash enables whole-file integrity verification.

---

## 7. Compression

### `Storage/compression/compression.go`

Uses **zstd** compression with intelligent skip logic:

#### ShouldCompress — Decision Engine

```go
func ShouldCompress(data []byte) bool
```

Checks in order:
1. **Too small** — skip if < 1 KiB (framing overhead exceeds savings).
2. **Magic-byte detection** — recognizes already-compressed formats: JPEG, PNG, ZIP/DOCX/XLSX/PPTX, gzip, zstd, MP4/MOV, MKV/WebM.
3. **Shannon entropy** — estimates bits-per-byte on first 4 KiB sample. If ≥ 7.0 (near-random), compression won't help.

#### Compression Levels

| Level         | zstd Preset            | Use Case             |
|---------------|------------------------|----------------------|
| `LevelFastest`| `SpeedFastest`        | Real-time uploads    |
| `LevelDefault`| `SpeedDefault`        | Balanced             |
| `LevelBest`   | `SpeedBestCompression`| Batch/archive        |

#### Safety Guarantee

If compressed output ≥ input size, returns original unchanged with `wasCompressed=false`. Data never grows.

#### Pool-Aware Variant

`CompressChunkWithPool` borrows output buffer from `sync.Pool`, eliminating per-chunk allocation. Result is **copied** before returning the pool buffer.

---

## 8. Encryption & Cryptographic Identity

### `Crypto/crypto.go` — AES-256-GCM Streaming Encryption

#### Wire Format

Per-chunk encryption format:
```
[4 bytes: chunk size (LE)][12 bytes: nonce][ciphertext + 16-byte GCM auth tag]
```

#### EncryptStreamWithDEK

```go
func EncryptStreamWithDEK(src io.Reader, dst io.Writer, dek []byte) error
```

1. Creates AES-256 cipher from the 32-byte DEK.
2. Creates GCM (Galois/Counter Mode) AEAD.
3. Reads 4 MiB chunks from `src`.
4. For each chunk: **deterministic nonce** from chunk index (safe because each DEK is unique per file).
5. `gcm.Seal` produces ciphertext + 16-byte authentication tag.
6. Writes `[4B size][12B nonce][ciphertext+tag]` to `dst`.

**`EncryptStreamWithDEKPool`** — same but borrows the GCM Seal output slab from `sync.Pool`, eliminating per-chunk heap allocation.

#### DecryptStreamWithDEK

Reverse process:
1. Reads 4-byte chunk size.
2. Reads that many bytes (nonce + ciphertext).
3. **Verifies nonce matches expected** (chunk index) — detects tampering/reordering.
4. `gcm.Open` decrypts and verifies authentication tag.
5. Writes plaintext to `dst`.

### `Crypto/identity/identity.go` — Per-Node Identity

Each node has a persistent identity stored at `~/.dfs/identity.json`:

```go
type Identity struct {
    Alias       string  // human-readable name (e.g. "alice")
    Ed25519Priv []byte  // 64 bytes — signs manifests
    Ed25519Pub  []byte  // 32 bytes — verifies signatures
    X25519Priv  []byte  // 32 bytes — ECDH key agreement
    X25519Pub   []byte  // 32 bytes — shared via gossip
}
```

**Fingerprint** — `SHA-256(Ed25519Pub)[:8]` hex-encoded (16 chars). Used as the storage namespace prefix: `<fingerprint>/filename`.

**GossipMetadata** — identity publishes `{alias, x25519_pub, ed25519_pub, fingerprint}` into gossip so other nodes can discover recipients by alias and wrap DEKs for them.

### `Crypto/envelope/envelope.go` — ECDH Key Wrapping

#### WrapDEKForRecipient

```
Sender X25519 priv × Recipient X25519 pub → ECDH shared secret
       → HKDF-SHA256(shared, info="dfs-dek-wrap") → 32-byte wrapping key
       → AES-256-GCM(wrapKey, randomNonce, DEK) → wrapped DEK
```

Result stored as `AccessEntry{RecipientPubKey, WrappedDEK}` in the manifest.

#### UnwrapDEK

Recipient performs the same ECDH with their private key and the sender's public key → derives the same wrapping key → AES-GCM unwraps the DEK.

#### Manifest Signing

`SignManifest` creates a deterministic canonical JSON payload of access-control fields, SHA-256 hashes it, signs with Ed25519.

`VerifyManifest` verifies the signature, proving who authorised access.

### `Crypto/tls.go` — Certificate Infrastructure

**CA Management:**
- `GenerateCA` — creates a self-signed ECDSA P-256 CA cert (10-year validity). Saved to `.certs/ca.crt` + `.certs/ca.key`.
- `LoadOrGenerateCA` — loads existing or generates new.

**Node Certificates:**
- `GenerateNodeCert` — creates a node cert signed by the CA (1-year validity). Supports both server and client auth for mTLS. SANs include localhost and all local IPs.
- `LoadOrGenerateNodeCert` — loads existing or generates + verifies against CA.

**TLS Configs:**
- `LoadServerTLSConfig` — requires client certs (`RequireAndVerifyClientCert`).
- `LoadClientTLSConfig` — presents client cert, verifies server against CA.
- `LoadMTLSConfig` — combined for P2P (each node is both server and client). Enforces mTLS with CA verification.
- `LoadTLSConfigInsecure` — development only, `InsecureSkipVerify`.

---

## 9. File Upload Flow (End-to-End)

### Plaintext Upload

```
CLI (dfs upload --name foo --file bar.pdf)
  │
  ├─ 1. Load identity → fingerprint "a1b2c3d4..."
  ├─ 2. Storage key = "a1b2c3d4.../foo"
  ├─ 3. Connect to /tmp/dfs-:3000.sock
  ├─ 4. Send IPC: [opcode=0x01][keyLen][key][fileSize][file bytes]
  │
  ↓ Unix socket
  │
  HandleClient → handleUpload
  │
  ├─ 5. Read IPC header → key="a1b2c3d4.../foo", size=N
  └─ 6. Call server.StoreData(key, reader, nil)
         │
         ↓ Server.StoreData()
         │
         ├─ Stage 1 (goroutine): Read → Chunk → Compress → (skip encrypt)
         │   │
         │   ├─ chunker.ChunkReaderWithPool(reader, 4MiB, pool)
         │   │   → produces Chunk{Index, Hash, Data} on channel
         │   │
         │   ├─ For each chunk:
         │   │   ├─ compression.ShouldCompress(data)?
         │   │   │   → CompressChunkWithPool(data, LevelFastest, pool)
         │   │   ├─ SHA-256(processed bytes) → encHash
         │   │   ├─ storageKey = "chunk:" + encHash
         │   │   └─ targets = HashRing.GetNodes(storageKey, replFactor)
         │   └─ Send processedChunk to Stage 2 channel
         │
         ├─ Stage 2 (main goroutine): Store + Replicate
         │   │
         │   ├─ For each processedChunk:
         │   │   ├─ If !Store.Has(storageKey):
         │   │   │   ├─ Store.WriteStream(storageKey, data)
         │   │   │   └─ Update metadata (timestamp)
         │   │   │
         │   │   └─ Fire replicateChunk() in background goroutine
         │   │       (max 4 in-flight via semaphore)
         │   │
         │   └─ Wait for all replication goroutines
         │
         ├─ Build ChunkManifest:
         │   ├─ Ordered ChunkInfo list
         │   ├─ Merkle root over plaintext hashes
         │   └─ Store manifest in metadata
         │
         ├─ replicateManifest() — fire-and-forget to all ring-responsible peers
         │
         └─ Update top-level FileMeta (vector clock, Chunked=true, timestamp)
```

### ECDH Encrypted Upload

Same pipeline as above, but in Stage 1, between compression and hashing:

```
Compressed data → EncryptStreamWithDEKPool(data, DEK, pool) → encrypted bytes
                  → SHA-256(encrypted bytes) → encHash (CAS key)
```

After manifest is built, the encryption metadata is attached:

```go
manifest.Encrypted = true
manifest.OwnerPubKey = enc.OwnerPubKey       // uploader's X25519 pub
manifest.OwnerEdPubKey = enc.OwnerEdPubKey   // uploader's Ed25519 pub
manifest.AccessList = enc.AccessList          // wrapped DEKs for each recipient
manifest.Signature = enc.Signature            // Ed25519 signature
```

### Memory Optimisation

Package-level `sync.Pool`s reuse large buffers across chunks:

- `chunkReadBufPool` — 4 MiB read buffer for `ChunkReaderWithPool`.
- `compBufPool` — compression output slab for `CompressChunkWithPool`.
- `encBufPool` — GCM ciphertext slab for `EncryptStreamWithDEKPool`.

This eliminates GC stop-the-world pauses (~100–200ms each) that dominated small-file upload latency.

---

## 10. File Download Flow (End-to-End)

### Plaintext Download

```
CLI (dfs download --name foo --output bar.pdf)
  │
  ├─ 1. Load identity → fingerprint
  ├─ 2. Storage key = "<fingerprint>/foo"
  ├─ 3. IPC opcode 0x05 (Get Manifest) → check if encrypted → no
  ├─ 4. IPC opcode 0x02 (Download) → key
  │
  ↓ Unix socket
  │
  HandleClient → handleDownload
  │
  └─ server.GetData(key, nil)
       │
       ├─ Check local metadata for key
       │   ├─ If Chunked=true → getChunked(key, nil)
       │   └─ If not found locally → fetchManifestFromPeers(key)
       │       └─ Cache manifest locally, then getChunked()
       │
       ↓ getChunked(key, dek=nil)
       │
       ├─ Load manifest from metadata
       │
       ├─ [Sprint 7] If Downloader is wired:
       │   └─ Downloader.Download(manifest, dst, progressFn, dek=nil)
       │       │
       │       ├─ For each chunk (up to 4 parallel):
       │       │   ├─ selector.BestPeer(candidates) → lowest-latency peer
       │       │   ├─ fetchChunkFromPeer(storageKey, peer) → encrypted bytes
       │       │   │   ├─ If Store.Has(storageKey): read from local disk
       │       │   │   └─ Else: send MessageGetFile to peer, wait for reply
       │       │   ├─ Decrypt (no-op when dek=nil)
       │       │   ├─ Decompress if info.Compressed=true
       │       │   └─ Verify SHA-256(plaintext) == info.Hash
       │       │
       │       └─ chunker.Reassemble(chunks, dst) — sorted by index
       │
       └─ Background: readRepair(key)
           └─ Probes all ring-responsible replicas
               → repairs any that are missing the data
```

### ECDH Encrypted Download

1. CLI resolves owner fingerprint (from `--from` alias or `--from-key`).
2. Fetches manifest via IPC — finds own `AccessEntry` by matching X25519 pub.
3. Unwraps DEK: `envelope.UnwrapDEK(ownX25519Priv, ownerX25519Pub, wrappedDEK)`.
4. Sends IPC opcode `0x04` with raw DEK.
5. Server calls `GetData(key, dek)` — same chunked pipeline but with decryption enabled.
6. Each chunk: `DecryptStreamWithDEK(data, dek)` → decompress → verify hash.

---

## 11. Consistent Hashing & Data Placement

### `Cluster/hashring/hashring.go`

Uses **consistent hashing with virtual nodes** to distribute data across the cluster with minimal key movement on membership changes.

**Configuration:**
- **Virtual nodes:** 150 per physical node (ensures balanced distribution).
- **Replication factor:** default 3 (configurable via `--replication`).

**Hash function:** SHA-256 of `"<addr>:vnode:<i>"`, first 8 bytes interpreted as big-endian uint64 → position on the ring.

**Key lookup — `GetNodes(key, n)`:**
1. Hash the file key → ring position.
2. Binary search for the successor in the sorted ring.
3. Walk clockwise, collecting distinct physical nodes (skipping virtual-node duplicates belonging to already-collected nodes).
4. Return up to `n` distinct nodes.

**Node management:**
- `AddNode(addr)` — computes 150 virtual node positions, inserts into sorted slice.
- `RemoveNode(addr)` — removes all virtual node positions, rebuilds sorted slice.
- `HasNode`, `Members`, `Size` — read-only accessors.
- Thread-safe via `sync.RWMutex`.

---

## 12. Replication

### Chunk Replication (`Server/server.go` — `replicateChunk`)

After storing a chunk locally, the server replicates it to all ring-responsible peers:

1. Gob-encodes a `MessageStoreFile{Key, Size}`.
2. For each target node (excluding self):
   - If peer is connected: `peer.SendStream(msgBytes, chunkData)` — atomic framed message + raw bytes.
   - If peer is not connected: `storeHint(targetAddr, key, data)` — hinted handoff for later delivery.
3. **Write quorum:** waits for `W` successful acks before returning:
   - `W = n` if n ≤ 2 (no spare replica).
   - `W = n/2 + 1` if n ≥ 3 (majority quorum — one slow node doesn't stall the uploader).

### Manifest Replication (`replicateManifest`)

Manifests are metadata-only (no physical CAS file). Replicated via `MessageStoreManifest{FileKey, ManifestJSON}` — a regular framed message (no stream). Fire-and-forget (non-blocking).

### Deduplication

Before storing a chunk locally, `StoreData` checks `Store.Has(storageKey)`. If the chunk already exists (e.g. from a previous upload with the same content), the write is skipped — CAS provides natural deduplication.

---

## 13. Cluster Membership & Gossip

### Membership State Machine (`Cluster/membership/membership.go`)

**Node states:** `StateAlive(0)` → `StateSuspect(1)` → `StateDead(2)` → `StateLeft(3)`

**NodeInfo:**
```go
type NodeInfo struct {
    Addr        string
    State       NodeState
    Generation  uint64              // monotonically increasing version
    LastUpdated time.Time
    Metadata    map[string]string   // {alias, fingerprint, x25519_pub, ed25519_pub}
}
```

**Generational guard:** `UpdateState` only applies if `generation > current`. Same-generation updates follow monotonic state progression: Alive < Suspect < Dead < Left. This prevents stale gossip from overriding newer state.

**Self-node generation:** Initialised to `time.Now().UnixNano()` — ensures a restarted node always wins over its stale "Dead" records at peers.

**`Merge(digests)`** — returns addresses needing full NodeInfo. Handles same-generation monotone state progression.

**`OnChange(fn)`** — registers listeners for state transitions. Used by the server to trigger ring updates, handoff delivery, and rebalancing.

### Gossip Protocol (`Cluster/gossip/gossip.go`)

Push-pull epidemic dissemination converging in O(log N) rounds:

**Config:** `FanOut=3`, `Interval=200ms`

**Round flow (`doGossipRound`):**
1. Pick up to `FanOut=3` random connected peers.
2. Filter digest to only include nodes that are currently connected (prevents phantom address propagation in Docker/NAT).
3. Send `MessageGossipDigest{From, Digests}` — compact `{Addr, State, Generation}` per node.

**Digest handling (`HandleDigest`):**
1. Merge incoming digest into local `ClusterState`.
2. Detect new/rejoining nodes → call `onNewPeer(addr)` to trigger a dial.
3. Reply with `MessageGossipResponse{Full, MyDigest}` — full NodeInfo for nodes the sender is behind on, plus own digest so the sender can send us theirs.

**Response handling (`HandleResponse`):**
1. Apply full NodeInfo updates.
2. Merge metadata (alias, fingerprint, keys) — identity propagates cluster-wide.
3. **Self-fingerprint detection:** if gossip delivers our own metadata under a different address (Docker/NAT), we detect and avoid overwriting our own state.

### Peer Discovery via Gossip

When gossip discovers a node address not in the local peers map, it calls `onNewPeer(addr)`:
1. **Self-connection guard:** rejects if `addr` has the same port as us AND its host is one of our own network interfaces.
2. **Deduplication:** skips if already in peers map or dial already in progress (via `dialingSet`).
3. **Dial:** `transport.Dial(addr)` → triggers the full OnPeer + Announce handshake.

---

## 14. Failure Detection

### `Cluster/failure/detector.go` — Phi Accrual Detector

Probabilistic failure detector based on heartbeat inter-arrival time distributions.

**How it works:**
1. Each node sends heartbeats to all connected peers at `HeartbeatInterval=1s`.
2. Receiving node records the heartbeat in a sliding window (`WindowSize=200` inter-arrival intervals).
3. The detector computes **phi** — the suspicion level:

$$\phi = -\log_{10}(1 - \text{CDF}(t_{\text{now}} - t_{\text{last}}))$$

Where CDF uses the normal distribution with mean and stddev computed from the window. A stddev floor of max(10% of mean, 1ms) prevents degenerate cases.

**State transitions:**
- phi ≥ `SuspectThreshold=8.0` and currently Alive → **Suspect** (fires `onSuspect` callback)
- Suspect for > `DeadTimeout=30s` → **Dead** (fires `onDead` callback → removes from ring, triggers rebalance)

**Reaper loop:** checks phi values at 2× heartbeat frequency (500ms).

**Integration:** `RecordHeartbeat` handles both ephemeral and canonical addresses (via `announceAdded` reverse-mapping).

---

## 15. Hinted Handoff

### `Cluster/handoff/handoff.go`

When a replication target is unreachable, the write is saved as a **hint** for later delivery.

**Hint struct:**
```go
type Hint struct {
    Key        string
    TargetAddr string
    Data       []byte
    CreatedAt  time.Time
    Attempts   int
}
```

**Disk persistence:** hints are stored as JSON files at `<storageRoot>/.hints/<sanitized_addr>.hints.json`. Survives process restarts.

**Per-target cap:** `MaxHints=1000` — oldest hint evicted when exceeded.

**TTL:** `MaxAge=24h` — expired hints purged on load and hourly.

**Delivery:**
- `OnPeerReconnect(addr)` triggers async delivery (coalesces rapid reconnects via `activeDeliveries sync.Map`).
- `deliverPending(addr)` iterates all hints for the target, calls `deliver(hint)` closure (which sends via `peer.SendStream`).
- Retries up to 5 attempts per hint, then discards.

---

## 16. Quorum Reads & Writes

### `Cluster/quorum/quorum.go`

Configurable quorum consistency:

| Parameter | Default | Description |
|-----------|---------|-------------|
| N         | 3       | Replication factor |
| W         | 2       | Write quorum (majority) |
| R         | 2       | Read quorum (majority) |
| Timeout   | 5s      | Per-operation timeout |

#### Write Path — `Coordinator.Write(key, data, clock)`

1. `getTargets(key)` → N responsible nodes from the hash ring.
2. Creates an ack channel in `writeAcks sync.Map`.
3. For each target:
   - **Self:** `localWrite(key, data, clock)` — stores bytes + updates metadata with vector clock.
   - **Remote:** `sendMsg(addr, MessageQuorumWrite{Key, Data, Clock})`.
4. Waits for W successful acks within timeout.
5. Returns error if quorum not met.

#### Read Path — `Coordinator.Read(key)`

1. `getTargets(key)` → N responsible nodes.
2. Creates a response channel in `readResps sync.Map`.
3. For each target:
   - **Self:** `localRead(key)` → vector clock, timestamp, found.
   - **Remote:** `sendMsg(addr, MessageQuorumRead{Key})`.
4. Waits for R found-responses within timeout.
5. Resolves conflict using `conflict.LWWResolver`.
6. Degrades gracefully on partial quorum (returns even with < R responses if some found).

---

## 17. Conflict Resolution & Vector Clocks

### `Cluster/vclock/vclock.go`

Tracks causal relationships between distributed writes.

```go
type VectorClock map[string]uint64
```

**Operations:**
- `Increment(nodeAddr)` → new clock with node's counter +1 (immutable, returns copy).
- `Merge(other)` → per-node maximum (immutable).
- `Compare(other)` → `Before`, `After`, `Concurrent`, or `Equal`.
  - **Before:** all entries ≤ other, at least one strictly <
  - **After:** all entries ≥ other, at least one strictly >
  - **Concurrent:** both have strictly greater entries — genuine conflict
  - **Equal:** identical

### `Cluster/conflict/conflict.go`

**`LWWResolver`** — Last-Write-Wins conflict resolution:

```go
type Version struct {
    NodeAddr  string
    Clock     VectorClock
    Timestamp int64
}
```

**Resolution rules (priority order):**
1. Causally later (vector clock `After`) always wins.
2. If `Concurrent`, higher `Timestamp` (wall-clock UnixNano) wins.
3. On exact tie, first candidate in input slice wins (deterministic).

---

## 18. Anti-Entropy (Merkle Tree Sync)

### `Cluster/merkle/merkle.go`

Periodic background sync to detect and repair data divergence between replicas.

**Merkle Tree construction:**
1. Sort all local file keys.
2. SHA-256 hash each key → leaf nodes.
3. Build tree bottom-up: each parent = SHA-256(left || right).
4. Odd leaf count → duplicate last leaf.

**Sync round (`doSync`, every 10 minutes):**
1. Build local Merkle tree.
2. Send root hash to each peer via `MessageMerkleSync{From, RootHash}`.
3. If root hashes match → trees are identical, no sync needed.
4. If different → peer sends `MessageMerkleDiffResponse{AllKeys}` with its full key list.

**Repair (`repair`):**
- Diff local tree vs. remote key list.
- Keys we have but they don't → `onSendKey(peerAddr, key)` (re-replicate).
- Keys they have but we don't → `onNeedKey(peerAddr, key)` (request from peer).

---

## 19. Rebalancing

### `Cluster/rebalance/rebalance.go`

Restores the replication factor when nodes join or leave.

#### OnNodeJoined(newAddr)

Triggered asynchronously when a new node joins the cluster:

1. Scans all locally-stored keys via `MetadataStore.Keys()`.
2. For each key: check if the new node is now ring-responsible AND self is also responsible.
3. If yes: migrate the data (raw bytes) and manifest (if applicable) to the new node.
4. Throttled at 50ms per chunk to avoid saturating connections.

#### OnNodeLeft(deadAddr)

Triggered when a node is declared dead:

1. Scans all local keys.
2. For each key: check if fewer than N replicas remain after removal.
3. If under-replicated: re-replicate to the next responsible nodes in the ring.

#### Manifest handling

Separately handles manifest keys (prefixed with `"manifest:"`) — serialises the manifest to JSON and sends via `MessageStoreManifest`.

---

## 20. Parallel Downloads & Peer Selection

### `Cluster/selector/selector.go` — EWMA Latency Tracker

Tracks per-peer latency and active download count.

**Scoring formula:**

$$\text{score}(p) = \text{ewma} \times (1 + 0.5 \times \text{activeDownloads})$$

- EWMA updated with α=0.2 on each completed fetch.
- Initial latency = 50ms (ensures untested peers get tried).
- `BestPeer(candidates)` returns the lowest-score candidate.

### `Server/downloader/downloader.go` — Parallel Chunk Fetcher

**Config:**
| Parameter    | Default | Description |
|-------------|---------|-------------|
| MaxParallel | 4       | Max concurrent chunk fetches |
| ChunkTimeout| 30s     | Per-chunk fetch timeout |
| MaxRetries  | 3       | Retries per chunk on failure |

**Download flow:**
1. Launch up to `MaxParallel` goroutines via semaphore.
2. Each goroutine calls `fetchChunk(info, dek)`:
   a. `getPeers(storageKey)` → ring-responsible nodes.
   b. `selector.BestPeer(available)` → pick lowest-latency untried peer.
   c. `selector.BeginDownload(peer)` → track active downloads.
   d. `fetch(storageKey, peer)` → get raw encrypted bytes.
   e. `selector.EndDownload(peer)` + `RecordLatency(peer, elapsed)`.
   f. `decrypt(storageKey, data, dek)` → plaintext (no-op when dek=nil).
   g. If `info.Compressed` → `compression.DecompressChunk(data)`.
   h. Verify `SHA-256(plaintext) == info.Hash`.
   i. On failure: try next best peer (up to MaxRetries).
3. Wait for all chunks.
4. `chunker.Reassemble(chunks, dst)` — sort by index, write sequentially.

---

## 21. Observability

### Health Server (`Observability/health/health.go`)

HTTP server at `port + 1000` (e.g., `:3000` → `:4000`):

| Endpoint | Description |
|----------|-------------|
| `GET /health` | JSON: `{status, nodeAddr, peerCount, ringSize, uptime, startedAt}`. Returns 200 if OK, 503 if degraded. |
| `GET /metrics` | Prometheus text exposition |
| `GET /debug/pprof/*` | Go runtime profiling |

### Metrics (`Observability/metrics/metrics.go`)

All metrics prefixed `dfs_`:

**Counters:**
- `dfs_store_ops_total{operation, status}` — store/replicate × ok/err
- `dfs_get_ops_total{source, status}` — local/remote/miss × ok/err
- `dfs_replication_total{status}` — ok/hint/skip
- `dfs_gossip_rounds_total`
- `dfs_heartbeats_total{direction}` — sent/received

**Histograms:**
- `dfs_store_duration_seconds{operation}`
- `dfs_get_duration_seconds{source}`
- `dfs_gossip_duration_seconds`
- `dfs_quorum_duration_seconds{type}` — read/write

**Gauges:**
- `dfs_peer_count`
- `dfs_ring_size`
- `dfs_hints_pending`

### Structured Logging (`Observability/logging/logger.go`)

zerolog-based JSON logging with levels: Debug, Info, Warn, Error.

```go
logging.Init("server", logging.LevelInfo)
logging.Global.Info("chunk stored", "key", storageKey, "size", n)
```

Child loggers with inherited fields:
```go
logger := logging.Global.With("component", "gossip")
```

### Distributed Tracing (`Observability/tracing/tracing.go`)

OpenTelemetry with OTLP/HTTP export. Only active if `OTEL_EXPORTER_OTLP_ENDPOINT` is set.

- `Init(serviceName, endpoint)` → OTLP/HTTP exporter with `AlwaysSample`.
- `StartSpan(ctx, name)` → creates child span.
- `InjectToMap(ctx, carrier)` / `ExtractFromMap(ctx, carrier)` → W3C Trace-Context propagation across nodes via `map[string]string`.
- `AddEvent(ctx, name, kvs...)` → structured span events.
- `RecordError(ctx, err)` → marks span as errored.

---

## 22. TLS & Mutual Authentication

When `DFS_ENABLE_TLS=true`:

1. **CA setup:** `LoadOrGenerateCA()` creates/loads a root CA at `.certs/ca.crt`.
2. **Node cert:** `LoadOrGenerateNodeCert()` creates a cert signed by the CA, valid for both server and client auth (mTLS).
3. **mTLS config:** `LoadMTLSConfig()` produces a `tls.Config` that:
   - Presents the node cert on connections.
   - Requires and verifies the peer's cert against the CA (`RequireAndVerifyClientCert`).
   - Enforces TLS 1.2+ with X25519/P-256 curve preferences.

Both TCP and QUIC transports use the same TLS config. QUIC additionally enforces TLS 1.3.

---

## 23. NAT Traversal

### `Peer2Peer/nat/nat.go` — STUN-Based Discovery

Uses RFC 5389 STUN Binding Requests to discover the node's public IP:

1. Sends binding request to Google STUN servers (`stun.l.google.com:19302`, etc.).
2. Parses XOR-MAPPED-ADDRESS or MAPPED-ADDRESS from response.
3. Caches result → `AnnotateGossip()` returns `{"public_addr": "1.2.3.4:PORT"}` for inclusion in gossip metadata.

### Docker/NAT Address Discovery

A multi-step process ensures nodes discover their externally-visible address:

1. Node starts with `transport.Addr()` (e.g., `:3000` → normalised to `127.0.0.1:3000`).
2. On outbound connect, sends `MessageAnnounce{ListenAddr: ":3000"}`.
3. Remote node resolves the announce to a full address using the inbound connection's remote IP (e.g., `172.17.0.2:3000`).
4. Remote replies with `MessageAnnounceAck{YourAddr: "172.17.0.2:3000"}`.
5. Node re-registers in the hash ring, cluster state, and gossip under the external address.

---

## 24. IPC Protocol (CLI ↔ Daemon)

Binary protocol over Unix domain socket at `/tmp/dfs-<port>.sock`.

### Opcodes

| Opcode | Value | Direction | Description |
|--------|-------|-----------|-------------|
| Upload | `0x01` | CLI → Daemon | Plaintext file upload |
| Download | `0x02` | CLI → Daemon | Plaintext file download |
| ECDH Upload | `0x03` | CLI → Daemon | Encrypted upload with DEK + access list |
| ECDH Download | `0x04` | CLI → Daemon | Encrypted download with raw DEK |
| Get Manifest | `0x05` | CLI → Daemon | Fetch ECDH encryption metadata |
| Resolve Alias | `0x06` | CLI → Daemon | Map alias → fingerprint + public keys |

### Framing

All fields use **big-endian length-prefixed** binary encoding:

```
Upload Request:   [1B opcode][2B keyLen][key bytes][8B fileSize][file bytes]
Download Request: [1B opcode][2B nameLen][name bytes]
ECDH Upload:      [1B opcode][2B keyLen][key][2B dekLen][dek][2B ownerPub]
                  [2B ownerEdPub][2B accessListJSON][2B signature][8B fileSize][data]
Response:         [1B status (0=ok/1=err)][8B dataSize][data bytes]
```

### HandleClient Dispatch

`cmd/handle_client.go` reads the opcode byte and dispatches:

- `0x01` → `handleUpload` → `server.StoreData(key, body, nil)`
- `0x02` → `handleDownload` → `server.GetData(key, nil)`
- `0x03` → `handleECDHUpload` → parses ECDH envelope → `server.StoreData(key, body, encMeta)`
- `0x04` → `handleECDHDownload` → `server.GetData(name, dek)`
- `0x05` → `handleGetManifestInfo` → `server.InspectManifest(name)` → returns manifest encryption metadata
- `0x06` → `handleResolveAlias` → `server.LookupAlias(alias)` → returns matching nodes

---

## 25. ECDH File Sharing Flow

### Upload (Sharing with Recipients)

```
dfs upload --name secret.pdf --file /path/to/secret.pdf --share-with alice,bob
```

1. **Generate DEK:** random 32 bytes.
2. **Wrap for self:** `envelope.WrapDEKForRecipient(ownX25519Priv, ownX25519Pub, dek)`.
3. **Resolve recipients:** for each alias (`alice`, `bob`):
   - IPC opcode `0x06` → `server.LookupAlias("alice")` → searches local identity + cluster gossip metadata.
   - Returns `{Fingerprint, X25519PubHex, Ed25519PubHex, NodeAddr}`.
4. **Wrap for each recipient:** `envelope.WrapDEKForRecipient(ownX25519Priv, recipientX25519Pub, dek)`.
5. **Sign manifest:** `envelope.SignManifest(ownEd25519Priv, payload)` — signs `{AccessList, Encrypted, FileKey, OwnerPubKey}`.
6. **Upload:** IPC opcode `0x03` with full envelope.
7. **Server encrypts each chunk** with the DEK via `EncryptStreamWithDEKPool`.
8. **Manifest stores** access list + signature → replicated to ring-responsible peers.

### Download (As Recipient)

```
dfs download --name secret.pdf --from alice --output decrypted.pdf
```

1. **Resolve owner:** `LookupAlias("alice")` → `fingerprint`, `X25519Pub`.
2. **Storage key:** `<alice-fingerprint>/secret.pdf`.
3. **Fetch manifest:** IPC opcode `0x05` → `InspectManifest(key)`.
4. **Find own access entry:** scan `manifest.AccessList` for own X25519 pub.
5. **Unwrap DEK:** `envelope.UnwrapDEK(ownX25519Priv, aliceX25519Pub, wrappedDEK)`.
6. **Download:** IPC opcode `0x04` with raw DEK.
7. **Server decrypts each chunk** with the DEK, decompresses if needed, verifies integrity.

### Cryptographic Verification

- **Manifest signature** proves who authorised access (Ed25519 over canonical JSON).
- **Chunk integrity** proven by `SHA-256(plaintext) == ChunkInfo.Hash` after decryption.
- **File integrity** proven by Merkle root over all plaintext chunk hashes.

---

## 26. Server Message Loop & Wire Protocol

### Message Registration

All message types are registered with `encoding/gob` in `init()`:

```go
gob.Register(&MessageStoreFile{})
gob.Register(&MessageGetFile{})
gob.Register(&MessageLocalFile{})
gob.Register(&MessageAnnounce{})
gob.Register(&MessageAnnounceAck{})
gob.Register(&MessageHeartbeat{})
gob.Register(&MessageHeartbeatAck{})
gob.Register(&MessageGossipDigest{})
gob.Register(&MessageGossipResponse{})
gob.Register(&MessageQuorumWrite{})
gob.Register(&MessageQuorumWriteAck{})
gob.Register(&MessageQuorumRead{})
gob.Register(&MessageQuorumReadResponse{})
gob.Register(&MessageMerkleSync{})
gob.Register(&MessageMerkleDiffResponse{})
gob.Register(&MessageLeaving{})
gob.Register(&MessageStoreManifest{})
gob.Register(&MessageGetManifest{})
gob.Register(&MessageManifestResponse{})
gob.Register(&MessageChunkOffer{})
gob.Register(&MessageChunkNeed{})
gob.Register(&MessageIdentityMeta{})
```

### Main Loop (`Server.loop`)

```go
for {
    select {
    case RPC := <-transport.Consume():
        message := gob.Decode(RPC.Payload)
        handleMessage(from, peer, message, streamWg, streamReader)
    case <-quitch:
        return
    }
}
```

### Message Dispatch (`handleMessage`)

| Message Type | Handler | Description |
|-------------|---------|-------------|
| `MessageStoreFile` | `handleStoreMessage` (async goroutine) | Receives replicated chunk bytes |
| `MessageGetFile` | `handleGetMessage` | Sends requested file bytes back |
| `MessageLocalFile` | `handleLocalMessage` (async goroutine) | Receives file data from peer, unblocks pending fetch |
| `MessageAnnounce` | `handleAnnounce` | Remaps inbound peer to canonical address |
| `MessageAnnounceAck` | `handleAnnounceAck` | Discovers external address, re-registers in ring |
| `MessageHeartbeat` | `handleHeartbeat` | Records heartbeat, sends ack |
| `MessageHeartbeatAck` | (logged) | RTT measurement |
| `MessageGossipDigest` | `GossipSvc.HandleDigest` | Gossip pull phase |
| `MessageGossipResponse` | `GossipSvc.HandleResponse` | Gossip push phase |
| `MessageQuorumWrite` | `handleQuorumWrite` | Stores data locally, sends ack |
| `MessageQuorumWriteAck` | `Quorum.HandleWriteAck` | Routes to waiting Write() caller |
| `MessageQuorumRead` | `handleQuorumRead` | Returns local metadata |
| `MessageQuorumReadResponse` | `Quorum.HandleReadResponse` | Routes to waiting Read() caller |
| `MessageMerkleSync` | `AntiEntropy.HandleSync` | Compares root hashes |
| `MessageMerkleDiffResponse` | `AntiEntropy.HandleDiffResponse` | Triggers key-level repair |
| `MessageLeaving` | `handleLeaving` | Removes departing node from ring |
| `MessageStoreManifest` | (inline) | Stores replicated manifest |
| `MessageGetManifest` | (inline) | Returns own copy of manifest |
| `MessageManifestResponse` | (inline) | Delivers to pending fetch caller |
| `MessageChunkOffer` | `handleChunkOffer` | Replies with missing hashes |
| `MessageChunkNeed` | (inline) | Delivers to replication goroutine |
| `MessageIdentityMeta` | (inline) | Stores identity for alias resolution |

**Async dispatch note:** `MessageStoreFile` and `MessageLocalFile` are dispatched to goroutines so the server loop can immediately process the next QUIC stream. Without this, concurrent chunk streams would pile up in the QUIC receive window, causing flow-control deadlocks.

---

## 27. Graceful Shutdown

### `Server.GracefulShutdown()`

Ordered shutdown sequence:

1. **Stop gossip** — prevents advertising "I am Alive" after departure announcement.
2. **Stop heartbeat sender** — stops sending heartbeats.
3. **Broadcast `MessageLeaving`** — includes current generation so peers can set `StateLeft` at gen+1, ensuring any stale gossip digest cannot override the departure.
4. **200ms pause** — lets the leaving message propagate.
5. **Stop health server** — graceful HTTP shutdown with 3s context.
6. **Stop handoff service** — stops hint delivery goroutines.
7. **Stop anti-entropy** — stops Merkle sync timer.
8. **300ms pause** — in-flight replication goroutines finish.
9. **Stop server** — closes quit channel → loop exits → transport closes.

### Peer-Side Handling (`handleLeaving`)

When a peer receives `MessageLeaving`:
1. Removes the departing node from the hash ring.
2. Updates cluster state to `StateLeft` at `msgGen + 1` (beats any stale gossip).
3. Closes the peer connection.
4. Cleans up `announceAdded` reverse-mapping.
5. Updates metrics (peer count, ring size).

---

## 28. Package Dependency Graph

```
main.go
  └─ cmd/
       ├─ root.go, start.go
       ├─ daemon.go ──── Server/server.go (MakeServer)
       │                   ├─ Storage/ (Store, MetadataStore)
       │                   │   ├─ chunker/ (ChunkReader, Reassemble, Manifest)
       │                   │   └─ compression/ (zstd compress/decompress)
       │                   ├─ Crypto/ (AES-GCM encrypt/decrypt)
       │                   │   ├─ envelope/ (ECDH DEK wrapping)
       │                   │   └─ identity/ (Ed25519 + X25519 keypairs)
       │                   ├─ Peer2Peer/ (Transport, Peer, Message, Decoder)
       │                   │   ├─ quic/ (QUIC transport)
       │                   │   └─ nat/ (STUN-based NAT traversal)
       │                   ├─ Cluster/
       │                   │   ├─ hashring/ (consistent hash ring)
       │                   │   ├─ gossip/ (epidemic dissemination)
       │                   │   ├─ membership/ (cluster state machine)
       │                   │   ├─ failure/ (phi accrual failure detector)
       │                   │   ├─ handoff/ (hinted handoff)
       │                   │   ├─ quorum/ (W/R quorum coordinator)
       │                   │   ├─ merkle/ (anti-entropy Merkle sync)
       │                   │   ├─ rebalance/ (data migration)
       │                   │   ├─ vclock/ (vector clocks)
       │                   │   ├─ selector/ (EWMA peer selection)
       │                   │   └─ conflict/ (LWW resolver)
       │                   ├─ Server/downloader/ (parallel chunk fetcher)
       │                   ├─ Observability/
       │                   │   ├─ health/ (HTTP server)
       │                   │   ├─ metrics/ (Prometheus counters/histograms/gauges)
       │                   │   ├─ logging/ (zerolog structured logging)
       │                   │   └─ tracing/ (OpenTelemetry OTLP)
       │                   └─ factory/ (transport factory)
       ├─ upload.go ──── Crypto/envelope/, Crypto/identity/
       ├─ download.go ── Crypto/envelope/, Crypto/identity/
       ├─ identity.go ── Crypto/identity/
       ├─ ipc.go ─────── Binary IPC protocol
       ├─ handle_client.go ── IPC dispatch
       ├─ handle_upload.go ── Upload handling
       ├─ handle_download.go ── Download handling
       └─ socket.go ──── Unix socket path
```

---

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `DFS_TRANSPORT` | `quic` | Transport protocol: `quic` or `tcp` |
| `DFS_ENABLE_TLS` | (unset) | Set to `true` to enable mTLS |
| `DFS_IDENTITY_PATH` | `~/.dfs/identity.json` | Path to node identity file |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | (unset) | OpenTelemetry OTLP endpoint for tracing |

---

## Concurrency & Safety Model

- **Server.peers** — protected by `peerLock sync.RWMutex`. Snapshot-copied for iteration during broadcast/replication.
- **Server.pendingFile / pendingOffer** — protected by `mu sync.Mutex`. Used as request-response channels for async peer operations.
- **HashRing** — internal `sync.RWMutex`.
- **ClusterState** — internal `sync.RWMutex`.
- **MetaFile** — internal `sync.Mutex` (BoltMetaStore inherits bbolt's transactional safety).
- **TCPPeer.sendMu** — serialises concurrent writes on TCP connections.
- **QUICPeer** — no per-peer mutex needed (QUIC streams are independent).
- **Server.dialingSet / dialingLock** — prevents concurrent duplicate dials.
- **Server.shutdownOnce** — ensures `close(quitch)` is called exactly once.
- **sync.Pool** for chunk/compression/encryption buffers — eliminates GC pauses.
