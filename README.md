<p align="center">
  <h1 align="center">DFS-Go</h1>
  <p align="center">
    A peer-to-peer distributed file system built from scratch in Go
    <br />
    <strong>Encrypted · Content-Addressed · Replicated</strong>
  </p>
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Go-1.24-00ADD8?style=flat&logo=go" alt="Go 1.24" />
  <img src="https://img.shields.io/badge/License-MIT-green?style=flat" alt="MIT License" />
  <img src="https://img.shields.io/badge/TLS-mTLS_1.2+-blue?style=flat&logo=letsencrypt" alt="mTLS" />
  <img src="https://img.shields.io/badge/Encryption-AES--256--GCM-orange?style=flat" alt="AES-256-GCM" />
</p>

---

## Overview

**DFS-Go** is a distributed file storage system where multiple nodes form a peer-to-peer network to store, replicate, and retrieve files. Files are encrypted at rest using AES-256-GCM streaming encryption, distributed across nodes via consistent hashing, and all peer communication can be secured with mutual TLS (mTLS).

Upload a file from any node, and it is automatically encrypted, content-addressed, and replicated to the responsible nodes in the cluster. Download from any node, and it transparently fetches and decrypts the file — even if the data lives on a remote peer.

### Key Features

- **Content-Addressable Storage (CAS)** — Files are stored at paths derived from their SHA-256 hash with MD5 content fingerprints, ensuring deduplication and integrity
- **AES-256-GCM Streaming Encryption** — Files are encrypted in 4 MB chunks with per-chunk nonces; encryption keys are themselves encrypted with a master key (two-layer envelope encryption)
- **Consistent Hashing** — A virtual-node hash ring determines which N nodes are responsible for each key, minimizing data movement when nodes join or leave
- **Configurable Replication** — Replication factor (default N=3) controls how many copies of each file exist across the cluster
- **Mutual TLS (mTLS)** — Optional CA-signed certificate infrastructure with auto-generated node certificates for authenticated, encrypted peer communication
- **CLI Interface** — Simple `dfs start`, `dfs upload`, `dfs download` commands backed by a Unix socket daemon
- **Peer Auto-Discovery** — Nodes bootstrap from a peer list and automatically register joining/departing peers on the hash ring

---

## Architecture

```
┌────────────────────────────────────────────────────────────────┐
│                         DFS-Go Node                            │
│                                                                │
│  ┌──────────┐    ┌──────────────┐    ┌───────────────────┐     │
│  │   CLI    │───▶│  Unix Socket │───▶│   Server (core)   │     │
│  │ (cobra)  │    │   Daemon     │    │                   │     │
│  └──────────┘    └──────────────┘    │  ┌─────────────┐  │     │
│                                      │  │  Hash Ring  │  │     │
│                                      │  │ (consistent │  │     │
│                                      │  │  hashing)   │  │     │
│                                      │  └──────┬──────┘  │     │
│                                      │         │         │     │
│                                      │  ┌──────▼──────┐  │     │
│                                      │  │ Encryption  │  │     │
│                                      │  │ (AES-256-   │  │     │
│                                      │  │  GCM stream)│  │     │
│                                      │  └──────┬──────┘  │     │
│                                      │         │         │     │
│                                      │  ┌──────▼──────┐  │     │
│                                      │  │   Storage   │  │     │
│                                      │  │ (CAS on     │  │     │
│                                      │  │  disk)      │  │     │
│                                      │  └─────────────┘  │     │
│                                      └────────┬──────────┘     │
│                                               │                │
│                                      ┌────────▼──────────┐     │
│                                      │  TCP Transport     │     │
│                                      │  (optional mTLS)   │     │
│                                      └────────┬──────────┘     │
└───────────────────────────────────────────────┼────────────────┘
                                                │
                         ┌──────────────────────┼──────────────────────┐
                         │                      │                      │
                    ┌────▼────┐            ┌────▼────┐            ┌────▼────┐
                    │ Node B  │            │ Node C  │            │ Node D  │
                    └─────────┘            └─────────┘            └─────────┘
```

---

## Tech Stack

