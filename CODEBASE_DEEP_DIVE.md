# DFS-Go: Complete Codebase Deep Dive

> A function-by-function, package-by-package analysis of how every component of this distributed file system works and how they all orchestrate together.

---

## Table of Contents

1. [Architecture Overview](#1-architecture-overview)
2. [Package: `main` — Entry Point](#2-package-main--entry-point)
3. [Package: `cmd` — CLI & IPC Layer](#3-package-cmd--cli--ipc-layer)
4. [Package: `Crypto` — Encryption & TLS](#4-package-crypto--encryption--tls)
5. [Package: `Storage` — Local Disk Persistence](#5-package-storage--local-disk-persistence)
6. [Package: `Storage/chunker` — File Chunking](#6-package-storagechunker--file-chunking)
7. [Package: `Storage/compression` — Zstd Compression](#7-package-storagecompression--zstd-compression)
8. [Package: `Peer2Peer` — Network Transport](#8-package-peer2peer--network-transport)
9. [Package: `Peer2Peer/quic` — QUIC Transport](#9-package-peer2peerquic--quic-transport)
10. [Package: `Peer2Peer/nat` — NAT Traversal](#10-package-peer2peernat--nat-traversal)
11. [Package: `factory` — Transport Factory](#11-package-factory--transport-factory)
12. [Package: `Cluster/vclock` — Vector Clocks](#12-package-clustervclock--vector-clocks)
13. [Package: `Cluster/conflict` — Conflict Resolution](#13-package-clusterconflict--conflict-resolution)
14. [Package: `Cluster/hashring` — Consistent Hashing](#14-package-clusterhashring--consistent-hashing)
15. [Package: `Cluster/membership` — Cluster Membership](#15-package-clustermembership--cluster-membership)
16. [Package: `Cluster/failure` — Failure Detection](#16-package-clusterfailure--failure-detection)
17. [Package: `Cluster/gossip` — Gossip Protocol](#17-package-clustergossip--gossip-protocol)
18. [Package: `Cluster/quorum` — Quorum Reads/Writes](#18-package-clusterquorum--quorum-readswrites)
19. [Package: `Cluster/handoff` — Hinted Handoff](#19-package-clusterhandoff--hinted-handoff)
20. [Package: `Cluster/rebalance` — Data Rebalancing](#20-package-clusterrebalance--data-rebalancing)
21. [Package: `Cluster/merkle` — Anti-Entropy Sync](#21-package-clustermerkle--anti-entropy-sync)
22. [Package: `Cluster/selector` — Peer Selection](#22-package-clusterselector--peer-selection)
23. [Package: `Server/downloader` — Parallel Downloads](#23-package-serverdownloader--parallel-downloads)
24. [Package: `Observability` — Health, Logging, Metrics, Tracing](#24-package-observability--health-logging-metrics-tracing)
25. [Package: `Server` — The Orchestrator](#25-package-server--the-orchestrator)
26. [End-to-End Data Flows](#26-end-to-end-data-flows)
27. [How All Packages Orchestrate Together](#27-how-all-packages-orchestrate-together)

---

## 1. Architecture Overview

DFS-Go is a **decentralised, peer-to-peer distributed file system** written in Go. It has no single master — every node is equal. Files uploaded through any node are:

1. **Chunked** into 4 MiB pieces
2. **Compressed** (when beneficial) using zstd
3. **Encrypted** with AES-256-GCM
4. **Replicated** to N nodes determined by a consistent hash ring
5. **Retrievable** from any node in the cluster, even if original uploader is down

The system uses **gossip protocol** for membership dissemination, **phi-accrual failure detection** for liveness, **vector clocks** for causality tracking, **quorum reads/writes** for consistency, **hinted handoff** for availability during partitions, **Merkle tree anti-entropy** for background repair, and an **EWMA-based peer selector** for intelligent parallel downloads.

### High-Level Layer Diagram

```
┌──────────────────────────────────────────────────────────────┐
│                    CLI (cmd package)                          │
│  dfs start / dfs upload / dfs download                       │
├──────────────────────────────────────────────────────────────┤
│              Unix Socket IPC (binary protocol)               │
├──────────────────────────────────────────────────────────────┤
│                    Server (orchestrator)                      │
│  MakeServer → NewServer → Run → loop                         │
├────────┬──────────┬──────────┬───────────┬───────────────────┤
│Chunker │Compress  │ Encrypt  │ Storage   │    Downloader     │
├────────┴──────────┴──────────┴───────────┴───────────────────┤
│              Cluster Coordination Layer                       │
│ HashRing│Gossip│Membership│Failure│Quorum│Handoff│Rebalance  │
│ VClock │Conflict│Merkle│Selector                             │
├──────────────────────────────────────────────────────────────┤
│           Transport Layer (TCP / QUIC via factory)            │
│  Peer2Peer│NAT Traversal│Handshake│mTLS                      │
├──────────────────────────────────────────────────────────────┤
│              Observability (Health/Metrics/Tracing/Logging)   │
└──────────────────────────────────────────────────────────────┘
```

---

## 2. Package: `main` — Entry Point

**File:** `main.go` (9 lines)

```go
func main() {
    cmd.Execute()
}
```

The entire program is a single call to `cmd.Execute()`. There is zero logic in `main` itself — everything is delegated to the Cobra command tree in the `cmd` package. This keeps the binary entry point clean and testable.

---

## 3. Package: `cmd` — CLI & IPC Layer

The `cmd` package implements the **command-line interface** and a **binary IPC protocol** over Unix domain sockets. It is the human-facing surface of the system.

### 3.1 `root.go` — Command Tree Root

```go
var rootCmd = &cobra.Command{
    Use:   "dfs",
    Short: "DFS is a distributed file system",
}
func Execute() {
    if err := rootCmd.Execute(); err != nil { os.Exit(1) }
}
```

- Defines the root `dfs` command using the Cobra library.
- `Execute()` is called from `main()` and triggers Cobra's command parsing.
- Subcommands (`start`, `upload`, `download`) attach themselves via `init()` functions.

### 3.2 `start.go` — Start Subcommand

```go
var startCmd = &cobra.Command{
    Use: "start",
    RunE: func(cmd *cobra.Command, args []string) error {
        return StartDaemon(port, peers, replicationFactor)
    },
}
```

**Flags:**
- `--port` (default `:3000`): The TCP/QUIC listen address for this node.
- `--peer`: Comma-separated list of bootstrap peer addresses.
- `--replication` (default `3`): Number of replicas per file.

When the user runs `dfs start --port :3000 --peer :3001,:3002`, it calls `StartDaemon()`.

### 3.3 `daemon.go` — `StartDaemon(port, peers, replicationFactor)`

This is the **heart of the startup sequence**:

1. **`logging.Init("daemon", LevelInfo)`** — Initialises structured JSON logging via zerolog.
2. **`tracing.Init("dfs-node", otlpEndpoint)`** — Sets up OpenTelemetry distributed tracing.
3. **`os.RemoveAll(sockPath)`** — Cleans up any stale Unix socket from a previous run.
4. **`net.Listen("unix", sockPath)`** — Creates a Unix domain socket at `/tmp/dfs-<port>.sock`.
5. **`serve.MakeServer(port, replicationFactor, peers...)`** — Constructs the entire server with all subsystems wired together (this is a huge function — see Section 25).
6. **Signal handler goroutine** — Catches `SIGTERM`/`SIGINT`, calls `server.GracefulShutdown()`, flushes tracing, exits.
7. **`go server.Run()`** — Starts the server event loop in a background goroutine.
8. **Accept loop** — Forever accepts connections on the Unix socket, dispatching each to `HandleClient()` in a goroutine.

**Why Unix sockets?** The CLI commands (`upload`/`download`) communicate with the running daemon via a local Unix socket. This avoids the overhead of TCP for localhost communication and provides a clean process boundary — the CLI is a thin client, the daemon does all the work.

### 3.4 `socket.go` — Socket Path Resolution

```go
func socketPath(port string) string {
    p := port
    if idx := strings.LastIndex(port, ":"); idx >= 0 { p = port[idx+1:] }
    return fmt.Sprintf("/tmp/dfs-%s.sock", p)
}
```

Extracts the port number from a listen address and constructs a deterministic socket path. For `:3000`, this produces `/tmp/dfs-3000.sock`. Multiple nodes on the same machine use different ports and thus different sockets.

### 3.5 `ipc.go` — Binary IPC Protocol

This file defines the **wire format** for CLI ↔ daemon communication. It is a compact binary protocol— no JSON, no protobuf — just raw bytes with length-prefixed fields.

#### Upload Request (CLI → Daemon)
```
[1B opcode=0x01] [2B key-len] [key bytes] [8B file-size] [raw file bytes]
```

#### Upload Response (Daemon → CLI)
```
[1B status: 0x00=OK | 0x01=ERROR] [4B msg-len] [msg bytes]
```

#### Download Request (CLI → Daemon)
```
[1B opcode=0x02] [2B key-len] [key bytes]
```

#### Download Response (Daemon → CLI)
```
[1B status] [8B content-len] [body bytes]
```

##### Key Functions:

- **`writeUploadRequest(conn, key, fileSize)`** — Writes opcode `0x01`, 2-byte key length, key bytes, 8-byte file size. Maximum key length: 65535 bytes.
- **`readUploadRequest(conn)`** — Inverse: reads the same fields and returns `(key, fileSize, error)`.
- **`writeStatus(conn, status, msg)`** — Writes a 1-byte status, 4-byte message length, and message body. Used for upload responses.
- **`readStatus(conn)`** — Reads status + message. Returns `(ok bool, msg string, error)`.
- **`writeDownloadRequest(conn, key)`** — Writes opcode `0x02` + key.
- **`readDownloadRequest(conn)`** — Reads key from download request.
- **`writeDownloadResponse(conn, contentLen)`** — Writes `statusOK` + 8-byte content length. Body follows separately.
- **`writeDownloadError(conn, errMsg)`** — Writes `statusError` + error message.
- **`readDownloadResponseHeader(conn)`** — Returns `(ok, contentLen, error)`.

### 3.6 `handle_client.go` — Daemon-Side IPC Dispatch

```go
func HandleClient(conn net.Conn, s *server.Server) {
    // Read 1-byte opcode
    switch opcode[0] {
    case opcodeUpload:   handleUpload(conn, s)
    case opcodeDownload: handleDownload(conn, s)
    }
}
```

- **`handleUpload(conn, s)`** — Reads the upload header (key + fileSize), wraps the connection as an `io.Reader`, calls `s.StoreData(key, body)`. On success, sends `statusOK` with a confirmation message. On error, sends `statusError`.

- **`handleDownload(conn, s)`** — Reads the download key, calls `s.GetData(key)` to get an `io.Reader`, writes the download response header with `ContentLength`, then `io.Copy`s the reader to the connection.

### 3.7 `upload.go` & `handle_upload.go` — Upload Flow

**`upload.go`** defines the Cobra subcommand:
```
dfs upload --key myfile --file /path/to/file --node :3000
```
It calls `UploadFile(key, filePath, socketPath)`.

**`UploadFile(key, filePath, sockPath)`** in `handle_upload.go`:
1. Opens the file, gets its size via `Stat()`.
2. Connects to the daemon's Unix socket.
3. Calls `writeUploadRequest(conn, key, size)` — sends the binary header.
4. `io.Copy(conn, f)` — streams the entire file to the daemon.
5. `uc.CloseWrite()` — signals end of data (half-close).
6. `readStatus(conn)` — reads success/failure response.

### 3.8 `download.go` & `handle_download.go` — Download Flow

**`download.go`** defines:
```
dfs download --key myfile --output /path/to/output --node :3000
```

**`DownloadFile(key, outputPath, sockPath)`** in `handle_download.go`:
1. Connects to Unix socket.
2. `writeDownloadRequest(conn, key)`.
3. `readDownloadResponseHeader(conn)` — gets status and content length.
4. If error: reads error message and returns it.
5. If OK: opens output file (or stdout), `io.Copy`s the body to disk.

---

## 4. Package: `Crypto` — Encryption & TLS

### 4.1 `crypto.go` — AES-256-GCM File Encryption

#### `EncryptionService` struct
```go
type EncryptionService struct {
    FileKey []byte  // 32-byte master key (SHA-256 of passphrase)
}
```

#### `NewEncryptionService(Key string) *EncryptionService`
- Takes a passphrase string.
- Computes `SHA-256(passphrase)` to derive a 32-byte AES-256 key.
- Stores it as `FileKey`.

#### `EncryptFile(src io.Reader, dst io.Writer) ([]byte, error)`
- Generates a random 32-byte **per-file key**.
- Creates AES-256-GCM cipher with that key.
- Generates a random 12-byte nonce.
- Reads all data from `src`, encrypts it with `AEAD.Seal()`.
- Writes `nonce + ciphertext` to `dst`.
- Returns the per-file key (which itself needs to be encrypted separately).

#### `DecryptFile(src io.Reader, dst io.Writer, key []byte) error`
- Uses the provided per-file key to create an AEAD cipher.
- Extracts the nonce (first 12 bytes).
- Decrypts the rest with `AEAD.Open()`.
- Writes plaintext to `dst`.

#### `EncryptKey(key []byte) (string, error)` / `DecryptKey(encKey string) ([]byte, error)`
- Encrypts/decrypts a per-file key using the master `FileKey`.
- Uses AES-GCM with a random nonce.
- Returns hex-encoded ciphertext.
- **Purpose:** The per-file key is stored in metadata. By encrypting it with the master key, even if someone reads the metadata file, they cannot decrypt the data without the master passphrase.

#### `EncryptStream(src io.Reader, dst io.Writer) ([]byte, error)`
- **Streaming chunk-based encryption** — handles files larger than memory.
- Generates a random 32-byte per-file key.
- Reads data in **4 MiB chunks**.
- For each chunk:
  - Derives a **deterministic nonce** from the chunk index (not random — allows parallel decryption).
  - Encrypts with AES-GCM.
  - Writes: `[4-byte chunk size][12-byte nonce][ciphertext + GCM tag]`.
- Returns the per-file encryption key.

#### `DecryptStream(src io.Reader, dst io.Writer, key []byte) error`
- Inverse of `EncryptStream`.
- Reads the 4-byte chunk size header, then nonce + ciphertext for each chunk.
- Decrypts each chunk and writes plaintext to `dst`.
- Stops when `io.EOF` is reached.

### 4.2 `tls.go` — mTLS Public Key Infrastructure

This file implements a complete **mutual TLS (mTLS) PKI** for securing peer-to-peer communication.

#### CA Management

- **`GenerateCA(certPath, keyPath string)`** — Creates an ECDSA P-256 Certificate Authority with a 10-year validity period. Generates a random serial number. Saves the CA certificate and private key to disk with restricted file permissions (0644 for cert, 0600 for key).

- **`LoadCA(certPath, keyPath string)`** — Loads an existing CA from PEM files.

- **`LoadOrGenerateCA(certPath, keyPath string)`** — Idempotent: loads if files exist, generates if they don't. This means the first node to start creates the CA, and you copy its CA files to other nodes.

#### Node Certificates

- **`GenerateNodeCert(caCert, caKey, certPath, keyPath, opts)`** — Creates a CA-signed node certificate. Uses ECDSA P-256, 1-year validity. Sets `ExtKeyUsage: [ServerAuth, ClientAuth]` — dual usage for mTLS where every node is both a server and a client.

- **`LoadOrGenerateNodeCert(...)`** — Idempotent: loads or generates. Also calls `verifyNodeCert()` to ensure existing certs haven't expired.

#### TLS Configuration Builders

- **`LoadServerTLSConfig(certPath, keyPath, caPath)`** — Creates `tls.Config` with `ClientAuth: RequireAndVerifyClientCert`. Peers MUST present a valid CA-signed certificate to connect.

- **`LoadClientTLSConfig(certPath, keyPath, caPath)`** — Creates `tls.Config` for outbound connections. Loads the CA certificate into `RootCAs` so the client trusts the server's cert.

- **`LoadMTLSConfig(certPath, keyPath, caPath)`** — Combined config: sets both `ClientCAs` and `RootCAs` from the same CA, sets `RequireAndVerifyClientCert`. Used by the transport layer where every node acts as both server and client. Minimum TLS 1.2, preferred curves: X25519, P-256.

---

## 5. Package: `Storage` — Local Disk Persistence

### 5.1 `storage.go` — Core Store

#### `Store` struct
```go
type Store struct {
    structOpts StructOpts
}
type StructOpts struct {
    PathTransformFunc PathTransform
    Metadata          MetadataStore
    Root              string  // e.g. ":3000_network"
}
```

The `Store` writes encrypted file bytes to disk using a **content-addressable** directory structure.

#### `CASPathTransformFunc(key string) PathKey`
- Takes a storage key (like `"chunk:a1b2c3..."`).
- Computes `SHA-256(key)` → 64 hex characters.
- Splits into 5-character blocks: `a1b2c/3d4e5/f6g7h/...`
- Creates a nested directory path.
- **Why?** Filesystems perform poorly with millions of files in one directory. This creates a balanced tree of directories, each containing at most ~1.05 million entries (16^5).

#### `WriteStream(key string, r io.Reader) (int64, error)`
1. Transforms key to path via `CASPathTransformFunc`.
2. Creates the directory tree with `os.MkdirAll`.
3. Creates a temporary file in that directory.
4. Streams data from `r` into the temp file while also computing an MD5 hash.
5. Renames the temp file to the final path (content-addressed by MD5).
6. Updates metadata via `Metadata.Set()`.
- The temp-file-then-rename pattern is **atomic** — a crash during write won't leave a corrupt file.

#### `ReadStream(key string) (int64, io.ReadCloser, error)`
- Looks up the file path from metadata.
- Opens the file and returns its size + handle.
- **Caller must close the ReadCloser.**

#### `Has(key string) bool`
- Checks if the key exists in metadata AND the file exists on disk.

#### `Remove(key string) error`
- Deletes the file at the metadata-recorded path.

#### `TearDown() error`
- Deletes the entire storage root directory. Used in tests.

### 5.2 `MetaFile` — JSON Metadata Store

```go
type MetaFile struct {
    path  string
    store map[string]FileMeta
    mu    sync.Mutex
}
```

`MetaFile` maintains a JSON file on disk (e.g., `:3000_metadata.json`) that maps storage keys to their metadata.

#### `FileMeta` struct
```go
type FileMeta struct {
    Path         string                     // absolute path to encrypted blob on disk
    EncryptedKey string                     // hex-encoded encrypted per-file AES key
    VClock       map[string]uint64          // vector clock (for conflict resolution)
    Timestamp    int64                      // UnixNano (for LWW tiebreaker)
    Chunked      bool                       // true if file was split into chunks
    Manifest     *chunker.ChunkManifest     // set if Chunked == true
}
```

#### `NewMetaFile(path string) *MetaFile`
- If the JSON file exists, loads it into memory.
- If not, creates an empty `{}` file.

#### `Get(key string) (FileMeta, bool)`
- Thread-safe lookup from in-memory map.

#### `Set(key string, meta FileMeta) error`
- Updates in-memory map, then **writes the entire map to disk** as indented JSON.
- Every write flushes to disk — slow but durable.

#### `Keys() []string`
- Returns all keys in the metadata store. Used by Rebalancer and AntiEntropy to enumerate locally-held files.

#### `GetManifest(fileKey string) (*ChunkManifest, bool)`
- Looks up the manifest using the key `"manifest:<fileKey>"`.
- Returns the `Manifest` field from the `FileMeta`.

#### `SetManifest(fileKey string, manifest *ChunkManifest) error`
- Stores the manifest under key `"manifest:<fileKey>"` with `Manifest` field set.

---

## 6. Package: `Storage/chunker` — File Chunking

### Constants & Types

```go
const DefaultChunkSize = 4 * 1024 * 1024  // 4 MiB
```

#### `Chunk` — In-memory chunk during upload
```go
type Chunk struct {
    Index int
    Hash  [32]byte   // SHA-256 of plaintext Data
    Size  int64
    Data  []byte
}
```

#### `ChunkInfo` — Durable record in manifest (no actual data)
```go
type ChunkInfo struct {
    Index      int
    Hash       string  // hex SHA-256 of plaintext
    Size       int64
    EncHash    string  // hex SHA-256 of *encrypted* bytes (CAS key)
    Compressed bool    // was this chunk compressed before encryption?
}
```

#### `ChunkManifest` — Table of contents for a chunked file
```go
type ChunkManifest struct {
    FileKey    string
    TotalSize  int64
    ChunkSize  int
    Chunks     []ChunkInfo
    MerkleRoot string   // integrity root hash
    CreatedAt  int64
}
```

### Core Functions

#### `ChunkReader(r io.Reader, chunkSize int) (<-chan Chunk, <-chan error)`
- Runs a **goroutine** that reads from `r` in fixed-size increments.
- Sends each chunk on the output channel (buffered with capacity 4 for pipelining).
- Computes SHA-256 of each chunk's plaintext data.
- Handles the last chunk being smaller than `chunkSize` (via `io.ErrUnexpectedEOF`).
- Sends any read errors on the error channel.
- **Designed for streaming:** the caller processes chunks as they arrive, never holding the entire file in memory.

#### `Reassemble(chunks []Chunk, dst io.Writer) error`
- Takes chunks in any order, sorts by Index.
- Verifies no gaps (index 0, 1, 2, ... must be contiguous).
- Writes each chunk's data sequentially to `dst`.
- Returns error if any index is missing.

#### `VerifyChunk(c Chunk) bool`
- Computes `SHA-256(c.Data)` and compares to `c.Hash`.
- Used after decryption to detect bit-rot or tampering.

#### `ChunkStorageKey(encHash string) string`
- Returns `"chunk:" + encHash`.
- The `"chunk:"` prefix prevents collision with user file keys.

#### `ManifestStorageKey(fileKey string) string`
- Returns `"manifest:" + fileKey`.

#### `BuildManifest(fileKey string, chunks []ChunkInfo, createdAt int64) *ChunkManifest`
- Sums `TotalSize` across all chunks.
- Computes a Merkle root hash over the plaintext chunk hashes (for whole-file integrity verification).
- Returns a complete `ChunkManifest`.

#### `computeMerkleRoot(chunks []ChunkInfo) string`
- Builds a binary Merkle tree bottom-up:
  - Leaf layer: each chunk's plaintext hash.
  - Internal nodes: `SHA-256(left.Hash || right.Hash)`.
  - Odd-count levels: last leaf is duplicated.
- Returns the hex-encoded root hash.

---

## 7. Package: `Storage/compression` — Zstd Compression

### `ShouldCompress(data []byte) bool`

A smart admission gate that decides whether compression is worthwhile:

1. **Size check:** Data smaller than 1 KiB is skipped (compression framing overhead would negate benefit).
2. **Magic-byte check** (`isAlreadyCompressed`): Recognises JPEG, PNG, ZIP/DOCX/XLSX, gzip, zstd, MP4/MOV, MKV/WebM by their first 2-4 bytes. These formats are already compressed.
3. **Shannon entropy** (`shannonEntropy`): Measures information density on the first 4 KiB. If entropy ≥ 7.0 bits/byte (out of max 8.0), data looks random/encrypted and won't compress. Only compress if entropy < 7.0.

### `CompressChunk(data []byte, level Level) (out []byte, wasCompressed bool, err error)`
- Creates a zstd encoder at the requested level (Fastest/Default/Best).
- Compresses the data.
- **If compressed output is larger than input**, returns the original unchanged with `wasCompressed=false`. Never make data bigger.

### `DecompressChunk(data []byte) ([]byte, error)`
- Creates a zstd decoder.
- Returns decompressed bytes.

### `shannonEntropy(data []byte) float64`
- Counts byte frequencies.
- Computes $H = -\sum_{i=0}^{255} p_i \log_2(p_i)$ where $p_i$ is the frequency of byte value $i$.
- Truly random data ≈ 8.0 bits/byte. ASCII text ≈ 4-5. Zeros ≈ 0.

---

## 8. Package: `Peer2Peer` — Network Transport

This package defines the **interfaces and TCP implementation** for peer-to-peer communication.

### 8.1 `transport.go` — Core Interfaces

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

- **`Peer`** wraps a network connection with higher-level send methods. `Outbound()` returns true if this node initiated the connection (dialled out), false if the remote side connected to us.
- **`Transport`** is the network listener that accepts connections and produces incoming messages.

### 8.2 `message.go` — RPC Message Envelope

```go
const (
    IncomingMessage           = 0x1
    IncomingStream            = 0x2
    IncomingMessageWithStream = 0x3
)

type RPC struct {
    From     net.Addr
    Peer     Peer
    Payload  []byte
    Stream   bool
    StreamWg *sync.WaitGroup
}
```

Three types of messages:
1. **`IncomingMessage` (0x1):** A regular message with a gob-encoded payload. Used for heartbeats, gossip digests, quorum reads, metadata requests, etc.
2. **`IncomingStream` (0x2):** Raw byte stream (rarely used directly).
3. **`IncomingMessageWithStream` (0x3):** A gob-encoded message header FOLLOWED by raw stream bytes. Used for file/chunk transfers where the header describes the key and size, and the body is the encrypted data.

**`StreamWg`** is a WaitGroup that the handler must call `Done()` on after consuming the stream. This synchronises the transport loop — it won't read the next message until the current stream is fully consumed.

### 8.3 `encoding.go` — Frame Decoders

#### `DefaultDecoder`
- Reads **length-prefixed frames**: `[4-byte big-endian length][payload bytes]`.
- Sanity limit: 64 MiB max frame size.
- This replaced an older fixed-4096-byte `Read()` that silently truncated large messages.

#### `GOBDecoder`
- Uses Go's `gob` encoding directly on the stream.
- Less commonly used; `DefaultDecoder` is the primary one.

### 8.4 `handshake.go` — Connection Authentication

#### `NOPEHandshakeFunc(Peer) error`
- No-op handshake. Always succeeds. For testing only.

#### `TLSVerifyHandshakeFunc(p Peer) error`
- Checks that the connection is actually TLS.
- Verifies the peer presented a valid certificate.
- Logs: Subject, Issuer, SHA-256 fingerprint, validity dates, TLS version, cipher suite.
- If the connection is plain TCP (no TLS), it logs a warning but succeeds.

#### `MakeFingerprintVerifyHandshake(allowedFingerprints map[string]bool) HandshakeFunc`
- Returns a handshake function that verifies the peer's certificate SHA-256 fingerprint against a whitelist.
- **Certificate pinning** — even if a rogue CA signs a fake cert, it will be rejected because the fingerprint won't match.

### 8.5 `tcpTransport.go` — TCP Implementation

#### `TCPPeer` struct
```go
type TCPPeer struct {
    net.Conn
    outbound bool
    Wg       *sync.WaitGroup
    sendMu   sync.Mutex        // serialises writes from concurrent goroutines
}
```

#### `NewTCPTransport(opts TCPTransportOpts) *TCPTransport`
- Creates a transport with a buffered RPC channel (capacity 1024).

#### `ListenAndAccept() error`
- If `TLSConfig` is non-nil: calls `tls.Listen()` for encrypted connections.
- Otherwise: `net.Listen()` for plain TCP.
- Starts `loopAndAccept()` in a goroutine.

#### `loopAndAccept()`
- Forever calls `Listener.Accept()`.
- For each connection: `go t.handleConn(conn, false)` — `false` = inbound.

#### `handleConn(conn, outbound)`
This is the **core connection handler**:
1. Wraps the connection as a `TCPPeer`.
2. Calls `HandshakeFunc(peer)` — verifies TLS certificates.
3. Calls `OnPeer(peer)` — notifies the server of a new peer.
4. Enters a read loop:
   - Reads 1 byte (control byte).
   - **`IncomingStream (0x2)`:** Creates a WaitGroup, sends an RPC with `Stream=true`, waits for `streamWg.Wait()`.
   - **`IncomingMessageWithStream (0x3)`:** Decodes the message payload first, then sets `Stream=true` and waits for consumption.
   - **Default (0x1 or other):** Decodes payload only (no stream), sends to `rpcch`.
5. On any read error (disconnect): calls `OnPeerDisconnect(peer)` and closes.

#### `Dial(addr string) error`
- Dials with TLS or plain TCP based on config.
- Calls `handleConn(conn, true)` — `true` = outbound.

#### `SendMsg(controlByte byte, payload []byte) error`
Thread-safe under `sendMu`:
- Writes 1-byte control code.
- Writes 4-byte big-endian length.
- Writes payload bytes.

#### `SendStream(msgPayload, streamData []byte) error`
Thread-safe:
- Writes `IncomingMessageWithStream` control byte.
- Writes length-prefixed `msgPayload`.
- Writes raw `streamData`.
- Atomic: all three writes happen under a single lock so they can't interleave with other messages.

---

## 9. Package: `Peer2Peer/quic` — QUIC Transport

A drop-in replacement for `TCPTransport` using the QUIC protocol (UDP-based, built on `quic-go`).

### Key Differences from TCP

| Feature | TCP | QUIC |
|---------|-----|------|
| Protocol | TCP | UDP |
| Multiplexing | Single stream per connection | Multiple independent streams per connection |
| TLS | Separate TLS handshake | TLS 1.3 built-in |
| Head-of-line blocking | Yes | No (per-stream) |

### `QUICPeer` struct
- Wraps a `*quic.Conn`.
- `Write()` opens a new QUIC stream for each write, writes data, closes the stream.
- `Send/SendMsg/SendStream` — same signature as `TCPPeer` but each opens a separate stream.

### `Transport` struct  
- `ListenAndAccept()`: Calls `quic.ListenAddr()` with `NextProtos: ["dfs-quic"]`. Configures 30s idle timeout and 10s keepalive.
- `Dial(addr)`: Calls `quic.DialAddr()` with `InsecureSkipVerify: true` (the handshake layer handles verification).
- `acceptStreams(conn, peer)`: For each QUIC stream, reads the control byte and dispatches similarly to TCP's `handleConn`.
- `selfSignedTLS()`: Generates a throwaway ECDSA P-256 self-signed certificate for QUIC's required TLS layer (when no mTLS config is provided).

---

## 10. Package: `Peer2Peer/nat` — NAT Traversal

Provides **STUN-based public address discovery** for nodes behind NAT (e.g., college campus LANs).

### `Config` struct
```go
STUNServers: ["stun.l.google.com:19302", ...]
DiscoverTimeout: 3s
```

### `Puncher` struct

#### `DiscoverPublicAddr() (string, error)`
- Tries each STUN server in order.
- Sends a STUN Binding Request, parses the response for `XOR-MAPPED-ADDRESS` or `MAPPED-ADDRESS`.
- Caches the result.
- **Why?** Behind NAT, a node doesn't know its public IP:port. STUN tells it.

#### `stunQuery(serverAddr string) (string, error)`
- Constructs a minimal 20-byte STUN Binding Request (type `0x0001`, random 12-byte transaction ID, magic cookie `0x2112A442`).
- Sends via UDP, reads response, parses attributes.

#### `parseXORMappedAddr(val, magic, txID)` / `parseMappedAddr(val)`
- XOR-MAPPED: XORs port with `0x2112`, XORs IPv4 with `0x2112A442`. Supports IPv6 (XOR with magic + txID).
- MAPPED: Direct extraction (older STUN servers).

#### `AnnotateGossip() map[string]string`
- Returns `{"public_addr": "203.0.113.42:54321"}` for including in gossip metadata.

---

## 11. Package: `factory` — Transport Factory

Abstracts the choice between TCP and QUIC transports.

```go
type Protocol string
const (
    ProtocolQUIC Protocol = "quic"
    ProtocolTCP  Protocol = "tcp"
    DefaultProtocol = ProtocolQUIC  // QUIC is the default
)
```

### `New(listenAddr string, protocol Protocol, opts Options) (Transport, error)`
- If `ProtocolQUIC`: creates a `quic.Transport` with `quic.New()`.
- If `ProtocolTCP`: creates a `TCPTransport` with `NewTCPTransport()`.
- Otherwise: returns error.

### `NewFromEnv(listenAddr string, opts Options) (Transport, error)`
- Reads `DFS_TRANSPORT` environment variable.
- Falls back to `DefaultProtocol` (QUIC) if unset.
- Calls `New()`.

### `ProtocolFromEnv() Protocol`
- Returns the effective protocol name. Used for logging.

---

## 12. Package: `Cluster/vclock` — Vector Clocks

Vector clocks track **causal relationships** between writes across distributed nodes. They are the mathematical foundation for conflict detection.

### `VectorClock` type
```go
type VectorClock map[string]uint64  // nodeAddr → logical timestamp
```

### `Relation` type
```go
const (
    Before     Relation = iota  // this happened first
    After                       // this happened after
    Concurrent                  // genuine conflict
    Equal                       // identical
)
```

### Functions

#### `Increment(nodeAddr string) VectorClock`
- Returns a **new** clock with `nodeAddr`'s counter incremented by 1.
- Original is never mutated (immutable semantics).

#### `Merge(other VectorClock) VectorClock`
- Returns a new clock where each entry is `max(vc[k], other[k])`.
- Used when a node receives a write from another node — it merges the sender's clock into its own.

#### `Compare(other VectorClock) Relation`
- Iterates all keys from both clocks.
- If `vc[k] > other[k]` for at least one k: sets `thisGreater`.
- If `other[k] > vc[k]` for at least one k: sets `otherGreater`.
- Result:
  - Both false → `Equal`
  - Only `thisGreater` → `After` (vc is causally later)
  - Only `otherGreater` → `Before` (vc is causally earlier)
  - Both → `Concurrent` (genuine conflict, neither dominates)

#### `Copy() VectorClock`
- Deep copy (new map with same entries).

#### `Encode() / Decode(data)` 
- Gob serialisation for wire transmission.

---

## 13. Package: `Cluster/conflict` — Conflict Resolution

### `Version` struct
```go
type Version struct {
    NodeAddr     string
    Clock        vclock.VectorClock
    Timestamp    int64      // UnixNano wall-clock
    EncryptedKey string     // per-file AES key
}
```

### `LWWResolver` — Last-Write-Wins Resolution

#### `Resolve(versions []Version) Version`

Decision rules in priority order:
1. **Causally later** always wins — if `A.Clock` is `Before` `B.Clock`, B wins regardless of timestamps.
2. **Concurrent + higher timestamp** wins — when clocks are `Concurrent` (genuine conflict), the one with the higher wall-clock timestamp wins.
3. **Exact tie** — first candidate in the input slice wins (deterministic, caller controls ordering).

This is the **same conflict resolution strategy used by Amazon Dynamo** and Apache Cassandra.

---

## 14. Package: `Cluster/hashring` — Consistent Hashing

Determines **which nodes are responsible** for which keys.

### `HashRing` struct
```go
type HashRing struct {
    sortedHashes []uint64            // sorted ring positions
    hashToNode   map[uint64]string   // ring position → physical node address
    nodeToHashes map[string][]uint64 // physical node → its virtual node positions
    virtualNodes int                  // default 150
    replFactor   int                  // default 3
}
```

### Functions

#### `New(cfg *Config) *HashRing`
- Creates a ring with 150 virtual nodes and replication factor 3 (or custom).

#### `AddNode(addr string)`
- For each of 150 virtual nodes: computes `SHA-256("<addr>:vnode:<i>")`, takes first 8 bytes as `uint64`.
- Inserts all 150 positions into the sorted ring.
- **Why 150 virtual nodes?** With only 1 position per physical node, the ring would be very uneven. 150 virtual nodes per physical node gives a statistically even distribution. Adding or removing a node moves ≈ 1/N of the key space.

#### `RemoveNode(addr string)`
- Removes all 150 virtual node positions from the ring.
- Rebuilds the sorted array (excludes removed positions).

#### `GetNodes(key string, n int) []string`
- Computes `SHA-256(key)` → `uint64` position.
- **Binary searches** for the first ring position ≥ key's position.
- Walks clockwise, collecting distinct physical nodes until `n` are found.
- Skips duplicate physical nodes (hit by multiple virtual nodes).
- If fewer than `n` physical nodes exist, returns all of them.

#### `GetPrimaryNode(key string) string`
- Returns `GetNodes(key, 1)[0]` — the single primary owner.

#### `search(hash uint64) int`
- Binary search on `sortedHashes`. Wraps around to index 0 if no position is ≥ hash (circular ring).

#### `hashKey(key string) uint64`
- `SHA-256(key)`, takes first 8 bytes as big-endian `uint64`.

---

## 15. Package: `Cluster/membership` — Cluster Membership

Tracks the **liveness state** of every node.

### `NodeState` enum
```go
StateAlive   = 0  // responding to heartbeats
StateSuspect = 1  // phi >= threshold
StateDead    = 2  // confirmed dead
StateLeft    = 3  // voluntarily departed
```

### `NodeInfo` struct
```go
type NodeInfo struct {
    Addr        string
    State       NodeState
    Generation  uint64            // monotonically increasing
    LastUpdated time.Time
    Metadata    map[string]string // e.g. "public_addr", "region"
}
```

### `ClusterState` struct
- In-memory map of `NodeInfo` entries keyed by address.
- Thread-safe (protected by `sync.RWMutex`).

### Functions

#### `New(selfAddr string) *ClusterState`
- Creates a new cluster state with self marked as `StateAlive`.
- **Uses `time.Now().UnixNano()` as the initial generation** — this ensures a restarted node always has a higher generation than any stale record on peers.

#### `UpdateState(addr, newState, generation) bool`
- Only applies if `generation > existing.Generation` — **stale updates are silently discarded**.
- Fires `onChange` callbacks on state transition.
- Returns `true` if applied, `false` if stale.

#### `Merge(digests []GossipDigest) []string`
- Compares incoming gossip digests against local state.
- Returns addresses where the sender has newer info (`generation > local`).
- For same-generation with higher state number, promotes locally (e.g., `Alive → Suspect`).

#### `Digest() []GossipDigest`
- Returns compact summaries of all nodes for gossip exchange.

#### `NextGeneration(addr string) uint64`
- Returns `generation + 1` for addr. Used when constructing state-change updates.

---

## 16. Package: `Cluster/failure` — Failure Detection

Implements the **phi-accrual failure detector** (Hayashibara et al., 2004) — the same algorithm used by Apache Cassandra.

### Key Concept: Phi (φ) Value

Instead of a binary alive/dead, the phi-accrual detector outputs a **continuous suspicion level**. Phi = 0 means definitely alive. Phi = 16 means almost certainly dead. The threshold (default 8.0) determines when a node is considered suspect.

### `peerWindow` struct
A **fixed-size sliding window** (ring buffer) that records intervals between heartbeats:
```go
type peerWindow struct {
    intervals   []float64  // ring buffer of inter-arrival times (ms)
    head        int
    count       int
    lastArrival time.Time
    capacity    int        // default 200
}
```

#### `record(now time.Time)`
- Computes the interval since the last heartbeat.
- Stores it in the ring buffer (overwrites oldest when full).

#### `phi(now time.Time) float64`
- Needs at least 2 samples to compute.
- Computes the mean and standard deviation of inter-arrival intervals.
- Calculates elapsed time since last heartbeat.
- Uses the **cumulative distribution function (CDF)** of a Gaussian distribution:
  
  $$\phi = -\log_{10}\left(1 - \Phi\left(\frac{\text{elapsed} - \mu}{\sigma \cdot \sqrt{2}}\right)\right)$$

  where $\Phi$ is the error function CDF.
- Clamps at 16.0 maximum.
- **Adaptive:** If a node normally sends heartbeats every 1 second and suddenly stops for 30 seconds, phi shoots up — even though a fixed timeout wouldn't adapt to varying network conditions.

### `PhiAccrualDetector`
- Maintains a `peerWindow` per peer address.
- **`RecordHeartbeat(addr, time)`** — records arrival time.
- **`Phi(addr) float64`** — returns current phi value.
- **`IsAlive(addr) bool`** — returns `phi < threshold`.
- **`Remove(addr)`** — cleans up when a peer is removed.

### `HeartbeatService`

#### State machine per peer:
```
Alive → Suspect → Dead
          ↑↓
        (heartbeat resets to Alive)
```

#### `Start()`
- Launches two goroutines:
  - **`senderLoop()`**: Every `HeartbeatInterval` (1s), sends heartbeats to all known peers.
  - **`reaperLoop()`**: Every 500ms, checks phi for all peers.

#### `reap()`
For each peer:
- If `phi >= threshold` AND state is `Alive` → transition to `Suspect`, call `onSuspect(addr)`.
- If state is `Suspect` AND `time.Since(suspectSince) >= DeadTimeout` (30s) → transition to `Dead`, call `onDead(addr)`.
- State transitions use `CompareAndSwap` for thread safety.

#### `RecordHeartbeat(from string)`
- Records the heartbeat in the detector.
- If peer was `Suspect` or `Dead`, atomically resets to `Alive`.

---

## 17. Package: `Cluster/gossip` — Gossip Protocol

Implements **push-pull epidemic dissemination** — each node periodically tells random peers what it knows, and they update each other. This converges the cluster to a consistent view in $O(\log N)$ rounds.

### Architecture

The gossip service uses **dependency injection** — it doesn't manage TCP connections directly. It calls closures provided by the caller:
- `getPeers()` — returns currently connected peers.
- `sendMsg(addr, msg)` — delivers a message.
- `onNewPeer(addr)` — ask the server to dial a newly discovered node.

### `GossipService` Core Functions

#### `Start()` / `Stop()`
- Start launches `gossipLoop()`, which ticks at `Interval` (200ms).

#### `doGossipRound()`
1. Gets all peers, filters out self.
2. **Shuffles** the list randomly.
3. Selects up to `FanOut` (3) random targets.
4. Builds a digest from only **connected** peers (prevents phantom addresses — nodes learned via third-party gossip but never directly reachable — from propagating).
5. Sends `MessageGossipDigest` to each target.

#### `HandleDigest(from, msg, replyFn)`

This is the most complex function in the gossip package:

1. **Records known nodes** before merge (for detecting new discoveries).
2. **Calls `cluster.Merge(digests)`** — returns addresses where the sender has newer info.
3. **New peer detection:** For each address in `needFull`:
   - If not connected AND not known before: **newly discovered** → add to dial set.
   - If not connected AND known but was Dead/Left AND now Alive: **rejoin** → add to dial set.
4. **Digest scan:** Also checks every digest entry for newly alive nodes.
5. **Dials** all queued addresses (deduplicated) via `onNewPeer`.
6. **Responds** with:
   - Full `NodeInfo` for addresses the sender wanted.
   - Own digest so the sender can update too (push-pull).
7. **Uses `replyFn`** to send the response directly on the peer's connection — bypasses the peers-map lookup that would fail after `handleAnnounce` remaps ephemeral addresses to canonical ones.

#### `HandleResponse(from, msg)`
1. Applies each `NodeInfo` update via `cluster.UpdateState()`.
2. Merges the sender's digest for any additional info.
3. Triggers `onNewPeer` for newly discovered alive nodes.

---

## 18. Package: `Cluster/quorum` — Quorum Reads/Writes

Implements **W-of-N write** and **R-of-N read** quorum for tuneable consistency.

### Configuration
```go
N=3  // total replicas
W=2  // write quorum (must get 2 of 3 acks)
R=2  // read quorum  (must get 2 of 3 responses)
Timeout = 5s
```

With W=2 and R=2, the system guarantees **at least one replica in a write quorum overlaps with a read quorum** (since W + R > N), ensuring reads see the latest write.

### `Coordinator`

#### `Write(key, encKey, data, clock) error`
1. Gets N target nodes from the hash ring.
2. For each target:
   - If **self**: calls `localWrite(key, encKey, data, clock)` directly.
   - If **remote**: sends `MessageQuorumWrite` and waits for ack.
3. Waits for W successful acks within `Timeout`.
4. Returns `nil` on quorum met; error on timeout.

#### `Read(key string) (encryptedKey, clock, error)`
1. Gets N target nodes.
2. Sends `MessageQuorumRead` to each (or calls `localRead` for self).
3. Waits for R `Found=true` responses.
4. Runs **conflict resolution** via `resolveRead()`:
   - Converts responses to `conflict.Version` structs.
   - Calls `LWWResolver.Resolve()` — causally-later wins, then LWW tiebreak.
   - Returns the winning version's encrypted key and clock.
5. On timeout with partial responses: resolves what it has (**degraded mode**).

#### `HandleWriteAck(ack)` / `HandleReadResponse(resp)`
- Routes incoming network messages to the correct waiting `Write()`/`Read()` call via `sync.Map` channels keyed by the file key.

---

## 19. Package: `Cluster/handoff` — Hinted Handoff

When a node tries to replicate data to a peer but the peer is unreachable, the data is saved as a **hint** for later delivery. This ensures writes don't fail even during temporary network partitions.

### `Hint` struct
```go
type Hint struct {
    Key          string     // storage key
    TargetAddr   string     // intended recipient
    EncryptedKey string     // hex-encoded per-file AES key
    Data         []byte     // encrypted file bytes
    CreatedAt    time.Time
    Attempts     int        // delivery attempts so far
}
```

### `Store` — Persistent Hint Storage

- Hints are stored as **JSON files on disk** (`<sanitised-addr>.hints.json`), surviving process restarts.
- **Per-target caps** (default 1000): When exceeded, oldest hint is evicted.
- **Expiry** (default 24 hours): Hints older than this are purged.

#### `AddHint(h Hint) error`
- Appends to in-memory list for `h.TargetAddr`.
- If over cap: evicts oldest (index 0).
- Persists to disk.

#### `GetHints(targetAddr) []Hint`
- Returns a copy of all pending hints for this target.

#### `DeleteHint(targetAddr, key)`
- Removes a specific hint (after successful delivery).

### `HandoffService`

#### `OnPeerReconnect(addr string)`
- Called by `server.OnPeer` when a previously-disconnected peer comes back.
- Checks `HasPending(addr)` first — no-op if nothing to deliver.
- Uses `activeDeliveries sync.Map` to prevent duplicate concurrent delivery loops for the same address.
- Launches `deliverPending(addr)` in a goroutine.

#### `deliverPending(addr string)`
- Iterates all pending hints for addr.
- Calls `deliver(hint)` for each.
- On success: deletes the hint.
- On failure: increments `Attempts`. If `Attempts >= 5`: discards the hint permanently.
- Falls back to `UpdateHint()` to persist the incremented attempt count.

#### `purgeLoop()`
- Runs hourly, calls `store.PurgeExpired()`.

---

## 20. Package: `Cluster/rebalance` — Data Rebalancing

Handles data migration when the cluster topology changes.

### Two Triggers

1. **`OnNodeJoined(newAddr)`** — A new node joined; some of our keys might now belong to it.
2. **`OnNodeLeft(deadAddr)`** — A node died; its keys need re-replication to maintain the replication factor.

### `migrateToNewNode(newAddr string)`
1. Guards against concurrent rebalancing with `inProgress` flag.
2. Gets all locally-held keys from metadata.
3. For each key:
   - Asks the hash ring: "Who is responsible for this key now?"
   - If `newAddr` is in the responsible set AND `self` is also responsible → migrate.
   - **Manifest keys** (`"manifest:*"`): sent via `sendManifest` (JSON metadata, no CAS file).
   - **Data keys**: sent via `sendFile` (encrypted bytes).
4. Logs migration count.

### `rereplicate(deadAddr string)`
1. Same concurrency guard.
2. For each local key:
   - Gets new responsible set (after dead node removed from ring).
   - If `len(responsible) < N`: under-replicated.
   - If self is responsible: reads encrypted data, sends to each other responsible node.
3. Restores replication factor.

### `localKeys() []string`
- Uses a type assertion to check if `MetadataStore` implements `Keys() []string`.
- Returns all locally tracked file keys.

---

## 21. Package: `Cluster/merkle` — Anti-Entropy Sync

Background reconciliation between replica pairs using Merkle trees. Ensures **eventual consistency** even without quorum coordination — catches any data that slipped through the cracks.

### Merkle Tree Construction

#### `Build(keys []string) *Tree`
1. **Sorts keys lexicographically** — ensures determinism.
2. Creates leaf nodes: `SHA-256(key)` for each key.
3. Builds internal nodes bottom-up:
   - Parent hash = `SHA-256(left.Hash || right.Hash)`.
   - Odd-count levels: last node is duplicated.
4. Returns the tree with root node.

#### `RootHash() [32]byte`
- Returns the root hash. Two trees with the same key set always have the same root hash.

#### `Diff(other *Tree) []string`
- If root hashes match: returns nil (trees are identical).
- Otherwise: recursively walks both trees simultaneously.
- At each node: if hashes match, prune (entire subtree is identical).
- At leaves: reports differing keys.
- Returns deduplicated list of differing keys.

### `AntiEntropyService`

#### `doSync()` (runs every 10 minutes)
1. Builds Merkle tree from local keys.
2. For each replica partner:
   - Sends `MessageMerkleSync{RootHash}`.
   - Waits up to 10 seconds for response.
3. If partner's root hash differs, partner responds with its full key list.

#### `HandleSync(from, msg)`
- Builds own Merkle tree.
- If root hash matches: does nothing (in sync).
- If different: sends back `MessageMerkleDiffResponse{AllKeys}`.

#### `HandleDiffResponse(from, msg)`
- Delivers peer's key list to the waiting `doSync()` goroutine.
- Also runs `repair()` directly for unsolicited responses.

#### `repair(peer, myKeys, theirKeys)`
1. Builds trees for both key sets.
2. Diffs them.
3. For each differing key:
   - **We have it, peer doesn't** → calls `onSendKey(peer, key)` (re-replicate).
   - **Peer has it, we don't** → calls `onNeedKey(peer, key)` (request it).
   - **Both have it but hashes differ** → logged for conflict layer to resolve.

---

## 22. Package: `Cluster/selector` — Peer Selection

An **EWMA (Exponentially Weighted Moving Average) latency tracker** that picks the fastest peer for chunk downloads.

### Scoring Formula

$$\text{score}(\text{peer}) = \text{EWMA}_\text{latency} \times (1 + 0.5 \times \text{activeDownloads})$$

- Lower score = better peer.
- EWMA smoothing factor α = 0.2 (each new sample contributes 20%).
- Initial latency: 50ms (high enough that untested peers get tried eventually).
- Busy penalty: each active download increases the score by 50%, spreading load.

### Functions

#### `RecordLatency(addr string, d time.Duration)`
- Updates the EWMA: `ewma = 0.2 * sample + 0.8 * ewma`.

#### `BeginDownload(addr)` / `EndDownload(addr)`
- Increments/decrements the active download counter for load balancing.

#### `BestPeer(candidates []string) (string, bool)`
- Returns the candidate with the lowest score.
- Used by the Downloader to pick the best peer for each chunk.

---

## 23. Package: `Server/downloader` — Parallel Downloads

Manages **parallel chunk fetching** with configurable concurrency, retries, and progress tracking.

### `Config`
```go
MaxParallel  = 4   // concurrent chunk downloads
ChunkTimeout = 30s // per-chunk deadline
MaxRetries   = 3   // retries per chunk
```

### `Manager` struct
Holds config, selector, and three function closures:
- `fetch(storageKey, peerAddr) → ([]byte, error)` — gets raw encrypted bytes from a peer.
- `decrypt(storageKey, encData) → ([]byte, error)` — decrypts using per-chunk key.
- `getPeers(storageKey) → []string` — ring-responsible nodes for a chunk.

### `Download(manifest *ChunkManifest, dst io.Writer, progress ProgressFunc) error`

1. Creates a **semaphore** (buffered channel) of size `MaxParallel`.
2. For each chunk in the manifest:
   - Acquires semaphore slot.
   - Launches `fetchChunk()` in a goroutine.
   - Stores result in a pre-allocated slice.
3. Waits for all goroutines to finish.
4. Checks for errors.
5. Calls `chunker.Reassemble(chunks, dst)`.

### `fetchChunk(info ChunkInfo) (Chunk, error)`

1. Gets candidate peers from `getPeers(storageKey)`.
2. **Retry loop** (up to MaxRetries):
   a. Calls `selector.BestPeer(candidates)` to pick fastest peer.
   b. `selector.BeginDownload(peer)`.
   c. Calls `fetch(storageKey, peer)` with a timeout.
   d. `selector.EndDownload(peer)`.
   e. Records latency via `selector.RecordLatency(peer, elapsed)`.
   f. Calls `decrypt(storageKey, encData)`.
   g. **Decompresses** if `info.Compressed`.
   h. **Integrity check**: `SHA-256(plaintext)` must match `info.Hash`.
   i. On success: returns the chunk.
   j. On failure: removes peer from candidates, retries with remaining peers.
3. If all retries fail: returns error.

---

## 24. Package: `Observability` — Health, Logging, Metrics, Tracing

### 24.1 `health/` — HTTP Health Server

#### Endpoints:
- **`GET /health`** — Returns JSON status: `{status, nodeAddr, peerCount, ringSize, uptime, startedAt}`. Returns HTTP 200 if OK, 503 if degraded.
- **`GET /metrics`** — Prometheus metrics endpoint.
- **`GET /debug/pprof/...`** — Go runtime profiling (goroutines, heap, CPU).

#### `New(addr, statusFn, registry)` / `Start()` / `Stop(ctx)`
- Creates an HTTP server on `port+1000` (e.g., `:3000` → `:4000`).
- `statusFn` is a closure that queries the server for current status.

### 24.2 `logging/` — Structured Logging

Based on **zerolog** (high-performance structured JSON logging).

- **`Init(component, level)`** — Sets up a global logger with component name and min level.
- **`New(component, level)`** — Creates a standalone logger.
- **`With(key, value)`** — Returns a child logger with additional context.
- **`Info/Warn/Error/Debug(msg, keyvals...)`** — Structured log methods.

### 24.3 `metrics/` — Prometheus Instrumentation

Comprehensive Prometheus metrics:

| Type | Name | Description |
|------|------|-------------|
| Counter | `StoreOpsTotal` | Total store operations |
| Counter | `GetOpsTotal` | Total get operations |
| Counter | `ReplicationTotal` | Replication events (by status: ok/hint) |
| Counter | `GossipRoundsTotal` | Gossip rounds completed |
| Counter | `HeartbeatsTotal` | Heartbeats sent/received |
| Histogram | `StoreDuration` | Store operation latency |
| Histogram | `GetDuration` | Get operation latency |
| Histogram | `GossipDuration` | Gossip round duration |
| Histogram | `QuorumDuration` | Quorum operation latency |
| Gauge | `PeerCount` | Current connected peers |
| Gauge | `RingSize` | Hash ring node count |
| Gauge | `HintsPending` | Pending hinted handoff entries |

### 24.4 `tracing/` — Distributed Tracing

Based on **OpenTelemetry** with OTLP/HTTP exporter.

- **`Init(serviceName, endpoint)`** — Sets up OTLP exporter if endpoint is provided; otherwise uses no-op.
- **`StartSpan(ctx, name)`** — Creates a new trace span.
- **`InjectToMap(ctx) / ExtractFromMap(carrier)`** — Propagates trace context across services via `map[string]string` carrier.
- **`AddEvent(span, name)` / `RecordError(span, err)`** — Annotates spans.

---

## 25. Package: `Server` — The Orchestrator

The `Server` package is the **central nervous system** — it ties every other package together. It handles all network message dispatch, storage operations, and cluster coordination.

### 25.1 Struct Overview

```go
type Server struct {
    peers          map[string]peer2peer.Peer  // connected peers
    dialingSet     map[string]struct{}        // prevents concurrent duplicate dials
    announceAdded  map[string]string          // canonical → ephemeral addr mapping

    serverOpts     ServerOpts
    Store          *storage.Store
    HashRing       *hashring.HashRing
    pendingFile    map[string]chan io.Reader   // async file fetch channels

    HeartbeatSvc   *failure.HeartbeatService
    HandoffSvc     *handoff.HandoffService
    Rebalancer     *rebalance.Rebalancer
    Cluster        *membership.ClusterState
    GossipSvc      *gossip.GossipService
    Quorum         *quorum.Coordinator
    AntiEntropy    *merkle.AntiEntropyService
    HealthSrv      *health.Server
    Downloader     *downloader.Manager
    Selector       *selector.Selector
}
```

### 25.2 Message Types

The server defines 20+ message types, all registered with Go's `gob` encoding in `init()`:

| Message | Direction | Purpose |
|---------|-----------|---------|
| `MessageStoreFile` | Sender→Replica | Store encrypted file bytes |
| `MessageGetFile` | Requester→Holder | Request file bytes |
| `MessageLocalFile` | Holder→Requester | Response with file bytes |
| `MessageAnnounce` | Outbound→Inbound | Share canonical listen address |
| `MessageHeartbeat` | Node→Peer | Liveness probe |
| `MessageHeartbeatAck` | Peer→Node | Liveness response |
| `MessageGossipDigest` | Node→Peer | Gossip first leg |
| `MessageGossipResponse` | Peer→Node | Gossip second leg |
| `MessageQuorumWrite` | Coord→Replica | Replicate with quorum |
| `MessageQuorumWriteAck` | Replica→Coord | Ack write |
| `MessageQuorumRead` | Coord→Replica | Read probe |
| `MessageQuorumReadResponse` | Replica→Coord | Read response |
| `MessageMerkleSync` | Node→Peer | Anti-entropy check |
| `MessageMerkleDiffResponse` | Peer→Node | Differing keys |
| `MessageLeaving` | Departing→All | Voluntary departure |
| `MessageStoreManifest` | Sender→Replica | Replicate manifest |
| `MessageGetManifest` | Requester→Holder | Request manifest |
| `MessageManifestResponse` | Holder→Requester | Return manifest |

### 25.3 `MakeServer()` — The Big Wiring Function

This ~450-line function constructs the entire server and wires all subsystems together. Here's what it does in order:

1. **Logging & Metrics** — `logging.Init()`, `metrics.Init()`.
2. **Environment** — Loads `.env` file, reads `DFS_ENCRYPTION_KEY`.
3. **TLS/mTLS** — If `DFS_ENABLE_TLS=true`: loads/generates CA, node cert, builds mTLS config.
4. **Transport** — `factory.NewFromEnv()` creates TCP or QUIC transport with `OnPeer`/`OnPeerDisconnect` closures forwarded to the server.
5. **Server Construction** — `NewServer(opts)` creates the base server with hash ring.
6. **HeartbeatService** — Wired with closures:
   - `getPeers`: returns outbound peer addresses.
   - `sendHeartbeat`: sends `MessageHeartbeat` via `sendToAddr`.
   - `onSuspect`: logs warning.
   - `onDead`: removes from ring, triggers `Rebalancer.OnNodeLeft()`.
7. **HandoffService** — Wired with:
   - `deliver`: sends `MessageStoreFile` + encrypted data via `SendStream`.
8. **Rebalancer** — Wired with:
   - `readFile` → `readEncryptedFile()`.
   - `sendFile` → `SendStream` to peer.
   - `readManifest` → JSON-serialise from metadata.
   - `sendManifest` → `MessageStoreManifest`.
9. **Quorum Coordinator** — Wired with:
   - `getTargets` → `HashRing.GetNodes()`.
   - `sendMsg` → translates quorum messages to server wire protocol.
   - `localWrite` → `Store.WriteStream()` + metadata update.
   - `localRead` → metadata lookup.
10. **AntiEntropyService** — Wired with:
    - `getKeys` → metadata `Keys()`.
    - `getPeers` → `HashRing.Members()`.
    - `sendMsg` → translates Merkle messages to wire protocol.
    - `onNeedKey` → sends `MessageGetFile`.
    - `onSendKey` → reads locally + stores hint.
11. **Selector + Downloader** — Wired with:
    - `fetch` → `fetchChunkFromPeer()`.
    - `decrypt` → metadata lookup + `DecryptStream()`.
    - `getPeers` → `HashRing.GetNodes()`.
12. **Health Server** — On `port + 1000`.
13. **ClusterState** — `membership.New(listenAddr)`.
14. **GossipService** — Wired with:
    - `sendMsg` → translates gossip messages.
    - `onNewPeer` → dials discovered nodes (with self-loop protection and dedup via `dialingSet`).

### 25.4 `Run()` — Server Startup

1. **`transport.ListenAndAccept()`** — Starts listening for peer connections.
2. **`HashRing.AddNode(selfAddr)`** — Adds self to the ring.
3. Starts `HeartbeatSvc`, `HandoffSvc`, `GossipSvc`, `AntiEntropy` background services.
4. **`BootstrapNetwork()`** — Dials all bootstrap peers concurrently.
5. **`loop()`** — Enters the main event loop.

### 25.5 `loop()` — Main Event Loop

```go
for {
    select {
    case RPC := <-transport.Consume():
        // Decode gob message, dispatch to handleMessage
    case <-quitch:
        return
    }
}
```

- Reads RPCs from the transport's channel.
- Decodes the gob-encoded `Message` wrapper.
- Dispatches to `handleMessage()`.

### 25.6 `handleMessage()` — Message Dispatcher

A giant type switch that routes messages to the correct handler:

- `*MessageStoreFile` → `handleStoreMessage()` — writes stream to disk + updates metadata.
- `*MessageGetFile` → `handleGetMessage()` — reads from disk and sends back.
- `*MessageLocalFile` → `handleLocalMessage()` — writes, reads back, delivers to pending channel.
- `*MessageHeartbeat` → `handleHeartbeat()` — records + sends ack.
- `*MessageAnnounce` → `handleAnnounce()` — remaps ephemeral to canonical address.
- `*MessageGossipDigest` → `GossipSvc.HandleDigest()` with type conversion.
- `*MessageGossipResponse` → `GossipSvc.HandleResponse()`.
- `*MessageQuorumWrite` → `handleQuorumWrite()`.
- `*MessageQuorumWriteAck` → `Quorum.HandleWriteAck()`.
- `*MessageQuorumRead` → `handleQuorumRead()`.
- `*MessageQuorumReadResponse` → `Quorum.HandleReadResponse()`.
- `*MessageMerkleSync` → `AntiEntropy.HandleSync()`.
- `*MessageMerkleDiffResponse` → `AntiEntropy.HandleDiffResponse()`.
- `*MessageLeaving` → `handleLeaving()`.
- `*MessageStoreManifest` → unmarshals and stores manifest in metadata.
- `*MessageGetManifest` → replies with manifest JSON.
- `*MessageManifestResponse` → delivers to pending fetch channel.

### 25.7 `StoreData()` — The Upload Pipeline

This is the function called when a user runs `dfs upload`. Here's the complete pipeline:

```
User file → ChunkReader → [for each chunk]:
    → ShouldCompress? → CompressChunk
    → EncryptStream (AES-256-GCM)
    → SHA-256(encrypted) → storageKey
    → Store.WriteStream (local disk)
    → replicateChunk (to ring-responsible peers)
    → record ChunkInfo
→ BuildManifest (Merkle root)
→ metaData.SetManifest
→ replicateManifest (to peers)
→ Update top-level FileMeta (VClock, Chunked=true)
```

#### Detailed Steps:

1. **Chunking**: `chunker.ChunkReader(w, 4MiB)` — splits file into 4 MiB chunks in a goroutine.
2. **For each chunk**:
   a. **Compression check**: `compression.ShouldCompress(chunk.Data)` — checks entropy/magic bytes.
   b. **Compress**: If compressible, `CompressChunk(data, LevelFastest)`.
   c. **Encrypt**: `EncryptStream(compressed_data, tempFile)` — returns per-chunk AES key.
   d. **CAS key**: `SHA-256(encrypted_bytes)` → `"chunk:" + hex_hash`.
   e. **Local store**: `Store.WriteStream(storageKey, encrypted_bytes)` — if not already present (dedup).
   f. **Metadata**: Sets `EncryptedKey` and `Timestamp` in FileMeta.
   g. **Replicate**: `replicateChunk(...)` — sends to ring-responsible peers via `SendStream`. Unreachable peers get hints.
   h. **Record**: Appends `ChunkInfo{Hash, EncHash, Compressed, Size}`.
3. **Manifest**: `BuildManifest(key, chunkInfos, now)` — computes Merkle root.
4. **Store manifest**: `metaData.SetManifest(key, manifest)`.
5. **Replicate manifest**: Sends `MessageStoreManifest` to peers.
6. **Update top-level meta**: Increments vector clock for self, sets `Chunked=true`.

### 25.8 `GetData()` — The Download Pipeline

```
Request key →
  Check local metadata:
    If Chunked → getChunked(key)
    If legacy blob → ReadStream + DecryptStream
    If not local → fetchManifestFromPeers → getChunked(key)
```

### 25.9 `getChunked()` — Chunked File Reassembly

**Parallel path (with Downloader):**
```
manifest → Downloader.Download(manifest, &out) →
  [parallel for each chunk]:
    → selector.BestPeer(candidates)
    → fetchChunkFromPeer(storageKey, bestPeer)
    → decrypt → decompress → verify SHA-256
  → Reassemble → readRepair (async)
```

**Serial fallback:**
For each chunk in manifest:
1. Check if locally stored → `ReadStream`.
2. If not local → `fetchChunkFromPeers` (ask ring-responsible peers).
3. Decrypt with per-chunk key.
4. Decompress if `info.Compressed`.
5. Verify `SHA-256(plaintext) == info.Hash`.
6. Reassemble all chunks in order.
7. Trigger `readRepair()` in background.

### 25.10 `OnPeer()` — New Peer Handler

Called by the transport when a new connection is established:

1. **Inbound peers**: Stored in peers map but NOT added to hash ring (their address is ephemeral).
2. **Outbound peers**: 
   - Duplicate check (prevents two connections to the same address).
   - Added to hash ring.
   - Sends `MessageAnnounce{ListenAddr: self}` — tells the remote its canonical address.
   - Triggers `HandoffSvc.OnPeerReconnect()` (deliver pending hints).
   - Triggers `Rebalancer.OnNodeJoined()` (migrate data).
   - Updates `ClusterState` to `StateAlive`.

### 25.11 `handleAnnounce()` — Address Remapping

Solves a critical distributed systems problem: when Node A connects to Node B, Node B sees A's connection from an **ephemeral OS port** (e.g., `172.17.0.2:47636`), not A's listen port (`172.17.0.2:3000`).

1. Resolves bare ports (`:3000`) using the remote IP from the TCP connection.
2. **Self-connection detection**: If the canonical address matches a local interface + our port → reject (gossip-triggered loopback).
3. Remaps the peer in the peers map: `delete(peers[ephemeral])`, `peers[canonical] = p`.
4. Adds canonical to hash ring.
5. Triggers handoff delivery and rebalancing.

### 25.12 `handleLeaving()` — Voluntary Departure

1. Removes the departing node from the hash ring.
2. Updates cluster state to `StateLeft` with `generation = msgGen + 1` (ensures it overrides any stale gossip).
3. Closes the peer connection.
4. Cleans up `announceAdded` mapping.

### 25.13 `GracefulShutdown()`

1. **Stops gossip first** — prevents sending "I am Alive" after announcing departure.
2. Stops heartbeat service.
3. Broadcasts `MessageLeaving{From, Generation}` to all peers.
4. Pauses 200ms for propagation.
5. Stops health server, handoff service, anti-entropy.
6. Pauses 300ms for in-flight replication.
7. Calls `Stop()` to close the event loop.

### 25.14 `replicateChunk()` — Chunk Replication

1. Encodes `MessageStoreFile` header (key, size, encrypted key).
2. For each target node (excluding self):
   - If peer is connected: `SendStream(header, encryptedData)` atomically.
   - If peer is NOT connected: stores a hint via `HandoffSvc.StoreHint()`.
3. All sends run concurrently via goroutines with WaitGroup.

### 25.15 `readRepair()` — Background Repair

After a successful read, asynchronously checks all responsible replicas:

1. For each replica peer:
   - Sends a `MessageGetFile` probe.
   - Waits 2 seconds for response.
   - If no response (peer is missing the file): reads local encrypted bytes and stores a hint for later delivery.

### 25.16 `Broadcast()` — Send to All Peers

Gob-encodes a message and sends it to every connected peer. Used for `MessageLeaving`.

---

## 26. End-to-End Data Flows

### 26.1 Upload Flow (Complete)

```
User: dfs upload --key photo.jpg --file ~/photo.jpg --node :3000

1. CLI (handle_upload.go):
   - Opens file, gets size
   - Connects to /tmp/dfs-3000.sock
   - Sends: [0x01][key_len][key][file_size][file_bytes]

2. Daemon (handle_client.go):
   - Reads opcode 0x01
   - Reads key + size
   - Calls server.StoreData(key, conn)

3. Server.StoreData (server.go):
   - ChunkReader splits file into 4 MiB chunks
   - For each chunk:
     a. ShouldCompress? → CompressChunk (if beneficial)
     b. EncryptStream → AES-256-GCM with random per-chunk key
     c. SHA-256(encrypted) → CAS storage key
     d. WriteStream to local disk under CAS directory tree
     e. Save EncryptedKey in metadata
     f. replicateChunk → SendStream to ring-responsible peers
        - Peers receive MessageStoreFile + stream data
        - Peers call WriteStream on their local store
        - Unreachable peers get hints stored to disk
   - BuildManifest with Merkle root
   - Store manifest in metadata
   - replicateManifest → MessageStoreManifest to peers
   - Update FileMeta: VClock[self]++, Chunked=true

4. Daemon response:
   - writeStatus(conn, OK, "stored N bytes under key K")

5. CLI output:
   - "Uploaded: stored N bytes under key K"
```

### 26.2 Download Flow (Complete)

```
User: dfs download --key photo.jpg --output ~/restored.jpg --node :3001

1. CLI (handle_download.go):
   - Connects to /tmp/dfs-3001.sock
   - Sends: [0x02][key_len][key]

2. Daemon:
   - Calls server.GetData(key)

3. Server.GetData:
   - Check metadata: is it Chunked?
   
   Case A: Chunked + manifest available locally:
     → getChunked(key)
   
   Case B: Not local at all:
     → fetchManifestFromPeers(key)
       → Asks ring-responsible peers for manifest
       → Receives MessageManifestResponse with JSON
     → Cache manifest locally
     → getChunked(key)

4. getChunked (with Downloader):
   - For each chunk (parallel, max 4 concurrent):
     a. selector.BestPeer picks fastest responsive peer
     b. fetchChunkFromPeer sends MessageGetFile
     c. Peer reads chunk from disk, sends MessageLocalFile + stream
     d. Receives encrypted bytes
     e. Decrypt with per-chunk key from metadata
     f. Decompress if Compressed=true
     g. Verify SHA-256(plaintext) == manifest.Hash
     h. Record latency in Selector
   - Reassemble chunks in index order
   - Trigger readRepair in background

5. Daemon response:
   - Writes [0x00][content_len] + streams reassembled plaintext

6. CLI:
   - Writes to ~/restored.jpg
   - "Downloaded N bytes → ~/restored.jpg"
```

### 26.3 Node Join Flow

```
New Node C starts: dfs start --port :3002 --peer :3000

1. MakeServer builds all subsystems.
2. Run() adds self to ring, starts services.
3. BootstrapNetwork() dials :3000.
4. OnPeer(outbound=true) fires:
   - Adds :3000 to ring
   - Sends MessageAnnounce{":3002"} to :3000
   - Triggers rebalance: "any keys I should own?"
   
5. On Node A (:3000):
   - handleAnnounce: remaps ephemeral → :3002
   - Adds :3002 to ring
   - Triggers rebalance: migrates relevant keys to C
   
6. Gossip discovers C:
   - A's gossip digest includes C
   - B sees C in digest, dials C
   - Full mesh forms within O(log N) gossip rounds
```

### 26.4 Node Failure Flow

```
Node B (:3001) crashes without warning.

1. HeartbeatService on A and C:
   - phi for B starts climbing (no heartbeats arriving)
   - phi exceeds 8.0 → transition to Suspect
   - After 30s in Suspect → transition to Dead
   - onDead callback:
     - RemoveNode(":3001") from ring
     - Rebalancer.OnNodeLeft(":3001")
   
2. Rebalancer on A and C:
   - Scans local keys
   - Any key that was on B is now under-replicated
   - Re-replicates to remaining responsible nodes

3. Gossip propagates B's death:
   - ClusterState marks B as StateDead
   - All nodes eventually converge on B's status

4. Meanwhile, hints stored for B:
   - Remain on disk up to 24 hours
   - If B comes back, delivered on reconnect
```

### 26.5 Node Graceful Departure

```
Node B runs: Ctrl+C (SIGINT)

1. Signal handler catches SIGINT
2. GracefulShutdown():
   - Stop gossip (stop sending "I'm alive")
   - Stop heartbeats
   - Broadcast MessageLeaving{":3001", generation=X}
   - Wait 200ms
   
3. On A and C:
   - handleLeaving(":3001", X)
   - Immediately remove from ring (no 30s wait)
   - ClusterState → StateLeft at gen=X+1
   - Close peer connection
   
4. No re-replication triggered immediately
   (data is still on B's disk — it might come back)
```

---

## 27. How All Packages Orchestrate Together

### The Dependency Graph

```
main → cmd → Server
              ├── Crypto (EncryptionService + mTLS)
              ├── Storage
              │   ├── chunker
              │   └── compression
              ├── Peer2Peer
              │   ├── quic
              │   └── nat
              ├── factory (creates Peer2Peer transport)
              ├── Cluster
              │   ├── hashring (key→node mapping)
              │   ├── membership (node liveness state)
              │   ├── vclock (causal ordering)
              │   ├── conflict (LWW resolution)
              │   ├── failure (phi-accrual detection)
              │   ├── gossip (membership dissemination)
              │   ├── quorum (consistency coordination)
              │   ├── handoff (availability during partitions)
              │   ├── rebalance (topology change migration)
              │   ├── merkle (background anti-entropy)
              │   └── selector (latency-based routing)
              ├── Server/downloader (parallel chunk fetch)
              └── Observability
                  ├── health (HTTP /health)
                  ├── logging (zerolog)
                  ├── metrics (Prometheus)
                  └── tracing (OpenTelemetry)
```

### How They Work Together — The Big Picture

1. **Data enters** through the `cmd` package (CLI) via Unix socket IPC.

2. **The Server** is the orchestrator — it doesn't implement cluster algorithms itself, but **wires closures** from each Cluster subpackage to bridge them together. This is a dependency-injection architecture: each package is independent and testable in isolation.

3. **The hash ring** is the single source of truth for "who owns what." Every data operation (store, get, replicate, rebalance) asks the ring.

4. **The transport layer** (TCP or QUIC) handles bytes on the wire. The server never touches raw sockets — it sends/receives `Message` structs through the transport abstraction.

5. **Encryption is mandatory** — every byte stored on disk and every byte sent over the wire is AES-256-GCM encrypted with a per-file key. The per-file key is itself encrypted with the master key. Even if disks are stolen, data is unreadable without the passphrase.

6. **Consistency is tuneable** via quorum parameters (W, R, N). The default W=2, R=2, N=3 provides strong consistency with tolerance for one node failure. Reducing W or R trades consistency for availability.

7. **Availability is ensured** through hinted handoff — writes never fail due to temporary node unavailability. Data is buffered and delivered when the node returns.

8. **Eventual consistency is guaranteed** through three mechanisms:
   - **Gossip** propagates membership changes in O(log N) rounds.
   - **Anti-entropy** (Merkle trees) catches any inconsistencies every 10 minutes.
   - **Read repair** fixes missing replicas discovered during reads.

9. **Performance is optimised** through:
   - 4 MiB chunk size (balances overhead vs memory).
   - Zstd compression (only when beneficial).
   - Parallel chunk downloads (semaphore-limited concurrency).
   - EWMA peer selection (routes to fastest peer, load-balanced).
   - QUIC multiplexing (no head-of-line blocking).

10. **Observability** provides production-ready monitoring:
    - Health checks for load balancers.
    - Prometheus metrics for dashboards.
    - OpenTelemetry tracing for request flow analysis.
    - Structured JSON logging for log aggregation.

### The Closure Wiring Pattern

A key architectural insight: the Server uses **closures as dependency injection**. Each cluster subsystem (gossip, heartbeat, handoff, rebalancer, quorum, anti-entropy, downloader) is constructed with function closures that bridge it to the server's internal state. This means:

- Each package has **zero imports** to the Server package (no circular dependencies).
- Each package can be **unit tested** in complete isolation with mock closures.
- The Server's `MakeServer()` function is the **composition root** — the single place where all dependencies are wired.

Example: The Gossip service doesn't know how to send a TCP message. It calls `sendMsg(addr, msg)`. The closure provided by MakeServer translates the gossip message type to the server's wire protocol type and calls `sendToAddr()`. This is clean separation of concerns.

---

*This document covers every public function in every package. The system implements the core ideas from the Amazon Dynamo paper (consistent hashing, vector clocks, quorum, hinted handoff, anti-entropy, gossip dissemination) in a modern Go codebase with production-grade encryption, compression, and observability.*
