<p align="center">
  <h1 align="center">Hermond</h1>
  <p align="center">
    The Swift P2P Data Herald
    <br />
    <strong>High-Speed LAN Transfers &middot; End-to-End Encrypted &middot; Zero Config</strong>
  </p>
</p>

<p align="center">
  <a href="https://github.com/MFZNK05/DFS-Go/releases/latest"><img src="https://img.shields.io/github/v/release/MFZNK05/DFS-Go?label=release&color=7D56F4&style=flat-square" alt="Release" /></a>
  <img src="https://img.shields.io/badge/Go-1.24-00ADD8?style=flat-square&logo=go" alt="Go 1.24" />
  <img src="https://img.shields.io/badge/License-MIT-green?style=flat-square" alt="MIT License" />
  <img src="https://img.shields.io/badge/Transport-QUIC-blue?style=flat-square" alt="QUIC" />
  <img src="https://img.shields.io/badge/Encryption-AES--256--GCM-orange?style=flat-square" alt="AES-256-GCM" />
</p>

---

## What is Hermond?

**Hermond** is a high-performance, decentralized file-sharing system built for campus LANs. It turns every machine on the network into a peer that can store, share, and retrieve files — no central server needed.

Think of it as a modern take on DC++ — but with end-to-end encryption, automatic peer discovery, and a real terminal UI.

### Why Hermond?

Moving large files on a campus network shouldn't require cloud uploads, USB drives, or sketchy FTP servers. Hermond lets you share game folders, movies, datasets, and project archives directly between machines at the full speed of your local network.

**What sets it apart:**

- **Encrypted Direct Send** — Push files directly to a specific person using ECDH (X25519) encryption. Only the intended recipient can decrypt the data. No server ever sees the plaintext.
- **LAN-Speed Transfers** — Built on QUIC with adaptive bandwidth management. Multi-GB transfers run at the physical limits of your network without saturating it for everyone else.
- **Zero-Config Discovery** — Nodes find each other via gossip protocol. No IP addresses to type, no config files to edit. Join the swarm and start sharing.
- **Persistent & Resumable** — Close your laptop mid-transfer? Hermond remembers exactly which chunks were downloaded and picks up where it left off.

---

## Table of Contents