| Component | Technology |
|-----------|------------|
| Language | **Go 1.24** |
| CLI Framework | [Cobra](https://github.com/spf13/cobra) |
| Environment Config | [godotenv](https://github.com/joho/godotenv) |
| Encryption | AES-256-GCM (stdlib `crypto/aes`, `crypto/cipher`) |
| TLS / Certificates | stdlib `crypto/tls`, `crypto/x509`, `crypto/ecdsa` (P-256) |
| Hashing | SHA-256 (path transform), MD5 (content fingerprint) |
| Serialization | `encoding/gob` (peer messages) |
| IPC | Unix domain sockets (`/tmp/dfs.sock`) |
| Build | `make build` / `make run` / `make test` |

---

## Project Structure

```
DFS-Go/
├── main.go                     # Entrypoint — executes the Cobra root command
├── go.mod                      # Module: github.com/Faizan2005/DFS-Go
├── Makefile                    # build / run / test targets
├── .env                        # DFS_ENCRYPTION_KEY, DFS_ENABLE_TLS
│
├── cmd/                        # CLI layer (Cobra commands + daemon)
│   ├── root.go                 #   Root command definition
│   ├── start.go                #   `dfs start` — launches the daemon
│   ├── upload.go               #   `dfs upload --key <k> --file <f>`
│   ├── download.go             #   `dfs download --key <k> --output <o>`
│   ├── daemon.go               #   Unix socket daemon loop
│   ├── handle_client.go        #   Routes client JSON requests
│   ├── handle_upload.go        #   Sends upload over Unix socket
│   ├── handle_download.go      #   Sends download over Unix socket
│   └── socket.go               #   Socket path constant
│
├── Server/                     # Core distributed logic
│   └── server.go               #   Server struct, StoreData, GetData,
│                                #   message handling, replication, hash ring
│                                #   integration, bootstrap, peer lifecycle
│
├── Storage/                    # Content-addressable disk storage
│   ├── storage.go              #   Store, CAS path transform, ReadStream,
│   │                           #   WriteStream, MetaFile (JSON metadata)
│   └── storage_test.go         #   Storage unit tests
│
├── Crypto/                     # Encryption & TLS
│   ├── crypto.go               #   AES-256-GCM encrypt/decrypt (file + stream)
│   │                           #   Two-layer envelope encryption
│   ├── crypto_test.go          #   Encryption unit tests
│   ├── tls.go                  #   CA generation, node cert signing, mTLS config
│   │                           #   LoadMTLSConfig, LoadServerTLSConfig, etc.
│   └── tls_test.go             #   mTLS handshake & certificate tests
│
├── Peer2Peer/                  # Network transport layer
│   ├── transport.go            #   Peer & Transport interfaces
│   ├── tcpTransport.go         #   TCP (+ optional TLS) listener, dialer,
│   │                           #   connection handler, stream protocol
│   ├── handshake.go            #   Handshake functions (NOPE, TLSVerify,
│   │                           #   fingerprint pinning)
│   ├── message.go              #   RPC struct, control byte constants
│   └── encoding.go             #   GOB & Default decoders
│
├── Cluster/                    # Distributed coordination
│   └── hashring/
│       ├── hashring.go         #   Consistent hashing with virtual nodes
│       │                       #   AddNode, RemoveNode, GetNodes, GetPrimaryNode
│       └── hashring_test.go    #   Distribution uniformity, consistency tests
│
├── .certs/                     # Auto-generated TLS certificates (gitignored)
└── bin/                        # Compiled binary (gitignored)
    └── dfs
```

---

## How It Works

### Write Flow (Upload)

```
User: dfs upload --key "report.pdf" --file ./report.pdf
                │
                ▼
   1. CLI sends JSON over Unix socket
                │
                ▼
   2. Daemon calls server.StoreData("report.pdf", fileReader)
                │
                ▼
   3. Generate random AES-256 file key
   4. Encrypt file in 4 MB streaming chunks → temp file
   5. Encrypt the file key with master key (envelope encryption)
                │
                ▼
   6. Store encrypted data to CAS disk path:
      <root>/816cc/20437/d8595/.../md5hash
                │
                ▼
   7. Save metadata (key → path + encrypted key) to JSON
                │
                ▼
   8. Hash ring lookup: GetNodes("report.pdf", N=3)
      → returns [Node A (self), Node C, Node D]
                │
                ▼
   9. Replicate to Node C and Node D in parallel:
      ├─ Send MessageStoreFile (gob-encoded) with encrypted key
      └─ Stream encrypted bytes over TCP
```

### Read Flow (Download)

```
User: dfs download --key "report.pdf" --output ./out.pdf
                │
                ▼
   1. CLI sends JSON over Unix socket
                │
                ▼
   2. Daemon calls server.GetData("report.pdf")
                │
                ▼
   3. Check local disk — found?
      ├─ YES → Read encrypted data, decrypt with stored key, return
      └─ NO  → Continue to step 4
                │
                ▼
   4. Hash ring lookup: GetNodes("report.pdf", N=3)
      → target specific replica nodes (not broadcast)
                │
                ▼
   5. Send MessageGetFile to target peers
                │
                ▼
   6. Remote peer reads encrypted data from disk
   7. Sends MessageLocalFile + streams encrypted bytes back
                │
                ▼
   8. Receiving node stores encrypted copy locally
   9. Decrypts with stored key and returns plaintext
                │
                ▼
  10. CLI writes plaintext to --output file
```

### Peer Lifecycle

```
Node starts → Adds self to hash ring
            → Dials bootstrap peers
            → On peer connect:  add to hash ring, store in peers map
            → On peer disconnect: remove from hash ring, remove from peers map
```

---

## Getting Started

### Prerequisites

- **Go 1.24+** installed ([download](https://go.dev/dl/))

### Installation

```bash
git clone https://github.com/Faizan2005/DFS-Go.git
cd DFS-Go
```

### Configuration

Create a `.env` file (or copy the provided one):

```env
# REQUIRED: Encryption key for file encryption (change this!)
DFS_ENCRYPTION_KEY=your-secure-random-key-here

# Optional: Enable mutual TLS for peer connections
DFS_ENABLE_TLS=true
```

### Build

```bash
make build        # Compiles to ./bin/dfs
```

### Run a Cluster

**Terminal 1 — Start Node A (port 3000):**

```bash
./bin/dfs start --port :3000
```

**Terminal 2 — Start Node B (port 4000, connects to A):**

```bash
./bin/dfs start --port :4000 --peer :3000
```

**Terminal 3 — Start Node C (port 5000, connects to A):**

```bash
./bin/dfs start --port :5000 --peer :3000 --replication 3
```

### Upload & Download

```bash
# Upload a file (connects to the local daemon via Unix socket)
./bin/dfs upload --key "myfile" --file ./document.pdf

# Download a file
./bin/dfs download --key "myfile" --output ./retrieved.pdf
```

### Run Tests

```bash
make test         # Runs all tests with -v flag
```

---

## Security Model

### Encryption at Rest

Every file is encrypted before touching disk using a **two-layer envelope encryption** scheme:

1. A **random 256-bit file key** is generated per file
2. The file is encrypted with AES-256-GCM in **4 MB streaming chunks**, each with a unique nonce derived from the chunk index
3. The file key is encrypted with the **master key** (derived from `DFS_ENCRYPTION_KEY` via SHA-256)
4. Only the encrypted file key is stored in metadata — the master key never touches disk

### Encryption in Transit (mTLS)

When `DFS_ENABLE_TLS=true`:

1. A **Certificate Authority (CA)** is auto-generated on first run (ECDSA P-256, 10-year validity)
2. Each node gets a **CA-signed certificate** (1-year validity, auto-renewed)
3. Peer connections use **mutual TLS** — both sides present and verify certificates
4. `RequireAndVerifyClientCert` ensures only nodes with valid CA-signed certs can join
5. Connections use TLS 1.2+ with modern cipher suites (X25519, P-256)

### Content Integrity

Files are content-addressed using SHA-256 (directory path) + MD5 (filename), ensuring:
- Identical files map to the same path (deduplication)
- Any bit-flip in stored data is detectable

---

## Consistent Hashing

The hash ring uses **150 virtual nodes** per physical node by default, providing:

- **Uniform distribution** — Keys spread evenly across nodes (verified by tests at < 30% deviation)
- **Minimal disruption** — Adding/removing a node only moves ~1/N of keys (tested at < 40% movement)
- **Configurable replication** — `GetNodes(key, N)` returns N distinct physical nodes for any key

```
         ┌─────── Hash Ring ───────┐
         │                         │
    Node A ●                  ● Node B
         │    ● (virtual)          │
         │         ● (virtual)     │
         │                         │
    Node C ●              ● Node D │
         │                         │
         └─────────────────────────┘

    hashKey("report.pdf") → position on ring
    → walk clockwise → pick N distinct physical nodes
```

---

## Wire Protocol

Peer-to-peer messages use a simple control-byte + gob encoding protocol over TCP:

| Byte | Meaning | Followed By |
|------|---------|-------------|
| `0x1` | `IncomingMessage` | Gob-encoded `Message` struct |
| `0x2` | `IncomingStream` | Raw byte stream (file data) |

### Message Types

| Type | Direction | Purpose |
|------|-----------|---------|
| `MessageStoreFile` | Sender → Replicas | Replicate a file (includes key, size, encrypted key) |
| `MessageGetFile` | Requester → Replicas | Request a file by key |
| `MessageLocalFile` | Replica → Requester | Return a file (includes key, size) |

---

## Roadmap

- [ ] **Vector Clocks** — Causality tracking with Last-Writer-Wins conflict resolution
- [ ] **Quorum Reads/Writes** — Tunable consistency (R + W > N)
- [ ] **Failure Detection** — Gossip-based heartbeat protocol
- [ ] **Anti-Entropy** — Merkle tree synchronization for replica repair
- [ ] **Hinted Handoff** — Buffer writes for temporarily unreachable nodes
- [ ] **File Chunking** — Split large files into chunks with Merkle tree integrity
- [ ] **Multi-Source Download** — Parallel chunk retrieval from multiple peers
- [ ] **Compression** — zstd compression before encryption
- [ ] **QUIC Transport** — UDP-based transport for lower latency

---

## License

This project is open source under the [MIT License](LICENSE).

---

<p align="center">
  Built with ❤️ in Go
</p>
