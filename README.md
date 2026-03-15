<p align="center">
  <h1 align="center">Hermod</h1>
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

## What is Hermod?

**Hermod** is a high-performance, decentralized file-sharing system built for campus LANs. It turns every machine on the network into a peer that can store, share, and retrieve files — no central server needed.

Think of it as a modern take on DC++ — but with end-to-end encryption, automatic peer discovery, and a real terminal UI.

### Why Hermod?

Moving large files on a campus network shouldn't require cloud uploads, USB drives, or sketchy FTP servers. Hermod lets you share game folders, movies, datasets, and project archives directly between machines at the full speed of your local network.

**What sets it apart:**

- **Encrypted Direct Send** — Push files directly to a specific person using ECDH (X25519) encryption. Only the intended recipient can decrypt the data. No server ever sees the plaintext.
- **LAN-Speed Transfers** — Built on QUIC with native congestion control. Multi-GB transfers run at the physical limits of your network without saturating it for everyone else.
- **Zero-Config Discovery** — Nodes on the same LAN find each other automatically via mDNS. Across subnets, gossip propagates membership. No IP addresses to type, no config files to edit.
- **Persistent & Resumable** — Close your laptop mid-transfer? Hermod remembers exactly which chunks were downloaded and picks up where it left off.

---

## Table of Contents