- [Installation](#installation)
- [Quick Start](#quick-start)
- [The TUI](#the-tui)
- [CLI Reference](#cli-reference)
- [How It Works](#how-it-works)
- [Architecture](#architecture)
- [Package Reference](#package-reference)
- [Security Model](#security-model)
- [Tech Stack](#tech-stack)
- [License](#license)

---

## Installation

Hermond ships as a single, statically-linked binary. No runtime dependencies.

### One-Liner (Linux & macOS)

```sh
curl -sSfL https://raw.githubusercontent.com/MFZNK05/DFS-Go/main/install.sh | sh
```

### Homebrew (macOS & Linux)

```sh
brew install MFZNK05/hermond/hermond
```

### Manual Download (All Platforms)

Download the archive for your platform from the [Releases page](https://github.com/MFZNK05/DFS-Go/releases/latest):

| Platform | Architecture | Archive |
|----------|-------------|---------|
| Linux | x86_64 | `hermond_<ver>_linux_amd64.tar.gz` |
| Linux | ARM64 | `hermond_<ver>_linux_arm64.tar.gz` |
| macOS | Intel | `hermond_<ver>_darwin_amd64.tar.gz` |
| macOS | Apple Silicon | `hermond_<ver>_darwin_arm64.tar.gz` |
| Windows | x86_64 | `hermond_<ver>_windows_amd64.zip` |
| Windows | ARM64 | `hermond_<ver>_windows_arm64.zip` |

Extract the binary, move it to a directory in your PATH, and you're done.

> **Detailed instructions** for all platforms (including Windows PATH setup and checksum verification) are in [INSTALL.md](INSTALL.md).

### Verify

```sh
hermond version
```

---

## Quick Start

### 1. Create your identity

Every node needs a keypair for encryption and peer identification:

```sh
hermond identity init --alias "alice"
```

### 2. Launch Hermond

Run without arguments to start the daemon and open the TUI:

```sh
hermond --port :3000
```

On another machine, join the first node:

```sh
hermond --port :4000 --peer <alice-ip>:3000
```

Nodes discover each other automatically from there — no need to manually connect every pair.

### 3. Share files

```sh
# Upload a file (visible to the whole network)
hermond upload --name "movie.mkv" --file ./movie.mkv --public

# Upload a directory
hermond upload --name "game-folder" --dir ./Elden\ Ring/ --public

# Encrypt and share with a specific person
hermond upload --name "notes.pdf" --file ./notes.pdf --share-with bob

# Download from the network
hermond download --name "movie.mkv" --output ./movie.mkv --from alice
```

---

## The TUI

Launch the full-screen terminal dashboard with `hermond` (or `hermond tui` to connect to a running daemon).

### Network Tab

Peer roster with live connection state, aliases, fingerprints, and public file counts. Select a peer and browse their shared catalog.

### Transfers Tab

Real-time view of all uploads and downloads with progress bars, speeds, and status (queued / active / paused / completed / failed). Pause, resume, or cancel any transfer.

### Vault Tab

Your file management hub. Two sub-views:

- **Shares** — Everything you've uploaded, filterable by All / Public / Private (encrypted)
- **Downloads** — Your download history with timestamps and status

### Diagnostics Tab

Node health dashboard: address, uptime, peer count, upload/download totals, and live metrics.

### TUI Keyboard Shortcuts

| Key | Action |
|-----|--------|
| `Tab` / `Shift+Tab` | Switch tabs |
| `Ctrl+U` | Open upload modal |
| `Ctrl+D` | Open download prompt |
| `Ctrl+H` | Show help overlay |
| `Ctrl+C` | Quit |

---

## CLI Reference

All commands support the `--node` flag (default `:3000`) to target a specific local daemon.

### Core Commands

| Command | Description |
|---------|-------------|
| `hermond` | Start daemon + launch TUI |
| `hermond upload --name <n> --file <f>` | Upload a file to the network |
| `hermond upload --name <n> --dir <d>` | Upload a directory |
| `hermond download --name <n> --output <o>` | Download a file or directory |
| `hermond send <file-key> <peer>` | Direct-send a file to a specific peer |
| `hermond search <query>` | Flood-search the network for files |

### Upload Flags

| Flag | Description |
|------|-------------|
| `--name`, `-k` | Name to associate with the upload |
| `--file`, `-f` | Path to a file |
| `--dir`, `-d` | Path to a directory |
| `--public` | Mark as publicly browsable |
| `--share-with` | Comma-separated aliases for ECDH encryption |
| `--share-with-key` | Comma-separated X25519 public keys (fallback) |

### Download Flags

| Flag | Description |
|------|-------------|
| `--name`, `-k` | Name to retrieve |
| `--output`, `-o` | Output path (optional, defaults to stdout for files) |
| `--from` | Alias of the file owner (defaults to self) |
| `--from-key` | Fingerprint of the owner (for duplicate aliases) |

### Network & Status

| Command | Description |
|---------|-------------|
| `hermond peers` | List connected peers |
| `hermond browse <peer-addr>` | Browse a peer's public files |
| `hermond status` | Show node status (uptime, peer count, files) |
| `hermond connect <addr-or-alias>` | Manually connect to a peer |

### Transfer Management

| Command | Description |
|---------|-------------|
| `hermond transfers` | List all active transfers |
| `hermond pause <id>` | Pause a transfer |
| `hermond resume <id>` | Resume a paused transfer |
| `hermond cancel <id>` | Cancel a transfer |

### File Management

| Command | Description |
|---------|-------------|
| `hermond list uploads` | Show upload history (`--public` / `--private` filters) |
| `hermond list downloads` | Show download history |
| `hermond list public` | Show public file catalog |
| `hermond remove <name>` | Remove a file (propagates tombstone across cluster) |

### Identity

| Command | Description |
|---------|-------------|
| `hermond identity init --alias <name>` | Generate a new Ed25519 + X25519 keypair |
| `hermond identity show` | Display fingerprint, alias, and public keys |
| `hermond version` | Print version, commit, and build date |

---

## How It Works

### Upload Flow

```
hermond upload --name "report.pdf" --file ./report.pdf --public
              |
              v
  1. File split into 4 MB chunks, each SHA-256 hashed
  2. Chunks encrypted with AES-256-GCM (random per-file DEK)
  3. Compressed with zstd before encryption
  4. Stored locally in content-addressed paths (SHA-256 dirs + MD5 filename)
  5. ChunkManifest built with Merkle root for integrity
  6. Hash ring determines N replica nodes
  7. Chunks replicated in parallel over QUIC
  8. Metadata persisted to local BoltDB state
```

### Download Flow

```
hermond download --name "report.pdf" --output ./out.pdf --from alice
              |
              v
  1. Fetch ChunkManifest from local store or peers
  2. Check for .part file (resume partial download if exists)
  3. Fast-verify already-downloaded chunks against manifest
  4. Download remaining chunks in parallel from multiple peers
  5. Each chunk verified on-the-fly (SHA-256 vs manifest)
  6. Bad peers banned, chunks re-fetched from alternates
  7. Atomic rename: .part -> final file after Merkle verify
  8. Download recorded in local state
```

### Peer Discovery

```
Node starts
  -> Dials bootstrap peers (--peer flag)
  -> Gossip protocol propagates membership (O(log N) convergence)
  -> Identity metadata exchanged (alias, fingerprint, public keys)
  -> Hash ring updated automatically
  -> STUN-based NAT traversal for cross-subnet connectivity
```

---

## Architecture

```
+-------------------------------------------------------------------+
|                         Hermond Node                              |
|                                                                   |
|  +----------+    +-----------+    +---------------------------+   |
|  | TUI      |--->| IPC       |--->|  Server (orchestrator)    |   |
|  | (Bubble  |    | (Unix     |    |                           |   |
|  |  Tea)    |    |  Socket)  |    |  Gossip    Hash Ring      |   |
|  +----------+    +-----------+    |  Quorum    Heartbeat      |   |
|  +----------+                     |  Handoff   Rebalancer     |   |
|  | CLI      |----+               |  Selector  Anti-Entropy   |   |
|  | (Cobra)  |    |               |  Bandwidth Transfer Mgr   |   |
|  +----------+    |               +-------------+-------------+   |
|                  |                             |                  |
|                  |               +-------------v-------------+   |
|                  |               |  Storage Engine            |   |
|                  |               |  CAS + Chunker + Compress  |   |
|                  |               |  + Resume + Dir Manifest   |   |
|                  |               +-------------+-------------+   |
|                  |                             |                  |
|                  |               +-------------v-------------+   |
|                  +-------------->|  QUIC Transport            |   |
|                                  |  (TLS 1.3, per-stream)    |   |
|                                  +-------------+-------------+   |
+---------------------------------------|---------------------------+
                                        |
              +-------------------------+-------------------------+
              |                         |                         |
        +-----v-----+            +-----v-----+            +-----v-----+
        |  Peer B   |            |  Peer C   |            |  Peer D   |
        +-----------+            +-----------+            +-----------+
```

---

## Package Reference

Hermond is organized into isolated sub-packages with no circular dependencies.

### Core

| Package | Description |
|---------|-------------|
| `Server/` | Central orchestrator — peer lifecycle, storage, replication, message routing |
| `Server/downloader/` | Parallel multi-source chunk fetcher with on-the-fly SHA-256 verification |
| `Server/transfer/` | In-memory transfer registry — progress tracking, pause/resume/cancel |
| `Server/ratelimit/` | LEDBAT-lite adaptive bandwidth — monitors QUIC RTT, auto-throttles |

### Storage

| Package | Description |
|---------|-------------|
| `Storage/` | Content-addressable store — SHA-256 directory paths, MD5 filenames, AES-GCM streaming |
| `Storage/chunker/` | File chunking (4 MB) with SHA-256 per-chunk hashing and Merkle tree construction |
| `Storage/compression/` | Zstandard compress/decompress layer applied before encryption |
| `Storage/dirmanifest/` | Directory manifest — wraps N file manifests as a table of contents |
| `Storage/resume/` | Download resume sidecar — append-only log, O(1) per chunk, fast-verify on restart |
| `Storage/pending/` | Upload resume sidecar — crash-safe tracking of partially uploaded directories |

### Cluster

| Package | Description |
|---------|-------------|
| `Cluster/gossip/` | Push-pull epidemic protocol — O(log N) convergence, fingerprint-based self-detection |
| `Cluster/hashring/` | Consistent hashing with 150 virtual nodes per physical node |
| `Cluster/quorum/` | Tunable W-of-N write / R-of-N read quorum with lock-free routing |
| `Cluster/membership/` | Node liveness tracking with generation counters (stale update prevention) |
| `Cluster/handoff/` | Hinted handoff — buffers writes for unreachable nodes, replays on rejoin |
| `Cluster/rebalance/` | Background chunk migration on topology changes |
| `Cluster/selector/` | Peer selection via EWMA latency tracking — picks fastest peer per chunk |
| `Cluster/conflict/` | Last-Write-Wins conflict resolution using wall-clock timestamps |
| `Cluster/failure/` | Failure detection with configurable probe intervals and suspect thresholds |
| `Cluster/vclock/` | Vector clocks for causal ordering of concurrent writes |
| `Cluster/merkle/` | Anti-entropy Merkle tree sync — periodic diff-based replica repair |

### Crypto

| Package | Description |
|---------|-------------|
| `Crypto/identity/` | Per-node Ed25519 (signing) + X25519 (key exchange) keypairs |
| `Crypto/envelope/` | ECDH key wrapping (`WrapDEKForRecipient` / `UnwrapDEK`) and manifest signing |

### Networking

| Package | Description |
|---------|-------------|
| `Peer2Peer/quic/` | QUIC transport — self-signed TLS 1.3, per-message streams, no head-of-line blocking |
| `Peer2Peer/nat/` | STUN-based NAT traversal — discovers external address, LAN-only fallback on failure |

### State & IPC

| Package | Description |
|---------|-------------|
| `State/` | Persistent local state (BoltDB) — uploads, downloads, public catalog, tombstones, peers |
| `ipc/` | Inter-process communication wire protocol — 18 opcodes for all CLI/TUI operations |

### Interface

| Package | Description |
|---------|-------------|
| `tui/` | Terminal UI (Bubble Tea) — 4 tabs, upload/download modals, keyboard-driven |
| `tui/tabs/` | Individual tab implementations (Network, Transfers, Vault, Diagnostics) |
| `tui/components/` | Reusable UI components (tables, modals, header, footer) |
| `cmd/` | Cobra CLI commands and IPC client helpers |

### Observability

| Package | Description |
|---------|-------------|
| `Observability/health/` | HTTP health-check server (`/health`, `/metrics`, `/debug/pprof/*`) |
| `Observability/metrics/` | Prometheus instrumentation (`dfs_*` metric families) |
| `Observability/tracing/` | OpenTelemetry distributed tracing (OTLP export to Jaeger) |
| `Observability/logging/` | Structured logging via zerolog |

---

## Security Model

### End-to-End Encryption (ECDH)

When you use `--share-with`, Hermond performs a full Diffie-Hellman key exchange:

1. A random **256-bit DEK** (Data Encryption Key) is generated per file
2. The file is encrypted with **AES-256-GCM** in 4 MB streaming chunks
3. The DEK is wrapped for each recipient using **X25519 ECDH** — only the recipient's private key can unwrap it
4. The manifest is signed with the uploader's **Ed25519** key for authenticity
5. No server, relay, or intermediary ever has access to the plaintext or the DEK

### Content Integrity

- Every chunk is SHA-256 hashed and recorded in a **Merkle tree**
- On download, each chunk is verified against the manifest before writing to disk
- Corrupted chunks are discarded and re-fetched from alternate peers
- Final file verified against the **Merkle root** before atomic rename

### Identity

Each node has a unique cryptographic identity:

- **Ed25519** key for signing manifests and proving ownership
- **X25519** key for ECDH key exchange
- **Fingerprint** = first 16 hex chars of SHA-256(Ed25519 public key)
- Fingerprints namespace all files — no name collisions between users

---

## Tech Stack

| Component | Technology |
|-----------|------------|
| Language | Go 1.24 |
| Transport | QUIC (quic-go) with TLS 1.3 |
| CLI | Cobra |
| TUI | Bubble Tea + Lip Gloss |
| Local State | BoltDB |
| Encryption | AES-256-GCM (streaming), X25519 ECDH, Ed25519 |
| Compression | Zstandard (zstd) |
| Hashing | SHA-256 (integrity), MD5 (CAS filenames) |
| Bandwidth | LEDBAT-lite (golang.org/x/time/rate) |
| Observability | Prometheus + OpenTelemetry + zerolog |
| CI/CD | GitHub Actions + GoReleaser |

---

## Building from Source

```sh
git clone https://github.com/MFZNK05/DFS-Go.git
cd DFS-Go
go build -o hermond .
./hermond version
```

### Running Tests

```sh
go test ./...
```

---

## License

This project is open source under the [MIT License](LICENSE).

---

<p align="center">
  <sub>Built with Go. Shipped as a single binary.</sub>
</p>
