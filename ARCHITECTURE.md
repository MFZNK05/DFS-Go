# Hermond — Architecture Deep Dive

A comprehensive technical reference for engineers who want to understand how Hermond works under the hood. Every design decision, algorithm, and safety mechanism is documented here — even if you didn't build the project, you should be able to understand it fully after reading this.

---

## Table of Contents

- [System Overview](#system-overview)
- [Data Flow: Upload](#data-flow-upload)
- [Data Flow: Download](#data-flow-download)
- [Transport Layer: Why QUIC](#transport-layer-why-quic)
- [Storage Engine](#storage-engine)
  - [Content-Addressable Storage (CAS)](#content-addressable-storage-cas)
  - [Chunking & Merkle Trees](#chunking--merkle-trees)
  - [Compression Pipeline](#compression-pipeline)
  - [Directory Manifests](#directory-manifests)
- [Cryptography](#cryptography)
  - [Identity System](#identity-system)
  - [ECDH Direct Encrypted Transfer](#ecdh-direct-encrypted-transfer)
  - [Manifest Signing](#manifest-signing)
- [Cluster Coordination](#cluster-coordination)
  - [Consistent Hashing](#consistent-hashing)
  - [Gossip Protocol](#gossip-protocol)
  - [Membership & Generation Counters](#membership--generation-counters)
  - [Failure Detection (Phi Accrual)](#failure-detection-phi-accrual)
  - [Quorum Reads & Writes](#quorum-reads--writes)
  - [Vector Clocks & Conflict Resolution](#vector-clocks--conflict-resolution)
  - [Hinted Handoff](#hinted-handoff)
  - [Anti-Entropy (Merkle Sync)](#anti-entropy-merkle-sync)
  - [Rebalancing](#rebalancing)
- [Bandwidth Management: LEDBAT-lite](#bandwidth-management-ledbat-lite)
- [Download Resume](#download-resume)
- [Peer Selection (EWMA Latency)](#peer-selection-ewma-latency)
- [NAT Traversal & External Address Discovery](#nat-traversal--external-address-discovery)
- [Persistent State (BoltDB)](#persistent-state-boltdb)
- [Transfer Management](#transfer-management)
- [IPC Protocol](#ipc-protocol)
- [Performance Optimizations](#performance-optimizations)
- [Package Map](#package-map)

---

## System Overview

Hermond is a peer-to-peer distributed file system. Every node is both a client and a server. There is no central coordinator — nodes discover each other via gossip, distribute files using consistent hashing, and replicate data to survive node failures.

```
┌─────────────────────────────────────────────────────────────┐
│                        Hermond Node                         │
│                                                             │
│  ┌─────────┐   ┌───────────┐   ┌────────────────────────┐  │
│  │  TUI    │──>│  IPC      │──>│  Server (orchestrator)  │  │
│  │ (Bubble │   │  (Unix    │   │                         │  │
│  │  Tea)   │   │  Socket)  │   │  ┌────────┐ ┌────────┐ │  │
│  └─────────┘   └───────────┘   │  │ Gossip │ │HashRing│ │  │
│  ┌─────────┐                   │  └────────┘ └────────┘ │  │
│  │  CLI    │──────────┐        │  ┌────────┐ ┌────────┐ │  │
│  │ (Cobra) │          │        │  │Quorum  │ │Handoff │ │  │
│  └─────────┘          │        │  └────────┘ └────────┘ │  │
│                       │        │  ┌────────┐ ┌────────┐ │  │
│                       │        │  │FailDet │ │Rebalncr│ │  │
│                       │        │  └────────┘ └────────┘ │  │
│                       │        │  ┌────────┐ ┌────────┐ │  │
│                       │        │  │BW Mgr  │ │Selector│ │  │
│                       │        │  └────────┘ └────────┘ │  │
│                       │        └──────────┬─────────────┘  │
│                       │                   │                 │
│                       │        ┌──────────▼─────────────┐  │
│                       │        │  Storage Engine         │  │
│                       │        │  CAS + Chunker +        │  │
│                       │        │  Compress + Resume      │  │
│                       │        └──────────┬─────────────┘  │
│                       │                   │                 │
│                       └───────>┌──────────▼─────────────┐  │
│                                │  QUIC Transport         │  │
│                                │  (TLS 1.3, per-stream)  │  │
│                                └──────────┬─────────────┘  │
└───────────────────────────────────────────┼─────────────────┘
                                            │
              ┌─────────────────────────────┼─────────────────────────┐
              │                             │                         │
        ┌─────▼─────┐               ┌─────▼─────┐            ┌─────▼─────┐
        │  Peer B   │               │  Peer C   │            │  Peer D   │
        └───────────┘               └───────────┘            └───────────┘
```

**Key principle:** Every subsystem is its own Go package with no circular dependencies. The `Server` package wires everything together.

---

## Data Flow: Upload

When a user runs `hermond upload --name "report.pdf" --file ./report.pdf --public`:

```
Step 1: CLI/TUI ──IPC──> Daemon
   Binary IPC over Unix socket. Opcode 0x01 (plaintext) or 0x03 (ECDH).
   Sends: name, file size, public flag, share-with list, raw file bytes.

Step 2: Chunking
   File split into 4 MiB chunks (last chunk may be smaller).
   Each chunk gets a SHA-256 hash (plaintext hash, used for integrity).

Step 3: Compression (selective)
   For each chunk, ShouldCompress() checks:
     - Skip if < 1 KiB (framing overhead exceeds benefit)
     - Skip known compressed formats (JPEG, PNG, ZIP, MP4, MKV, gzip, zstd)
       detected by magic bytes
     - Compute Shannon entropy on first 4 KiB sample:
       entropy < 7.0 → compress with zstd
       entropy >= 7.5 → skip (already high entropy = incompressible)
   If compressed size >= original size, discard compressed version.

Step 4: Encryption
   Random 256-bit DEK (Data Encryption Key) generated per file.
   Each chunk encrypted with AES-256-GCM:
     - Per-chunk nonce derived from chunk index (deterministic, no nonce reuse)
     - Ciphertext includes 16-byte GCM authentication tag
   If ECDH mode: DEK wrapped for each recipient using X25519 ECDH + HKDF + AES-GCM.

Step 5: CAS Storage
   Encrypted chunk stored at content-addressed path:
     SHA-256 of ciphertext → split into 5-char directory segments → MD5 filename
     Example: storage_root/816cc/20437/d8595/a1b2c3d4e5f6.dat
   Deduplication: if two files share identical chunks, only one copy on disk.

Step 6: Manifest Construction
   ChunkManifest built with:
     - Per-chunk metadata: index, plaintext hash, encrypted hash, size, compressed flag
     - Merkle root: binary tree of plaintext chunk hashes
     - ECDH fields: owner pubkey, access list (wrapped DEKs), Ed25519 signature

Step 7: Replication
   Hash ring lookup: GetNodes("report.pdf", RF=3) → [self, NodeB, NodeC]
   For each remote replica:
     - Send MessageStoreFile (gob-encoded) with key, size, encrypted chunk
     - Stream chunk data over QUIC (throttled by bandwidth manager)
   Quorum: for RF >= 3, wait for majority (RF/2 + 1) acks before returning success.
   If a target is unreachable → store hint for hinted handoff.

Step 8: State Persistence
   Record upload in BoltDB (uploads bucket).
   If --public: add to public file catalog.
   Metadata batch write for directory uploads (single fsync, not 3N+1).
```

---

## Data Flow: Download

When a user runs `hermond download --name "report.pdf" --output ./out.pdf --from alice`:

```
Step 1: Manifest Resolution
   Resolve "alice" → fingerprint via gossip metadata or local alias cache.
   Storage key = "<fingerprint>/report.pdf"
   Fetch ChunkManifest: check local store first, then ask peers via
   MessageGetManifest. Unified stat: checks directory manifest first,
   then file manifest (single round-trip).

Step 2: Resume Check
   Look for existing .part file at output path:
     ./out.pdf.part    — partially downloaded data
     ./out.pdf.resume  — append-only log of completed chunk indices
   If sidecar exists and matches current manifest (same key, merkle root, size):
     Fast-verify: for each claimed chunk index in sidecar, read bytes from
     .part file at correct offset, SHA-256 hash them, compare against manifest.
     Only verified chunks enter the skip set.
   If sidecar doesn't match (manifest changed): discard, start fresh.

Step 3: Parallel Multi-Source Download
   Downloader spawns up to MaxParallel (4) concurrent chunk fetchers.
   For each missing chunk:
     a. Selector picks best peer via EWMA latency scoring:
        score = ewma_latency × (1 + 0.5 × active_downloads)
        Lower score wins. Prefers fast, idle peers.
     b. Send MessageGetFile to chosen peer over QUIC.
     c. Peer streams back encrypted chunk data.

Step 4: On-the-Fly Verification (per chunk)
   BEFORE writing to disk, each chunk is verified:
     - SHA-256 hash of received plaintext chunk
     - Compare against manifest's recorded hash for that chunk index
   If mismatch:
     - Chunk discarded (not written)
     - Peer penalized: RecordLatency(peer, 10 minutes) — effectively banned
       via EWMA score inflation
     - Chunk retried from a different peer (up to MaxRetries)
   This catches: corrupted data, malicious peers, bit-rot, network errors.
   A bad peer is detected on the FIRST bad chunk, not after the entire
   file is downloaded.

Step 5: Direct-to-Disk Write
   DownloadToFile uses io.WriterAt for random-access writes:
     - Chunks write directly to their correct offset in the .part file
     - Out-of-order arrival is fine (no head-of-line blocking)
     - Memory bounded: MaxParallel × ChunkSize = 4 × 4 MiB = 16 MiB
   Each successful chunk: append index to .resume sidecar (O(1) per chunk).

Step 6: Final Verification
   After all chunks downloaded:
     - Recompute Merkle root from all chunk hashes
     - Compare against manifest's recorded Merkle root
     - This catches: missing chunks, reordered chunks, manifest tampering
   If Merkle root matches:
     - Delete .resume sidecar
     - Atomic rename: ./out.pdf.part → ./out.pdf

Step 7: Decryption (if ECDH)
   If file was encrypted:
     - Find own entry in manifest's AccessList
     - X25519 ECDH: own_privkey × owner_pubkey → shared secret
     - HKDF-SHA256 derives wrapping key from shared secret
     - AES-GCM unwraps the per-file DEK
     - Each chunk decrypted in streaming mode

Step 8: Read Repair (background)
   After successful download, trigger background read repair:
     - Check if any replica nodes are under-replicated
     - If so, send chunks to restore replication factor
```

**Why this matters:** If you're downloading a 10 GB movie and chunk 2,491 out of 2,560 is corrupted, Hermond catches it immediately at step 4, bans the bad peer, and re-fetches just that one chunk from another peer. You don't download 10 GB, discover corruption at the end, and start over.

---

## Transport Layer: Why QUIC

Hermond uses QUIC (over UDP) instead of TCP for all peer-to-peer communication.

### The Head-of-Line Blocking Problem

With TCP, all messages share a single byte stream. If packet #3 in a sequence is lost, packets #4, #5, #6 are buffered by the OS until #3 is retransmitted — even if they belong to completely different logical operations.

```
TCP (single stream):
  Message A [chunk 1]──┐
  Message B [chunk 2]──┤── single TCP stream ──> receiver
  Message C [heartbeat]┘

  If Message A's packet is lost:
  B and C are BLOCKED until A is retransmitted.

QUIC (per-message streams):
  Message A [chunk 1]  ── stream 1 ──> receiver
  Message B [chunk 2]  ── stream 2 ──> receiver  (independent)
  Message C [heartbeat]── stream 3 ──> receiver  (independent)

  If stream 1's packet is lost:
  Streams 2 and 3 continue unaffected.
```

### Stream Lifecycle

Each logical message opens a new short-lived QUIC stream:
1. `OpenStreamSync()` — open stream
2. Write `[controlByte][4-byte length][payload]` in a single buffer (atomic header)
3. `Close()` — stream ends
4. Next message gets a entirely new stream

### Built-in TLS 1.3

QUIC mandates TLS 1.3. Hermond generates a self-signed certificate on first run. All peer traffic is encrypted at the transport level, independent of application-layer ECDH encryption.

### SmoothedRTT for Bandwidth Management

QUIC maintains a kernel-level smoothed RTT estimate per connection. Hermond's bandwidth manager reads `connection.ConnectionStats().SmoothedRTT` every 2 seconds to drive adaptive throttling — no application-level RTT probing needed.

### SendStreamThrottled

For large data transfers (chunk replication), the header and data are split:
- **Header** (< 100 bytes): sent at wire speed (no throttling)
- **Data** (4 MiB chunk): streamed through rate-limited writer via `io.CopyBuffer` with 32 KiB slices

This prevents the rate limiter from "wasting" tokens on tiny metadata bytes.

---

## Storage Engine

### Content-Addressable Storage (CAS)

Every chunk is stored at a path derived from its content hash:

```
SHA-256(ciphertext) = "816cc20437d8595b1a3f9e..."

Storage path:
  <root>/816cc/20437/d8595/b1a3f/9e.../md5(content).dat
         ^^^^^ ^^^^^ ^^^^^
         5-char directory segments from SHA-256 hex
```

**Deduplication:** If two different files contain an identical chunk (same plaintext → same ciphertext), only one physical file is stored. The second upload detects the existing CAS entry and skips writing.

**Integrity:** The path encodes the hash. Any bit-flip in stored data means the content no longer matches its path — detectable on any read.

### Chunking & Merkle Trees

Files are split into fixed **4 MiB chunks**. This size balances:
- Too small → excessive per-chunk overhead (metadata, replication round-trips, gossip)
- Too large → high memory pressure (each in-flight chunk occupies RAM)
- 4 MiB: a 1 GB file = 256 chunks, manageable for parallel download

**Merkle tree construction:**

```
                    Root Hash
                   /          \
            Hash(H01+H23)    Hash(H45+H55)    ← H5 duplicated (odd count)
            /         \       /         \
        Hash(H0+H1)  H(H2+H3) Hash(H4+H5)  Hash(H5+H5)
        /    \       /    \     /    \
      H0    H1    H2    H3   H4    H5     ← SHA-256 of plaintext chunks
```

- **Leaves** = SHA-256 hash of each plaintext chunk
- **Parents** = SHA-256(left_child || right_child)
- **Odd count handling:** last leaf is duplicated (standard Merkle convention)
- **Root** stored in the ChunkManifest

**Defense in depth:**
1. Per-chunk verification catches bad data on-the-fly during download
2. Merkle root verification after all chunks catches any structural corruption (missing/reordered chunks, manifest tampering)

### Compression Pipeline

Compression sits between chunking and encryption:

```
plaintext chunk → ShouldCompress? → CompressChunk (zstd) → Encrypt (AES-GCM)
```

**ShouldCompress heuristic (avoids wasting CPU on incompressible data):**

| Check | Threshold | Rationale |
|-------|-----------|-----------|
| Size | < 1 KiB → skip | Framing overhead exceeds savings |
| Magic bytes | JPEG, PNG, ZIP, gzip, zstd, MP4, MKV → skip | Already compressed formats |
| Shannon entropy | Sample first 4 KiB of chunk | |
| | < 7.0 bits/byte → compress | Compressible (text, source code, logs) |
| | >= 7.5 bits/byte → skip | High entropy = already compressed or random |

**Safety rule:** If compressed output >= original size, discard compressed version and store original. Never grow data.

Each chunk's `Compressed` flag is stored in the manifest so the download path knows whether to decompress.

### Directory Manifests

A directory upload produces a **two-level manifest**:

```
DirectoryManifest
├── StorageKey: "abc123/game-folder"
├── FileCount: 1072
├── TotalSize: 85 GB
├── MerkleRoot: (over per-file hashes)
├── Files:
│   ├── FileEntry { RelPath: "data/level1.pak", ManifestKey: "abc123/game-folder/data/level1.pak", ... }
│   ├── FileEntry { RelPath: "data/level2.pak", ManifestKey: "abc123/game-folder/data/level2.pak", ... }
│   └── ... (1072 entries)
└── ECDH fields (if encrypted)

Each FileEntry.ManifestKey → its own ChunkManifest (file-level chunks + Merkle root)
```

**Path traversal prevention:** `SafeOutputPath` validates that no `../` escape is possible. Double-checks: `filepath.Clean()` + reject absolute paths + verify final resolved path starts with output directory.

**Two-phase upload optimization:** See [Performance Optimizations](#performance-optimizations).

---

## Cryptography

### Identity System

Every Hermond node has a cryptographic identity:

| Key | Algorithm | Purpose |
|-----|-----------|---------|
| Ed25519 | Signing key | Sign manifests, prove file ownership |
| X25519 | Key exchange | ECDH encryption for direct transfers |

**Fingerprint** = first 16 hex characters of SHA-256(Ed25519 public key).
Used as namespace prefix for all files: `<fingerprint>/filename`. This prevents name collisions — two users can both upload "report.pdf" without conflict.

Generated once with `hermond identity init --alias "alice"` and stored at `~/.hermond/identity.json` (or `$DFS_IDENTITY_PATH`).

### ECDH Direct Encrypted Transfer

This is Hermond's signature feature. When you upload with `--share-with bob`:

```
┌─────────────────────────────────────────────────────────┐
│                ECDH Key Exchange Flow                    │
│                                                         │
│  Alice (uploader)              Bob (recipient)          │
│                                                         │
│  1. Generate random DEK                                 │
│     (256-bit AES key)                                   │
│                                                         │
│  2. Encrypt file with DEK                               │
│     (AES-256-GCM, 4MiB streaming chunks)                │
│                                                         │
│  3. ECDH: Alice_X25519_priv × Bob_X25519_pub            │
│     → shared_secret (32 bytes)                          │
│                                                         │
│  4. HKDF-SHA256(shared_secret)                          │
│     → wrapping_key (32 bytes)                           │
│                                                         │
│  5. AES-GCM-Wrap(wrapping_key, DEK)                     │
│     → wrapped_DEK = [12B nonce][ciphertext+16B tag]     │
│                                                         │
│  6. Store wrapped_DEK in manifest AccessList:           │
│     { RecipientPubKey: bob_x25519_pub,                  │
│       WrappedDEK: hex(wrapped_DEK) }                    │
│                                                         │
│  ─── File uploaded, replicated across cluster ───       │
│                                                         │
│  7. Bob downloads, finds his entry in AccessList        │
│                                                         │
│  8. ECDH: Bob_X25519_priv × Alice_X25519_pub            │
│     → same shared_secret (ECDH symmetry)                │
│                                                         │
│  9. HKDF → same wrapping_key                            │
│                                                         │
│  10. AES-GCM-Unwrap → original DEK                      │
│                                                         │
│  11. Decrypt file chunks with DEK                       │
│                                                         │
│  No server, relay, or intermediary ever sees the        │
│  plaintext or the DEK. Only Alice and Bob can decrypt.  │
└─────────────────────────────────────────────────────────┘
```

**Multiple recipients:** Each gets their own `AccessEntry` with the DEK wrapped for their specific public key. Recipient A cannot decrypt recipient B's wrapped DEK.

**Why separate DEK per file:** If one DEK is compromised, only that file is exposed. Different files can have different access lists without key management complexity.

### Manifest Signing

Every ChunkManifest is signed with the uploader's Ed25519 key:

1. Canonical payload = JSON of `{AccessList, Encrypted, FileKey, OwnerPubKey}` (alphabetically sorted keys for determinism)
2. SHA-256 hash of canonical JSON
3. Ed25519 signature of the hash
4. Signature stored in manifest as hex

**Why sign?** ECDH alone is unauthenticated (susceptible to MITM). The Ed25519 signature proves who authorized the access list. A malicious node cannot forge a manifest — the signature verification would fail.

---

## Cluster Coordination

### Consistent Hashing

Hermond uses a hash ring with **150 virtual nodes per physical node**.

```
         ┌─────── Hash Ring ───────┐
         │                         │
    A-v1 ●                    ● B-v3      (virtual nodes scattered)
         │    ● A-v2               │
         │         ● C-v1          │
         │              ● B-v1     │
    C-v3 ●              ● A-v3    │
         │                         │
         └─────────────────────────┘

    SHA-256("report.pdf") → position on ring
    → walk clockwise → collect 3 distinct physical nodes
    → [Node A, Node C, Node B]  (replication targets)
```

**GetNodes algorithm:**
1. SHA-256 hash the key, take first 8 bytes as uint64
2. Binary search sorted ring for first position >= hash
3. Walk clockwise, skip virtual nodes of already-seen physical nodes
4. Return up to RF distinct physical nodes

**Why 150 vnodes?** Balances load distribution uniformity (< 30% deviation in tests) against ring rebuild cost on topology changes.

### Gossip Protocol

Push-pull epidemic dissemination with O(log N) convergence.

**How it works:**

```
Every 200ms, each node runs a gossip round:

1. PUSH (Digest):
   Pick 3 random connected peers (FanOut = 3).
   Send compact digest: [(addr, generation, state)] for each known node.
   Only include nodes we're directly connected to (+ self).
   ↳ Prevents phantom addresses from propagating.

2. MERGE (at receiver):
   Compare received digest against local state.
   For entries with higher generation: request full NodeInfo.

3. PULL (Response):
   Send back full NodeInfo for entries the sender is behind on.

4. APPLY (at original sender):
   Apply received updates if generation > local.
   Merge metadata (identity info) regardless of generation.
   For newly-discovered addresses: initiate outbound dial.
```

**Convergence:** With FanOut=3, information reaches the entire cluster in O(log N) rounds. At 200ms intervals, a 100-node cluster converges in ~1.5 seconds.

**Fingerprint-based self-detection (Docker/NAT fix):** In Docker, multiple containers share `127.0.0.1`. Gossip can't use address to detect "is this me?" — instead it compares Ed25519 fingerprints. `HandleResponse` skips entries matching `selfFingerprint`, not `selfAddr`.

### Membership & Generation Counters

Each node has a **monotonically increasing generation counter** that guards against stale updates arriving out-of-order.

**Liveness states (monotonic progression):**
```
Alive (0) → Suspect (1) → Dead (2) → Left (3)
```
If two updates arrive with the same generation but different states, the higher state number wins. This prevents a stale "Alive" from overriding a legitimate "Dead".

**Generation initialization:** Set to `time.Now().UnixNano()` on startup. Guarantees the restarted node's generation exceeds any stale record from before the restart.

**SWIM Refutation Pattern:**
1. Node A is declared Dead by peers (ForceLocalState sets gen = now)
2. Node A comes back online, calls `BumpSelfGeneration()` (gen = now, which is higher)
3. Node A gossips its Alive state with the bumped generation
4. Peers see higher generation → accept Alive → Node A is back

**Metadata is separate from state:** `SetMetadata()` does not bump generation. This allows identity metadata to propagate through gossip independently of liveness state changes.

### Failure Detection (Phi Accrual)

Instead of fixed timeouts, Hermond uses the **Phi Accrual Failure Detector** — an adaptive algorithm that adjusts to network conditions.

**How it works:**

```
Per peer, maintain a ring buffer of heartbeat inter-arrival intervals.

On each heartbeat:
  interval = now - last_heartbeat_time
  append interval to ring buffer (max 200 samples)

To check if peer is alive:
  elapsed = now - last_heartbeat_time
  mean = average(intervals)
  stddev = stdev(intervals)  (clamped: min 1ms, min 10% of mean)

  y = (elapsed - mean) / (stddev × √2)
  cdf = 0.5 × (1 + erf(y))
  phi = -log10(1 - cdf)

  phi >= 8.0 → Suspect
  Suspect for > 30s → Dead
```

**Why adaptive?**
- Slow network: mean interval increases → phi threshold effectively increases → fewer false positives
- Jittery network: stddev increases → wider tolerance band → fewer false alarms
- Zero samples: phi = 0 (not suspicious)

**Dual-loop architecture:**
- **Sender loop** (every 1s): sends heartbeat to all peers
- **Reaper loop** (every 500ms): checks phi values, transitions Alive → Suspect → Dead
- Reaper runs at 2× heartbeat frequency for faster detection

### Quorum Reads & Writes

Hermond implements tunable consistency via W-of-N write quorum and R-of-N read quorum.

**Default configuration:** W=2, R=2, N=3 → `W + R = 4 > N = 3` → guarantees read-after-write consistency.

**Write flow:**
1. Create buffered channel for acks (capacity = N)
2. Local replica: goroutine writes locally, sends ack on channel
3. Remote replicas: goroutine sends `MessageQuorumWrite` (fire-and-forget)
4. Wait for W acks within 5s timeout
5. Early return on success; partial quorum returns error with actual count

**Read flow:**
1. Create buffered channel for responses (capacity = N)
2. Send `MessageQuorumRead` to all N replicas
3. Collect R responses, resolve conflicts via LWW (see below)
4. Graceful degradation: if timeout fires before R responses, resolve with what we have

**Lock-free pending operations:** `sync.Map[key → chan]` routes inbound acks to the correct waiter. Multiple concurrent writes to different keys don't contend.

### Vector Clocks & Conflict Resolution

**Vector clocks** track causality between writes:

```
Node A writes "file.txt" v1:  clock = {A: 1}
Node B reads v1, modifies:    clock = {A: 1, B: 1}
Node A writes again:          clock = {A: 2}

Compare {A: 1, B: 1} vs {A: 2}:
  B has B:1 (A doesn't) → B is greater on one dimension
  A has A:2 > A:1       → A is greater on another dimension
  → CONCURRENT (neither dominates)
```

**Last-Write-Wins (LWW) conflict resolution, in priority order:**

| Priority | Rule | Rationale |
|----------|------|-----------|
| 1 | Causal order wins | If clock A < clock B, B is the winner (strongest property) |
| 2 | Timestamp tiebreak | For concurrent clocks, higher `time.Now().UnixNano()` wins |
| 3 | First candidate | On exact timestamp tie, keep first in input slice |

### Hinted Handoff

When a write target is unreachable, the data is stored locally as a **hint** and replayed when the target comes back online.

```
Upload "report.pdf" → replicate to [A, B, C]
  A: success (local)
  B: success (remote)
  C: UNREACHABLE
    → Store hint: {key: "report.pdf", target: C, data: <encrypted bytes>}
    → Return success (quorum met with A + B)

Later, C reconnects:
  → OnPeerReconnect(C) → check hints for C
  → Replay: send stored data to C
  → On success: delete hint
  → After 5 failed attempts: give up, delete hint
```

**Safety mechanisms:**
- Per-target cap: 1000 hints max (oldest dropped when exceeded)
- Per-hint age limit: 24 hours (hourly purge removes stale hints)
- Hints are persisted to disk (survive process restart)
- `activeDeliveries` sync.Map prevents concurrent replay races

### Anti-Entropy (Merkle Sync)

Background repair process that runs every 10 minutes per peer pair.

**Protocol:**
1. Node A builds Merkle tree over its locally-held keys (sorted, deterministic)
2. Sends `MessageMerkleSync` with root hash to peer B
3. If B's root hash matches → in sync, done
4. If different → B sends full key list via `MessageMerkleDiffResponse`
5. Both sides build trees, compute diff:
   - Key in A but not B → A sends to B (push repair)
   - Key in B but not A → A requests from B (pull repair)

**Why Merkle trees?** Detects divergence in O(log k) tree traversal instead of O(k) full key comparison. For 10,000 keys, only ~14 hash comparisons needed to identify the differing subtree.

### Rebalancing

When the hash ring topology changes, chunks may need to migrate.

**Node joins:** For each locally-held key, ask the hash ring "who should own this now?" If the new node is in the result, send a copy.

**Node leaves:** For each locally-held key, check if replication factor is met. If under-replicated, send copies to other nodes in the new responsible set.

**Rate limiting:** 50ms delay between consecutive chunk migrations. This prevents rebalancing from saturating the network and blocking user uploads. At 50ms per chunk, 466 chunks migrate in ~23 seconds, interleaved with normal traffic.

**Mutual exclusion:** Only one rebalance operation runs at a time (`inProgress` flag). Prevents storms when multiple nodes join/leave simultaneously.

---

## Bandwidth Management: LEDBAT-lite

Hermond automatically manages bandwidth — no user configuration. The system monitors network congestion and backs off to keep the LAN smooth for everyone.

**Algorithm:**

```
Every 2 seconds, per-peer:
  rtt = connection.ConnectionStats().SmoothedRTT  (from QUIC)

  if rtt < 5ms:
    rate *= 1.20  (grow 20% — network is clear)
  elif rtt > 25ms:
    rate *= 0.60  (slash 40% — bufferbloat detected)
  else:
    hold steady

  Clamp: 1 MB/s <= rate <= 200 MB/s
  Initial rate: 50 MB/s
  Burst size: 64 KiB
```

**Why no user-configurable limits?** Users are selfish — they'd set it to maximum and saturate the network. LEDBAT detects congestion via RTT increases and automatically yields bandwidth.

**Critical implementation detail — burst-size chunking:**

The token bucket has a burst of 64 KiB. But `bytes.Reader.WriteTo` bypasses `io.CopyBuffer`'s 32 KiB buffer and sends the full chunk (100 KiB+) in one `Write()` call. Calling `WaitN(len(p))` where `len(p) > burst` would deadlock.

**Fix:** `RateLimitedWriter.Write()` internally chunks data into burst-sized pieces. Each piece waits for tokens before writing. Same fix for `RateLimitedReader.Read()` — caps the read buffer to burst before calling underlying Read. This chunking is invisible to QUIC — backpressure propagates naturally.

---

## Download Resume

If a download is interrupted (network drop, laptop closed, process killed), Hermond resumes from where it left off.

**On-disk state:**

```
./movie.mkv.part     — sparse file with downloaded chunks at correct offsets
./movie.mkv.resume   — append-only log:
  Line 1: {"key":"abc/movie.mkv","merkleRoot":"f3a1...","chunks":256,"size":1073741824}
  Line 2: 0
  Line 3: 1
  Line 4: 5       ← chunk indices, one per line
  Line 5: 2
  ...
```

**Resume flow on restart:**
1. Open `.resume` sidecar, parse header
2. Validate: does key, merkle root, chunk count, total size match current manifest?
   - If no (file was re-uploaded): discard sidecar, start fresh
3. **Fast-verify:** For each chunk index in sidecar:
   - Seek to offset in `.part` file
   - Read chunk bytes, compute SHA-256
   - Compare against manifest hash
   - Only verified chunks enter skip set (never trust sidecar over disk state)
4. Fetch only missing chunks from network

**Why append-only log instead of BoltDB?**
- Simpler, faster, crash-proof at byte level
- No transaction semantics needed (log is idempotent)
- O(1) per chunk write (single append syscall)
- No write-ahead log overhead

**Atomic completion:**
1. All chunks downloaded + verified
2. Merkle root re-verified
3. Delete `.resume` sidecar
4. Atomic rename: `.part` → final path (single syscall, no partial state)

---

## Peer Selection (EWMA Latency)

When downloading chunks from multiple peers, Hermond picks the best peer for each chunk using Exponentially Weighted Moving Average latency tracking.

**EWMA formula:**
```
ewma_new = 0.2 × sample + 0.8 × ewma_old
```
Each new measurement contributes 20%, history contributes 80%. Smooths jitter while reacting to trend changes.

**Scoring (load-balanced):**
```
score(peer) = ewma_latency × (1 + 0.5 × active_downloads)
```
Lower score wins. A peer with 3 active downloads is penalized 2.5× vs an idle peer — prevents hot spots.

**Implicit bad peer banning:**
When a chunk fails verification, `RecordLatency(peer, 10*time.Minute)` is called. The EWMA jumps to ~600 seconds, making the peer's score astronomical. It won't be selected for any chunk downloads for approximately 10 minutes (EWMA decays over subsequent good measurements, if any).

No explicit blocklist needed — just extremely low attractiveness in the scoring function.

---

## NAT Traversal & External Address Discovery

Hermond works across network boundaries (Docker containers, NAT, different subnets) via two mechanisms:

### STUN-Based NAT Traversal

On startup, the node queries a STUN server to discover its external IP:port mapping. Non-blocking — if STUN fails (no internet, corporate firewall), the node falls back to LAN-only mode with a warning.

### Announce-Ack Handshake (Zero-Config)

Even without STUN, nodes discover their external address via the announce handshake:

```
Node A (inside Docker, thinks it's 172.17.0.2:3000)
  connects to →
Node B (host, sees A as 172.22.0.3:3000)

1. A sends MessageAnnounce{ListenAddr: "172.17.0.2:3000"}
2. B remaps: "ah, this peer is reachable at 172.22.0.3:3000"
   → Adds A to hash ring under canonical address
   → Replies: MessageAnnounceAck{YourAddr: "172.22.0.3:3000"}
3. A receives ack → "my external address is 172.22.0.3:3000"
   → Updates effectiveSelfAddr()
   → Re-registers in hash ring, cluster, gossip under external address
4. All future communication uses the canonical external address

Result: Docker container A is a full cluster citizen, reachable by all peers.
```

**Identity metadata migration:** If identity metadata arrives before the announce (stored under ephemeral address), `handleAnnounce` migrates it to the canonical address. Prevents identity loss during the handshake race.

---

## Persistent State (BoltDB)

All local state is persisted in BoltDB (embedded key-value store, single-file database).

**Buckets:**

| Bucket | Key | Stores |
|--------|-----|--------|
| `uploads` | storage key | Upload history (name, size, type, public flag, timestamp) |
| `downloads` | storage key | Download history (name, size, source, timestamp) |
| `public_files` | storage key | Public file catalog (browsable by peers) |
| `tombstones` | storage key | Soft-deleted files (propagated via gossip) |
| `known_peers` | address | Auto-reconnect list (survives restart) |
| `ignored_peers` | address/fingerprint | Blocklist |
| `inbox` | entry ID | Direct-share notifications received |
| `outbox` | entry ID | Queued notifications for offline peers |
| `transfer_history` | timestamp_id | Completed/failed transfer records |

**Tombstone soft-delete:** `hermond remove <name>` doesn't immediately delete data. Instead, a tombstone record is created with `DeletedAt` timestamp and propagated via gossip. This ensures eventual consistency — all nodes eventually learn the file was deleted.

**Public catalog change detection:** `PublicCatalogSummary()` computes SHA-256 of sorted public keys (first 16 hex chars). Gossip metadata includes this hash. Peers compare hashes — if different, they request the full catalog. Avoids sending the full file list every gossip round.

---

## Transfer Management

The transfer manager provides pause/resume/cancel for active transfers.

**Architecture:** `trackedTransfer` wraps `TransferInfo` (pure data) with a `sync.Mutex` and `context.CancelFunc`. The mutex is on the wrapper, not the data struct — this avoids Go's `go vet` copy-by-value warnings when returning snapshots.

```go
type trackedTransfer struct {
    TransferInfo              // embedded data (safe to copy)
    cancel context.CancelFunc // cancellation
    mu     sync.Mutex         // guards mutations
}
```

**Pause:** Cancels the context (stops the download goroutine) but keeps the entry in the registry with `Status = Paused`. The `.part` file and `.resume` sidecar remain on disk.

**Resume:** Creates a fresh context and cancel function. The download goroutine is re-spawned, reads the `.resume` sidecar, fast-verifies chunks, and continues from where it left off.

**Cancel:** Cancels the context AND removes the entry from the registry.

---

## IPC Protocol

CLI/TUI communicates with the daemon via Unix domain socket using a binary protocol.

**Wire format:**
```
[1 byte opcode][payload bytes...]

Response format varies by opcode:
  Success: [0x01][optional data...]
  Error:   [0x00][error message bytes]
  Progress: [0x02][4B completed][4B total]  (file)
            [0x02][4B fileIdx][4B fileTotal][4B chunkIdx][4B chunkTotal]  (directory)
```

**Opcodes:**

| Opcode | Operation |
|--------|-----------|
| 0x01 | Plaintext file upload |
| 0x02 | Plaintext file download (streaming) |
| 0x03 | ECDH encrypted file upload |
| 0x04 | ECDH encrypted file download |
| 0x05 | Get manifest info (unified: checks dir first, then file) |
| 0x06 | Resolve alias → fingerprint + keys |
| 0x07 | Download-to-file (direct-to-disk) |
| 0x08 | ECDH download-to-file |
| 0x09 | Directory upload (plaintext) |
| 0x0A | Directory download (plaintext) |
| 0x0B | Directory upload (ECDH) |
| 0x0C | Directory download (ECDH) |
| 0x0D | List uploads (JSON response) |
| 0x0E | List downloads (JSON response) |
| 0x0F | Browse peer (JSON response) |
| 0x10 | List peers (JSON response) |
| 0x11 | Node status (JSON response) |
| 0x12 | List active transfers (JSON response) |
| 0x13 | Cancel transfer |
| 0x14 | Pause transfer |
| 0x15 | Resume transfer |
| 0x17 | Remove file (tombstone) |
| 0x18 | Network search (JSON response) |

---

## Performance Optimizations

### Zero-Allocation Upload Pipeline

Four levels of `sync.Pool` eliminate GC pauses during bulk uploads:

```
chunkReadBufPool   → 4 MiB read buffers (reused per chunk read)
chunkDataPool      → output slices traveling through pipeline (returned after replication)
compBufPool        → zstd compression output buffers
encBufPool         → AES-GCM ciphertext buffers (ciphertext + nonce + tag)
```

Without pools, uploading 1000 chunks allocates 4 GB of transient buffers → GC stop-the-world pauses of 100-200ms dominate latency. With pools, buffers are borrowed and returned — near-zero GC pressure.

### Two-Phase Directory Upload

Uploading a directory with 1072 files normally requires:
- Per-file: 3 BoltDB fsyncs (metadata write) = 3,216 fsyncs total
- Plus directory manifest = 3,217 fsyncs

**Optimization:**
- **Phase 1:** Process all files with `StoreDataCollectMeta(deferMeta=true)` — uploads chunks but defers metadata writing
- **Phase 2:** Single `WithBatch()` call writes all file metadata + directory manifest atomically (1 fsync total)

**Result:** 1072 files upload: 2.457s → 1.995s (~19% faster). The floor is CAS I/O (hashing + writing chunk files to disk).

### Single-Chunk File Optimization

For files smaller than one chunk (< 4 MiB): skip the upload resume sidecar entirely. There's only one chunk — either it's uploaded or it's not. No need for a sidecar to track partial progress.

### Sliding Window Download

For streaming downloads (pipes, IPC sockets without random-access):
- Window of `MaxParallel` (4) in-flight chunks
- Out-of-order chunks buffered in a map
- When the expected next chunk arrives, flush all consecutive chunks
- Backpressure: workers pause when window is full, preventing unbounded memory growth

Memory bounded at `MaxParallel × ChunkSize = 16 MiB` regardless of file size.

---

## Package Map

```
hermond/
├── Server/
│   ├── server.go              Core orchestrator (peer lifecycle, message dispatch)
│   ├── downloader/            Parallel multi-source chunk fetcher
│   ├── transfer/              Transfer registry (pause/resume/cancel)
│   └── ratelimit/             LEDBAT-lite adaptive bandwidth
│
├── Storage/
│   ├── storage.go             CAS engine (SHA-256 paths, AES-GCM encryption)
│   ├── chunker/               4 MiB chunking, SHA-256 hashing, Merkle trees
│   ├── compression/           Zstd compression with entropy heuristic
│   ├── dirmanifest/           Multi-file directory manifests
│   ├── resume/                Download resume (append-only sidecar)
│   └── pending/               Upload resume (append-only sidecar)
│
├── Cluster/
│   ├── gossip/                Push-pull epidemic protocol (O(log N))
│   ├── hashring/              Consistent hashing (150 vnodes, SHA-256)
│   ├── quorum/                W-of-N write, R-of-N read quorum
│   ├── membership/            Liveness states + generation counters
│   ├── handoff/               Hinted handoff (buffer for unreachable nodes)
│   ├── rebalance/             Chunk migration on topology changes
│   ├── selector/              EWMA latency tracking + peer selection
│   ├── failure/               Phi accrual failure detection
│   ├── merkle/                Anti-entropy Merkle tree sync
│   ├── vclock/                Vector clocks (causality tracking)
│   └── conflict/              LWW conflict resolution
│
├── Crypto/
│   ├── identity/              Ed25519 + X25519 keypairs
│   └── envelope/              ECDH key wrapping + Ed25519 signing
│
├── Peer2Peer/
│   ├── quic/                  QUIC transport (TLS 1.3, per-stream)
│   └── nat/                   STUN-based NAT traversal
│
├── State/                     BoltDB persistent state (9 buckets)
├── ipc/                       Binary IPC protocol (18 opcodes)
├── tui/                       Bubble Tea terminal UI (4 tabs)
├── cmd/                       Cobra CLI commands
└── Observability/
    ├── health/                HTTP health + pprof endpoints
    ├── metrics/               Prometheus instrumentation
    ├── tracing/               OpenTelemetry (OTLP → Jaeger)
    └── logging/               Zerolog structured logging
```

---

*This document reflects the architecture as of v0.1.x. Last updated March 2026.*
