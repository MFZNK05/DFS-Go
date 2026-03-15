# Hermod — Architecture Deep Dive

A comprehensive technical reference for engineers who want to understand how Hermod works under the hood. Every design decision, algorithm, and safety mechanism is documented here — even if you didn't build the project, you should be able to understand it fully after reading this.

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
- [Bandwidth Management: QUIC Native](#bandwidth-management-quic-native)
- [Download Resume](#download-resume)
- [Peer Selection (EWMA Latency)](#peer-selection-ewma-latency)
- [NAT Traversal & External Address Discovery](#nat-traversal--external-address-discovery)
- [Persistent State (BoltDB)](#persistent-state-boltdb)
- [Transfer Management](#transfer-management)
- [IPC Protocol](#ipc-protocol)
- [mDNS LAN Auto-Discovery](#mdns-lan-auto-discovery-peer2peermdns)
- [IP Resolution & Address Normalization](#ip-resolution--address-normalization-serverservergo)
- [Cross-Platform Firewall Auto-Configuration](#cross-platform-firewall-auto-configuration-cmdfirewallgo)
- [TUI Logging Safety](#tui-logging-safety-cmdunifiedgo)
- [Dual-Port Architecture: Control Plane + Data Plane](#dual-port-architecture-control-plane--data-plane)
- [Cross-Platform Connectivity: Challenges & Solutions](#cross-platform-connectivity-challenges--solutions)
- [Daemon/TUI Lifecycle: Process Architecture](#daemontui-lifecycle-process-architecture)
- [Swarm Cache & Seed-in-Place](#swarm-cache--seed-in-place)
- [Memory Monitor & OOM Forensics](#memory-monitor--oom-forensics)
- [Upload Priority & Background Task Yielding](#upload-priority--background-task-yielding)
- [Performance Optimizations](#performance-optimizations)
- [Package Map](#package-map)

---

## System Overview

Hermod is a peer-to-peer distributed file system. Every node is both a client and a server. There is no central coordinator — nodes discover each other via gossip, distribute files using consistent hashing, and replicate data to survive node failures.

```
┌─────────────────────────────────────────────────────────────┐
│                        Hermod Node                         │
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
│                       │        │  ┌────────┐ ┌────────┐ │  │
│                       │        │  │ mDNS   │ │Firewall│ │  │
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

When a user runs `hermod upload --name "report.pdf" --file ./report.pdf --public`:

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

When a user runs `hermod download --name "report.pdf" --output ./out.pdf --from alice`:

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

**Why this matters:** If you're downloading a 10 GB movie and chunk 2,491 out of 2,560 is corrupted, Hermod catches it immediately at step 4, bans the bad peer, and re-fetches just that one chunk from another peer. You don't download 10 GB, discover corruption at the end, and start over.

---

## Transport Layer: Why QUIC

Hermod uses QUIC (over UDP) instead of TCP for all peer-to-peer communication.

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

QUIC mandates TLS 1.3. Hermod generates a self-signed certificate on first run. All peer traffic is encrypted at the transport level, independent of application-layer ECDH encryption.

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

Every Hermod node has a cryptographic identity:

| Key | Algorithm | Purpose |
|-----|-----------|---------|
| Ed25519 | Signing key | Sign manifests, prove file ownership |
| X25519 | Key exchange | ECDH encryption for direct transfers |

**Fingerprint** = first 16 hex characters of SHA-256(Ed25519 public key).
Used as namespace prefix for all files: `<fingerprint>/filename`. This prevents name collisions — two users can both upload "report.pdf" without conflict.

Generated once with `hermod identity init --alias "alice"` and stored at `~/.hermod/identity.json` (or `$DFS_IDENTITY_PATH`).

### ECDH Direct Encrypted Transfer

This is Hermod's signature feature. When you upload with `--share-with bob`:

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

Hermod uses a hash ring with **150 virtual nodes per physical node**.

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

Instead of fixed timeouts, Hermod uses the **Phi Accrual Failure Detector** — an adaptive algorithm that adjusts to network conditions.

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

Hermod implements tunable consistency via W-of-N write quorum and R-of-N read quorum.

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

## Bandwidth Management: QUIC Native

Hermod delegates all bandwidth management to QUIC's built-in congestion control. There is no application-layer throttling.

### Why No Application-Layer Throttling

An earlier version (v0.2.x) implemented a custom LEDBAT-lite layer: an application-level token bucket that monitored `SmoothedRTT` and adjusted rate limits dynamically. It was removed for three reasons:

1. **Redundant with QUIC:** QUIC already implements cubic/BBR congestion control at the transport level. Adding a second throttling layer on top created double-buffering — QUIC would back off, then the token bucket would independently back off, resulting in severe bandwidth underutilization (often 10-20% of available capacity).

2. **Burst-size deadlocks:** The token bucket had a burst of 64 KiB, but `bytes.Reader.WriteTo` could send 4 MiB in a single `Write()` call, exceeding the burst limit. This required complex workarounds (chunking writes into burst-sized pieces) that added latency without improving throughput.

3. **Dual-port separation solved the real problem:** The original motivation for LEDBAT was preventing chunk transfers from starving heartbeats. The [dual-port architecture](#dual-port-architecture-control-plane--data-plane) eliminates this entirely — control messages have their own dedicated QUIC connection with independent congestion windows.

### Current Design

- **No rate limiting code** — the `Server/ratelimit/` package was deleted
- **QUIC handles congestion** — each connection maintains its own congestion window, RTT estimates, and flow control credits
- **Dual-port isolation** — control messages are never starved by data transfers (separate UDP sockets, independent congestion state)
- **Per-stream flow control** — QUIC's per-stream windows prevent any single chunk transfer from monopolizing a connection

---

## Download Resume

If a download is interrupted (network drop, laptop closed, process killed), Hermod resumes from where it left off.

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

When downloading chunks from multiple peers, Hermod picks the best peer for each chunk using Exponentially Weighted Moving Average latency tracking.

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

Hermod works across network boundaries (Docker containers, NAT, different subnets) via two mechanisms:

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

**Tombstone soft-delete:** `hermod remove <name>` doesn't immediately delete data. Instead, a tombstone record is created with `DeletedAt` timestamp and propagated via gossip. This ensures eventual consistency — all nodes eventually learn the file was deleted.

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
| 0x19 | Connect to peer |
| 0x1A | Disconnect and blocklist peer |
| 0x1B | Full browse (all files from peer) |
| 0x1C | Send direct-share notification |
| 0x1D | List inbox (received notifications) |
| 0x1E | Unblock peer |
| 0x1F | Scan LAN via mDNS |
| 0x20 | Seed-in-place upload (no CAS copy) |
| 0x21 | Seed-in-place ECDH upload |
| 0x22 | Seed-in-place directory upload |
| 0x23 | Seed-in-place ECDH directory upload |
| 0x24 | Graceful daemon shutdown |

---

## Dual-Port Architecture: Control Plane + Data Plane

One of Hermod's most impactful architectural decisions is separating network traffic into two dedicated QUIC ports: a **control port** (N) for cluster coordination and a **data port** (N+1) for bulk chunk transfers.

### The Problem: Head-of-Line Blocking at the Application Layer

Even though QUIC eliminates TCP's head-of-line blocking at the transport level (each stream is independent), a single UDP socket still shares the same congestion window and flow control budget. When a 4 MiB chunk transfer saturates the connection, tiny gossip digests (< 500 bytes) and heartbeats (< 100 bytes) queue behind the chunk data at the application level. This delays failure detection (heartbeats arrive late → phi accrual spikes → false suspect/dead transitions) and slows membership convergence.

```
PROBLEM — Single Port:
┌─────────────────────────────────────────────┐
│           Port 3000 (shared UDP socket)      │
│                                              │
│  Gossip [500B] ─┐                            │
│  Heartbeat [100B]┼── compete for same ──→ 🐢│
│  Chunk [4 MiB]  ─┘   congestion window       │
│                                              │
│  Result: heartbeat delayed 200ms → phi spike │
│          → false Dead transition              │
└─────────────────────────────────────────────┘

SOLUTION — Dual Port:
┌──────────────────────┐  ┌──────────────────────┐
│  Port 3000 (control) │  │  Port 3001 (data)    │
│                      │  │                      │
│  Gossip [500B]    ✓  │  │  Chunk [4 MiB]    ✓  │
│  Heartbeat [100B] ✓  │  │  StoreFile        ✓  │
│  Announce         ✓  │  │  GetFile          ✓  │
│  Metadata         ✓  │  │  LocalFile        ✓  │
│                      │  │  GetChunkSwarm    ✓  │
│  Tiny QUIC windows   │  │  Large QUIC windows  │
│  (256KB/stream)      │  │  (8MB/stream)        │
└──────────────────────┘  └──────────────────────┘

Control messages: sub-millisecond delivery (no congestion from chunks)
Data transfers: full bandwidth (no starvation from gossip flood)
```

### Port Assignment

Automatic — the user configures one port; the data port is always N+1:

```go
// cmd/daemon.go
if factory.ProtocolFromEnv() == factory.ProtocolQUIC {
    dataPort := fmt.Sprintf(":%d", portNum+1)
    makeOpts.DataPort = dataPort
}
```

User runs `hermod --port :3000` → control on `:3000`, data on `:3001`. No extra configuration.

### QUIC Flow Control Tuning by Role

Each transport has tuned flow control windows matching its traffic pattern:

| Role | Stream Window | Connection Window | Rationale |
|------|---------------|-------------------|-----------|
| `RoleControl` | 256 KiB | 1 MiB | Gossip/heartbeat are tiny — small windows prevent buffer bloat |
| `RoleData` | 8 MiB | 64 MiB | Chunks are 4 MiB — window must hold ≥ 2 in-flight chunks |
| `RoleUnified` (legacy) | 8 MiB | 32 MiB | Balanced for single-port backward compatibility |

ALPN protocol negotiation prevents misrouted connections: `"dfs-ctrl"` vs `"dfs-data"` vs `"dfs-quic"`.

### DialDual: Atomic Parallel Connection

When connecting to a peer, both transports are dialed simultaneously:

```go
// Peer2Peer/quic/transport.go
func DialDual(ctrlTransport, dataTransport *Transport, addr string) error {
    host, portStr, _ := net.SplitHostPort(addr)
    port, _ := strconv.Atoi(portStr)
    dataAddr := net.JoinHostPort(host, strconv.Itoa(port+1))

    var wg sync.WaitGroup
    var ctrlErr, dataErr error
    wg.Add(2)
    go func() { defer wg.Done(); ctrlErr = ctrlTransport.Dial(addr) }()
    go func() { defer wg.Done(); dataErr = dataTransport.Dial(dataAddr) }()
    wg.Wait()

    if ctrlErr != nil {
        return fmt.Errorf("control dial failed: %w", ctrlErr)
    }
    if dataErr != nil {
        log.Printf("[DUAL-DIAL] data dial failed (peer may be single-port): %v", dataErr)
        // Degraded mode: all traffic over control port. Still works.
    }
    return nil
}
```

**Backward compatible:** If the remote peer is an older single-port node, the data dial fails gracefully. The system falls back to unified mode — all traffic over the control port. No crash, no error.

### Data Peer Routing with Fallback

```go
// Server/server.go
func (s *Server) getDataPeer(addr string) (peer2peer.Peer, bool) {
    // Try data-plane peer first (fast path)
    s.dataPeerLock.RLock()
    dp, ok := s.dataPeers[addr]
    s.dataPeerLock.RUnlock()
    if ok { return dp, true }

    // Fallback: control-plane peer (single-port mode)
    s.peerLock.RLock()
    cp, ok := s.peers[addr]
    s.peerLock.RUnlock()
    return cp, ok
}
```

Data peers are indexed by their **control port address** (derived by subtracting 1 from the data port). This allows lookup by canonical peer identity while routing through the correct transport.

### Message Routing Rules

The `dataLoop()` goroutine explicitly rejects non-data messages — any control message that arrives on the data port is logged and dropped:

| Message Type | Port | Handler |
|---|---|---|
| `MessageGossipDigest` | Control (N) | `handleGossipDigest` |
| `MessageGossipResponse` | Control (N) | `handleGossipResponse` |
| `MessageHeartbeat` | Control (N) | `handleHeartbeat` |
| `MessageAnnounce` / `Ack` | Control (N) | `handleAnnounce` / `handleAnnounceAck` |
| `MessageIdentityMeta` | Control (N) | `handleIdentityMeta` |
| `MessageGetManifest` | Control (N) | `handleGetManifest` |
| `MessageStoreFile` | **Data (N+1)** | `handleStoreMessage` |
| `MessageGetFile` | **Data (N+1)** | `handleGetMessage` |
| `MessageLocalFile` | **Data (N+1)** | `handleLocalMessage` |
| `MessageGetChunkSwarm` | **Data (N+1)** | `handleGetChunkSwarmBidi` |

### Impact

In a 3-node cluster with concurrent upload + gossip:
- **Before (single-port):** Heartbeat jitter spikes to 200ms during 4 MiB chunk transfer → phi reaches suspect threshold → false Dead transitions
- **After (dual-port):** Heartbeat delivery remains < 5ms regardless of data transfer load → zero false positives

---

## Cross-Platform Connectivity: Challenges & Solutions

Building a P2P application that works identically on Linux, macOS, and Windows required solving several non-obvious platform differences.

### Challenge 1: IPC — Unix Sockets vs Windows Named Pipes

Unix domain sockets don't exist on Windows. Named pipes have entirely different semantics (virtual filesystem, no file on disk).

```
Linux/macOS:                        Windows:
  /tmp/hermod-3000.sock               \\.\pipe\hermod-3000
  Real file on disk                   Virtual (no file to clean up)
  net.Listen("unix", path)            winio.ListenPipe(path, nil)
  os.RemoveAll() for cleanup          No cleanup needed
```

**Solution:** Build-tag-gated implementations with a shared interface:

```go
// cmd/ipcconn_unix.go — //go:build !windows
func ipcListen(path string) (net.Listener, error) {
    return net.Listen("unix", path)
}
func platformSocketPath(port string) string {
    return filepath.Join(os.TempDir(), fmt.Sprintf("hermod-%s.sock", port))
}

// cmd/ipcconn_windows.go — //go:build windows
func ipcListen(path string) (net.Listener, error) {
    return winio.ListenPipe(path, nil)  // github.com/Microsoft/go-winio
}
func platformSocketPath(port string) string {
    return fmt.Sprintf(`\\.\pipe\hermod-%s`, port)
}
```

Originally used `npipe.v2`, but it didn't support Windows ARM64. Replaced with `go-winio` (maintained by Microsoft, supports all architectures).

### Challenge 2: Firewall Blocks Inbound UDP

QUIC runs over UDP. **Windows and macOS block inbound UDP by default.** A user installs Hermod, starts it, and no peers can connect — because the OS firewall silently drops all inbound packets. This is the #1 reason peers can't discover each other.

**Solution:** Auto-configure the firewall on first startup, per-platform:

```
Windows:
  1. Try netsh directly (may already be admin)
  2. If not admin → ShellExecuteW("runas", "netsh", ...)
     → Standard UAC dialog appears, user clicks "Yes" once
  3. Adds port-only rules (no program= filter for binary-location independence)

macOS:
  1. osascript "with administrator privileges"
     → macOS shows native password prompt
  2. socketfilterfw --add + --unblockapp

Linux:
  1. Try ufw → firewall-cmd → iptables (in order)
  2. Uses sudo -n (non-interactive) if not root
  3. No GUI dependency (works on headless/SSH)
```

**Version-gated re-trigger:** A flag file (`~/.hermod/.firewall-configured`) contains a version string (e.g., `v4`). On upgrade, version bumps force re-execution of firewall rules. This allowed us to fix the Windows `program=` filter bug — `v3` had `program=hermod.exe` which broke when the binary moved; `v4` uses port-only rules.

Three ports are opened: control port, data port (+1), and mDNS (5353).

### Challenge 3: Process Detachment

The daemon must survive the parent terminal closing. Unix and Windows handle this differently:

```go
// cmd/detach_unix.go — //go:build !windows
func detachCmd(cmd *exec.Cmd) {
    cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
    // New session → disowned from controlling terminal
}

// cmd/detach_windows.go — //go:build windows
func detachCmd(cmd *exec.Cmd) {
    cmd.SysProcAttr = &syscall.SysProcAttr{
        CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
    }
}
```

Both redirect stdout/stderr to `~/.hermod/daemon.log` for post-mortem debugging.

### Challenge 4: mDNS Multicast Isolation

mDNS uses multicast UDP (224.0.0.251:5353). On campus networks, multicast is frequently blocked between clients (wireless isolation). Two peers on `10.145.16.x` and `10.145.113.x` are on different broadcast domains — mDNS packets never reach each other.

**Solution:** mDNS is for **same-subnet zero-config** discovery only. Cross-subnet connectivity uses explicit `--peer <ip:port>` or the TUI connect dialog. Both work because QUIC connections are unicast (not multicast).

### Challenge 5: Binary Runs from Any Working Directory

Early versions stored CAS data, metadata DB, and state DB relative to the current working directory. Running `hermod` from different directories created fragmented storage.

**Solution:** All runtime data roots under `~/.hermod/` via `MakeServerOpts.DataDir`:

```go
// cmd/daemon.go
homeDir, _ := os.UserHomeDir()
dataDir := filepath.Join(homeDir, ".hermod")
os.MkdirAll(dataDir, 0700)
makeOpts.DataDir = dataDir

// Server/server.go — MakeServer()
if makeOpts.DataDir != "" {
    pathAddr = filepath.Join(makeOpts.DataDir, pathAddr)
}
// All CAS storage, metadata DBs, and network state now under ~/.hermod/
```

```
~/.hermod/
├── identity.json          # Ed25519 + X25519 keypairs (permanent)
├── config.json            # Port preference (from first-run prompt)
├── daemon.log             # Daemon stdout/stderr
├── tui-debug.log          # Bubbletea debug output
├── state-3000.db          # BoltDB (uploads, downloads, peers)
├── .firewall-configured   # Version flag ("v4\n")
├── _3000_metadata.db      # Chunk manifests, file metadata
├── _3000_network/         # CAS storage (SHA-256 dirs)
│   └── <sha256 segments>/<md5>.dat
└── _3000_network/.hints/  # Hinted handoff queue
```

Port-specific DB names (`state-3000.db`) allow multiple daemons on different ports simultaneously.

---

## Daemon/TUI Lifecycle: Process Architecture

Hermod follows a **daemon + client** model inspired by DC++: a background daemon handles all P2P networking, and the TUI is a stateless client that connects via IPC.

### Lifecycle Flow

```
User runs: hermod
                │
                ▼
        ┌──────────────┐
        │ ensureIdentity│ ← First run? Prompt for alias + port
        └──────┬───────┘
               │
        ┌──────▼───────┐
        │isDaemonRunning│ ← Dial socket with 500ms timeout
        └──┬────────┬──┘
           │        │
         yes       no
           │        │
           │   ┌────▼─────┐
           │   │forkDaemon │ ← Resolve executable, build args,
           │   │           │   redirect stdout/stderr to daemon.log,
           │   │           │   call detachCmd(), cmd.Start(), cmd.Process.Release()
           │   └────┬──────┘
           │        │
           │   ┌────▼──────────┐
           │   │waitForDaemon  │ ← Poll socket every 100ms until connectable
           │   │(10s timeout)  │   or timeout
           │   └────┬──────────┘
           │        │
        ┌──▼────────▼──┐
        │  Launch TUI   │ ← Stateless IPC client (Bubble Tea)
        │  (blocks)     │   Connect to daemon socket, render terminal UI
        └──────┬────────┘
               │
         User presses 'q'
               │
        ┌──────▼────────────────────────────────────┐
        │ "Hermod is still running in the background.│
        │  Use 'hermod stop' to shut it down."       │
        └───────────────────────────────────────────┘
```

### Stale Socket Detection

A socket file existing on disk doesn't mean a daemon is running. The previous daemon may have crashed without cleaning up.

```go
func isDaemonRunning(sockPath string) bool {
    conn, err := ipcDialTimeout(sockPath, 500*time.Millisecond)
    if err != nil {
        cleanupSocket(sockPath)  // Remove stale socket file
        return false
    }
    conn.Close()
    return true  // Something answered — daemon is alive
}
```

This prevents "address already in use" errors when restarting after a crash.

### Graceful Shutdown (`hermod stop`)

```
hermod stop --node :3000
        │
        ▼
  ┌─────────────┐
  │ Dial socket  │ ← 500ms timeout
  └──────┬──────┘
         │
  ┌──────▼──────┐
  │ Send 0x24   │ ← OpShutdown opcode (1 byte)
  │ (shutdown)  │
  └──────┬──────┘
         │
  Server side:
  ┌──────▼──────────────┐
  │ writeStatus(OK)      │
  │ conn.Close()         │
  │ getDaemonStopFunc()()│ ← Registered by StartDaemonAsync
  │   → GracefulShutdown │     Broadcast MessageLeaving to peers
  │   → Close listeners  │     Close IPC socket, QUIC transports
  │   → Flush tracing    │
  │ os.Exit(0)           │
  └─────────────────────┘
```

The shutdown function is registered via `setDaemonStopFunc()` at daemon startup, making it callable from the IPC handler without circular dependencies.

### First-Time Setup

On first run (no identity file exists), the user is prompted for alias and port:

```
$ hermod
Welcome to Hermod!
Enter your alias: alice
Control port (default 3000, data port will be 3001): 3000

Identity created: alice (fingerprint: c34144eb91e1cb10)
Port configured: 3000 (data port: 3001)
```

Both are saved to disk (`~/.hermod/identity.json` and `~/.hermod/config.json`). Subsequent runs load the config unless `--port` is explicitly passed (detected via `cmd.Flags().Changed("port")` — not string comparison, which can't distinguish "user passed :3000" from "cobra default :3000").

---

## Swarm Cache & Seed-in-Place

Traditional P2P file systems require the uploader to copy file data into internal storage (CAS) before sharing. For a 50 GB game folder, this means 50 GB of disk space wasted on a duplicate copy.

### Seed-in-Place Architecture

The uploader serves chunks **directly from the original file** on disk — no CAS copy needed:

```
Traditional:
  Game folder (50 GB) → CAS copy (50 GB) → Serve from CAS
  Total disk: 100 GB for one file

Seed-in-Place:
  Game folder (50 GB) → Serve directly from source path
  Total disk: 50 GB (zero overhead)
```

```go
type SeedSource struct {
    OriginalPath string   // "/home/alice/Games/Elden Ring/" — read directly
    CASBacked    bool     // false for uploader (serves from OriginalPath)
    ChunkSize    int
    TotalChunks  int
    Bitfield     []byte   // bit-packed availability (1 bit per chunk)
}
```

The `Bitfield` is compact: 1 MB covers 8 million chunks (32 TB of data at 4 MiB chunks).

### Secondary Seeder Cache

When a peer downloads a file, it caches chunks in CAS for re-seeding. This is managed by `CacheManager` with LRU eviction:

```go
type CacheManager struct {
    store    CacheStore
    limit    int64        // default 50 GB
    evict    EvictFunc    // callback to delete CAS chunks
}
```

- **Touch-on-access:** Each chunk read updates `LastAccessed` timestamp
- **LRU eviction:** Background goroutine periodically sorts by `LastAccessed`, evicts oldest until under limit
- **Non-blocking:** Eviction runs in its own goroutine, never blocks the hot path

### Provider Registry

```go
type ProviderRecord struct {
    Addr         string   // peer that has this chunk
    ChunkCount   int      // how many chunks this peer has
    TotalChunks  int
    RegisteredAt int64    // for expiry
}
```

The download path queries the provider registry: "who has chunks for key X?" → returns a list of seeders, ranked by availability (chunk count) and latency (EWMA score).

---

## Memory Monitor & OOM Forensics

When a process is killed by the OOM killer (`SIGKILL`), it cannot catch the signal, write a log, or dump a stack trace. The process simply vanishes, leaving no forensic evidence.

### Proactive Heap Profiling

The `memlog.Monitor` runs a background goroutine that samples `runtime.MemStats` and auto-dumps heap profiles at predefined thresholds:

```go
type Monitor struct {
    dumpDir  string
    interval time.Duration  // default 30s
    dumped   map[uint64]bool // tracks which thresholds have been dumped
}

// Auto-dump thresholds: 500 MB, 1 GB, 2 GB, 4 GB
// Only ONE dump per threshold (prevents disk flooding)
```

Every 30 seconds:
1. Read `runtime.MemStats` → log `HeapAlloc`, `HeapSys`, `HeapInuse`, `Goroutines`, `GC runs`
2. If `HeapAlloc` crosses a threshold not yet dumped → write `heap-<timestamp>-<sizeMB>.prof`
3. After OOM kill: `go tool pprof ~/.hermod/heap-1710432000-2048MB.prof` reveals the allocation pattern

**Key insight:** The 2 GB dump captured *before* the 4 GB OOM kill still shows the allocation trend. Combined with the periodic log entries, you can reconstruct the memory growth curve.

---

## Upload Priority & Background Task Yielding

Hermod runs several background maintenance tasks concurrently: anti-entropy repair, hinted handoff delivery, and rebalancing. All of these send large chunk data over the same QUIC connections used by foreground uploads.

### The Problem

Without coordination, a 1,072-file directory upload competes with anti-entropy's bulk repair transfers. Both saturate the connection → uploads slow to a crawl → user experience degrades.

### The Solution: Atomic Upload Counter

```go
type Server struct {
    activeUploads atomic.Int32  // incremented at upload start, decremented at end
}

// Upload path:
func (s *Server) StoreDataWithProgress(...) error {
    s.activeUploads.Add(1)
    defer s.activeUploads.Add(-1)
    // ... upload logic ...
}
```

All background tasks check `activeUploads` before doing heavy work:

```go
// Hinted handoff: waits until uploads finish
if s.activeUploads.Load() > 0 {
    for s.activeUploads.Load() > 0 {
        time.Sleep(500 * time.Millisecond)
    }
}

// Rebalancer: same wait pattern
if s.activeUploads.Load() > 0 {
    // defer migration until upload completes
}

// Anti-entropy: completely yields (returns immediately)
if s.activeUploads.Load() > 0 {
    return  // foreground upload has priority — try again next cycle
}
```

**Design choice:** Anti-entropy returns immediately (it runs every 10 minutes, so missing one cycle is fine). Handoff and rebalance wait patiently (they have data that must eventually be delivered). All three give absolute priority to user-initiated uploads.

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

### Async CAS Cache Write

When a node downloads a chunk from a peer, the chunk is delivered to the downloader **immediately** (zero disk I/O on the hot path). The CAS cache write happens asynchronously via a buffered channel:

```go
// Hot path: deliver chunk to waiting downloader (no disk I/O)
ch <- data  // non-blocking send

// Background: queue CAS write (best-effort, loss acceptable)
select {
case s.casWriteCh <- casWriteRequest{key: msg.Key, data: copy}:
default:  // channel full? drop (chunk still exists in cluster)
}

// Background goroutine: serialized CAS writes
func (s *Server) casWriteLoop() {
    for req := range s.casWriteCh {
        s.Store.WriteStream(req.key, bytes.NewReader(req.data))
    }
}
```

**Why this matters:** Disk I/O for a 4 MiB CAS write takes 10-50ms. On the hot download path, this blocks chunk delivery and inflates EWMA latency scores. Moving the write to a background queue keeps chunk delivery under 1ms.

---

## Package Map

```
hermod/
├── Server/
│   ├── server.go              Core orchestrator (peer lifecycle, message dispatch,
│   │                           dual-port routing, sync.Pool pipeline, upload priority)
│   ├── downloader/            Parallel multi-source chunk fetcher (3 strategies:
│   │                           DownloadToFile, DownloadToStream, DownloadToFileResumable)
│   └── transfer/              Transfer registry (pause/resume/cancel)
│
├── Storage/
│   ├── storage.go             CAS engine (SHA-256 paths, AES-GCM encryption)
│   ├── chunker/               4 MiB chunking, SHA-256 hashing, Merkle trees
│   ├── compression/           Zstd compression with entropy heuristic
│   ├── dirmanifest/           Multi-file directory manifests
│   ├── resume/                Download resume (append-only sidecar)
│   ├── pending/               Upload resume (append-only sidecar)
│   └── swarm/                 Seed-in-place + LRU cache manager (50 GB default)
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
│   ├── quic/                  QUIC transport (TLS 1.3, per-stream, dual-port,
│   │                           DialDual, role-based flow control windows)
│   ├── mdns/                  mDNS LAN auto-discovery (advertiser + scanner)
│   └── nat/                   STUN-based NAT traversal
│
├── State/                     BoltDB persistent state (10 buckets)
├── ipc/                       Binary IPC protocol (35 opcodes)
├── tui/                       Bubble Tea terminal UI (stateless IPC client)
├── cmd/                       Cobra CLI commands + daemon lifecycle
│   ├── unified.go             Daemon fork, TUI launch, config persistence
│   ├── daemon.go              StartDaemonAsync, DataDir setup, shutdown registration
│   ├── stop.go                `hermod stop` — OpShutdown over IPC
│   ├── detach_unix.go         Unix process detach (Setsid)
│   ├── detach_windows.go      Windows process detach (CREATE_NEW_PROCESS_GROUP)
│   ├── firewall.go            Cross-platform firewall auto-configuration
│   ├── firewall_darwin.go     macOS: osascript + socketfilterfw
│   ├── firewall_linux.go      Linux: ufw → firewall-cmd → iptables
│   ├── firewall_windows.go    Windows: ShellExecuteW + netsh
│   ├── ipcconn_unix.go        Unix domain socket IPC
│   └── ipcconn_windows.go     Windows named pipe IPC (go-winio)
└── Observability/
    ├── memlog/                In-memory ring-buffer monitor + auto heap dump
    ├── health/                HTTP health + pprof endpoints
    ├── metrics/               Prometheus instrumentation
    ├── tracing/               OpenTelemetry (OTLP → Jaeger)
    └── logging/               Zerolog structured logging
```

---

## Implementation Reference

This section covers the concrete types, function signatures, and wiring that make the high-level design work. Reading this alongside the package map above gives you a complete picture — from algorithm to line of code.

---

### Core Server (`Server/server.go`)

The `Server` struct is the root of the entire system. Every subsystem is a field on it.

```go
type Server struct {
    // Transport
    transport peer2peer.Transport   // QUIC transport (listens + dials)
    peerLock  sync.RWMutex
    peers     map[string]peer2peer.Peer  // addr → live connection

    // Storage
    Store     *storage.Store        // CAS storage engine + metadata
    StateDB   *State.StateDB        // BoltDB persistent state (history, catalog)

    // Cluster
    HashRing     *hashring.HashRing          // consistent hashing (150 vnodes)
    Cluster      *membership.ClusterState    // liveness + metadata per node
    GossipSvc    *gossip.GossipService       // epidemic dissemination
    HeartbeatSvc *heartbeat.Service          // sender + phi-accrual reaper
    HandoffSvc   *handoff.Store             // hinted handoff queue
    Rebalancer   *rebalance.Rebalancer       // migration on join/leave
    Quorum       *quorum.Coordinator         // W-of-N write/read quorum
    AntiEntropy  *merkle.AntiEntropyService  // background Merkle repair

    // Downloads + Transfers
    Downloader  *downloader.Manager   // parallel chunk fetcher
    Selector    *selector.Selector    // EWMA latency-aware peer picker
    TransferMgr *transfer.Manager     // pause/resume/cancel registry

    // Networking
    NATService     *nat.Puncher          // STUN external address discovery
    MDNSAdvertiser *peermdns.Advertiser  // LAN DNS-SD advertisement

    // Identity
    identityMeta map[string]string  // {"alias", "fingerprint", "x25519_pub", "ed25519_pub"}
    externalAddr string             // set after MessageAnnounceAck or STUN

    // Misc
    startedAt  time.Time
    searchSeen sync.Map  // flood-search dedup (msgID → bool)
}
```

**Server startup sequence** (`MakeServer` → `Start`):

```
MakeServer(opts MakeServerOpts) *Server
  1. NewBoltMetaStore(...)      → persistent metadata (manifests, filemeta)
  2. storage.NewStore(root, meta)
  3. membership.New(selfAddr)   → ClusterState
  4. hashring.New(cfg)          → adds selfAddr as first node
  5. gossip.New(...)            → wires ClusterState, getPeers, sendMsg, onNewPeer
  6. quorum.New(...)            → wires HashRing, sendMsg, local read/write
  7. handoff.NewStore(dir, ...)
  8. rebalance.New(...)
  9. selector.New()
  10. downloader.New(cfg, selector, fetchChunk, decryptChunk, getPeers)
  11. transfer.NewManager()
  12. State.Open(dbPath)        → StateDB
  13. merkle.NewAntiEntropyService(...)
  14. nat.New(cfg)              (if !DisableSTUN)
  15. peermdns.NewAdvertiser(...)

Server.Start():
  1. transport.ListenAndAccept() in goroutine
  2. go discoverNAT()           → non-blocking STUN query
  3. GossipSvc.Start()
  4. HeartbeatSvc.Start()
  5. AntiEntropy.Start()
  6. HandoffSvc.StartPurgeLoop()
  7. HealthSrv.Start()          (if enabled)
  8. go rpcLoop()               → reads transport.Consume() channel
```

**Message dispatch** (`rpcLoop` → `handleMessage`):

```go
// Every inbound RPC is dispatched by first byte of payload (the "message type")
// All message types are registered in init() via gob.Register(...)

func (s *Server) handleMessage(rpc peer2peer.RPC) {
    switch msg := decoded.(type) {
    case *MessageAnnounce:       s.handleAnnounce(rpc.Peer, msg)
    case *MessageAnnounceAck:    s.handleAnnounceAck(msg)
    case *MessageStoreFile:      s.handleStoreFile(rpc.Peer, msg, rpc.StreamReader)
    case *MessageGetFile:        s.handleGetFile(rpc.Peer, msg)
    case *MessageLocalFile:      s.handleLocalFile(rpc.Peer, msg, rpc.StreamReader)
    case *MessageStoreManifest:  s.handleStoreManifest(msg)
    case *MessageGetManifest:    s.handleGetManifest(rpc.Peer, msg)
    case *MessageManifestResponse: s.handleManifestResponse(msg)
    case *MessageHeartbeat:      s.handleHeartbeat(rpc.Peer, msg)
    case *MessageHeartbeatAck:   s.handleHeartbeatAck(msg)
    case *MessageGossipDigest:   s.handleGossipDigest(rpc.Peer, msg)
    case *MessageGossipResponse: s.handleGossipResponse(msg)
    case *MessageQuorumWrite:    s.handleQuorumWrite(msg)
    case *MessageQuorumRead:     s.handleQuorumRead(rpc.Peer, msg)
    case *MessageQuorumWriteAck: s.Quorum.HandleWriteAck(...)
    case *MessageQuorumReadResponse: s.Quorum.HandleReadResponse(...)
    case *MessageIdentityMeta:   s.handleIdentityMeta(msg)
    case *MessageLeaving:        s.handleLeaving(msg)
    case *MessageMerkleSync:     s.handleMerkleSync(rpc.Peer, msg)
    case *MessageMerkleDiffResponse: s.handleMerkleDiffResponse(rpc.Peer, msg)
    }
}
```

**Peer lifecycle** (connect → announce → gossip → disconnect):

```
Outbound dial (s.Connect(addr)):
  transport.Dial(addr)
  → OnPeer(peer) fires:
      peers[addr] = peer
      HashRing.AddNode(addr)
      Cluster.AddNode(addr, nil)
      send MessageAnnounce{ListenAddr: effectiveSelfAddr()}
      send MessageIdentityMeta{Meta: identityMeta}

Inbound connection accepted:
  → OnPeer(peer) same as above (minus announce — waits for remote to send it)

MessageAnnounce arrives:
  handleAnnounce(peer, msg):
    remappedAddr = peer.RemoteAddr()  // what *we* see (canonical)
    if remappedAddr != msg.ListenAddr:
        // Docker/NAT: remap all state from ephemeral to canonical addr
        migrateMetadata(msg.ListenAddr, remappedAddr)
    HashRing.AddNode(remappedAddr)
    Cluster.AddNode(remappedAddr, ...)
    send MessageAnnounceAck{YourAddr: remappedAddr}

MessageAnnounceAck arrives:
  handleAnnounceAck(msg):
    externalAddr = msg.YourAddr  // now we know our external addr
    GossipSvc.SetSelfAddr(externalAddr)
    Cluster.BumpSelfGeneration()  // gossip our new canonical address

Disconnect (OnPeerDisconnect):
    delete peers[addr]
    HashRing.RemoveNode(addr)
    Cluster.ForceLocalState(addr, StateDead)
    Rebalancer.OnNodeLeft(addr)
    HandoffSvc: replay hints for addr on reconnect
```

---

### Storage Engine (`Storage/`)

#### `Store` struct (`Storage/storage.go`)

```go
type Store struct {
    structOpts StructOpts  // {Root string, Meta MetadataStore}
}

type StructOpts struct {
    Root string           // filesystem root for CAS files
    Meta MetadataStore    // manifests + filemeta (BoltDB-backed)
}

// Core operations
func NewStore(root string, meta MetadataStore) *Store

// Write a file (chunked, encrypted, CAS-stored)
// Called by server replication handlers (not upload path directly)
func (s *Store) StoreData(key string, r io.Reader, opts EncryptionMeta) error

// Write + collect metadata (deferred write for batch directory uploads)
func (s *Store) StoreDataCollectMeta(key string, r io.Reader, opts EncryptionMeta, deferMeta bool) (*StoredFileResult, error)

// Write + progress callbacks (used in directory upload pipeline)
func (s *Store) StoreDataWithProgress(key string, r io.Reader, opts EncryptionMeta, onChunk func(idx, total int)) error

// CAS read: stream decrypted chunk from disk
func (s *Store) GetData(key string, dek []byte) (io.ReadCloser, error)

// Direct-to-disk write (resumable, Merkle-verified, atomic rename)
func (s *Store) GetDataToFile(key string, outputPath string, dek []byte) error

// Directory operations
func (s *Store) StoreDirectory(dirPath, baseKey string, opts EncryptionMeta, onFile, onChunk func(int,int)) error
func (s *Store) GetDirectory(dirKey, outputDir string, dek []byte) error

// Metadata lookups
func (s *Store) Has(key string) bool
func (s *Store) GetManifest(key string) (*chunker.ChunkManifest, bool)
func (s *Store) GetDirManifest(key string) (*dirmanifest.DirectoryManifest, bool)
func (s *Store) Keys() []string  // all locally-held keys

// CAS path derivation
// SHA-256(ciphertext) → hex string → split into 5-char dirs
// e.g. "816cc20437d8595b..." → root/816cc/20437/d8595/<md5>.dat
func casPath(root, hexHash string) string
```

#### `EncryptionMeta` — upload configuration

```go
type EncryptionMeta struct {
    Encrypt     bool
    DEK         []byte                  // 32-byte AES-256 key (nil = generate)
    OwnerPubKey string                  // hex X25519 pub
    OwnerEdPub  string                  // hex Ed25519 pub
    AccessList  []chunker.AccessEntry   // wrapped DEKs for recipients
    Signature   string                  // Ed25519 sig over manifest
    Public      bool
}
```

#### `MetadataStore` interface + implementations

```go
type MetadataStore interface {
    Get(key string) (FileMeta, bool)
    Set(key string, meta FileMeta) error
    GetManifest(key string) (*chunker.ChunkManifest, bool)
    SetManifest(key string, manifest *chunker.ChunkManifest) error
    GetDirManifest(key string) ([]byte, bool)
    SetDirManifest(key string, data []byte) error
    WithBatch(fn func(batch MetadataBatch) error) error
    Keys() []string
    Delete(key string) error
}

type MetadataBatch interface {
    SetManifest(key string, manifest *chunker.ChunkManifest) error
    SetDirManifest(key string, data []byte) error
    Set(key string, meta FileMeta) error
}

// BoltMetaStore — bbolt-backed, production implementation
// Buckets: "filemeta" (FileMeta JSON), "manifests" (ChunkManifest JSON),
//          "dirmanifests" (DirectoryManifest JSON)
// WithBatch() opens a single bolt.Tx, runs fn, commits once (1 fsync)
type BoltMetaStore struct { db *bolt.DB }
func NewBoltMetaStore(path string) (*BoltMetaStore, error)
```

#### Chunker (`Storage/chunker/`)

```go
// All chunk hashes are SHA-256 of plaintext (before compression + encryption)
type ChunkInfo struct {
    Index      int
    Hash       string   // hex SHA-256 of plaintext chunk
    Size       int64
    EncHash    string   // hex SHA-256 of ciphertext (CAS path key)
    Compressed bool
}

type ChunkManifest struct {
    FileKey       string
    TotalSize     int64
    ChunkSize     int
    Chunks        []ChunkInfo
    MerkleRoot    string           // SHA-256 of Merkle tree over plaintext hashes
    CreatedAt     int64
    Encrypted     bool
    OwnerPubKey   string           // hex X25519
    OwnerEdPubKey string           // hex Ed25519
    AccessList    []AccessEntry    // ECDH: one entry per recipient
    Signature     string           // Ed25519 sig (hex)
}

type AccessEntry struct {
    RecipientPubKey string  // hex X25519
    WrappedDEK      string  // hex [12B nonce || ciphertext || 16B GCM tag]
}

// Streaming chunker — sends Chunk values over channel as it reads
func ChunkReader(r io.Reader, chunkSize int) (<-chan Chunk, <-chan error)

// Pool-aware variant (zero-alloc uploads)
func ChunkReaderWithPool(r io.Reader, chunkSize int,
    readPool *sync.Pool, dataPool ...*sync.Pool) (<-chan Chunk, <-chan error)

// Merkle tree: leaves = SHA-256(plaintext chunk), parents = SHA-256(left||right)
// Odd-count: last leaf duplicated (standard convention)
func BuildMerkleRoot(chunks []ChunkInfo) string
func VerifyMerkleRoot(chunks []ChunkInfo, expected string) error
func BuildManifest(fileKey string, chunks []ChunkInfo, createdAt int64) *ChunkManifest
func VerifyChunk(c Chunk) bool  // re-hash and compare
```

#### Compression (`Storage/compression/`)

```go
// Entry point — decides whether to compress before returning
func ShouldCompress(data []byte) bool
// Logic:
//   len(data) < 1024                → false (framing overhead)
//   magic bytes match JPEG/PNG/ZIP/gzip/zstd/MP4/MKV → false (already compressed)
//   entropy := shannonEntropy(data[:min(4096, len(data))])
//   entropy < 7.0  → true
//   entropy >= 7.5 → false
//   7.0–7.5: borderline, compress and check ratio

func CompressChunk(data []byte, level Level) (out []byte, wasCompressed bool, err error)
// Calls ShouldCompress; if yes, zstd-compresses; if result >= original, returns original

func CompressChunkWithPool(data []byte, level Level, pool *sync.Pool) (out []byte, wasCompressed bool, err error)

func DecompressChunk(data []byte) ([]byte, error)  // zstd decompress
```

#### Resume sidecar (`Storage/resume/`)

```go
// On-disk layout (append-only text file at <outputPath>.resume):
// Line 1: JSON Header
// Line N: chunk index (decimal)

type Header struct {
    StorageKey  string
    TotalChunks int
    TotalSize   int64
    ChunkSize   int
    MerkleRoot  string
    CreatedAt   int64
}

func Create(outputPath, storageKey, merkleRoot string,
    totalChunks int, totalSize int64, chunkSize int) (*Sidecar, error)

func Open(outputPath string) (s *Sidecar, skipSet map[int]bool, err error)
// Reads header, validates against manifest before trusting, builds skipSet
// Fast-verify: for each index in skipSet, re-hash from .part file before trusting

func (s *Sidecar) RecordChunk(index int) error  // single append syscall
func (s *Sidecar) Close() error
func (s *Sidecar) Delete() error                // called after atomic rename
```

#### Pending sidecar (`Storage/pending/`)

```go
// Tracks in-progress uploads; lives next to CAS files, keyed by storageKey

type ChunkRecord struct {
    Index      int
    Hash       string   // plaintext hash
    Size       int64
    EncHash    string   // CAS key
    Compressed bool
}

func Create(storageRoot string, h Header) (*Sidecar, error)
func Load(storageRoot, storageKey string) (*LoadResult, error)
// Returns completed set + ChunkInfos for already-uploaded chunks
// Allows upload to skip re-encrypting+re-hashing completed chunks

func (s *Sidecar) RecordChunk(cr ChunkRecord) error
func (s *Sidecar) Finalize(merkleRoot string) error  // write root, close
func (s *Sidecar) Delete() error
```

---

### Cryptography (`Crypto/`)

#### Identity (`Crypto/identity/`)

```go
type Identity struct {
    Alias       string
    Ed25519Priv []byte  // 64 bytes (seed + public)
    Ed25519Pub  []byte  // 32 bytes
    X25519Priv  []byte  // 32 bytes (Diffie-Hellman)
    X25519Pub   []byte  // 32 bytes
}

func Generate(alias string) (*Identity, error)  // crypto/rand, stored to disk
func Load(path string) (*Identity, error)        // reads ~/.hermod/identity.json
func (id *Identity) Save(path string) error
func (id *Identity) Fingerprint() string
// hex(SHA-256(Ed25519Pub))[:16]
// Used as namespace prefix: "<fingerprint>/filename"

func (id *Identity) GossipMetadata() map[string]string
// Returns: {"alias": ..., "fingerprint": ..., "x25519_pub": hex, "ed25519_pub": hex}
// Spread via MessageIdentityMeta so peers can ECDH-share without out-of-band key exchange
```

#### Envelope (`Crypto/envelope/`)

```go
// ECDH key wrapping
func WrapDEKForRecipient(
    senderX25519Priv []byte,
    recipientX25519PubHex string,
    dek []byte,
) (*chunker.AccessEntry, error)
// 1. X25519(senderPriv, recipientPub) → sharedSecret (32 bytes)
// 2. HKDF-SHA256(sharedSecret, salt="hermod-dek-wrap") → wrappingKey (32 bytes)
// 3. AES-256-GCM(wrappingKey, dek) → [12B nonce || ciphertext || 16B tag]
// 4. Returns AccessEntry{RecipientPubKey: hex(recipientPub), WrappedDEK: hex(...)}

func UnwrapDEK(
    recipientX25519Priv []byte,
    senderX25519PubHex string,
    wrappedDEKHex string,
) ([]byte, error)
// Reverse: ECDH symmetry → same sharedSecret → same wrappingKey → AES-GCM decrypt

// Manifest signing
func SignManifest(edPriv ed25519.PrivateKey, payload ManifestSigningPayload) (string, error)
// payload = {AccessList, Encrypted, FileKey, OwnerPubKey} — JSON, alphabetically sorted keys
// Sign SHA-256(JSON) with Ed25519 → hex signature

func VerifyManifest(edPubHex string, payload ManifestSigningPayload, sigHex string) (bool, error)
// Reconstruct canonical JSON → SHA-256 → Ed25519 verify
```

#### Streaming encryption (`Crypto/crypto.go`)

```go
const ChunkSize = 4 * 1024 * 1024  // 4 MiB, matches chunker

// Wire format per chunk: [4B big-endian chunk_len][12B nonce][ciphertext][16B GCM tag]
// Nonce = chunk_index encoded as 12-byte big-endian (deterministic, no state)

func EncryptStreamWithDEK(src io.Reader, dst io.Writer, dek []byte) error
func EncryptStreamWithDEKPool(src io.Reader, dst io.Writer, dek []byte, pool *sync.Pool) error
func DecryptStreamWithDEK(src io.Reader, dst io.Writer, dek []byte) error
```

---

### Peer-to-Peer Transport (`Peer2Peer/`)

#### Core interfaces (`Peer2Peer/transport.go`)

```go
type Peer interface {
    net.Conn
    Send(b []byte) error                              // fire-and-forget (new stream per call)
    SendMsg(controlByte byte, payload []byte) error   // [ctrl][4B len][payload]
    SendStream(msgPayload, streamData []byte) error   // header + data streamed
    CloseStream()
    Outbound() bool
}

type Transport interface {
    Addr() string
    ListenAndAccept() error
    Consume() <-chan RPC  // inbound messages
    Close() error
    Dial(addr string) error
}

type RPC struct {
    From         net.Addr
    Peer         Peer
    Payload      []byte
    Stream       bool
    StreamWg     *sync.WaitGroup
    StreamReader io.Reader  // QUIC stream; holds data until StreamWg.Done()
}
```

#### QUIC transport (`Peer2Peer/quic/transport.go`)

```go
type TransportOpts struct {
    ListenAddr  string
    TLSConfig   *tls.Config   // self-signed cert generated on first run
    Decoder     peer2peer.Decoder
}

// QUICPeer wraps a quic.Conn; each Send/SendMsg opens a new short-lived stream
type QUICPeer struct {
    conn     *quic.Conn
    outbound bool
}

func (p *QUICPeer) SendMsg(controlByte byte, payload []byte) error {
    stream, _ := p.conn.OpenStreamSync(ctx)
    defer stream.Close()
    buf := make([]byte, 1+4+len(payload))
    buf[0] = controlByte
    binary.BigEndian.PutUint32(buf[1:5], uint32(len(payload)))
    copy(buf[5:], payload)
    _, err = stream.Write(buf)  // single write = atomic header
    return err
}

// SendStream: writes header + data over a single QUIC stream
func (p *QUICPeer) SendStream(msgPayload, streamData []byte) error
```

#### NAT Traversal (`Peer2Peer/nat/`)

```go
type Puncher struct {
    cfg        Config
    publicAddr string  // cached after STUN discovery
}

// DiscoverPublicAddr sends RFC 5389 STUN Binding Request over UDP
// Returns "ip:port" as seen by the STUN server
func (p *Puncher) DiscoverPublicAddr() (string, error)

// Non-blocking startup: if STUN fails, node falls back to LAN-only with warning
// Server wires this as:
func (s *Server) discoverNAT() {
    addr, err := s.NATService.DiscoverPublicAddr()
    if err != nil {
        log.Warn().Err(err).Msg("STUN failed, LAN-only mode")
        return
    }
    s.externalAddr = addr
    s.GossipSvc.SetSelfAddr(addr)
}
```

#### mDNS (`Peer2Peer/mdns/`)

```go
// Advertiser registers a DNS-SD "_hermod._udp" service on the LAN
type Advertiser struct { server *zeroconf.Server }
func NewAdvertiser(alias, fingerprint, listenAddr string) (*Advertiser, error)
func (a *Advertiser) Stop()

// Scanner does a one-shot DNS-SD lookup, returns discovered peers
func Scan(timeout time.Duration) ([]DiscoveredPeer, error)
// Used by IPC OpScanLAN to show user what nodes are on LAN without explicit --peer flag
```

---

### Cluster Coordination (`Cluster/`)

#### Membership (`Cluster/membership/`)

```go
type NodeState int32
const (
    StateAlive   NodeState = 0
    StateSuspect NodeState = 1
    StateDead    NodeState = 2
    StateLeft    NodeState = 3
)
// Monotonic — higher value always wins at same generation

type NodeInfo struct {
    Addr        string
    State       NodeState
    Generation  uint64            // time.Now().UnixNano() on startup; bumped on state change
    LastUpdated time.Time
    Metadata    map[string]string // gossip key-value bag (alias, fingerprint, keys, public_addr)
}

// UpdateState applies only if generation > current (guards stale gossip)
// Returns true if state was applied
func (cs *ClusterState) UpdateState(addr string, state NodeState, gen uint64) bool

// ForceLocalState: called on socket close (no generation check needed — we observed it)
func (cs *ClusterState) ForceLocalState(addr string, state NodeState)

// BumpSelfGeneration: SWIM refutation — call after being declared Dead
func (cs *ClusterState) BumpSelfGeneration()
// Sets generation = time.Now().UnixNano() (guaranteed > any stale record)

// SetMetadata: does NOT bump generation (metadata independent of liveness)
func (cs *ClusterState) SetMetadata(addr string, meta map[string]string)

// Merge: apply gossip digest; return addrs where we need full NodeInfo
func (cs *ClusterState) Merge(digests []GossipDigest) []string
```

#### Gossip (`Cluster/gossip/`)

```go
type GossipService struct {
    cfg             Config           // FanOut=3, Interval=200ms
    selfAddr        string           // guarded by RWMutex (changes after AnnounceAck)
    selfFingerprint string           // fingerprint-based self-detection (Docker safe)
    cluster         *membership.ClusterState
    getPeers        func() []string
    sendMsg         func(addr string, msg interface{}) error
    onNewPeer       func(addr string)  // triggers s.Connect(addr) on new gossip address
}

// One gossip round (every 200ms):
// 1. pick FanOut random peers from getPeers()
// 2. send MessageGossipDigest{Digests: cluster.Digest()}
// 3. receiver: Merge(digests) → collect needFull list
// 4. receiver: send MessageGossipResponse{Nodes: fullEntries for needFull}
// 5. HandleResponse: for each entry, UpdateState + SetMetadata
//    if fingerprint != selfFingerprint: apply (Docker: multiple containers may share 127.0.0.1)
//    if entry.Addr is unknown + Alive: call onNewPeer(addr) → auto-connect

func (gs *GossipService) HandleDigest(peer peer2peer.Peer, digests []membership.GossipDigest)
func (gs *GossipService) HandleResponse(entries []membership.NodeInfo)
```

#### Consistent Hashing (`Cluster/hashring/`)

```go
type HashRing struct {
    mu           sync.RWMutex
    sortedHashes []uint64             // sorted virtual node positions
    hashToNode   map[uint64]string    // position → physical addr
    nodeToHashes map[string][]uint64  // physical addr → its positions (for removal)
    virtualNodes int                  // default 150
    replFactor   int
}

// AddNode: creates 150 virtual nodes
// key = "addr-0" through "addr-149" → SHA-256 → first 8 bytes as uint64 → sorted insert
func (hr *HashRing) AddNode(addr string)

// GetNodes: up to N distinct physical nodes for a key
// 1. SHA-256(key) → uint64 position
// 2. binary.Search(sortedHashes, position) → start index
// 3. walk clockwise, collect distinct physical nodes until N found or ring exhausted
func (hr *HashRing) GetNodes(key string, n int) []string
```

#### Quorum (`Cluster/quorum/`)

```go
// pendingWrites: sync.Map[key → chan WriteAck]
// pendingReads:  sync.Map[key → chan ReadResponse]
// Both use buffered channels (capacity = N) for lock-free concurrent operations

// Write flow:
// 1. create chan WriteAck (cap=N)
// 2. store in pendingWrites[key]
// 3. goroutine: write locally → send ack on chan
// 4. goroutine each remote: send MessageQuorumWrite → remote sends MessageQuorumWriteAck
//    HandleWriteAck routes ack to correct chan via pendingWrites
// 5. Wait: read W acks from chan within Timeout (5s default)

// Read flow:
// 1. create chan ReadResponse (cap=N)
// 2. send MessageQuorumRead to all N replicas
// 3. HandleReadResponse routes to chan
// 4. Collect R responses, pass to conflict.LWWResolver.Resolve()

func (c *Coordinator) Write(key string, data []byte, clock vclock.VectorClock) error
func (c *Coordinator) Read(key string) (vclock.VectorClock, error)
func (c *Coordinator) HandleWriteAck(ack WriteAck)
func (c *Coordinator) HandleReadResponse(resp ReadResponse)
```

#### Failure Detection (`Cluster/failure/`)

```go
// Per-peer ring buffer of inter-arrival intervals (max 200 samples)
type peerWindow struct {
    intervals []float64  // milliseconds
    head      int
    count     int
    lastSeen  time.Time
}

// Phi calculation:
// elapsed = now - lastSeen
// mean, stddev = stats over intervals (stddev clamped: min 1ms, min 10% of mean)
// y = (elapsed - mean) / (stddev * sqrt(2))
// cdf = 0.5 * (1 + erf(y))
// phi = -log10(1 - cdf)
// phi=0: definitely alive; phi=8: suspect; phi=16: almost certainly dead

func (d *PhiAccrualDetector) RecordHeartbeat(addr string, at time.Time)
func (d *PhiAccrualDetector) Phi(addr string) float64
// Zero samples → phi=0 (new node is not suspicious)

// Heartbeat service: two goroutines
// sender (every 1s):  send MessageHeartbeat to all peers
// reaper (every 500ms): for each peer, compute phi
//   phi >= SuspectThreshold (8.0) → UpdateState(addr, StateSuspect, gen)
//   StateSuspect for > DeadTimeout (30s) → UpdateState(addr, StateDead, gen)
```

#### Hinted Handoff (`Cluster/handoff/`)

```go
// Hints stored as JSON files: <dir>/<targetAddr_escaped>/<key_hash>.json
// Survives process restart

type Hint struct {
    Key        string
    TargetAddr string
    Data       []byte
    CreatedAt  time.Time
    Attempts   int
}

// AddHint: if len(hints[target]) >= maxHints (1000), drop oldest
func (s *Store) AddHint(h Hint) error

// Replay (called by OnPeerReconnect in server.go):
func (s *Store) GetHints(targetAddr string) []Hint
// Server iterates, retries delivery, calls DeleteHint on success
// After MaxAttempts (5): give up, DeleteHint anyway
// activeDeliveries sync.Map prevents concurrent replay for same target

// Hourly purge removes hints older than maxAge (24h)
func (s *Store) PurgeExpired()
```

#### Rebalancer (`Cluster/rebalance/`)

```go
// Called by server on topology changes (after announce / after disconnect)

func (r *Rebalancer) OnNodeJoined(newAddr string)
// For each locally-held key:
//   if newAddr ∈ HashRing.GetNodes(key, RF):
//     sendFile(newAddr, key, data)  (with MigrationDelay=50ms between sends)

func (r *Rebalancer) OnNodeLeft(deadAddr string)
// For each locally-held key:
//   current replicas = HashRing.GetNodes(key, RF)  (ring already removed dead node)
//   if len(current) < RF: send copies to fill up to RF

// inProgress flag (atomic): only one rebalance at a time
// Prevents storms when multiple nodes join/leave simultaneously
```

#### Peer Selection (`Cluster/selector/`)

```go
type peerState struct {
    ewmaLatency     float64  // milliseconds, alpha=0.2
    activeDownloads int32
}

// EWMA update: new = 0.2*sample + 0.8*old
func (s *Selector) RecordLatency(addr string, d time.Duration)

// Implicit bad peer banning:
// On chunk verification failure: RecordLatency(peer, 10*time.Minute)
// EWMA jumps to ~600,000ms → score astronomical → not selected for ~10 minutes

func (s *Selector) BestPeer(candidates []string) (string, bool)
// score(peer) = ewmaLatency * (1 + 0.5 * activeDownloads)
// returns candidate with minimum score
// ties broken by order in candidates slice
```

#### Vector Clocks & Conflict Resolution

```go
// Cluster/vclock/
type VectorClock map[string]uint64  // nodeAddr → logical counter

type Relation int
const (
    Before     Relation = iota  // vc < other (causally dominated)
    After                       // vc > other (causally dominates)
    Concurrent                  // neither dominates (conflict)
    Equal
)

func (vc VectorClock) Increment(addr string) VectorClock  // returns new copy
func (vc VectorClock) Merge(other VectorClock) VectorClock  // element-wise max
func (vc VectorClock) Compare(other VectorClock) Relation

// Cluster/conflict/
type Version struct {
    NodeAddr  string
    Clock     vclock.VectorClock
    Timestamp int64  // UnixNano for LWW tiebreak
}

type LWWResolver struct{}
func (r *LWWResolver) Resolve(versions []Version) Version
// Priority:
//   1. If Clock(A) < Clock(B): B wins  (causal order is strongest)
//   2. If concurrent: higher Timestamp wins  (LWW tiebreak)
//   3. Exact tie: first element in slice wins
```

#### Anti-Entropy (`Cluster/merkle/`)

```go
// Background service: every 10 minutes, sync with each peer

// Protocol per peer pair (A syncing with B):
// 1. A builds Merkle tree over sorted locally-held keys
// 2. A sends MessageMerkleSync{RootHash: tree.RootHash()}
// 3. B compares with own root:
//      same  → in sync, done (no further traffic)
//      diff  → B sends MessageMerkleDiffResponse{Keys: b.Keys()}
// 4. A computes diff:
//      key in A not B → A.sendManifest + A.sendFile to B  (push repair)
//      key in B not A → A requests file from B            (pull repair)

func Build(keys []string) *Tree
// Leaves = SHA-256(key) for each key in sorted order
// Odd count: duplicate last leaf

func (t *Tree) RootHash() [32]byte
func (t *Tree) Diff(other *Tree) (onlyInSelf, onlyInOther []string)
```

---

### Downloader (`Server/downloader/`)

```go
type Manager struct {
    cfg      Config            // MaxParallel=4, ChunkTimeout=30s, MaxRetries=3
    sel      *selector.Selector
    fetch    FetchFunc         // calls server.fetchChunkFromPeer
    decrypt  DecryptFunc       // calls Crypto.DecryptChunk
    getPeers GetPeersFunc      // calls HashRing.GetNodes
}

// Direct-to-disk (random-access, zero HoL blocking)
func (m *Manager) DownloadToFile(ctx context.Context,
    manifest *chunker.ChunkManifest,
    dst io.WriterAt,          // *os.File opened for the .part path
    progress ProgressFunc,
    dek []byte) error

// Resumable variant (same as above + skip set + record callback)
func (m *Manager) DownloadToFileResumable(ctx context.Context,
    manifest *chunker.ChunkManifest,
    dst io.WriterAt,
    skipSet map[int]bool,     // pre-verified chunks from resume sidecar
    progress ProgressFunc,
    onChunkDone ChunkRecordFunc,  // called after each verified write → sidecar.RecordChunk
    dek []byte) error

// Both methods:
// - spawn up to MaxParallel goroutines (semaphore via buffered chan)
// - for each missing chunk:
//     candidates = getPeers(manifest.FileKey)
//     peer = sel.BestPeer(candidates)
//     encData = fetch(manifest.Chunks[i].EncHash, peer)
//     plaintext = decrypt(encData, dek)
//     SHA-256(plaintext) == manifest.Chunks[i].Hash ? OK : ban peer, retry
//     dst.WriteAt(plaintext, int64(i)*chunkSize)
//     onChunkDone(i) [for resume]
// - after all chunks: VerifyMerkleRoot(manifest.Chunks, manifest.MerkleRoot)

// Streaming variant (sliding window, for IPC pipes)
func (m *Manager) DownloadToStream(ctx context.Context,
    manifest *chunker.ChunkManifest,
    dst io.Writer,            // IPC conn or stdout
    progress ProgressFunc,
    dek []byte) error
// Window of MaxParallel in-flight chunks
// Out-of-order buffer: map[int][]byte
// Flush consecutive chunks in order to dst
// Backpressure: pause workers when window full
```

---

### Transfer Manager (`Server/transfer/`)

```go
// trackedTransfer wraps TransferInfo with a mutex (Go vet: sync.Mutex not embedded in copyable type)
type trackedTransfer struct {
    TransferInfo               // embedded (safe to copy as snapshot)
    cancel context.CancelFunc  // stops the download/upload goroutine
    mu     sync.Mutex          // guards mutations to TransferInfo fields
}

type Manager struct {
    mu        sync.RWMutex
    transfers map[string]*trackedTransfer
    counter   uint32  // atomic ID generator
}

// Register: creates entry + context; caller uses ctx to pass to downloader
func (m *Manager) Register(dir Direction, key, name, filePath string,
    size int64, isDir, encrypted, public bool) (id string, ctx context.Context, cancel context.CancelFunc)

// Pause: cancel the context (stops goroutine), set Status=Paused
// .part + .resume files remain on disk for later Resume
func (m *Manager) Pause(id string) error

// Resume: new context + cancel, re-spawn download goroutine
// Downloader reads .resume sidecar, fast-verifies, continues
func (m *Manager) Resume(id string) error

// Cancel: cancel context + remove from registry (no resume possible)
func (m *Manager) Cancel(id string) error
```

---

### Persistent State (`State/state.go`)

```go
type StateDB struct {
    db *bolt.DB
}

// BoltDB buckets:
// "uploads"          → key=storageKey,    val=JSON UploadEntry
// "downloads"        → key=storageKey,    val=JSON DownloadEntry
// "public_files"     → key=storageKey,    val=JSON PublicFileEntry
// "tombstones"       → key=storageKey,    val=JSON TombstoneEntry
// "known_peers"      → key=addr,          val=JSON {Addr, Alias, Fingerprint}
// "ignored_peers"    → key=addr/fp,       val=timestamp
// "inbox"            → key=entryID,       val=JSON InboxEntry
// "outbox"           → key=entryID,       val=JSON OutboxEntry
// "transfer_history" → key=<ts>_<id>,     val=JSON TransferHistoryEntry
// "config"           → key="node_alias",  val=alias string

func Open(path string) (*StateDB, error)
func (s *StateDB) RecordUpload(entry UploadEntry) error
func (s *StateDB) ListUploads() ([]UploadEntry, error)
func (s *StateDB) RecordDownload(entry DownloadEntry) error
func (s *StateDB) ListDownloads() ([]DownloadEntry, error)
func (s *StateDB) AddPublicFile(e PublicFileEntry) error
func (s *StateDB) RemovePublicFile(key string) error
func (s *StateDB) ListPublicFiles() ([]PublicFileEntry, error)
func (s *StateDB) RecordTransfer(e TransferHistoryEntry) error

// PublicCatalogSummary: for gossip change detection
// returns (count, totalBytes, SHA-256(sorted keys)[:16 hex])
// Peers compare hashes; if different, request full catalog via IPC OpBrowsePeer
func (s *StateDB) PublicCatalogSummary() (count, size int64, hash string)

func (s *StateDB) RecordTombstone(e TombstoneEntry) error
func (s *StateDB) IsTombstoned(key string) (bool, int64)
func (s *StateDB) SaveKnownPeer(addr, alias, fp string) error
func (s *StateDB) LoadKnownPeers() ([]KnownPeer, error)  // used at startup for auto-reconnect
func (s *StateDB) AddToInbox(e InboxEntry) error
func (s *StateDB) ListInbox() ([]InboxEntry, error)
```

---

### IPC Protocol (`ipc/` + `cmd/`)

#### Wire format

```
Client → Daemon:
  [1B opcode][payload bytes...]

  For file uploads, payload = gob-encoded request struct + raw file bytes appended

Daemon → Client responses:
  Success:    [0x00][JSON or raw bytes]
  Error:      [0x01][error message]
  Progress:   [0x02][4B completed][4B total]               (file download)
              [0x02][4B fileIdx][4B fileTotal]
                    [4B chunkIdx][4B chunkTotal]           (directory)
```

#### Dispatcher (`cmd/handle_client.go`)

```go
func HandleClient(conn net.Conn, s *server.Server) {
    opcode := readByte(conn)
    switch opcode {
    case ipc.OpUpload:             handleUpload(conn, s)
    case ipc.OpDownload:           handleDownload(conn, s)
    case ipc.OpECDHUpload:         handleECDHUpload(conn, s)
    case ipc.OpECDHDownload:       handleECDHDownload(conn, s)
    case ipc.OpGetManifest:        handleGetManifestInfo(conn, s)
    case ipc.OpResolveAlias:       handleResolveAlias(conn, s)
    case ipc.OpDownloadToFile:     handleDownloadToFile(conn, s)
    case ipc.OpECDHDownloadToFile: handleECDHDownloadToFile(conn, s)
    case ipc.OpDirUpload:          handleDirUpload(conn, s)
    case ipc.OpDirDownload:        handleDirDownload(conn, s)
    case ipc.OpECDHDirUpload:      handleECDHDirUpload(conn, s)
    case ipc.OpECDHDirDownload:    handleECDHDirDownload(conn, s)
    case ipc.OpListUploads:        handleListUploads(conn, s)
    case ipc.OpListDownloads:      handleListDownloads(conn, s)
    case ipc.OpBrowsePeer:         handleBrowsePeer(conn, s)
    case ipc.OpListPeers:          handleListPeers(conn, s)
    case ipc.OpNodeStatus:         handleNodeStatus(conn, s)
    case ipc.OpListTransfers:      handleListTransfers(conn, s)
    case ipc.OpCancelTransfer:     handleCancelTransfer(conn, s)
    case ipc.OpPauseTransfer:      handlePauseTransfer(conn, s)
    case ipc.OpResumeTransfer:     handleResumeTransfer(conn, s)
    case ipc.OpRemoveFile:         handleRemoveFile(conn, s)
    case ipc.OpSearch:             handleSearch(conn, s)
    case ipc.OpScanLAN:            handleScanLAN(conn, s)
    // ...
    }
}

}
```

#### Upload flow (`cmd/handle_upload.go`)

```go
// Plaintext upload (OpUpload):
// 1. Read gob-encoded UploadRequest{Key, Size, Public} from conn
// 2. Read file bytes from conn (Size bytes)
// 3. id, ctx, cancel = TransferMgr.Register(Upload, key, ...)
// 4. s.Store.StoreDataWithProgress(key, bytes.Reader, EncryptionMeta{}, onChunk)
//    onChunk → writeProgress(conn, done, total)
// 5. HashRing.GetNodes(key, RF) → replicas
// 6. for each remote replica:
//      s.replicateChunk(replica, key, chunkData)  // QUIC MessageStoreFile
// 7. TransferMgr.SetStatus(id, Completed, "")
// 8. StateDB.RecordUpload(...)
// 9. if public: StateDB.AddPublicFile(...)

// ECDH upload (OpECDHUpload):
// Same flow but:
// 3b. Decode ECDHUploadRequest (DEK, AccessList, OwnerPubKey, Signature, ...)
// 4b. EncryptionMeta{Encrypt: true, DEK: req.DEK, AccessList: req.AccessList, ...}
//     passed to StoreDataWithProgress

// Directory upload (OpDirUpload / OpECDHDirUpload):
// Two-phase (Sprint D.1 optimization):
// Phase 1: for each file in directory:
//   StoreDataCollectMeta(key, fileReader, opts, deferMeta=true) → StoredFileResult
// Phase 2: meta.WithBatch(func(batch) {
//   for each result: batch.SetManifest(key, result.Manifest)
//   batch.SetDirManifest(dirKey, dirManifestJSON)
// }) → single fsync for all metadata
```

#### Download flow (`cmd/handle_download.go`)

```go
// Download-to-file (OpDownloadToFile):
// 1. Read DownloadRequest{Key, OutputPath, Alias} from conn
// 2. Resolve alias → fingerprint (LookupAlias)
// 3. fullKey = fingerprint + "/" + key
// 4. manifest = s.Store.GetManifest(fullKey) OR fetchManifestFromPeers(fullKey)
// 5. id, ctx, _ = TransferMgr.Register(Download, fullKey, ...)
// 6. Check for .resume sidecar:
//      sidecar, skipSet, _ = resume.Open(outputPath)
//      fast-verify skipSet against manifest
// 7. Open .part file (os.OpenFile with O_RDWR|O_CREATE)
// 8. Downloader.DownloadToFileResumable(ctx, manifest, partFile, skipSet,
//      onProgress, sidecar.RecordChunk, dek)
//    → per-chunk: verify SHA-256, ban bad peer, write to offset
//    → after all: VerifyMerkleRoot
// 9. sidecar.Delete()
//    os.Rename(outputPath+".part", outputPath)  // atomic
// 10. TransferMgr.SetStatus(id, Completed, "")
//     StateDB.RecordDownload(...)
```

#### Manifest resolution (`handleGetManifestInfo`)

```go
// Unified stat: checks directory first, then file
// Returns 3-byte header: [statusByte][encryptedByte][isDirectoryByte]
func handleGetManifestInfo(conn net.Conn, s *server.Server) {
    key := readString(conn)
    // 1. Try local dirmanifest
    if dm, ok := s.Store.GetDirManifest(key); ok {
        writeHeader(conn, OK, dm.Encrypted, true)
        writeJSON(conn, dm)
        return
    }
    // 2. Try local chunk manifest
    if cm, ok := s.Store.GetManifest(key); ok {
        writeHeader(conn, OK, cm.Encrypted, false)
        writeJSON(conn, cm)
        return
    }
    // 3. Fetch from peers (MessageGetManifest → MessageManifestResponse)
    // Peer does same dual-check, responds with IsDirectory flag
    resp := s.fetchMetadataFromPeers(key)
    writeHeader(conn, OK, resp.Encrypted, resp.IsDirectory)
    writeJSON(conn, resp.Payload)
}
```

---

### Observability (`Observability/`)

```go
// health/
type HealthServer struct {
    httpServer *http.Server
}
// GET /health  → JSON {status, nodeAddr, peerCount, ringSize, uptime}
// GET /metrics → Prometheus text format
// GET /debug/pprof/* → pprof endpoints

// metrics/ — Prometheus counters and histograms wired throughout server
var (
    uploadsTotal     prometheus.Counter
    downloadsTotal   prometheus.Counter
    bytesUploaded    prometheus.Counter
    bytesDownloaded  prometheus.Counter
    activePeers      prometheus.Gauge
    transferDuration prometheus.Histogram
    quorumWriteTime  prometheus.Histogram
)

// tracing/ — OpenTelemetry spans exported via OTLP to Jaeger
// Key spans: upload, download, gossip round, rebalance, quorum write

// logging/ — Zerolog structured logging
// All logs include: "component", "peer", "key", "duration" fields
// TUI captures log output via io.Pipe for in-app display
```

---

### mDNS LAN Auto-Discovery (`Peer2Peer/mdns/`)

Hermod uses multicast DNS (mDNS) for AirDrop-style zero-config peer discovery on the local network. When a node starts, it advertises itself; when the TUI opens, it scans for other nodes.

#### Advertiser (`Peer2Peer/mdns/advertiser.go`)

```go
const ServiceTag = "_hermod._udp"

type Advertiser struct {
    server *hmdns.Server
}

// NewAdvertiser broadcasts this node's presence on the LAN.
// host must be a routable IP (from resolveOutboundIP, NOT 0.0.0.0).
func NewAdvertiser(host, alias, fingerprint string, port int) (*Advertiser, error) {
    ip := net.ParseIP(host)
    txt := []string{
        "alias=" + alias,
        "fp=" + fingerprint,
        "port=" + strconv.Itoa(port),
    }
    svc, _ := hmdns.NewMDNSService(alias, ServiceTag, "", "", port, []net.IP{ip}, txt)
    server, _ := hmdns.NewServer(&hmdns.Config{Zone: svc})
    return &Advertiser{server: server}, nil
}
```

The advertiser is started in `Server.Start()` after the transport binds, using the identity alias and fingerprint from `MakeServerOpts.IdentityMeta`. It is stopped in `GracefulShutdown()`.

#### Scanner (`Peer2Peer/mdns/scanner.go`)

```go
type DiscoveredPeer struct {
    Alias       string
    Fingerprint string
    Addr        string // "ip:port"
}

func Scan(timeout time.Duration) ([]DiscoveredPeer, error) {
    // Queries _hermod._udp via mDNS multicast (224.0.0.251).
    // Parses TXT records for alias, fingerprint, port.
    // Prefers IPv4 addresses over IPv6.
    // Returns all discovered Hermod nodes on the LAN.
}
```

#### Self-Filtering (`cmd/handle_peers.go`)

The IPC handler `handleScanLAN` filters out the scanning node itself by comparing both fingerprint and address:

```go
func handleScanLAN(conn net.Conn, s *server.Server) {
    found, _ := peermdns.Scan(3 * time.Second)
    selfFP := s.SelfFingerprint()
    selfAddr := s.SelfAddr()
    var peers []ipc.DiscoveredPeer
    for _, f := range found {
        if selfFP != "" && f.Fingerprint == selfFP { continue } // skip self
        if f.Addr == selfAddr { continue }                       // skip self
        peers = append(peers, ...)
    }
    writeJSONResponse(conn, peers)
}
```

#### TUI Integration

The TUI Network tab merges connected peers with discovered (unconnected) peers into a single table. Discovered peers show state "LAN" and pressing Enter triggers a connect action instead of browse:

```go
func (n *NetworkTab) rebuildPeerTable() {
    // 1. Add connected peers (state = "Alive"/"Dead"/etc)
    // 2. Deduplicate by fingerprint + address
    // 3. Append discovered peers with state "LAN"
}

// Enter key dispatch:
//   idx < len(connected)  → NetworkBrowseMsg (browse files)
//   idx >= len(connected) → NetworkConnectMsg (dial the peer)
```

IPC opcode: `0x1F` (`OpScanLAN`). Shared type: `ipc.DiscoveredPeer`.

---

### IP Resolution & Address Normalization (`Server/server.go`)

#### `resolveOutboundIP()` — 3-tier LAN IP discovery

Returns this machine's routable LAN IP. Critical for mDNS advertisement and peer announcements — advertising `0.0.0.0` or `docker0`'s `172.17.0.1` causes remote peers to receive an unreachable address.

```go
func resolveOutboundIP() string {
    // Tier 1: UDP routing trick — net.Dial("udp", "8.8.8.8:80")
    //   OS picks the interface that would route to 8.8.8.8.
    //   No packet sent (UDP connect only). Works on any LAN with internet.

    // Tier 2: Scored interface enumeration (air-gapped campus LAN)
    //   Skips virtual interfaces: docker*, veth*, virbr*, vmnet*, br-*,
    //     utun*, tun*, tap*, vbox*, hyperv*, lxcbr*, flannel*, cni*,
    //     calico*, podman*, tailscale*, wg*
    //   Skips: loopback, link-local, point-to-point
    //   Prefers RFC-1918 private IPs (10.x, 172.16-31.x, 192.168.x)
    //   Falls back to any non-virtual IPv4 address

    // Tier 3: "127.0.0.1" — absolute last resort (single-machine only)
}
```

**Why scored enumeration matters:** On a Linux host running Docker, `net.Interfaces()` returns `docker0` (172.17.0.1) before `wlan0` (192.168.x.x). Without filtering, the node advertises a Docker-internal IP that no remote peer can reach.

#### `effectiveSelfAddr()` — externally-visible address

```go
func (s *Server) effectiveSelfAddr() string {
    // Priority order:
    // 1. External address (from AnnounceAck or STUN) — works behind NAT
    // 2. resolveOutboundIP() — when bound to ":3000" or "0.0.0.0:3000"
    // 3. normalizeAddr() — when bound to a specific IP
}
```

#### `NormalizeUserAddr()` — bare IP default port

Ensures user-provided addresses always have a port. Bare IPs (`192.168.1.5`) get the default port (`:3000`) appended. Applied at all user-input boundaries: `--peer` flag, TUI connect, CLI connect.

```go
const DefaultPort = "3000"

func NormalizeUserAddr(addr string) string {
    if _, _, err := net.SplitHostPort(addr); err == nil {
        return addr // already has port
    }
    if ip := net.ParseIP(addr); ip != nil {
        return net.JoinHostPort(addr, DefaultPort) // "192.168.1.5" → "192.168.1.5:3000"
    }
    return addr // hostname or alias — return as-is for LookupAlias
}
```

---

### Cross-Platform Firewall Auto-Configuration (`cmd/firewall*.go`)

On first startup, Hermod automatically configures the OS firewall to allow inbound UDP traffic on its listening port. This is critical for cross-platform connectivity — Windows and macOS block inbound UDP by default.

#### Architecture

```go
// cmd/firewall.go — shared logic (all platforms)
const firewallFlagFile = ".firewall-configured"
// Flag file at ~/.hermod/.firewall-configured prevents re-prompting.

// cmd/firewall_windows.go
func ensureFirewallRule(port int) error {
    // 1. Check if rule already exists: netsh show rule name=Hermod
    // 2. Try direct netsh add rule (if already admin)
    // 3. Fall back to ShellExecuteW("runas", "netsh", ...) for UAC prompt
    //    → Standard Windows UAC dialog appears, user clicks "Yes" once
    // Rule: inbound UDP on port, program-scoped to hermod.exe
}

// cmd/firewall_linux.go
func ensureFirewallRule(port int) error {
    // Tries in order: ufw → firewall-cmd → iptables
    // Uses sudo -n (non-interactive) if not root
    // No GUI dependency (pkexec removed — doesn't work on headless/SSH)
}

// cmd/firewall_darwin.go
func ensureFirewallRule(port int) error {
    // Uses socketfilterfw via osascript "with administrator privileges"
    // macOS shows native admin password prompt
    // Adds + unblocks the hermod binary in Application Firewall
}
```

Called from `StartDaemonAsync()` in `cmd/daemon.go` before the server starts. Failure is non-fatal (logged as warning) — on networks without firewalls, it silently succeeds.

---

### TUI Logging Safety (`cmd/unified.go`)

The TUI uses bubbletea which takes exclusive control of stdout for terminal rendering (alt-screen mode). Any writes to stdout corrupt the display.

```go
// SAFE: redirect Go's standard logger to a file
log.SetOutput(logFile) // captures log.Printf from third-party packages

// SAFE: bubbletea's own debug capture
tea.LogToFile(filepath.Join(homeDir, ".hermod", "tui-debug.log"), "debug")

// DANGEROUS (removed): os.Stdout = logFile
// This redirected bubbletea's own terminal output to the log file,
// causing a blank screen with a blinking cursor.
```

The server's `MakeServer()` conditionally initializes logging only if no logger exists yet:

```go
if logging.Global == nil {
    logging.Init("server", logging.LevelInfo)
}
```

This prevents the daemon (started from the TUI process) from resetting the global logger to stdout after the TUI has already captured it.

---

### Critical Wiring Summary

The table below shows which struct field on `Server` wires to which subsystem call for the most common operations.

| Operation | Server field used | Key call |
|-----------|-------------------|----------|
| Store chunk locally | `Store` | `Store.StoreData(key, r, opts)` |
| Find replica nodes | `HashRing` | `HashRing.GetNodes(key, RF)` |
| Send chunk to peer | `peers[addr]` | `peer.SendStream(hdr, data)` |
| Pick best peer | `Selector` | `Selector.BestPeer(candidates)` |
| Download chunks | `Downloader` | `Downloader.DownloadToFileResumable(...)` |
| Verify chunk integrity | (inline in downloader) | `sha256(plaintext) == manifest.Chunks[i].Hash` |
| Write quorum | `Quorum` | `Quorum.Write(key, data, clock)` |
| Detect node failure | `HeartbeatSvc` | `PhiAccrualDetector.Phi(addr) >= 8.0` |
| Buffer unreachable write | `HandoffSvc` | `HandoffSvc.AddHint(hint)` |
| Repair divergence | `AntiEntropy` | `AntiEntropyService` runs every 10 min |
| Migrate on join | `Rebalancer` | `Rebalancer.OnNodeJoined(addr)` |
| Spread membership | `GossipSvc` | `GossipService` every 200ms |
| Persist history | `StateDB` | `StateDB.RecordUpload/Download(...)` |
| ECDH wrap key | (in upload handler) | `envelope.WrapDEKForRecipient(...)` |
| ECDH unwrap key | (in download handler) | `envelope.UnwrapDEK(...)` |
| Resume download | (in handle_download) | `resume.Open(path)` → `skipSet` → `DownloadToFileResumable` |
| Pause/cancel | `TransferMgr` | `TransferMgr.Pause/Cancel(id)` → context cancel |

---

*Implementation reference reflects codebase as of v0.3.0. Last updated March 2026.*