- [Installation](#installation)
- [First Run](#first-run)
- [Quick Start](#quick-start)
- [The TUI](#the-tui)
- [CLI Reference](#cli-reference)
- [How It Works](#how-it-works)
- [Architecture](#architecture)
- [Package Reference](#package-reference)
- [Security Model](#security-model)
- [Tech Stack](#tech-stack)
- [Data Directory](#data-directory)
- [License](#license)

---

## Installation

Hermod ships as a single, statically-linked binary. No runtime dependencies.

### One-Liner (Linux & macOS)

```sh
curl -sSfL https://raw.githubusercontent.com/MFZNK05/DFS-Go/main/install.sh | sh
```

### Homebrew (macOS & Linux)

```sh
brew install MFZNK05/hermod/hermod
```

### Manual Download (All Platforms)

Download the archive for your platform from the [Releases page](https://github.com/MFZNK05/DFS-Go/releases/latest):

| Platform | Architecture | Archive |
|----------|-------------|---------|
| Linux | x86_64 | `hermod_<ver>_linux_amd64.tar.gz` |
| Linux | ARM64 | `hermod_<ver>_linux_arm64.tar.gz` |
| macOS | Intel | `hermod_<ver>_darwin_amd64.tar.gz` |
| macOS | Apple Silicon | `hermod_<ver>_darwin_arm64.tar.gz` |
| Windows | x86_64 | `hermod_<ver>_windows_amd64.zip` |
| Windows | ARM64 | `hermod_<ver>_windows_arm64.zip` |

Extract the binary, move it to a directory in your PATH, and you're done.

> **Detailed instructions** for all platforms (including Windows PATH setup and checksum verification) are in [INSTALL.md](INSTALL.md).

### Verify

```sh
hermod version
```

---

## First Run

The first time you run `hermod`, it walks you through a one-time setup:

```
$ hermod
Welcome to Hermod!
Enter your alias: alice
Control port (default 3000, data port will be 3001): 3000

Identity created: alice (fingerprint: c34144eb91e1cb10)
Port configured: 3000 (data port: 3001)
```

| Setting | Details |
|---------|---------|
| **Alias** | Your permanent name on the network. Tied to your cryptographic fingerprint — cannot be changed later. |
| **Control port** | Used for gossip, heartbeat, and metadata. Default `3000`. |
| **Data port** | Used for chunk transfers. Always control port + 1 (automatic). |

Both are saved to `~/.hermod/identity.json` and `~/.hermod/config.json`. On subsequent launches, Hermod loads these automatically.

Hermod also auto-configures your OS firewall on first startup:
- **Windows:** UAC prompt to add netsh inbound UDP rules
- **macOS:** Admin password prompt to configure Application Firewall
- **Linux:** Uses `ufw`, `firewall-cmd`, or `iptables` (non-interactive `sudo -n`)

If firewall setup fails, Hermod continues — you may need to open the ports manually.

> **Non-interactive setup:** If you prefer to skip the prompt (scripting, CI), run `hermod identity init --alias <name>` before first launch.

---

## Quick Start

### 1. Launch Hermod

```sh
hermod
```

This forks a background daemon and opens the TUI dashboard. First run will prompt for setup (see [First Run](#first-run)).

### 2. Connect to a peer

On another machine on the same LAN subnet, just run `hermod` — peers discover each other automatically via mDNS. No flags needed.

To connect across subnets or VLANs:

```sh
hermod --peer <alice-ip>:3000
```

Nodes discover the rest of the cluster automatically from there via gossip — no need to manually connect every pair.

### 3. Share files

```sh
# Upload a file (visible to the whole network)
hermod upload --name "movie.mkv" --file ./movie.mkv --public

# Upload a directory
hermod upload --name "game-folder" --dir ./Elden\ Ring/ --public

# Encrypt and share with a specific person
hermod upload --name "notes.pdf" --file ./notes.pdf --share-with bob

# Download from the network
hermod download --name "movie.mkv" --output ./movie.mkv --from alice
```

---

## The TUI

The TUI is a **stateless IPC client** — it connects to the background daemon over a Unix socket (or named pipe on Windows). Pressing `q` or `Ctrl+C` exits only the TUI; the daemon keeps running.

```
Hermod is still running in the background. Use 'hermod stop' to shut it down.
```

To reattach to a running daemon at any time: `hermod tui`

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
| `d` | Download (in Network tab) |
| `?` | Show help overlay |
| `q` / `Ctrl+C` | Quit TUI (daemon keeps running) |

---

## CLI Reference

### Core Commands

| Command | Description |
|---------|-------------|
| `hermod` | Fork daemon + launch TUI (first run prompts for setup) |
| `hermod start` | Start daemon only (headless, blocks until stopped) |
| `hermod stop` | Gracefully shut down a running daemon |
| `hermod tui` | Attach TUI to an already-running daemon |
| `hermod upload --name <n> --file <f>` | Upload a file to the network |
| `hermod upload --name <n> --dir <d>` | Upload a directory |
| `hermod download --name <n> --output <o>` | Download a file or directory |
| `hermod send <file-key> <peer>` | Send a direct-share notification to a peer |
| `hermod search <query>` | Flood-search the network for files |

### Daemon Flags

| Flag | Applies to | Description |
|------|-----------|-------------|
| `--port` | `hermod`, `hermod start` | Control port (default `:3000`, data port is automatically N+1) |
| `--peer` | `hermod`, `hermod start` | Bootstrap peer address(es) (repeatable or comma-separated) |
| `--no-stun` | `hermod`, `hermod start` | Disable STUN NAT traversal |
| `--replication` | `hermod start` | Replicas per file (default 3) |
| `--max-peers` | `hermod start` | Target number of active peer connections (default 8) |
| `--node` (`-n` for stop) | `hermod stop`, `hermod tui` | Port of daemon to target (default `:3000`) |

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
| `hermod peers` | List connected peers |
| `hermod browse <peer-addr>` | Browse a peer's public files |
| `hermod status` | Show node status (uptime, peer count, files) |
| `hermod connect <addr-or-alias>` | Connect to a peer (also unblocks if blocklisted) |
| `hermod disconnect <addr-or-alias>` | Disconnect and blocklist a peer |
| `hermod unblock <addr-or-alias>` | Remove a peer from the blocklist |

### Transfer Management

| Command | Description |
|---------|-------------|
| `hermod transfers` | List all active transfers |
| `hermod pause <id>` | Pause a transfer |
| `hermod resume <id>` | Resume a paused transfer |
| `hermod cancel <id>` | Cancel a transfer |

### File Management

| Command | Description |
|---------|-------------|
| `hermod list uploads` | Show upload history (`--public` / `--private` filters) |
| `hermod list downloads` | Show download history |
| `hermod list public` | Show public file catalog |
| `hermod remove <name>` | Remove a file (propagates tombstone across cluster) |

### Identity

| Command | Description |
|---------|-------------|
| `hermod identity init --alias <name>` | Generate a new Ed25519 + X25519 keypair (optional — handled by first-run prompt) |
| `hermod identity show` | Display fingerprint, alias, and public keys |
| `hermod version` | Print version, commit, and build date |

---

## How It Works

### Upload Flow

```
hermod upload --name "report.pdf" --file ./report.pdf --public
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
hermod download --name "report.pdf" --output ./out.pdf --from alice
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
Node starts (hermod forks daemon in background)
  -> mDNS advertises on LAN (_hermod._udp)
  -> Same-subnet peers discovered automatically (no config)
  -> Dials bootstrap peers if provided (--peer flag)
  -> Gossip protocol propagates membership (O(log N) convergence)
  -> Identity metadata exchanged (alias, fingerprint, public keys)
  -> Hash ring updated automatically
  -> STUN-based NAT traversal for cross-subnet connectivity
```

---

## Architecture

Hermod uses a **daemon + client** model. The daemon runs as a detached background process; the TUI and CLI connect to it via IPC. Network traffic is split across **two QUIC ports**: a control port for cluster coordination and a data port for bulk chunk transfers.

```
+-------------------------------------------------------------------+
|                     Hermod Daemon (background)                     |
|                                                                    |
|  +------------------------+   +------------------------+          |
|  | Control Port (:N)      |   | Data Port (:N+1)       |          |
|  | Gossip, Heartbeat,     |   | Chunk Transfers         |          |
|  | Metadata, Search       |   | (QUIC streams)          |          |
|  +-----------+------------+   +-----------+------------+          |
|              |                            |                        |
|  +-----------v----------------------------v------------+          |
|  |  Server (orchestrator)                               |          |
|  |                                                      |          |
|  |  Gossip    Hash Ring    Quorum    Heartbeat           |          |
|  |  Handoff   Rebalancer   Selector  Anti-Entropy        |          |
|  |  Bandwidth  Transfer Mgr  mDNS    NAT/STUN           |          |
|  +-------------------------+----------------------------+          |
|                            |                                       |
|  +-------------------------v----------------------------+          |
|  |  Storage Engine                                       |          |
|  |  CAS + Chunker + Compress + Resume + Swarm Cache      |          |
|  +-------------------------+----------------------------+          |
|                            |                                       |
|  +-------------------------v----------------------------+          |
|  |  IPC Socket                                           |          |
|  |  Unix: /tmp/hermod-N.sock  |  Win: \\.\pipe\hermod-N  |          |
|  +------+----------------------+------------------------+          |
+---------|----------------------|-------------------------------+
          |                      |
   +------v------+       +------v------+
   | TUI         |       | CLI         |
   | (Bubble Tea)|       | (Cobra)     |
   | hermod /    |       | hermod ...  |
   | hermod tui  |       |             |
   +-------------+       +-------------+
```

---

## Package Reference

Hermod is organized into isolated sub-packages with no circular dependencies.

### Core

| Package | Description |
|---------|-------------|
| `Server/` | Central orchestrator — peer lifecycle, storage, replication, dual-port routing |
| `Server/downloader/` | Parallel multi-source chunk fetcher with on-the-fly SHA-256 verification |
| `Server/transfer/` | In-memory transfer registry — progress tracking, pause/resume/cancel |

### Storage

| Package | Description |
|---------|-------------|
| `Storage/` | Content-addressable store — SHA-256 directory paths, MD5 filenames, AES-GCM streaming |
| `Storage/chunker/` | File chunking (4 MB) with SHA-256 per-chunk hashing and Merkle tree construction |
| `Storage/compression/` | Zstandard compress/decompress layer applied before encryption |
| `Storage/dirmanifest/` | Directory manifest — wraps N file manifests as a table of contents |
| `Storage/resume/` | Download resume sidecar — append-only log, O(1) per chunk, fast-verify on restart |
| `Storage/pending/` | Upload resume sidecar — crash-safe tracking of partially uploaded directories |
| `Storage/swarm/` | Seed-in-place cache — serve chunks from original file, LRU eviction (50 GB default) |

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
| `Peer2Peer/quic/` | QUIC transport — dual-port (control + data), TLS 1.3, per-message streams |
| `Peer2Peer/mdns/` | mDNS LAN auto-discovery — advertises node presence, scans for peers on same subnet |
| `Peer2Peer/nat/` | STUN-based NAT traversal — discovers external address, LAN-only fallback on failure |

### State & IPC

| Package | Description |
|---------|-------------|
| `State/` | Persistent local state (BoltDB) — uploads, downloads, public catalog, tombstones, peers |
| `ipc/` | Inter-process communication wire protocol — 35 opcodes for all CLI/TUI operations |

### Interface

| Package | Description |
|---------|-------------|
| `tui/` | Terminal UI (Bubble Tea) — stateless IPC client, 4 tabs, keyboard-driven |
| `tui/tabs/` | Individual tab implementations (Network, Transfers, Vault, Diagnostics) |
| `tui/components/` | Reusable UI components (tables, modals, header, footer) |
| `cmd/` | Cobra CLI, daemon lifecycle (`start`/`stop`), cross-platform firewall, IPC helpers |

### Observability

| Package | Description |
|---------|-------------|
| `Observability/health/` | HTTP health-check server (`/health`, `/metrics`, `/debug/pprof/*`) |
| `Observability/metrics/` | Prometheus instrumentation (`dfs_*` metric families) |
| `Observability/tracing/` | OpenTelemetry distributed tracing (OTLP export to Jaeger) |
| `Observability/logging/` | Structured logging via zerolog |
| `Observability/memlog/` | OOM forensics — periodic heap stats, auto heap profile dumps at thresholds |

---

## Security Model

### End-to-End Encryption (ECDH)

When you use `--share-with`, Hermod performs a full Diffie-Hellman key exchange:

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
| Discovery | mDNS (hashicorp/mdns) + Gossip |
| Bandwidth | QUIC native congestion control (no application-layer throttling) |
| Observability | Prometheus + OpenTelemetry + zerolog |
| CI/CD | GitHub Actions + GoReleaser |

---

## Data Directory

All runtime data lives under `~/.hermod/` regardless of where you run the binary from.

| File / Directory | Purpose |
|-----------------|---------|
| `identity.json` | Ed25519 + X25519 keypair and alias (permanent) |
| `config.json` | Saved port preference from first-run prompt |
| `daemon.log` | Daemon stdout/stderr output |
| `tui-debug.log` | TUI debug output (Bubble Tea) |
| `state-<port>.db` | BoltDB local state (uploads, downloads, peers, public catalog) |
| `_<port>_metadata.db` | Chunk and directory manifests |
| `_<port>_network/` | CAS chunk storage (SHA-256 directory tree) |
| `.firewall-configured` | Flag file to prevent re-running firewall setup on each launch |

> **Upgrading?** Hermod auto-migrates `~/.dfs/` to `~/.hermod/` on first run after upgrade.

---

## Building from Source

```sh
git clone https://github.com/MFZNK05/DFS-Go.git
cd DFS-Go
go build -o hermod .
./hermod version
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
