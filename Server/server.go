package Server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/joho/godotenv"

	"github.com/Faizan2005/DFS-Go/Cluster/failure"
	"github.com/Faizan2005/DFS-Go/Cluster/gossip"
	"github.com/Faizan2005/DFS-Go/Cluster/handoff"
	"github.com/Faizan2005/DFS-Go/Cluster/hashring"
	"github.com/Faizan2005/DFS-Go/Cluster/membership"
	"github.com/Faizan2005/DFS-Go/Cluster/merkle"
	"github.com/Faizan2005/DFS-Go/Cluster/quorum"
	"github.com/Faizan2005/DFS-Go/Cluster/rebalance"
	"github.com/Faizan2005/DFS-Go/Cluster/selector"
	"github.com/Faizan2005/DFS-Go/Cluster/vclock"
	crypto "github.com/Faizan2005/DFS-Go/Crypto"
	"github.com/Faizan2005/DFS-Go/Crypto/envelope"
	"github.com/Faizan2005/DFS-Go/Observability/health"
	"github.com/Faizan2005/DFS-Go/Observability/logging"
	"github.com/Faizan2005/DFS-Go/Observability/metrics"
	peer2peer "github.com/Faizan2005/DFS-Go/Peer2Peer"
	peermdns "github.com/Faizan2005/DFS-Go/Peer2Peer/mdns"
	"github.com/Faizan2005/DFS-Go/Peer2Peer/nat"
	quicTransport "github.com/Faizan2005/DFS-Go/Peer2Peer/quic"
	"github.com/Faizan2005/DFS-Go/Server/downloader"
	"github.com/Faizan2005/DFS-Go/Server/transfer"
	"github.com/Faizan2005/DFS-Go/State"
	storage "github.com/Faizan2005/DFS-Go/Storage"
	"github.com/Faizan2005/DFS-Go/Storage/chunker"
	"github.com/Faizan2005/DFS-Go/Storage/compression"
	"github.com/Faizan2005/DFS-Go/Storage/dirmanifest"
	"github.com/Faizan2005/DFS-Go/Storage/pending"
	"github.com/Faizan2005/DFS-Go/Storage/resume"
	"github.com/Faizan2005/DFS-Go/Storage/swarm"
	"github.com/Faizan2005/DFS-Go/factory"
	"github.com/prometheus/client_golang/prometheus"
)

// Package-level sync.Pools for the StoreData upload pipeline.
// Reusing these large slabs across chunks eliminates GC stop-the-world pauses
// (~100-200 ms each) that dominated small-file upload latency.
var (
	// chunkReadBufPool pools the 4 MiB read buffer used by ChunkReaderWithPool.
	chunkReadBufPool = sync.Pool{
		New: func() any { b := make([]byte, chunker.DefaultChunkSize); return &b },
	}
	// chunkDataPool pools the 4 MiB output slices that travel through the
	// entire upload pipeline (chunker → Stage 1 → Stage 2 → replication).
	// Returned to the pool only after replication completes, creating a
	// zero-allocation pipeline bounded by maxInFlight + channel capacity.
	chunkDataPool = sync.Pool{
		New: func() any { b := make([]byte, chunker.DefaultChunkSize); return &b },
	}
	// compBufPool pools the compression output slab used by CompressChunkWithPool.
	compBufPool = sync.Pool{
		New: func() any { b := make([]byte, 0, chunker.DefaultChunkSize); return &b },
	}
	// encBufPool pools the GCM ciphertext slab used by EncryptStreamWithDEKPool.
	// Capacity = ChunkSize + GCM overhead (16) + nonce (12) + 4-byte size prefix.
	encBufPool = sync.Pool{
		New: func() any { b := make([]byte, 0, crypto.ChunkSize+64); return &b },
	}
)

type ServerOpts struct {
	storageRoot       string
	pathTransform     storage.PathTransform
	transport         peer2peer.Transport
	metaData          storage.MetadataStore
	bootstrapNodes    []string
	ReplicationFactor int
}

type Server struct {
	peerLock sync.RWMutex
	peers    map[string]peer2peer.Peer

	// dialingLock protects dialingSet — prevents concurrent duplicate dials to the same addr.
	dialingLock sync.Mutex
	dialingSet  map[string]struct{}

	// announceAdded maps canonical listen addr → ephemeral remote addr for inbound
	// peers that were added to the ring via handleAnnounce. Used by OnPeerDisconnect
	// to remove them from the ring when the connection drops (inbound peers are
	// otherwise excluded from ring cleanup).
	announceAdded map[string]string // canonical → ephemeral

	serverOpts   ServerOpts
	Store        *storage.Store
	HashRing     *hashring.HashRing
	quitch       chan struct{}
	pendingFile  map[string]chan []byte
	pendingOffer map[string]chan []string // key: peerAddr → missing hashes reply
	casWriteCh   chan casWriteRequest     // async CAS cache writes (non-blocking from hot path)
	mu           sync.Mutex
	shutdownOnce sync.Once

	// Sprint 2: failure detection, hinted handoff, rebalancing
	HeartbeatSvc *failure.HeartbeatService
	HandoffSvc   *handoff.HandoffService
	Rebalancer   *rebalance.Rebalancer

	// Sprint 3: cluster membership + gossip
	Cluster   *membership.ClusterState
	GossipSvc *gossip.GossipService

	// Sprint 4: quorum writes/reads + anti-entropy
	Quorum      *quorum.Coordinator
	AntiEntropy *merkle.AntiEntropyService

	// Sprint 5: observability
	HealthSrv *health.Server
	startedAt time.Time

	// Sprint 7: parallel downloads + compression
	Downloader *downloader.Manager
	Selector   *selector.Selector

	// Phase 2: identity metadata for this node (alias, fingerprint, keys).
	identityMeta map[string]string

	// Phase 3: NAT traversal via STUN.
	NATService *nat.Puncher

	// externalAddr is the externally-visible address learned via MessageAnnounceAck.
	// Before the first ack, this is empty and canonAddr (127.0.0.1:port) is used.
	// After the ack, the node re-registers under this routable address in the
	// ring, cluster, and gossip — critical for Docker/NAT/cloud.
	externalAddr   string
	externalAddrMu sync.Mutex

	// Sprint F: persistent local state (upload/download history, public files).
	StateDB *State.StateDB

	// Sprint G: active transfer queue (pause/resume/cancel).
	TransferMgr *transfer.Manager

	// Sprint G: search request dedup cache (flood loop prevention).
	searchSeen *SearchRequestCache

	// Swarm: DEK cache for on-demand ECDH encryption (fileKey → unwrapped DEK).
	dekCache sync.Map
	// Swarm: X25519 private key for DEK unwrapping from manifest AccessList.
	x25519Priv []byte
	// Swarm: CAS cache manager for LRU eviction of secondary seeder chunks.
	CacheMgr *swarm.CacheManager

	// Two-Port Architecture: separate data-plane transport (nil = single-port mode).
	// When non-nil, chunk transfers (StoreFile, GetFile, LocalFile, GetChunkSwarm)
	// are routed through this transport on port N+1, keeping the main transport
	// (port N) free for gossip/heartbeat/announce/metadata.
	dataTransport peer2peer.Transport
	dataPeers     map[string]peer2peer.Peer
	dataPeerLock  sync.RWMutex

	// mDNS LAN auto-discovery advertiser (nil if identity not loaded).
	MDNSAdvertiser *peermdns.Advertiser

	// Cached mDNS LAN discovery results (updated by background goroutine).
	lanPeersMu sync.RWMutex
	lanPeers   []peermdns.DiscoveredPeer

	// Connection Manager: auto-reconnection with exponential backoff.
	backoffMu  sync.Mutex
	backoffMap map[string]time.Time // addr → earliest next dial
	backoffExp map[string]int       // addr → exponent (0→1min, 1→5min, 2+→1hr)
	maxPeers   int                  // target active peer connections

	// bootstrapAddrs holds the normalized addresses from --peer flags.
	// Used to bypass fingerprint blocklist for explicitly requested peers.
	bootstrapAddrs map[string]struct{}

	// activeUploads tracks in-progress uploads. Anti-entropy yields when > 0
	// to avoid Head-of-Line blocking on the shared QUIC connection.
	activeUploads atomic.Int32
}

type Message struct {
	Payload any
}

type MessageStoreFile struct {
	Key  string
	Size int64
}

type MessageGetFile struct {
	Key string
}

type MessageLocalFile struct {
	Key  string
	Size int64
}

// Sprint 2 message types

// MessageAnnounce is the first message an outbound node sends after OnPeer fires.
// It carries the sender's listen address so the receiving node can immediately
// remap the inbound peer from its ephemeral TCP port to the canonical listen
// address and add it to the hash ring — no heartbeat wait required.
type MessageAnnounce struct {
	ListenAddr string // e.g. "172.17.0.2:3000"
}

// MessageAnnounceAck is sent back by the inbound node after processing an announce.
// It tells the outbound node its externally-visible address so the node can
// re-register itself in the cluster/ring under a routable address instead of
// 127.0.0.1:port. This is critical for Docker/NAT/cloud deployments where the
// node doesn't know its own externally-reachable IP.
type MessageAnnounceAck struct {
	YourAddr string // the address the remote side sees us as (e.g. "172.22.0.3:3000")
}

type MessageHeartbeat struct {
	From      string
	Timestamp int64 // UnixNano
}

type MessageHeartbeatAck struct {
	From      string
	Timestamp int64 // echo of request timestamp for RTT measurement
}

// Sprint 3 message types

type MessageGossipDigest struct {
	From    string
	Digests []membership.GossipDigest
}

type MessageGossipResponse struct {
	From     string
	Full     []membership.NodeInfo
	MyDigest []membership.GossipDigest
}

// Sprint 4 message types

type MessageQuorumWrite struct {
	Key   string
	Data  []byte
	Clock map[string]uint64
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
	Key       string
	From      string
	Found     bool
	Clock     map[string]uint64
	Timestamp int64
}

type MessageMerkleSync struct {
	From     string
	RootHash [32]byte
}

type MessageMerkleDiffResponse struct {
	From    string
	AllKeys []string
}

// MessageLeaving is broadcast by a node that is voluntarily leaving the cluster.
// Peers immediately remove it from their hash ring and cluster state — no need
// to wait for failure detection timeouts.
type MessageLeaving struct {
	From       string // address of the departing node
	Generation uint64 // the sender's current generation — peers use gen+1 as StateLeft generation
	// so that any stale gossip digest (with gen==Generation) cannot override StateLeft
}

// MessageStoreManifest replicates a ChunkManifest to peers.
// The manifest lives only in metadata (no physical file), so a dedicated message
// is needed rather than the regular store-file stream protocol.
type MessageStoreManifest struct {
	FileKey      string // original file key (not the "manifest:" prefixed key)
	ManifestJSON []byte // JSON-encoded chunker.ChunkManifest
}

// MessageGetManifest asks a peer to return its copy of a manifest for FileKey.
type MessageGetManifest struct {
	FileKey string
}

// MessageManifestResponse is the reply to MessageGetManifest.
// ManifestJSON contains either a ChunkManifest or DirectoryManifest JSON.
// IsDirectory distinguishes the two — the receiver unmarshals accordingly.
type MessageManifestResponse struct {
	FileKey      string
	ManifestJSON []byte // nil/empty if not found
	IsDirectory  bool   // true = DirectoryManifest, false = ChunkManifest
}


// MessageIdentityMeta carries a node's identity metadata (alias, fingerprint,
// public keys) and is sent right after the announce handshake so that the remote
// node can store the metadata under the canonical address for alias resolution.
type MessageIdentityMeta struct {
	From     string            // canonical listen address
	Metadata map[string]string // {"alias","fingerprint","x25519_pub","ed25519_pub"}
}

// MessageChunkOffer is sent by the uploader before streaming chunk data.
// The receiver replies with MessageChunkNeed listing which hashes it is missing.
// Only missing chunks are then streamed, eliminating redundant transfers.
type MessageChunkOffer struct {
	Hashes []string // SHA-256 hex hashes of encrypted chunks being offered
}

// MessageChunkNeed is the receiver's reply to MessageChunkOffer.
// Missing contains the subset of offered hashes the receiver does not yet have.
type MessageChunkNeed struct {
	Missing []string // hashes the receiver needs; empty means "I have all of them"
}

// MessageGetPublicCatalog requests the remote peer's public file catalog.
type MessageGetPublicCatalog struct{}

// MessagePublicCatalogResponse carries the remote peer's public file list as JSON.
type MessagePublicCatalogResponse struct {
	CatalogJSON []byte // JSON-encoded []State.PublicFileEntry
}

// MessageDirectShare notifies a peer that a file has been shared with them.
// The sender uploads the file first, then sends this notification so the
// recipient can see it in their inbox and download it.
type MessageDirectShare struct {
	Key         string // storage key (fingerprint/name)
	Name        string // human-readable file name
	Size        int64
	IsDir       bool
	SenderAlias string
	SenderFP    string // sender's fingerprint
}

// MessageDeleteFile is a P2P tombstone propagation message.
// Only the owner (matching fingerprint) can delete a file.
type MessageDeleteFile struct {
	Key         string
	Fingerprint string
}

// MessageSearchRequest is a flood-based search query.
type MessageSearchRequest struct {
	Query     string
	RequestID string
	Origin    string // originating node addr — responses sent directly here
	TTL       int
}

// MessageSearchResponse carries search results back to the requester.
type MessageSearchResponse struct {
	RequestID string
	Results   []SearchResult
	FromNode  string
}

// ── Swarm architecture messages ───────────────────────────────────────

// MessageGetChunkSwarm requests a chunk by file key + index (swarm mode).
// Unlike MessageGetFile which uses a CAS storage key, this uses the file-level
// key and chunk index so the seeder can serve from the original file on disk.
type MessageGetChunkSwarm struct {
	FileKey    string
	ChunkIndex int
	EncHash    string // expected SHA-256 of encrypted/compressed chunk
}

// MessageRegisterProvider registers a node as a seeder for a file.
// Sent to hash ring nodes responsible for the file key.
type MessageRegisterProvider struct {
	FileKey     string
	Provider    string // addr of the seeder
	ChunkCount  int
	TotalChunks int
}

// MessageGetProviders queries hash ring nodes for the seeder list of a file.
type MessageGetProviders struct {
	FileKey string
}

// MessageProvidersResponse carries the seeder list back to the requester.
type MessageProvidersResponse struct {
	FileKey   string
	Providers []ProviderInfo
}

// ProviderInfo is the wire-format provider record (matches swarm.ProviderRecord).
type ProviderInfo struct {
	Addr        string
	ChunkCount  int
	TotalChunks int
}

// SearchResult represents a single file matching a search query.
type SearchResult struct {
	Key        string `json:"key"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	IsDir      bool   `json:"isDir"`
	OwnerAlias string `json:"ownerAlias"`
	OwnerFP    string `json:"ownerFingerprint"`
	NodeAddr   string `json:"nodeAddr"`
}

// SearchRequestCache prevents flood loops by tracking recently seen request IDs.
type SearchRequestCache struct {
	mu    sync.Mutex
	items map[string]time.Time
}

// NewSearchRequestCache creates a new dedup cache.
func NewSearchRequestCache() *SearchRequestCache {
	return &SearchRequestCache{items: make(map[string]time.Time)}
}

// SeenOrAdd returns true if the request was already seen; otherwise records it.
func (c *SearchRequestCache) SeenOrAdd(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[id]; ok {
		return true
	}
	c.items[id] = time.Now()
	// Inline GC: remove entries older than 30s.
	if len(c.items) > 100 {
		cutoff := time.Now().Add(-30 * time.Second)
		for k, t := range c.items {
			if t.Before(cutoff) {
				delete(c.items, k)
			}
		}
	}
	return false
}

// ContentLength returns the total plaintext byte count for a stored key.
// Returns 0 when the size is unknown (legacy single-blob files or key not found).
func (s *Server) ContentLength(key string) int64 {
	meta, ok := s.serverOpts.metaData.Get(key)
	if !ok {
		return 0
	}
	if meta.Manifest != nil {
		return meta.Manifest.TotalSize
	}
	// Manifest may be stored separately (replicated or fetched from peers).
	if meta.Chunked {
		if m, ok := s.serverOpts.metaData.GetManifest(key); ok {
			return m.TotalSize
		}
	}
	return 0
}

func (s *Server) GetData(key string, dek []byte) (io.ReadCloser, error) {
	t0 := time.Now()

	// Check whether this key is stored as chunked.
	fm, hasMeta := s.serverOpts.metaData.Get(key)
	log.Printf("[LIFECYCLE] GET_DATA: key=%s hasMeta=%v Chunked=%v", key, hasMeta, fm.Chunked)
	if hasMeta && fm.Chunked {
		defer func() { metrics.RecordGet("local", nil, time.Since(t0)) }()
		return s.getChunked(key, dek)
	}

	// Not local — try to fetch the manifest from a peer, then reassemble.
	defer func() { metrics.RecordGet("remote", nil, time.Since(t0)) }()
	log.Printf("GET_DATA: key '%s' not local, checking peers for manifest", key)

	manifest, err := s.fetchManifestFromPeers(key)
	if err != nil {
		return nil, fmt.Errorf("GET_DATA: manifest for '%s' unavailable: %w", key, err)
	}

	// Cache the manifest locally so subsequent calls use getChunked.
	_ = s.serverOpts.metaData.SetManifest(key, manifest)
	_ = s.serverOpts.metaData.Set(key, storage.FileMeta{Chunked: true, Timestamp: time.Now().UnixNano()})

	return s.getChunked(key, dek)
}

// GetDataToFile downloads a chunked file directly to the specified path using
// random-access I/O (io.WriterAt). Resume-aware: writes to <filePath>.part,
// tracks progress in an append-only <filePath>.part.resume sidecar, fast-verifies
// on restart, and atomically renames to filePath on completion.
//
// The progress callback receives (completedChunks, totalChunks) after each chunk.
func (s *Server) GetDataToFile(ctx context.Context, key string, filePath string, dek []byte, progress downloader.ProgressFunc) error {
	manifest, err := s.EnsureManifest(key)
	if err != nil {
		return err
	}

	partPath := resume.PartPath(filePath)
	n := len(manifest.Chunks)

	// ── Resume boot: check for existing sidecar ──────────────────────
	skipSet := make(map[int]bool)
	var sidecar *resume.Sidecar

	existing, claimed, err := resume.Open(filePath)
	if err != nil {
		log.Printf("[RESUME] corrupt sidecar for %q, starting fresh: %v", filePath, err)
		_ = resume.Cleanup(filePath)
	} else if existing != nil {
		if existing.Matches(key, manifest.MerkleRoot, n, manifest.TotalSize) {
			// Fast-verify: read each claimed chunk from .part, hash it, keep only verified ones.
			skipSet = s.fastVerifyChunks(partPath, manifest, claimed)
			sidecar = existing
			log.Printf("[RESUME] verified %d/%d chunks for %q, resuming", len(skipSet), n, filePath)
		} else {
			// Manifest changed — stale sidecar.
			log.Printf("[RESUME] manifest changed for %q, starting fresh", filePath)
			existing.Close()
			_ = resume.Cleanup(filePath)
			_ = os.Remove(partPath)
		}
	}

	// ── Create sidecar if we don't have one ──────────────────────────
	if sidecar == nil {
		sidecar, err = resume.Create(filePath, key, manifest.MerkleRoot, n, manifest.TotalSize, manifest.ChunkSize)
		if err != nil {
			return fmt.Errorf("GetDataToFile: create sidecar: %w", err)
		}
	}
	defer sidecar.Close()

	// ── Open/pre-allocate .part file ─────────────────────────────────
	f, err := os.OpenFile(partPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("GetDataToFile: open %q: %w", partPath, err)
	}
	defer f.Close()

	if err := f.Truncate(manifest.TotalSize); err != nil {
		return fmt.Errorf("GetDataToFile: truncate: %w", err)
	}

	// ── Download (skip already-verified chunks) ──────────────────────
	onChunkDone := func(index int, hash string) {
		if err := sidecar.RecordChunk(index); err != nil {
			log.Printf("[RESUME] failed to record chunk %d: %v", index, err)
		}
	}

	if s.Downloader != nil {
		if err := s.Downloader.DownloadToFileResumable(ctx, key, manifest, f, skipSet, progress, onChunkDone, dek); err != nil {
			return fmt.Errorf("GetDataToFile: %w", err)
		}
	} else {
		if err := s.serialDownloadToFile(ctx, manifest, f, skipSet, onChunkDone, dek, progress); err != nil {
			return fmt.Errorf("GetDataToFile: %w", err)
		}
	}

	// ── Success: cleanup sidecar, atomic rename .part → final ────────
	f.Close()
	sidecar.Close()
	_ = resume.Cleanup(filePath)

	if err := storage.SafeRename(partPath, filePath); err != nil {
		return fmt.Errorf("GetDataToFile: rename %q → %q: %w", partPath, filePath, err)
	}

	go s.readRepair(key)

	// Swarm: after successful download, register as CAS-backed seeder so other
	// nodes can fetch chunks from us (downloaders become seeders).
	if manifest.SeedMode && s.StateDB != nil {
		_ = s.StateDB.SetSeedSource(key, &swarm.SeedSource{
			CASBacked:   true,
			ChunkSize:   manifest.ChunkSize,
			TotalChunks: len(manifest.Chunks),
			Bitfield:    swarm.NewFullBitfield(len(manifest.Chunks)),
		})
		s.registerProvider(key, len(manifest.Chunks), len(manifest.Chunks))
	}

	return nil
}

// fastVerifyChunks reads each claimed chunk from the .part file, hashes it,
// and returns only the indices whose hash matches the manifest. This ensures
// we never trust a sidecar over actual disk state.
func (s *Server) fastVerifyChunks(partPath string, manifest *chunker.ChunkManifest, claimed map[int]bool) map[int]bool {
	verified := make(map[int]bool)
	if len(claimed) == 0 {
		return verified
	}

	f, err := os.Open(partPath)
	if err != nil {
		return verified // .part missing — no chunks verified
	}
	defer f.Close()

	buf := make([]byte, manifest.ChunkSize)
	for idx := range claimed {
		if idx < 0 || idx >= len(manifest.Chunks) {
			continue
		}
		info := manifest.Chunks[idx]
		offset := int64(idx) * int64(manifest.ChunkSize)

		// Determine actual chunk size (last chunk may be smaller).
		chunkLen := int64(manifest.ChunkSize)
		if int64(idx) == int64(len(manifest.Chunks)-1) {
			remainder := manifest.TotalSize - offset
			if remainder > 0 && remainder < chunkLen {
				chunkLen = remainder
			}
		}

		n, err := f.ReadAt(buf[:chunkLen], offset)
		if err != nil || int64(n) != chunkLen {
			continue // can't read — will re-download
		}

		got := sha256.Sum256(buf[:chunkLen])
		if hex.EncodeToString(got[:]) == info.Hash {
			verified[idx] = true
		}
	}
	return verified
}

// EnsureManifest returns the manifest for key, fetching from peers if needed.
func (s *Server) EnsureManifest(key string) (*chunker.ChunkManifest, error) {
	fm, hasMeta := s.serverOpts.metaData.Get(key)
	if hasMeta && fm.Chunked {
		m, ok := s.serverOpts.metaData.GetManifest(key)
		if ok {
			return m, nil
		}
	}

	manifest, err := s.fetchManifestFromPeers(key)
	if err != nil {
		return nil, fmt.Errorf("manifest for '%s' unavailable: %w", key, err)
	}
	_ = s.serverOpts.metaData.SetManifest(key, manifest)
	_ = s.serverOpts.metaData.Set(key, storage.FileMeta{Chunked: true, Timestamp: time.Now().UnixNano()})
	return manifest, nil
}

// serialDownloadToFile is the fallback when no Downloader is wired. Fetches
// chunks one-by-one and writes each to dst at the correct offset.
// Chunks in skipSet are skipped (already verified on disk).
func (s *Server) serialDownloadToFile(ctx context.Context, manifest *chunker.ChunkManifest, dst io.WriterAt, skipSet map[int]bool, onChunkDone downloader.ChunkRecordFunc, dek []byte, progress downloader.ProgressFunc) error {
	n := len(manifest.Chunks)
	completed := len(skipSet)

	for i, info := range manifest.Chunks {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if skipSet[i] {
			continue
		}

		storageKey := chunker.ChunkStorageKey(info.EncHash)

		var chunkData []byte
		if s.Store.Has(storageKey) {
			_, r, err := s.Store.ReadStream(storageKey)
			if err != nil {
				return fmt.Errorf("read local chunk %d: %w", info.Index, err)
			}
			chunkData, err = io.ReadAll(r)
			r.Close()
			if err != nil {
				return fmt.Errorf("read bytes chunk %d: %w", info.Index, err)
			}
		} else {
			data, err := s.fetchChunkFromPeers(ctx, storageKey)
			if err != nil {
				return fmt.Errorf("fetch chunk %d from peers: %w", info.Index, err)
			}
			chunkData = data
			_, _ = s.Store.WriteStream(storageKey, bytes.NewReader(chunkData))
		}

		plainBytes := chunkData
		if dek != nil {
			var plain bytes.Buffer
			if err := crypto.DecryptStreamWithDEK(bytes.NewReader(chunkData), &plain, dek, uint64(info.Index)); err != nil {
				return fmt.Errorf("decrypt chunk %d: %w", info.Index, err)
			}
			plainBytes = plain.Bytes()
		}
		if info.Compressed {
			var err error
			plainBytes, err = compression.DecompressChunk(plainBytes)
			if err != nil {
				return fmt.Errorf("decompress chunk %d: %w", info.Index, err)
			}
		}

		got := sha256.Sum256(plainBytes)
		gotHex := hex.EncodeToString(got[:])
		if gotHex != info.Hash {
			return fmt.Errorf("integrity check failed for chunk %d (want %s got %s)",
				info.Index, info.Hash, gotHex)
		}

		offset := int64(i) * int64(manifest.ChunkSize)
		if _, err := dst.WriteAt(plainBytes, offset); err != nil {
			return fmt.Errorf("write chunk %d: %w", info.Index, err)
		}

		if onChunkDone != nil {
			onChunkDone(i, info.Hash)
		}

		completed++
		if progress != nil {
			progress(completed, n)
		}
	}

	return chunker.VerifyMerkleRoot(manifest.Chunks, manifest.MerkleRoot)
}


// getChunked streams a chunked file via io.Pipe. When a Downloader is wired in
// it uses DownloadToStream (sliding-window, bounded memory); otherwise it falls
// back to a serial loop. Either path decrypts + decompresses + verifies each
// chunk on-the-fly before writing to the pipe.
func (s *Server) getChunked(key string, dek []byte) (io.ReadCloser, error) {
	manifest, ok := s.serverOpts.metaData.GetManifest(key)
	if !ok {
		return nil, fmt.Errorf("getChunked: no manifest for key '%s'", key)
	}
	log.Printf("[LIFECYCLE] GET_CHUNKED: key=%s numChunks=%d usingDownloader=%v encrypted=%v", key, len(manifest.Chunks), s.Downloader != nil, dek != nil)

	pr, pw := io.Pipe()

	if s.Downloader != nil {
		// Parallel path: sliding-window streaming via DownloadToStream.
		go func() {
			err := s.Downloader.DownloadToStream(context.Background(), key, manifest, pw, nil, dek)
			pw.CloseWithError(err) // nil err closes cleanly
			if err == nil {
				go s.readRepair(key)
			}
		}()
		return pr, nil
	}

	// Serial fallback: write chunks one-by-one to the pipe.
	go func() {
		for _, info := range manifest.Chunks {
			storageKey := chunker.ChunkStorageKey(info.EncHash)

			var chunkData []byte
			if s.Store.Has(storageKey) {
				_, r, err := s.Store.ReadStream(storageKey)
				if err != nil {
					pw.CloseWithError(fmt.Errorf("getChunked: read local chunk %d: %w", info.Index, err))
					return
				}
				chunkData, err = io.ReadAll(r)
				r.Close()
				if err != nil {
					pw.CloseWithError(fmt.Errorf("getChunked: read bytes chunk %d: %w", info.Index, err))
					return
				}
			} else {
				fetchCtx, fetchCancel := context.WithTimeout(context.Background(), 30*time.Second)
				data, err := s.fetchChunkFromPeers(fetchCtx, storageKey)
				fetchCancel()
				if err != nil {
					pw.CloseWithError(fmt.Errorf("getChunked: fetch chunk %d from peers: %w", info.Index, err))
					return
				}
				chunkData = data
				_, _ = s.Store.WriteStream(storageKey, bytes.NewReader(chunkData))
			}

			plainBytes := chunkData
			if dek != nil {
				var plain bytes.Buffer
				if err := crypto.DecryptStreamWithDEK(bytes.NewReader(chunkData), &plain, dek, uint64(info.Index)); err != nil {
					pw.CloseWithError(fmt.Errorf("getChunked: decrypt chunk %d: %w", info.Index, err))
					return
				}
				plainBytes = plain.Bytes()
			}

			if info.Compressed {
				var err error
				plainBytes, err = compression.DecompressChunk(plainBytes)
				if err != nil {
					pw.CloseWithError(fmt.Errorf("getChunked: decompress chunk %d: %w", info.Index, err))
					return
				}
			}

			got := sha256.Sum256(plainBytes)
			gotHex := hex.EncodeToString(got[:])
			if gotHex != info.Hash {
				pw.CloseWithError(fmt.Errorf("getChunked: integrity check failed for chunk %d (want %s got %s)",
					info.Index, info.Hash, gotHex))
				return
			}

			if _, err := pw.Write(plainBytes); err != nil {
				pw.CloseWithError(fmt.Errorf("getChunked: write chunk %d: %w", info.Index, err))
				return
			}
		}
		pw.Close() // success
		go s.readRepair(key)
	}()

	return pr, nil
}

// fetchChunkFromPeers requests a chunk's encrypted bytes from the ring-
// responsible peers and returns the raw encrypted data.
func (s *Server) fetchChunkFromPeers(ctx context.Context, storageKey string) ([]byte, error) {
	selfAddr := s.serverOpts.transport.Addr()
	targetNodes := s.HashRing.GetNodes(storageKey, s.HashRing.ReplicationFactor())

	// If another goroutine is already fetching this key, share its channel
	// rather than overwriting it. Overwriting would orphan the first caller's
	// select, causing it to time out even when a response arrives.
	s.mu.Lock()
	ch, alreadyFetching := s.pendingFile[storageKey]
	if !alreadyFetching {
		ch = make(chan []byte, 1)
		s.pendingFile[storageKey] = ch
	}
	s.mu.Unlock()

	if alreadyFetching {
		// Another goroutine owns this fetch — wait for its result.
		select {
		case data := <-ch:
			if data == nil {
				return nil, fmt.Errorf("fetchChunkFromPeers: nil data for '%s'", storageKey)
			}
			// Put it back so any other waiter also gets it (best-effort).
			select {
			case ch <- data:
			default:
			}
			return data, nil
		case <-ctx.Done():
			return nil, fmt.Errorf("fetchChunkFromPeers: timeout waiting for in-flight fetch of '%s'", storageKey)
		}
	}

	defer func() {
		s.mu.Lock()
		delete(s.pendingFile, storageKey)
		s.mu.Unlock()
	}()

	getMsg := &Message{Payload: &MessageGetFile{Key: storageKey}}
	buf := new(bytes.Buffer)
	if err := gob.NewEncoder(buf).Encode(getMsg); err != nil {
		return nil, err
	}
	msgBytes := buf.Bytes()

	s.peerLock.RLock()
	sent := 0
	for _, nodeAddr := range targetNodes {
		if nodeAddr == selfAddr {
			continue
		}
		peer, ok := s.peers[nodeAddr]
		if !ok {
			continue
		}
		if err := peer.SendMsg(peer2peer.IncomingMessage, msgBytes); err != nil {
			continue
		}
		sent++
	}
	s.peerLock.RUnlock()

	if sent == 0 {
		return nil, fmt.Errorf("no reachable peers for chunk '%s'", storageKey)
	}

	select {
	case data := <-ch:
		if data == nil {
			return nil, fmt.Errorf("nil data for chunk '%s'", storageKey)
		}
		return data, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("timeout fetching chunk '%s'", storageKey)
	}
}

// fetchChunkFromPeer fetches the encrypted bytes for storageKey from one
// specific peer. Used by the Downloader which has already selected the best
// peer via the Selector — so we send to that peer only.
// When peerAddr == selfAddr the chunk is read directly from local storage.
func (s *Server) fetchChunkFromPeer(ctx context.Context, storageKey, peerAddr string) ([]byte, error) {
	// Local read: chunk is on disk here — no network hop needed.
	// Check s.Store.Has first so that a node whose ring address doesn't match
	// its transport.Addr() string (e.g. Docker container whose canonical addr
	// is "172.17.0.2:3000" but transport.Addr() is ":3000") still serves the
	// chunk from local storage instead of attempting a self-dial.
	if s.Store.Has(storageKey) {
		_, r, err := s.Store.ReadStream(storageKey)
		if err != nil {
			return nil, fmt.Errorf("fetchChunkFromPeer: local read: %w", err)
		}
		defer r.Close()
		return io.ReadAll(r)
	}

	// If another goroutine is already fetching this key, share its channel.
	s.mu.Lock()
	ch, alreadyFetching := s.pendingFile[storageKey]
	if !alreadyFetching {
		ch = make(chan []byte, 1)
		s.pendingFile[storageKey] = ch
	}
	s.mu.Unlock()

	if alreadyFetching {
		select {
		case data := <-ch:
			if data == nil {
				return nil, fmt.Errorf("fetchChunkFromPeer: nil data for '%s'", storageKey)
			}
			select {
			case ch <- data:
			default:
			}
			return data, nil
		case <-ctx.Done():
			return nil, fmt.Errorf("fetchChunkFromPeer: timeout waiting for in-flight fetch of '%s'", storageKey)
		}
	}

	defer func() {
		s.mu.Lock()
		delete(s.pendingFile, storageKey)
		s.mu.Unlock()
	}()

	getMsg := &Message{Payload: &MessageGetFile{Key: storageKey}}
	buf := new(bytes.Buffer)
	if err := gob.NewEncoder(buf).Encode(getMsg); err != nil {
		return nil, err
	}

	s.peerLock.RLock()
	peer, ok := s.peers[peerAddr]
	s.peerLock.RUnlock()
	if !ok {
		return nil, fmt.Errorf("fetchChunkFromPeer: peer %s not connected", peerAddr)
	}
	if err := peer.SendMsg(peer2peer.IncomingMessage, buf.Bytes()); err != nil {
		return nil, fmt.Errorf("fetchChunkFromPeer: send msg: %w", err)
	}

	select {
	case data := <-ch:
		if data == nil {
			return nil, fmt.Errorf("fetchChunkFromPeer: nil data for '%s'", storageKey)
		}
		return data, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("fetchChunkFromPeer: timeout for '%s'", storageKey)
	}
}

// fetchMetadataFromPeers sends a unified metadata request to peers.
// Returns raw JSON bytes and isDirectory flag. The caller unmarshals
// into ChunkManifest or DirectoryManifest based on the flag.
func (s *Server) fetchMetadataFromPeers(ctx context.Context, key string) (data []byte, isDirectory bool, err error) {
	selfAddr := s.serverOpts.transport.Addr()
	targetNodes := s.HashRing.GetNodes(key, s.HashRing.ReplicationFactor())

	pendingKey := "__manifest__" + key
	ch := make(chan []byte, len(targetNodes))
	s.mu.Lock()
	s.pendingFile[pendingKey] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pendingFile, pendingKey)
		s.mu.Unlock()
	}()

	getMsg := &Message{Payload: &MessageGetManifest{FileKey: key}}
	buf := new(bytes.Buffer)
	if err := gob.NewEncoder(buf).Encode(getMsg); err != nil {
		return nil, false, err
	}
	msgBytes := buf.Bytes()

	s.peerLock.RLock()
	sent := 0
	for _, nodeAddr := range targetNodes {
		if nodeAddr == selfAddr {
			continue
		}
		peer, ok := s.peers[nodeAddr]
		if !ok {
			continue
		}
		if err := peer.SendMsg(peer2peer.IncomingMessage, msgBytes); err != nil {
			continue
		}
		sent++
	}
	s.peerLock.RUnlock()

	if sent == 0 {
		return nil, false, fmt.Errorf("fetchMetadata: no reachable peers for '%s'", key)
	}

	select {
	case payload := <-ch:
		if len(payload) < 1 {
			return nil, false, fmt.Errorf("fetchMetadata: empty response")
		}
		// First byte is type flag: 0x00=file, 0x01=directory.
		return payload[1:], payload[0] == 0x01, nil
	case <-ctx.Done():
		return nil, false, fmt.Errorf("timeout fetching metadata for '%s'", key)
	}
}

// fetchManifestFromPeers retrieves a ChunkManifest from peers for key.
func (s *Server) fetchManifestFromPeers(key string) (*chunker.ChunkManifest, error) {
	fetchCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	data, isDir, err := s.fetchMetadataFromPeers(fetchCtx, key)
	if err != nil {
		return nil, err
	}
	if isDir {
		return nil, fmt.Errorf("key '%s' is a directory, not a file", key)
	}
	var manifest chunker.ChunkManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("fetchManifest: unmarshal: %w", err)
	}
	return &manifest, nil
}

// processedChunk carries all per-chunk results from Stage 1 (compress + optional encrypt)
// to Stage 2 (local store + replicate) of the StoreData pipeline.
type processedChunk struct {
	info       chunker.ChunkInfo
	data       []byte
	storageKey string
	targets    []string
	pooled     bool // true if data is from chunkDataPool and must be returned
}

// EncryptionMeta carries ECDH encryption metadata for StoreData.
// Pass nil for plaintext uploads (no encryption).
type EncryptionMeta struct {
	DEK           []byte                // raw DEK for chunk encryption
	OwnerPubKey   string                // hex X25519 pub of uploader
	OwnerEdPubKey string                // hex Ed25519 pub of uploader
	AccessList    []chunker.AccessEntry // one entry per recipient
	Signature     string                // hex Ed25519 signature over manifest fields
}

// UploadProgressFunc is called after each chunk is stored and replicated.
// completed is the number of chunks finished so far, total is the estimated
// total (may be 0 if file size was unknown).
type UploadProgressFunc func(completed, total int)

// StoredFileResult holds metadata collected during a deferred-metadata upload.
// Used by StoreDirectory to batch all BoltDB writes into one transaction.
type StoredFileResult struct {
	ChunkMetas []ChunkMetaEntry          // per-chunk metadata (storageKey → FileMeta)
	Manifest   *chunker.ChunkManifest    // file manifest
	FileKey    string                     // top-level file key
	FileMeta   storage.FileMeta           // top-level file meta
}

// ChunkMetaEntry pairs a storage key with its FileMeta for batch writing.
type ChunkMetaEntry struct {
	Key  string
	Meta storage.FileMeta
}

func (s *Server) StoreData(key string, w io.Reader, enc *EncryptionMeta) error {
	_, err := s.storeDataInternal(context.Background(), key, w, enc, 0, nil, false)
	return err
}

func (s *Server) StoreDataWithProgress(ctx context.Context, key string, w io.Reader, enc *EncryptionMeta, fileSize int64, progress UploadProgressFunc) error {
	_, err := s.storeDataInternal(ctx, key, w, enc, fileSize, progress, false)
	return err
}

// StoreDataCollectMeta performs a full upload (chunk, compress, encrypt, CAS
// store, replicate) but defers the 3 BoltDB metadata writes. Returns the
// collected metadata so the caller can batch-write them in a single transaction.
func (s *Server) StoreDataCollectMeta(ctx context.Context, key string, w io.Reader, enc *EncryptionMeta, fileSize int64, progress UploadProgressFunc) (*StoredFileResult, error) {
	return s.storeDataInternal(ctx, key, w, enc, fileSize, progress, true)
}

func (s *Server) storeDataInternal(ctx context.Context, key string, w io.Reader, enc *EncryptionMeta, fileSize int64, progress UploadProgressFunc, deferMeta bool) (*StoredFileResult, error) {
	s.activeUploads.Add(1)
	defer s.activeUploads.Add(-1)

	t0 := time.Now()
	log.Println("STORE_DATA: Starting chunked storage for key:", key)
	defer func() { metrics.RecordStore("store", nil, time.Since(t0)) }()

	selfAddr := normalizeAddr(s.serverOpts.transport.Addr())
	replFactor := s.HashRing.ReplicationFactor()

	// ── Resume: load any existing pending sidecar for this key ──
	// Skip the sidecar entirely for single-chunk files (fileSize known and
	// fits in one chunk) — there is nothing to resume for a 1-chunk upload,
	// and the sidecar I/O dominates directory uploads with many small files.
	storageRoot := s.serverOpts.storageRoot
	useSidecar := fileSize == 0 || fileSize > int64(chunker.DefaultChunkSize)

	skipSet := make(map[int]bool)
	var resumedInfos []chunker.ChunkInfo
	var sidecar *pending.Sidecar

	if useSidecar {
		prevResult, err := pending.Load(storageRoot, key)
		if err != nil {
			log.Printf("STORE_DATA: failed to load pending sidecar (starting fresh): %v", err)
		}
		if prevResult != nil {
			skipSet = prevResult.Completed
			resumedInfos = prevResult.ChunkInfos
			log.Printf("STORE_DATA: resuming upload for %q — %d chunks already done", key, len(skipSet))
		}

		// Create (or re-create) the pending sidecar.
		sidecar, err = pending.Create(storageRoot, pending.Header{
			StorageKey: key,
			ChunkSize:  chunker.DefaultChunkSize,
			CreatedAt:  time.Now().UnixNano(),
		})
		if err != nil {
			return nil, fmt.Errorf("STORE_DATA: create pending sidecar: %w", err)
		}
		// Pre-record all resumed chunks so a second crash doesn't lose them.
		for _, ci := range resumedInfos {
			sidecar.RecordChunk(ci) //nolint:errcheck
		}
		defer sidecar.Close()
	}

	// Estimate total chunks for progress reporting.
	totalChunks := 0
	if fileSize > 0 {
		totalChunks = int((fileSize + int64(chunker.DefaultChunkSize) - 1) / int64(chunker.DefaultChunkSize))
	}

	// procCh connects Stage 1 (compress + optional encrypt) with Stage 2 (store+replicate).
	// Buffered=2 lets Stage 1 stay up to 1 chunk ahead of Stage 2.
	procCh := make(chan processedChunk, 2)
	stage1ErrCh := make(chan error, 1)

	// Stage 1: read → compress → (optionally encrypt) → push to procCh.
	// The chunker borrows data slices from chunkDataPool.  When compression
	// or encryption transforms the data into a new buffer, the original
	// pooled slice is returned immediately.  Otherwise the pooled slice
	// travels through to Stage 2 and is returned after replication.
	go func() {
		defer close(procCh)
		chunkCh, errCh := chunker.ChunkReaderWithPool(w, chunker.DefaultChunkSize, &chunkReadBufPool, &chunkDataPool)
		for chunk := range chunkCh {
			// Skip chunks already completed in a previous partial upload.
			if skipSet[chunk.Index] {
				// Return pooled data immediately.
				b := chunk.Data[:cap(chunk.Data)]
				chunkDataPool.Put(&b)
				continue
			}

			processed := chunk.Data
			pooledData := chunk.Data // keep reference for return-to-pool
			wasCompressed := false
			if compression.ShouldCompress(chunk.Data) {
				if c, wc, err := compression.CompressChunkWithPool(chunk.Data, compression.LevelFastest, &compBufPool); err == nil && wc {
					processed = c
					wasCompressed = true
				}
			}

			// Encrypt only when encryption metadata was provided (ECDH path).
			if enc != nil {
				var encBuf bytes.Buffer
				if err := crypto.EncryptStreamWithDEKPool(bytes.NewReader(processed), &encBuf, enc.DEK, &encBufPool, uint64(chunk.Index)); err != nil {
					// Return pooled data before bailing.
					b := pooledData[:cap(pooledData)]
					chunkDataPool.Put(&b)
					stage1ErrCh <- fmt.Errorf("STORE_DATA: encrypt chunk %d: %w", chunk.Index, err)
					return
				}
				processed = encBuf.Bytes()
			}

			// If processed is a different buffer (compression or encryption
			// created a new slice), return the original pooled data now.
			// Otherwise mark it as pooled so Stage 2 returns it after replication.
			dataIsPooled := false
			if cap(processed) != cap(pooledData) {
				b := pooledData[:cap(pooledData)]
				chunkDataPool.Put(&b)
			} else {
				dataIsPooled = true
			}

			hashRaw := sha256.Sum256(processed)
			hashHex := hex.EncodeToString(hashRaw[:])
			storageKey := chunker.ChunkStorageKey(hashHex)
			targets := s.HashRing.GetNodes(storageKey, replFactor)

			log.Printf("STORE_DATA: chunk %d processed (compressed=%v encrypted=%v storageKey=%s)", chunk.Index, wasCompressed, enc != nil, storageKey)

			procCh <- processedChunk{
				info: chunker.ChunkInfo{
					Index:      chunk.Index,
					Hash:       hex.EncodeToString(chunk.Hash[:]),
					Size:       chunk.Size,
					EncHash:    hashHex,
					Compressed: wasCompressed,
				},
				data:       processed,
				storageKey: storageKey,
				targets:    targets,
				pooled:     dataIsPooled,
			}
		}
		stage1ErrCh <- <-errCh
	}()

	// Stage 2: local store + replicate.
	// The semaphore is acquired BEFORE spawning the goroutine so the for-loop
	// blocks when all replication slots are busy.  This creates backpressure
	// all the way back to Stage 1 / the disk reader, capping resident memory
	// at (maxInFlight + procCh cap) × chunkSize regardless of file size.
	const maxInFlight = 8
	sem := make(chan struct{}, maxInFlight)
	var replWg sync.WaitGroup

	// Start with any chunks recovered from a previous partial upload.
	chunkInfos := make([]chunker.ChunkInfo, 0, len(resumedInfos))
	chunkInfos = append(chunkInfos, resumedInfos...)
	completed := len(chunkInfos)

	// Report initial progress for resumed chunks.
	if progress != nil && completed > 0 {
		progress(completed, totalChunks)
	}

	// Collect per-chunk metadata when deferring writes.
	var collectedChunkMetas []ChunkMetaEntry

	for pc := range procCh {
		// Check for pause/cancel between chunks.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// Store locally (skip if already present — dedup).
		if !s.Store.Has(pc.storageKey) {
			if _, err := s.Store.WriteStream(pc.storageKey, bytes.NewReader(pc.data)); err != nil {
				return nil, fmt.Errorf("STORE_DATA: local store chunk %d: %w", pc.info.Index, err)
			}
			fm, _ := s.serverOpts.metaData.Get(pc.storageKey)
			fm.Timestamp = time.Now().UnixNano()
			if deferMeta {
				collectedChunkMetas = append(collectedChunkMetas, ChunkMetaEntry{Key: pc.storageKey, Meta: fm})
			} else if err := s.serverOpts.metaData.Set(pc.storageKey, fm); err != nil {
				return nil, fmt.Errorf("STORE_DATA: metadata chunk %d: %w", pc.info.Index, err)
			}
		}

		log.Printf("[LIFECYCLE] STORE_DATA: chunk stored locally storageKey=%s targets=%v",
			pc.storageKey, pc.targets)

		// Block HERE until a replication slot is free — this is the
		// backpressure point that prevents unbounded goroutine/memory growth.
		sem <- struct{}{}

		replWg.Add(1)
		go func(sk string, data []byte, targets []string, idx int, pooled bool) {
			defer replWg.Done()
			defer func() { <-sem }()
			defer func() {
				if pooled {
					b := data[:cap(data)]
					chunkDataPool.Put(&b)
				}
			}()
			s.replicateChunk(selfAddr, sk, data, targets)
			log.Printf("STORE_DATA: chunk %d replicated (storageKey=%s)", idx, sk)
		}(pc.storageKey, pc.data, pc.targets, pc.info.Index, pc.pooled)

		chunkInfos = append(chunkInfos, pc.info)
		completed++

		// Record in sidecar (crash-safe).
		if sidecar != nil {
			sidecar.RecordChunk(pc.info) //nolint:errcheck
		}

		// Report progress.
		if progress != nil {
			progress(completed, totalChunks)
		}
	}

	// Wait for all replication goroutines to finish before building the manifest.
	replWg.Wait()

	// Check for error from Stage 1.
	if err := <-stage1ErrCh; err != nil {
		return nil, fmt.Errorf("STORE_DATA: stage1: %w", err)
	}

	// Build and persist the manifest locally.
	manifest := chunker.BuildManifest(key, chunkInfos, time.Now().UnixNano())
	if enc != nil {
		manifest.Encrypted = true
		manifest.OwnerPubKey = enc.OwnerPubKey
		manifest.OwnerEdPubKey = enc.OwnerEdPubKey
		manifest.AccessList = enc.AccessList
		manifest.Signature = enc.Signature
	}
	// Manifest replication is fire-and-forget (non-blocking) regardless of deferMeta.
	s.replicateManifest(key, selfAddr, manifest)

	// Build top-level FileMeta.
	topFm, _ := s.serverOpts.metaData.Get(key)
	if topFm.VClock == nil {
		topFm.VClock = make(map[string]uint64)
	}
	topFm.VClock[selfAddr]++
	topFm.Chunked = true
	topFm.Timestamp = time.Now().UnixNano()

	if deferMeta {
		// Return collected metadata for the caller to batch-write.
		result := &StoredFileResult{
			ChunkMetas: collectedChunkMetas,
			Manifest:   manifest,
			FileKey:    key,
			FileMeta:   topFm,
		}
		// Upload succeeded — delete the pending sidecar.
		if sidecar != nil {
			sidecar.Close()
			pending.Finalize(storageRoot, key) //nolint:errcheck
		}
		log.Printf("STORE_DATA: key '%s' stored as %d chunks (deferred metadata)", key, len(chunkInfos))
		return result, nil
	}

	if err := s.serverOpts.metaData.SetManifest(key, manifest); err != nil {
		return nil, fmt.Errorf("STORE_DATA: store manifest: %w", err)
	}
	if err := s.serverOpts.metaData.Set(key, topFm); err != nil {
		return nil, fmt.Errorf("STORE_DATA: update file meta: %w", err)
	}

	// Upload succeeded — delete the pending sidecar.
	if sidecar != nil {
		sidecar.Close()
		pending.Finalize(storageRoot, key) //nolint:errcheck
	}

	log.Printf("STORE_DATA: key '%s' stored as %d chunks", key, len(chunkInfos))
	return nil, nil
}

// ── Seed In Place (swarm mode upload) ─────────────────────────────────

// SeedInPlace indexes a file for swarm-mode serving without copying it to CAS.
// It reads the file to compute chunk hashes, builds a manifest, replicates the
// manifest to hash ring nodes, and registers this node as a provider. The file
// stays where it is — chunks are served on-demand from the original path.
func (s *Server) SeedInPlace(ctx context.Context, key, filePath string, enc *EncryptionMeta, fileSize int64, progress UploadProgressFunc) error {
	s.activeUploads.Add(1)
	defer s.activeUploads.Add(-1)

	t0 := time.Now()
	log.Printf("SEED_IN_PLACE: Starting for key=%q path=%q size=%d", key, filePath, fileSize)
	defer func() { metrics.RecordStore("seed_in_place", nil, time.Since(t0)) }()

	selfAddr := normalizeAddr(s.serverOpts.transport.Addr())

	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("SEED_IN_PLACE: open: %w", err)
	}
	defer f.Close()

	// If fileSize wasn't provided, stat the file.
	if fileSize <= 0 {
		info, err := f.Stat()
		if err != nil {
			return fmt.Errorf("SEED_IN_PLACE: stat: %w", err)
		}
		fileSize = info.Size()
	}

	totalChunks := int((fileSize + int64(chunker.DefaultChunkSize) - 1) / int64(chunker.DefaultChunkSize))

	chunkCh, errCh := chunker.ChunkReaderWithPool(f, chunker.DefaultChunkSize, &chunkReadBufPool, &chunkDataPool)
	var chunkInfos []chunker.ChunkInfo

	for chunk := range chunkCh {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		processed := chunk.Data
		pooledData := chunk.Data
		wasCompressed := false
		if compression.ShouldCompress(chunk.Data) {
			if c, wc, compErr := compression.CompressChunkWithPool(chunk.Data, compression.LevelFastest, &compBufPool); compErr == nil && wc {
				processed = c
				wasCompressed = true
			}
		}

		// Encrypt if ECDH — needed to compute EncHash (deterministic nonces
		// guarantee identical ciphertext when re-encrypted on-demand).
		if enc != nil {
			var encBuf bytes.Buffer
			if encErr := crypto.EncryptStreamWithDEKPool(bytes.NewReader(processed), &encBuf, enc.DEK, &encBufPool, uint64(chunk.Index)); encErr != nil {
				b := pooledData[:cap(pooledData)]
				chunkDataPool.Put(&b)
				return fmt.Errorf("SEED_IN_PLACE: encrypt chunk %d: %w", chunk.Index, encErr)
			}
			processed = encBuf.Bytes()
		}

		// Compute hash BEFORE returning pooled data — processed may alias the
		// pooled buffer (plaintext + no compression), so the chunker goroutine
		// could overwrite it once we return it to the pool.
		hashRaw := sha256.Sum256(processed)
		hashHex := hex.EncodeToString(hashRaw[:])

		// Return pooled data — we only needed the hash.
		b := pooledData[:cap(pooledData)]
		chunkDataPool.Put(&b)

		chunkInfos = append(chunkInfos, chunker.ChunkInfo{
			Index:      chunk.Index,
			Hash:       hex.EncodeToString(chunk.Hash[:]),
			Size:       chunk.Size,
			EncHash:    hashHex,
			Compressed: wasCompressed,
		})

		if progress != nil {
			progress(len(chunkInfos), totalChunks)
		}
	}

	if err := <-errCh; err != nil {
		return fmt.Errorf("SEED_IN_PLACE: chunker: %w", err)
	}

	// Build manifest with swarm fields.
	manifest := chunker.BuildManifest(key, chunkInfos, time.Now().UnixNano())
	manifest.SeedMode = true
	manifest.OriginalSeeder = s.effectiveSelfAddr()
	if enc != nil {
		manifest.Encrypted = true
		manifest.OwnerPubKey = enc.OwnerPubKey
		manifest.OwnerEdPubKey = enc.OwnerEdPubKey
		manifest.AccessList = enc.AccessList
		manifest.Signature = enc.Signature
	}

	// Store manifest locally.
	if err := s.serverOpts.metaData.SetManifest(key, manifest); err != nil {
		return fmt.Errorf("SEED_IN_PLACE: store manifest: %w", err)
	}

	// Update top-level file metadata.
	topFm, _ := s.serverOpts.metaData.Get(key)
	if topFm.VClock == nil {
		topFm.VClock = make(map[string]uint64)
	}
	topFm.VClock[selfAddr]++
	topFm.Chunked = true
	topFm.Timestamp = time.Now().UnixNano()
	if err := s.serverOpts.metaData.Set(key, topFm); err != nil {
		return fmt.Errorf("SEED_IN_PLACE: update file meta: %w", err)
	}

	// Replicate manifest to hash ring (fire-and-forget).
	s.replicateManifest(key, selfAddr, manifest)

	// Store local seed source (so this node can serve chunks on-demand).
	if s.StateDB != nil {
		_ = s.StateDB.SetSeedSource(key, &swarm.SeedSource{
			OriginalPath: filePath,
			ChunkSize:    chunker.DefaultChunkSize,
			TotalChunks:  len(chunkInfos),
			Bitfield:     swarm.NewFullBitfield(len(chunkInfos)),
		})
	}

	// Register as provider on hash ring nodes.
	s.registerProvider(key, len(chunkInfos), len(chunkInfos))

	elapsed := time.Since(t0)
	log.Printf("SEED_IN_PLACE: key=%q indexed %d chunks in %v (zero CAS writes)", key, len(chunkInfos), elapsed)
	return nil
}

// registerProvider sends a provider registration to hash ring nodes for fileKey.
func (s *Server) registerProvider(fileKey string, chunkCount, totalChunks int) {
	selfAddr := s.effectiveSelfAddr()
	targets := s.HashRing.GetNodes(fileKey, s.HashRing.ReplicationFactor())
	msg := &Message{Payload: &MessageRegisterProvider{
		FileKey:     fileKey,
		Provider:    selfAddr,
		ChunkCount:  chunkCount,
		TotalChunks: totalChunks,
	}}
	for _, addr := range targets {
		if normalizeAddr(addr) == normalizeAddr(selfAddr) {
			// Store locally instead of sending to self.
			if s.StateDB != nil {
				_ = s.StateDB.AddProvider(fileKey, swarm.ProviderRecord{
					Addr:         selfAddr,
					ChunkCount:   chunkCount,
					TotalChunks:  totalChunks,
					RegisteredAt: time.Now().UnixNano(),
				})
			}
			continue
		}
		_ = s.sendToAddr(addr, msg)
	}
}

// deregisterProvider removes this node from the provider list for fileKey.
func (s *Server) deregisterProvider(fileKey string) {
	selfAddr := s.effectiveSelfAddr()
	if s.StateDB != nil {
		_ = s.StateDB.RemoveProvider(fileKey, selfAddr)
	}
	// Note: remote hash ring nodes will expire the record via TTL.
}

// DirUploadProgressFunc reports directory upload progress.
// fileIdx/fileTotal track which file, chunkIdx/chunkTotal track chunks within.
type DirUploadProgressFunc func(fileIdx, fileTotal, chunkIdx, chunkTotal int)

// StoreDirectory uploads all files in dirPath as individual chunked uploads,
// then creates and stores a DirectoryManifest that binds them together.
//
// Two-phase design for performance:
//   Phase 1: Process all files (chunk, compress, encrypt, CAS store, replicate)
//            with deferred metadata — no BoltDB writes per file.
//   Phase 2: Single WithBatch call writes all metadata atomically (1 fsync
//            instead of 3N for N files).
func (s *Server) StoreDirectory(ctx context.Context, key string, dirPath string, enc *EncryptionMeta, progress DirUploadProgressFunc) error {
	entries, err := dirmanifest.Walk(dirPath)
	if err != nil {
		return fmt.Errorf("StoreDirectory: walk: %w", err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("StoreDirectory: directory is empty")
	}

	// Phase 1: process all files, collecting metadata for batch write.
	fileTotal := len(entries)
	fileResults := make([]*StoredFileResult, 0, fileTotal)

	for i := range entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		filePath := filepath.Join(dirPath, filepath.FromSlash(entries[i].RelativePath))
		fileKey := key + "/" + entries[i].RelativePath

		f, err := os.Open(filePath)
		if err != nil {
			return fmt.Errorf("StoreDirectory: open %s: %w", entries[i].RelativePath, err)
		}

		fi, err := f.Stat()
		if err != nil {
			f.Close()
			return fmt.Errorf("StoreDirectory: stat %s: %w", entries[i].RelativePath, err)
		}

		fileIdx := i
		fileProgress := func(chunkIdx, chunkTotal int) {
			if progress != nil {
				progress(fileIdx+1, fileTotal, chunkIdx, chunkTotal)
			}
		}

		result, err := s.StoreDataCollectMeta(ctx, fileKey, f, enc, fi.Size(), fileProgress)
		if err != nil {
			f.Close()
			return fmt.Errorf("StoreDirectory: upload %s: %w", entries[i].RelativePath, err)
		}
		f.Close()

		entries[i].FileHash = result.Manifest.MerkleRoot
		entries[i].ManifestKey = fileKey
		fileResults = append(fileResults, result)
	}

	// Build directory manifest.
	var totalSize int64
	for _, e := range entries {
		totalSize += e.Size
	}
	dm := &dirmanifest.DirectoryManifest{
		StorageKey:  key,
		TotalSize:   totalSize,
		FileCount:   len(entries),
		Files:       entries,
		MerkleRoot:  dirmanifest.ComputeMerkleRoot(entries),
		CreatedAt:   time.Now().UnixNano(),
		IsDirectory: true,
	}
	if enc != nil {
		dm.Encrypted = true
		dm.OwnerPubKey = enc.OwnerPubKey
		dm.OwnerEdPubKey = enc.OwnerEdPubKey
		dm.AccessList = enc.AccessList
		dm.Signature = enc.Signature
	}

	dmData, err := dirmanifest.Marshal(dm)
	if err != nil {
		return fmt.Errorf("StoreDirectory: marshal manifest: %w", err)
	}

	// Phase 2: batch-write all metadata in a single transaction (1 fsync).
	selfAddr := normalizeAddr(s.serverOpts.transport.Addr())
	topFm, _ := s.serverOpts.metaData.Get(key)
	if topFm.VClock == nil {
		topFm.VClock = make(map[string]uint64)
	}
	topFm.VClock[selfAddr]++
	topFm.Chunked = true
	topFm.Timestamp = time.Now().UnixNano()

	if err := s.serverOpts.metaData.WithBatch(func(batch storage.MetadataBatch) error {
		for _, r := range fileResults {
			for _, cm := range r.ChunkMetas {
				if err := batch.Set(cm.Key, cm.Meta); err != nil {
					return err
				}
			}
			if err := batch.SetManifest(r.FileKey, r.Manifest); err != nil {
				return err
			}
			if err := batch.Set(r.FileKey, r.FileMeta); err != nil {
				return err
			}
		}
		if err := batch.SetDirManifest(key, dmData); err != nil {
			return err
		}
		return batch.Set(key, topFm)
	}); err != nil {
		return fmt.Errorf("StoreDirectory: batch metadata write: %w", err)
	}

	log.Printf("STORE_DIRECTORY: key '%s' stored as %d files", key, len(entries))
	return nil
}

// GetDirectoryManifest retrieves and parses the DirectoryManifest for a key.
func (s *Server) GetDirectoryManifest(key string) (*dirmanifest.DirectoryManifest, error) {
	data, ok := s.serverOpts.metaData.GetDirManifest(key)
	if !ok {
		// Not local — ask peers via unified metadata fetch.
		fetchCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		peerData, isDir, err := s.fetchMetadataFromPeers(fetchCtx, key)
		if err != nil || !isDir {
			return nil, fmt.Errorf("no directory manifest for %q", key)
		}
		// Cache locally for subsequent requests.
		s.serverOpts.metaData.SetDirManifest(key, peerData)
		data = peerData
	}
	dm, err := dirmanifest.Unmarshal(data)
	if err != nil {
		return nil, fmt.Errorf("parse directory manifest: %w", err)
	}
	return dm, nil
}

// IsDirectoryManifest returns true if a directory manifest exists for the key.
func (s *Server) IsDirectoryManifest(key string) bool {
	_, ok := s.serverOpts.metaData.GetDirManifest(key)
	return ok
}

// DownloadReport summarises a directory download.
type DownloadReport struct {
	Succeeded   int
	Failed      int
	FailedFiles []string
}

// GetDirectory downloads all files from a directory manifest to outputDir
// using a global chunk work pool (BitTorrent-style). All chunks from all
// files are flattened into a single queue with N concurrent workers.
//
// Phase 1 (resume-first): scans local disk for already-complete files.
// Phase 2 (global queue): downloads remaining chunks across all files in parallel.
func (s *Server) GetDirectory(ctx context.Context, key string, outputDir string, dek []byte, progress DirUploadProgressFunc) (*DownloadReport, error) {
	dm, err := s.GetDirectoryManifest(key)
	if err != nil {
		return nil, err
	}

	files, totalChunks, alreadyDoneFiles, err := s.prepareDirectoryDownload(ctx, dm, outputDir, dek)
	if err != nil {
		cleanupFileContexts(files)
		return nil, err
	}
	defer cleanupFileContexts(files)

	log.Printf("GET_DIRECTORY: %d/%d files already complete, %d files need chunks (%d total chunks)",
		alreadyDoneFiles, dm.FileCount, len(files), totalChunks)

	if len(files) == 0 {
		report := &DownloadReport{Succeeded: alreadyDoneFiles}
		log.Printf("GET_DIRECTORY: key '%s' — all %d files already on disk", key, alreadyDoneFiles)
		return report, nil
	}

	// Adapt DirProgressFunc → DirUploadProgressFunc for IPC.
	var progressAdapter downloader.DirProgressFunc
	if progress != nil {
		progressAdapter = func(fc, ft, cc, ct int) {
			progress(alreadyDoneFiles+fc, dm.FileCount, cc, ct)
		}
	}

	result := s.Downloader.DownloadDirectory(ctx, files, 0, dek, progressAdapter)

	report := &DownloadReport{
		Succeeded:   alreadyDoneFiles + result.Succeeded,
		Failed:      result.Failed,
		FailedFiles: result.FailedFiles,
	}

	log.Printf("GET_DIRECTORY: key '%s' — %d/%d files downloaded to %s, %d failed",
		key, report.Succeeded, dm.FileCount, outputDir, report.Failed)
	return report, nil
}

// prepareDirectoryDownload scans outputDir for already-complete files, fetches
// manifests for remaining files, loads resume sidecars, and returns FileContext
// objects ready for the global chunk queue.
func (s *Server) prepareDirectoryDownload(
	ctx context.Context,
	dm *dirmanifest.DirectoryManifest,
	outputDir string,
	dek []byte,
) (files []*downloader.FileContext, totalChunks int, alreadyDoneFiles int, err error) {

	for i, entry := range dm.Files {
		if ctx.Err() != nil {
			return files, totalChunks, alreadyDoneFiles, ctx.Err()
		}

		outPath, pathErr := dirmanifest.SafeOutputPath(outputDir, entry.RelativePath)
		if pathErr != nil {
			log.Printf("GET_DIRECTORY: SKIP %s: %v", entry.RelativePath, pathErr)
			continue
		}

		if mkErr := os.MkdirAll(filepath.Dir(outPath), 0o755); mkErr != nil {
			log.Printf("GET_DIRECTORY: SKIP %s: mkdir: %v", entry.RelativePath, mkErr)
			continue
		}

		// Phase 1: Check if file is already complete on disk.
		if info, statErr := os.Stat(outPath); statErr == nil && info.Size() == entry.Size {
			// Clean up any orphaned .part / .part.resume from prior attempts.
			partPath := resume.PartPath(outPath)
			if _, pErr := os.Stat(partPath); pErr == nil {
				_ = os.Remove(partPath)
				_ = resume.Cleanup(outPath)
			}
			alreadyDoneFiles++
			continue
		}

		// Fetch this file's chunk manifest.
		manifest, manErr := s.EnsureManifest(entry.ManifestKey)
		if manErr != nil {
			log.Printf("GET_DIRECTORY: SKIP %s: manifest: %v", entry.RelativePath, manErr)
			continue
		}

		// Load existing resume sidecar (if any).
		partPath := resume.PartPath(outPath)
		skipSet := make(map[int]bool)
		var sidecar *resume.Sidecar

		existing, claimed, openErr := resume.Open(outPath)
		if openErr != nil {
			log.Printf("GET_DIRECTORY: corrupt sidecar for %q, starting fresh: %v", outPath, openErr)
			_ = resume.Cleanup(outPath)
		} else if existing != nil {
			if existing.Matches(entry.ManifestKey, manifest.MerkleRoot, len(manifest.Chunks), manifest.TotalSize) {
				skipSet = s.fastVerifyChunks(partPath, manifest, claimed)
				sidecar = existing
				log.Printf("GET_DIRECTORY: [RESUME] %s: verified %d/%d chunks", entry.RelativePath, len(skipSet), len(manifest.Chunks))
			} else {
				existing.Close()
				_ = resume.Cleanup(outPath)
				_ = os.Remove(partPath)
			}
		}

		// If all chunks are already verified, finalize immediately.
		if len(skipSet) == len(manifest.Chunks) {
			if sidecar != nil {
				sidecar.Close()
			}
			if verErr := chunker.VerifyMerkleRoot(manifest.Chunks, manifest.MerkleRoot); verErr == nil {
				_ = resume.Cleanup(outPath)
				_ = storage.SafeRename(partPath, outPath)
				alreadyDoneFiles++
				log.Printf("GET_DIRECTORY: [RESUME] %s: all chunks verified, finalized", entry.RelativePath)
				continue
			}
			// Merkle failed — re-download all chunks.
			skipSet = make(map[int]bool)
			_ = os.Remove(partPath)
		}

		// Create sidecar if we don't have one.
		if sidecar == nil {
			sidecar, err = resume.Create(outPath, entry.ManifestKey, manifest.MerkleRoot,
				len(manifest.Chunks), manifest.TotalSize, manifest.ChunkSize)
			if err != nil {
				log.Printf("GET_DIRECTORY: SKIP %s: sidecar create: %v", entry.RelativePath, err)
				continue
			}
		}

		// Open/allocate .part file.
		f, fErr := os.OpenFile(partPath, os.O_CREATE|os.O_RDWR, 0644)
		if fErr != nil {
			sidecar.Close()
			log.Printf("GET_DIRECTORY: SKIP %s: open .part: %v", entry.RelativePath, fErr)
			continue
		}
		if tErr := f.Truncate(manifest.TotalSize); tErr != nil {
			f.Close()
			sidecar.Close()
			log.Printf("GET_DIRECTORY: SKIP %s: truncate: %v", entry.RelativePath, tErr)
			continue
		}

		remaining := len(manifest.Chunks) - len(skipSet)

		// Capture variables for closures.
		outPathCopy := outPath
		manifestCopy := manifest
		sidecarCopy := sidecar
		fileCopy := f

		fc := &downloader.FileContext{
			Index:        i,
			FileKey:      entry.ManifestKey,
			RelativePath: entry.RelativePath,
			ChunkSize:    manifest.ChunkSize,
			Chunks:       manifest.Chunks,
			Writer:       f,
			SkipSet:      skipSet,
			TotalChunks:  len(manifest.Chunks),
			OnChunkDone: func(index int, hash string) {
				if err := sidecarCopy.RecordChunk(index); err != nil {
					log.Printf("GET_DIRECTORY: [RESUME] failed to record chunk %d for %s: %v", index, outPathCopy, err)
				}
			},
			OnFinalize: func() error {
				if err := chunker.VerifyMerkleRoot(manifestCopy.Chunks, manifestCopy.MerkleRoot); err != nil {
					return err
				}
				fileCopy.Close()
				sidecarCopy.Close()
				_ = resume.Cleanup(outPathCopy)
				return storage.SafeRename(resume.PartPath(outPathCopy), outPathCopy)
			},
			Cleanup: func() {
				fileCopy.Close()
				sidecarCopy.Close()
			},
		}
		fc.Remaining.Store(int32(remaining))
		fc.InitErrors(len(manifest.Chunks))

		files = append(files, fc)
		totalChunks += len(manifest.Chunks)
	}

	return files, totalChunks, alreadyDoneFiles, nil
}

// cleanupFileContexts closes any file handles that weren't finalized.
func cleanupFileContexts(files []*downloader.FileContext) {
	for _, fc := range files {
		if fc.Cleanup != nil && fc.Remaining.Load() > 0 {
			fc.Cleanup()
		}
	}
}

// replicateChunk sends storageKey's encrypted bytes to all targetNodes that
// are not selfAddr. Unreachable nodes get a hint entry.
// writeQuorum returns the number of successful replica ACKs required before
// replicateChunk may return to the caller. For n<=2 we need all replicas (no
// spare); for n>=3 we use majority (n/2 + 1) so one slow node doesn't stall
// the uploader.
func (s *Server) writeQuorum(n int) int {
	if n <= 2 {
		return n
	}
	return n/2 + 1
}

func (s *Server) replicateChunk(selfAddr, storageKey string, data []byte, targetNodes []string) {
	msg := &Message{
		Payload: &MessageStoreFile{
			Key:  storageKey,
			Size: int64(len(data)),
		},
	}
	buf := new(bytes.Buffer)
	if err := gob.NewEncoder(buf).Encode(msg); err != nil {
		log.Printf("replicateChunk: encode: %v", err)
		return
	}
	msgBytes := buf.Bytes()

	log.Printf("REPLICATE_CHUNK: key=%s selfAddr=%s targets=%v", storageKey, selfAddr, targetNodes)

	type result struct{ err error }
	needed := 0
	resultCh := make(chan result, len(targetNodes))

	// Timeout: 30s base + 1s per MB. Prevents stalled peers from freezing the pipeline.
	replicateTimeout := 30*time.Second + time.Duration(len(data)/(1024*1024))*time.Second

	for _, nodeAddr := range targetNodes {
		if nodeAddr == selfAddr {
			continue
		}
		// Use data-plane peer when available (Two-Port), fallback to control peer.
		p, ok := s.getDataPeer(nodeAddr)
		if !ok {
			log.Printf("REPLICATE_CHUNK: peer %s NOT FOUND, storing hint", nodeAddr)
			s.storeHint(nodeAddr, storageKey, int64(len(data)))
			metrics.RecordReplication("hint")
			continue
		}
		needed++
		go func(addr string, peer peer2peer.Peer) {
			done := make(chan error, 1)
			go func() { done <- peer.SendStream(msgBytes, data) }()

			var err error
			select {
			case err = <-done:
			case <-time.After(replicateTimeout):
				err = fmt.Errorf("timeout after %s", replicateTimeout)
			}

			if err != nil {
				log.Printf("REPLICATE_CHUNK: SendStream to %s failed: %v, storing hint", addr, err)
				s.storeHint(addr, storageKey, int64(len(data)))
				metrics.RecordReplication("hint")
			} else {
				log.Printf("REPLICATE_CHUNK: SendStream to %s OK for key=%s", addr, storageKey)
				metrics.RecordReplication("ok")
			}
			resultCh <- result{err}
		}(nodeAddr, p)
	}

	w := s.writeQuorum(needed)
	successes := 0
	for i := 0; i < needed; i++ {
		r := <-resultCh
		if r.err == nil {
			successes++
			if successes >= w {
				log.Printf("REPLICATE_CHUNK: quorum met (%d/%d) for key=%s", successes, needed, storageKey)
				return
			}
		}
	}

	// Quorum not met — data is stored locally, hints will retry later.
	log.Printf("REPLICATE_CHUNK: quorum NOT met (%d/%d) for key=%s, continuing with local copy + hints", successes, w, storageKey)
}

// replicateManifest sends a ChunkManifest to every ring-responsible peer for key.
// The manifest is stored only in metadata (no physical CAS file), so it must be
// propagated via a dedicated message rather than the regular stream protocol.
func (s *Server) replicateManifest(key, selfAddr string, manifest *chunker.ChunkManifest) {
	data, err := json.Marshal(manifest)
	if err != nil {
		log.Printf("replicateManifest: marshal: %v", err)
		return
	}

	msg := &Message{Payload: &MessageStoreManifest{
		FileKey:      key,
		ManifestJSON: data,
	}}

	targetNodes := s.HashRing.GetNodes(key, s.HashRing.ReplicationFactor())

	s.peerLock.RLock()
	peers := make(map[string]peer2peer.Peer, len(s.peers))
	for a, p := range s.peers {
		peers[a] = p
	}
	s.peerLock.RUnlock()

	// Manifest replication is fire-and-forget — the uploader does not need to
	// wait for peers to acknowledge storing the manifest before returning.
	for _, nodeAddr := range targetNodes {
		if nodeAddr == selfAddr {
			continue
		}
		p, ok := peers[nodeAddr]
		if !ok {
			log.Printf("replicateManifest: peer %s not connected, skipping", nodeAddr)
			continue
		}
		go func(addr string, peer peer2peer.Peer) {
			buf := new(bytes.Buffer)
			if err := gob.NewEncoder(buf).Encode(msg); err != nil {
				log.Printf("replicateManifest: encode for %s: %v", addr, err)
				return
			}
			if err := peer.SendMsg(peer2peer.IncomingMessage, buf.Bytes()); err != nil {
				log.Printf("replicateManifest: send to %s: %v", addr, err)
			}
		}(nodeAddr, p)
	}
}

// readRepair checks all responsible replicas for key and asynchronously
// repairs any that are missing it by re-sending from local storage.
func (s *Server) readRepair(key string) {
	if isSwarmCacheKey(key) {
		return // chunk keys managed by swarm/upload replication, not read-repair
	}
	// SeedMode files have no CAS entry for the file-level key — swarm handles distribution.
	if manifest, ok := s.serverOpts.metaData.GetManifest(key); ok && manifest != nil && manifest.SeedMode {
		return
	}
	if !s.Store.Has(key) {
		return // we don't have it locally, can't repair
	}

	selfAddr := s.serverOpts.transport.Addr()
	targetNodes := s.HashRing.GetNodes(key, s.HashRing.ReplicationFactor())

	for _, nodeAddr := range targetNodes {
		if nodeAddr == selfAddr {
			continue
		}

		s.peerLock.RLock()
		_, connected := s.peers[nodeAddr]
		s.peerLock.RUnlock()

		if !connected {
			continue
		}

		// Probe: send a get request and see if the peer responds within 2s.
		// If the peer already has it, it will respond; if not, we repair it.
		probeCh := make(chan struct{}, 1)
		probeKey := "__probe__" + key

		s.mu.Lock()
		s.pendingFile[probeKey] = make(chan []byte, 1)
		s.mu.Unlock()

		// Send a lightweight check — reuse MessageGetFile
		checkMsg := &Message{Payload: &MessageGetFile{Key: key}}
		if err := s.sendToAddr(nodeAddr, checkMsg); err != nil {
			s.mu.Lock()
			delete(s.pendingFile, probeKey)
			s.mu.Unlock()
			continue
		}

		// Wait briefly for a response
		go func(addr, pk string, ch chan struct{}) {
			s.mu.Lock()
			probeChan := s.pendingFile[pk]
			s.mu.Unlock()

			select {
			case r := <-probeChan:
				if r != nil {
					// Peer has the file — no repair needed
					ch <- struct{}{}
				}
			case <-time.After(2 * time.Second):
				// No response — peer is missing the file
			}

			s.mu.Lock()
			delete(s.pendingFile, pk)
			s.mu.Unlock()
		}(nodeAddr, probeKey, probeCh)

		select {
		case <-probeCh:
			// peer responded — has the file
			log.Printf("[readRepair] key=%s replica %s is healthy", key, nodeAddr)
		case <-time.After(2500 * time.Millisecond):
			// peer did not respond — repair it
			log.Printf("[readRepair] key=%s replica %s is stale, repairing", key, nodeAddr)
			data, err := s.readFile(key)
			if err != nil {
				log.Printf("[readRepair] failed to read key=%s for repair: %v", key, err)
				continue
			}
			s.storeHint(nodeAddr, key, int64(len(data)))
		}
	}
}

// handleQuorumWrite stores the incoming write locally and sends an ack back.
func (s *Server) handleQuorumWrite(from string, peer peer2peer.Peer, msg *MessageQuorumWrite) error {
	selfAddr := s.serverOpts.transport.Addr()

	// Write the data into local store via the existing WriteStream path.
	// We store the raw encrypted bytes directly (already encrypted by sender).
	_, err := s.Store.WriteStream(msg.Key, bytes.NewReader(msg.Data))
	if err == nil {
		// Merge into the FileMeta that WriteStream just wrote (which has Path set).
		fm, _ := s.serverOpts.metaData.Get(msg.Key)
		fm.VClock = msg.Clock
		fm.Timestamp = time.Now().UnixNano()
		_ = s.serverOpts.metaData.Set(msg.Key, fm)
	}

	ack := &Message{Payload: &MessageQuorumWriteAck{
		Key:     msg.Key,
		From:    selfAddr,
		Success: err == nil,
		ErrMsg: func() string {
			if err != nil {
				return err.Error()
			}
			return ""
		}(),
	}}
	// Send ack directly on the same connection — avoids peers[from] map lookup
	// which fails after handleAnnounce remaps the ephemeral key to canonical.
	buf := new(bytes.Buffer)
	if encErr := gob.NewEncoder(buf).Encode(ack); encErr == nil {
		_ = peer.SendMsg(peer2peer.IncomingMessage, buf.Bytes())
	}
	return nil
}

// handleQuorumRead responds with local metadata for the requested key.
func (s *Server) handleQuorumRead(from string, peer peer2peer.Peer, msg *MessageQuorumRead) error {
	selfAddr := s.serverOpts.transport.Addr()
	fm, found := s.serverOpts.metaData.Get(msg.Key)

	resp := &Message{Payload: &MessageQuorumReadResponse{
		Key:       msg.Key,
		From:      selfAddr,
		Found:     found,
		Clock:     fm.VClock,
		Timestamp: fm.Timestamp,
	}}
	buf := new(bytes.Buffer)
	if err := gob.NewEncoder(buf).Encode(resp); err == nil {
		_ = peer.SendMsg(peer2peer.IncomingMessage, buf.Bytes())
	}
	return nil
}

// deriveHealthPort returns the health-server listen address by adding 1000 to
// the port in listenAddr (e.g. ":3000" → ":4000", "127.0.0.1:3000" → "127.0.0.1:4000").
// Falls back to ":14000" if parsing fails.
// normalizeAddr canonicalizes loopback and unspecified addresses to
// 127.0.0.1 so that [::1]:3011, 127.0.0.1:3011, and :3011 all map to
// the same ring key. Real LAN IPs (e.g. 192.168.1.50:3011) pass through
// unchanged, so two machines on the same port never collide.
func normalizeAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	ip := net.ParseIP(host)
	if host == "" || (ip != nil && (ip.IsLoopback() || ip.IsUnspecified())) {
		return net.JoinHostPort("127.0.0.1", port)
	}
	return addr
}

// DefaultPort is the well-known default port for Hermod nodes.
const DefaultPort = "3000"

// NormalizeUserAddr ensures addr has a port. Bare IPs get DefaultPort appended.
// Used at all user-input boundaries (--peer flag, TUI connect, CLI connect).
func NormalizeUserAddr(addr string) string {
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr // already has port
	}
	if ip := net.ParseIP(addr); ip != nil {
		return net.JoinHostPort(addr, DefaultPort)
	}
	return addr // hostname or alias — return as-is
}

// isLocalAddr returns true if host matches one of this machine's own network
// interface addresses. Used by handleAnnounce to reject self-connections that
// arrive when gossip causes a node to dial its own alternate IP.
func isLocalAddr(host string) bool {
	ifaces, err := net.Interfaces()
	if err != nil {
		return false
	}
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip != nil && ip.String() == host {
				return true
			}
		}
	}
	return false
}

func deriveHealthPort(listenAddr string) string {
	host, portStr, err := strings.Cut(listenAddr, ":")
	if !err {
		return ":14000"
	}
	port := 0
	for _, ch := range portStr {
		if ch < '0' || ch > '9' {
			return ":14000"
		}
		port = port*10 + int(ch-'0')
	}
	healthPort := port + 1000
	if host == "" {
		return fmt.Sprintf(":%d", healthPort)
	}
	return fmt.Sprintf("%s:%d", host, healthPort)
}

// isSwarmCacheKey returns true if key is a CAS-cached chunk that should be
// excluded from background subsystem operations (anti-entropy, rebalancer,
// read-repair). These chunk: keys are either already replicated during upload
// (replicateChunk handles this) or cached from a swarm download (served
// on-demand via handleGetChunkSwarm, never pushed by background subsystems).
func isSwarmCacheKey(key string) bool {
	return strings.HasPrefix(key, "chunk:")
}

// storeHint saves a hinted handoff entry for a key that could not be delivered.
// Data is NOT stored in the hint — only the key and size. Data lives in CAS and
// is read on delivery, preventing multi-GB memory bloat from hint accumulation.
func (s *Server) storeHint(targetAddr, key string, dataSize int64) {
	if s.HandoffSvc == nil {
		return
	}
	s.HandoffSvc.StoreHint(handoff.Hint{
		Key:        key,
		TargetAddr: targetAddr,
		DataSize:   dataSize,
		CreatedAt:  time.Now(),
	})
}

// readFile reads the raw bytes for key from local storage.
// Used by the Rebalancer's ReadFileFunc closure.
func (s *Server) readFile(key string) (data []byte, err error) {
	_, r, err := s.Store.ReadStream(key)
	if err != nil {
		return nil, fmt.Errorf("readFile: open %s: %w", key, err)
	}
	defer r.Close()

	data, err = io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("readFile: read %s: %w", key, err)
	}

	return data, nil
}

// HealthStatus returns the current health snapshot for /health endpoint.
func (s *Server) HealthStatus() health.Status {
	ringSize := s.HashRing.Size()
	// Ring size includes self, so peers = ringSize - 1.
	// This counts both outbound and announced-inbound peers correctly.
	peerCount := ringSize - 1
	if peerCount < 0 {
		peerCount = 0
	}

	status := "ok"
	if peerCount == 0 {
		status = "degraded" // isolated single node
	}

	uptime := time.Since(s.startedAt).Truncate(time.Second).String()
	return health.Status{
		Status:    status,
		NodeAddr:  s.serverOpts.transport.Addr(),
		PeerCount: peerCount,
		RingSize:  ringSize,
		Uptime:    uptime,
		StartedAt: s.startedAt.UTC().Format(time.RFC3339),
	}
}

// InspectManifest returns the ChunkManifest stored for key, or nil if not found.
// Intended for use in integration and E2E tests.
func (s *Server) InspectManifest(key string) (*chunker.ChunkManifest, bool) {
	// Try local first.
	if m, ok := s.serverOpts.metaData.GetManifest(key); ok {
		return m, true
	}
	// Not local — try peers (manifest may be on a different replica).
	m, err := s.fetchManifestFromPeers(key)
	if err != nil || m == nil {
		return nil, false
	}
	// Cache locally so subsequent requests are fast.
	s.serverOpts.metaData.SetManifest(key, m)
	return m, true
}

// AliasResult holds the resolved identity for a single node matching an alias.
type AliasResult struct {
	Fingerprint   string
	X25519PubHex  string
	Ed25519PubHex string
	NodeAddr      string
}

// LookupAlias searches cluster gossip metadata for nodes with the given alias.
// Returns all matches (may be >1 if multiple nodes share the same alias).
func (s *Server) LookupAlias(alias string) []AliasResult {
	seen := make(map[string]bool) // dedup by fingerprint
	var results []AliasResult

	// Check local identity first — always available regardless of gossip state.
	if s.identityMeta != nil && s.identityMeta["alias"] == alias {
		fp := s.identityMeta["fingerprint"]
		seen[fp] = true
		results = append(results, AliasResult{
			Fingerprint:   fp,
			X25519PubHex:  s.identityMeta["x25519_pub"],
			Ed25519PubHex: s.identityMeta["ed25519_pub"],
			NodeAddr:      normalizeAddr(s.serverOpts.transport.Addr()),
		})
	}

	// Check cluster gossip state for remote nodes.
	for _, node := range s.Cluster.AllNodes() {
		if node.Metadata["alias"] == alias {
			fp := node.Metadata["fingerprint"]
			if seen[fp] {
				continue
			}
			seen[fp] = true
			results = append(results, AliasResult{
				Fingerprint:   fp,
				X25519PubHex:  node.Metadata["x25519_pub"],
				Ed25519PubHex: node.Metadata["ed25519_pub"],
				NodeAddr:      node.Addr,
			})
		}
	}
	return results
}

// InspectMeta returns the vector clock and timestamp stored for key.
// It is intended for use in integration and E2E tests.
func (s *Server) InspectMeta(key string) (VClock map[string]uint64, Timestamp int64, found bool) {
	fm, ok := s.serverOpts.metaData.Get(key)
	if !ok {
		return nil, 0, false
	}
	// Return a copy so callers don't alias the live metadata map.
	vcCopy := make(map[string]uint64, len(fm.VClock))
	for k, v := range fm.VClock {
		vcCopy[k] = v
	}
	return vcCopy, fm.Timestamp, true
}

// handleGetPublicCatalog responds to a remote peer requesting our public file catalog.
func (s *Server) handleGetPublicCatalog(from string) error {
	var catalogJSON []byte
	if s.StateDB != nil {
		files, err := s.StateDB.ListPublicFiles()
		if err != nil {
			log.Printf("[public-catalog] error listing public files: %v", err)
			catalogJSON = []byte("[]")
		} else {
			catalogJSON, _ = json.Marshal(files)
		}
	} else {
		catalogJSON = []byte("[]")
	}

	resp := &Message{Payload: &MessagePublicCatalogResponse{CatalogJSON: catalogJSON}}
	return s.sendToAddr(from, resp)
}

// GetPublicCatalog fetches the public file catalog from a remote peer.
func (s *Server) GetPublicCatalog(peerAddr string) ([]State.PublicFileEntry, error) {
	ch := make(chan []byte, 1)
	catalogKey := peerAddr + "#catalog"

	s.mu.Lock()
	s.pendingFile[catalogKey] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pendingFile, catalogKey)
		s.mu.Unlock()
	}()

	msg := &Message{Payload: &MessageGetPublicCatalog{}}
	if err := s.sendToAddr(peerAddr, msg); err != nil {
		return nil, fmt.Errorf("failed to send catalog request to %s: %w", peerAddr, err)
	}

	select {
	case data := <-ch:
		var entries []State.PublicFileEntry
		if err := json.Unmarshal(data, &entries); err != nil {
			return nil, fmt.Errorf("invalid catalog response: %w", err)
		}
		return entries, nil
	case <-time.After(10 * time.Second):
		return nil, fmt.Errorf("timeout waiting for catalog from %s", peerAddr)
	}
}

// UpdatePublicCatalogMetadata updates gossip metadata with the current public catalog summary.
func (s *Server) UpdatePublicCatalogMetadata() {
	if s.StateDB == nil || s.GossipSvc == nil {
		return
	}
	count, size, hash := s.StateDB.PublicCatalogSummary()
	selfAddr := s.effectiveSelfAddr()
	if s.Cluster != nil {
		s.Cluster.SetMetadata(selfAddr, map[string]string{
			"public_catalog_hash":  hash,
			"public_shared_count":  fmt.Sprintf("%d", count),
			"public_shared_size":   fmt.Sprintf("%d", size),
		})
	}
}

// ── Direct Share / Inbox ─────────────────────────────────────────────

// handleDirectShare processes an incoming direct-share notification from a peer.
// It stores the entry in our inbox so the user can see and download it.
func (s *Server) handleDirectShare(from string, m *MessageDirectShare) error {
	if s.StateDB == nil {
		log.Printf("[direct-share] no StateDB, ignoring share from %s", from)
		return nil
	}
	entry := State.InboxEntry{
		Key:         m.Key,
		Name:        m.Name,
		Size:        m.Size,
		IsDir:       m.IsDir,
		SenderAlias: m.SenderAlias,
		SenderFP:    m.SenderFP,
	}
	if err := s.StateDB.AddInboxEntry(entry); err != nil {
		log.Printf("[direct-share] failed to record inbox entry: %v", err)
		return err
	}
	log.Printf("[direct-share] received %q from %s (%s)", m.Name, m.SenderAlias, from)
	return nil
}

// ── Swarm message handlers ────────────────────────────────────────────

// chunkServeSem limits concurrent outbound chunk sends. Without this,
// N simultaneous GetChunkSwarm requests spawn N goroutines that all call
// peer.SendStream (each writing 4 MiB). With N > ~30 the QUIC connection
// receive window (128 MiB) fills up, OpenStreamSync blocks for 30s, and
// the downloader times out waiting for responses. Capping at 16 matches
// the receiver's MaxParallel (8) with room for pipeline overlap.
var chunkServeSem = make(chan struct{}, 8)

// handleGetChunkSwarm serves a chunk from the original file (seed in place)
// or from CAS cache (secondary seeder).
//
// Original seeder path: read plaintext from file → compress → encrypt → send.
// CAS-backed seeder path: read from CAS store (already encrypted) → send.
func (s *Server) handleGetChunkSwarm(from string, peer peer2peer.Peer, msg *MessageGetChunkSwarm) error {
	chunkServeSem <- struct{}{}        // acquire slot
	defer func() { <-chunkServeSem }() // release slot

	// Try CAS cache first (secondary seeder — already has encrypted bytes).
	storageKey := chunker.ChunkStorageKey(msg.EncHash)
	if s.Store.Has(storageKey) {
		return s.handleGetMessage(from, peer, &MessageGetFile{Key: storageKey})
	}

	// Original seeder path: read from the original file and process on-demand.
	if s.StateDB == nil {
		return fmt.Errorf("SWARM_GET: no StateDB")
	}
	seedSrc := s.StateDB.GetSeedSource(msg.FileKey)
	if seedSrc == nil {
		return fmt.Errorf("SWARM_GET: no seed source for %s", msg.FileKey)
	}
	if seedSrc.OriginalPath == "" {
		return fmt.Errorf("SWARM_GET: no original path for %s (CAS-only seeder with missing CAS data)", msg.FileKey)
	}
	if msg.ChunkIndex < 0 || msg.ChunkIndex >= seedSrc.TotalChunks {
		return fmt.Errorf("SWARM_GET: chunk index %d out of range [0, %d)", msg.ChunkIndex, seedSrc.TotalChunks)
	}

	// Get the manifest to know compression/encryption state per chunk.
	manifest, ok := s.serverOpts.metaData.GetManifest(msg.FileKey)
	if !ok || manifest == nil {
		return fmt.Errorf("SWARM_GET: manifest not found for %s", msg.FileKey)
	}
	if msg.ChunkIndex >= len(manifest.Chunks) {
		return fmt.Errorf("SWARM_GET: chunk index %d beyond manifest (%d chunks)", msg.ChunkIndex, len(manifest.Chunks))
	}
	chunkMeta := manifest.Chunks[msg.ChunkIndex]

	// Open file and seek to the chunk offset.
	f, err := os.Open(seedSrc.OriginalPath)
	if err != nil {
		// File moved/deleted — remove seed source so we stop claiming we have it.
		log.Printf("[swarm] original file gone for %s: %v — removing seed source", msg.FileKey, err)
		_ = s.StateDB.RemoveSeedSource(msg.FileKey)
		s.deregisterProvider(msg.FileKey)
		return fmt.Errorf("SWARM_GET: original file: %w", err)
	}
	defer f.Close()

	chunkSize := int64(seedSrc.ChunkSize)
	offset := int64(msg.ChunkIndex) * chunkSize
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return fmt.Errorf("SWARM_GET: seek: %w", err)
	}

	// Read exactly this chunk's raw bytes.
	raw := make([]byte, chunkMeta.Size)
	if _, err := io.ReadFull(f, raw); err != nil {
		return fmt.Errorf("SWARM_GET: read chunk %d: %w", msg.ChunkIndex, err)
	}

	// Process: compress if needed, then encrypt if ECDH.
	processed := raw
	if chunkMeta.Compressed {
		c, wc, compErr := compression.CompressChunkWithPool(raw, compression.LevelFastest, &compBufPool)
		if compErr != nil {
			return fmt.Errorf("SWARM_GET: compress chunk %d: %w", msg.ChunkIndex, compErr)
		}
		if wc {
			processed = c
		}
	}

	if manifest.Encrypted {
		dek, err := s.getDEK(msg.FileKey, manifest)
		if err != nil {
			return fmt.Errorf("SWARM_GET: get DEK for %s: %w", msg.FileKey, err)
		}
		var encBuf bytes.Buffer
		if err := crypto.EncryptStreamWithDEKPool(bytes.NewReader(processed), &encBuf, dek, &encBufPool, uint64(msg.ChunkIndex)); err != nil {
			return fmt.Errorf("SWARM_GET: encrypt chunk %d: %w", msg.ChunkIndex, err)
		}
		processed = encBuf.Bytes()
	}

	// Verify EncHash matches expected (sanity check for deterministic crypto).
	hashRaw := sha256.Sum256(processed)
	hashHex := hex.EncodeToString(hashRaw[:])
	if hashHex != msg.EncHash {
		log.Printf("[swarm] hash mismatch for %s chunk %d: got %s want %s",
			msg.FileKey, msg.ChunkIndex, hashHex, msg.EncHash)
	}

	// Send as MessageLocalFile — reuses existing download pipeline on receiver side.
	resp := &Message{
		Payload: MessageLocalFile{
			Key:  storageKey,
			Size: int64(len(processed)),
		},
	}
	buf := new(bytes.Buffer)
	if err := gob.NewEncoder(buf).Encode(resp); err != nil {
		return fmt.Errorf("handleGetChunkSwarm: encode response: %w", err)
	}

	return peer.SendStream(buf.Bytes(), processed)
}

// handleGetChunkSwarmBidi serves a chunk via bidirectional RPC — the response
// is written back to the same stream the request arrived on (w).
// Wire format: [1B status: 0x00=ok, 0x01=error][4B big-endian size][data or error msg]
func (s *Server) handleGetChunkSwarmBidi(from string, msg *MessageGetChunkSwarm, w io.Writer, streamWg *sync.WaitGroup) {
	defer streamWg.Done()

	// Acquire semaphore with timeout — if all 8 slots are held by goroutines
	// stuck on dead streams, new requests bail after 30s instead of blocking forever.
	select {
	case chunkServeSem <- struct{}{}:
	case <-time.After(30 * time.Second):
		log.Printf("[BIDI-SERVE] chunk %d for %s: sem timeout (all slots busy), dropping", msg.ChunkIndex, msg.FileKey)
		hdr := make([]byte, 5)
		hdr[0] = 0x01
		errMsg := "server busy"
		binary.BigEndian.PutUint32(hdr[1:5], uint32(len(errMsg)))
		w.Write(hdr)
		w.Write([]byte(errMsg))
		return
	}
	defer func() { <-chunkServeSem }()

	// Write deadline AFTER acquiring semaphore so the full 60s is available
	// for actual I/O. Prevents w.Write() from blocking forever when the
	// downloader times out and closes its end of the stream.
	type deadliner interface{ SetWriteDeadline(time.Time) error }
	if dl, ok := w.(deadliner); ok {
		dl.SetWriteDeadline(time.Now().Add(60 * time.Second))
	}

	writeError := func(errMsg string) {
		log.Printf("[BIDI-SERVE] chunk %d ERROR: %s", msg.ChunkIndex, errMsg)
		hdr := make([]byte, 5)
		hdr[0] = 0x01 // error status
		binary.BigEndian.PutUint32(hdr[1:5], uint32(len(errMsg)))
		w.Write(hdr)
		w.Write([]byte(errMsg))
	}

	// Try CAS cache first (secondary seeder — already has encrypted bytes).
	storageKey := chunker.ChunkStorageKey(msg.EncHash)
	if s.Store.Has(storageKey) {
		_, r, err := s.Store.ReadStream(storageKey)
		if err == nil {
			data, err := io.ReadAll(r)
			r.Close()
			if err == nil {
				hdr := make([]byte, 5)
				hdr[0] = 0x00 // success
				binary.BigEndian.PutUint32(hdr[1:5], uint32(len(data)))
				w.Write(hdr)
				w.Write(data)
				return
			}
		}
	}

	// Original seeder path: read from the original file and process on-demand.
	if s.StateDB == nil {
		writeError("no StateDB")
		return
	}
	seedSrc := s.StateDB.GetSeedSource(msg.FileKey)
	if seedSrc == nil {
		writeError(fmt.Sprintf("no seed source for %s", msg.FileKey))
		return
	}
	if seedSrc.OriginalPath == "" {
		writeError("no original path (CAS-only seeder with missing CAS data)")
		return
	}
	if msg.ChunkIndex < 0 || msg.ChunkIndex >= seedSrc.TotalChunks {
		writeError(fmt.Sprintf("chunk index %d out of range [0, %d)", msg.ChunkIndex, seedSrc.TotalChunks))
		return
	}

	manifest, ok := s.serverOpts.metaData.GetManifest(msg.FileKey)
	if !ok || manifest == nil {
		writeError(fmt.Sprintf("manifest not found for %s", msg.FileKey))
		return
	}
	if msg.ChunkIndex >= len(manifest.Chunks) {
		writeError(fmt.Sprintf("chunk index %d beyond manifest (%d chunks)", msg.ChunkIndex, len(manifest.Chunks)))
		return
	}
	chunkMeta := manifest.Chunks[msg.ChunkIndex]

	f, err := os.Open(seedSrc.OriginalPath)
	if err != nil {
		log.Printf("[BIDI-SERVE] original file gone for %s: %v — removing seed source", msg.FileKey, err)
		_ = s.StateDB.RemoveSeedSource(msg.FileKey)
		s.deregisterProvider(msg.FileKey)
		writeError(fmt.Sprintf("original file: %v", err))
		return
	}
	defer f.Close()

	chunkSize := int64(seedSrc.ChunkSize)
	offset := int64(msg.ChunkIndex) * chunkSize
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		writeError(fmt.Sprintf("seek: %v", err))
		return
	}

	raw := make([]byte, chunkMeta.Size)
	if _, err := io.ReadFull(f, raw); err != nil {
		writeError(fmt.Sprintf("read chunk %d: %v", msg.ChunkIndex, err))
		return
	}

	// Process: compress if needed, then encrypt if ECDH.
	processed := raw
	if chunkMeta.Compressed {
		c, wc, compErr := compression.CompressChunkWithPool(raw, compression.LevelFastest, &compBufPool)
		if compErr != nil {
			writeError(fmt.Sprintf("compress chunk %d: %v", msg.ChunkIndex, compErr))
			return
		}
		if wc {
			processed = c
		}
	}

	if manifest.Encrypted {
		dek, err := s.getDEK(msg.FileKey, manifest)
		if err != nil {
			writeError(fmt.Sprintf("get DEK: %v", err))
			return
		}
		var encBuf bytes.Buffer
		if err := crypto.EncryptStreamWithDEKPool(bytes.NewReader(processed), &encBuf, dek, &encBufPool, uint64(msg.ChunkIndex)); err != nil {
			writeError(fmt.Sprintf("encrypt chunk %d: %v", msg.ChunkIndex, err))
			return
		}
		processed = encBuf.Bytes()
	}

	// Verify EncHash matches expected (sanity check for deterministic crypto).
	hashRaw := sha256.Sum256(processed)
	hashHex := hex.EncodeToString(hashRaw[:])
	if hashHex != msg.EncHash {
		log.Printf("[BIDI-SERVE] WARNING hash mismatch for %s chunk %d: got %s want %s",
			msg.FileKey, msg.ChunkIndex, hashHex, msg.EncHash)
	}

	// Write success response: [0x00][4B size][data]
	hdr := make([]byte, 5)
	hdr[0] = 0x00
	binary.BigEndian.PutUint32(hdr[1:5], uint32(len(processed)))
	w.Write(hdr)
	w.Write(processed)
}

// getDEK retrieves the DEK for a seed-mode ECDH file, using a cache to avoid
// repeated unwrapping. The DEK is unwrapped from the manifest's AccessList
// using this node's X25519 private key.
func (s *Server) getDEK(fileKey string, manifest *chunker.ChunkManifest) ([]byte, error) {
	if v, ok := s.dekCache.Load(fileKey); ok {
		return v.([]byte), nil
	}

	if len(s.x25519Priv) == 0 {
		return nil, fmt.Errorf("no X25519 private key — cannot unwrap DEK")
	}

	selfPubHex := ""
	if s.identityMeta != nil {
		selfPubHex = s.identityMeta["x25519_pub"]
	}
	if selfPubHex == "" {
		return nil, fmt.Errorf("no X25519 public key in identity metadata")
	}

	// Find our entry in the access list and unwrap the DEK.
	for _, entry := range manifest.AccessList {
		if entry.RecipientPubKey == selfPubHex {
			dek, err := envelope.UnwrapDEK(s.x25519Priv, manifest.OwnerPubKey, entry.WrappedDEK)
			if err != nil {
				return nil, fmt.Errorf("unwrap DEK: %w", err)
			}
			s.dekCache.Store(fileKey, dek)
			return dek, nil
		}
	}

	return nil, fmt.Errorf("this node is not in the access list for %s", fileKey)
}

// fetchChunkSwarm sends a MessageGetChunkSwarm to peerAddr and waits for the
// response on the pendingFile channel. The response arrives as MessageLocalFile
// (same as hash-ring chunk fetches), so the existing handleLocalMessage path
// stores it in CAS and delivers via pendingFile.
func (s *Server) fetchChunkSwarm(ctx context.Context, fileKey string, chunkIndex int, encHash, peerAddr string) ([]byte, error) {
	storageKey := chunker.ChunkStorageKey(encHash)

	// Set up pending channel for the response.
	s.mu.Lock()
	ch, alreadyFetching := s.pendingFile[storageKey]
	if !alreadyFetching {
		ch = make(chan []byte, 1)
		s.pendingFile[storageKey] = ch
	}
	s.mu.Unlock()

	if alreadyFetching {
		select {
		case data := <-ch:
			if data == nil {
				return nil, fmt.Errorf("fetchChunkSwarm: nil data for %s", storageKey)
			}
			select {
			case ch <- data:
			default:
			}
			return data, nil
		case <-ctx.Done():
			return nil, fmt.Errorf("fetchChunkSwarm: timeout for %s (already fetching)", storageKey)
		}
	}

	defer func() {
		s.mu.Lock()
		delete(s.pendingFile, storageKey)
		s.mu.Unlock()
	}()

	msg := &Message{Payload: &MessageGetChunkSwarm{
		FileKey:    fileKey,
		ChunkIndex: chunkIndex,
		EncHash:    encHash,
	}}
	buf := new(bytes.Buffer)
	if err := gob.NewEncoder(buf).Encode(msg); err != nil {
		return nil, err
	}

	// Use data-plane peer when available (Two-Port), fallback to control peer.
	peer, ok := s.getDataPeer(peerAddr)
	if !ok {
		return nil, fmt.Errorf("fetchChunkSwarm: peer %s not connected", peerAddr)
	}
	if err := peer.SendMsg(peer2peer.IncomingMessage, buf.Bytes()); err != nil {
		return nil, fmt.Errorf("fetchChunkSwarm: send: %w", err)
	}

	select {
	case data := <-ch:
		if data == nil {
			return nil, fmt.Errorf("fetchChunkSwarm: nil data for %s", storageKey)
		}
		return data, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("fetchChunkSwarm: timeout for %s", storageKey)
	}
}

// fetchChunkSwarmBidi sends a GetChunkSwarm request via bidirectional RPC and
// reads the response on the same stream. No pendingFile map, no second stream.
// Falls back to fetchChunkSwarm if the peer doesn't support OpenBidiRPC.
func (s *Server) fetchChunkSwarmBidi(ctx context.Context, fileKey string, chunkIndex int, encHash, peerAddr string) ([]byte, error) {
	peer, ok := s.getDataPeer(peerAddr)
	if !ok {
		return nil, fmt.Errorf("fetchChunkSwarmBidi: peer %s not connected", peerAddr)
	}

	// Type-assert to bidi-capable peer.
	type bidiRPCer interface {
		OpenBidiRPC([]byte) (io.ReadCloser, error)
	}
	bidi, ok := peer.(bidiRPCer)
	if !ok {
		return s.fetchChunkSwarm(ctx, fileKey, chunkIndex, encHash, peerAddr) // fallback
	}

	// Encode request — gob-encoded Message, matching t.decoder.Decode() on receiver.
	msg := &Message{Payload: &MessageGetChunkSwarm{FileKey: fileKey, ChunkIndex: chunkIndex, EncHash: encHash}}
	buf := new(bytes.Buffer)
	if err := gob.NewEncoder(buf).Encode(msg); err != nil {
		return nil, err
	}

	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		stream, err := bidi.OpenBidiRPC(buf.Bytes())
		if err != nil {
			ch <- result{nil, fmt.Errorf("fetchChunkSwarmBidi: open stream: %w", err)}
			return
		}
		defer stream.Close()

		// Read response: [1B status][4B size][data or error]
		hdr := make([]byte, 5)
		if _, err := io.ReadFull(stream, hdr); err != nil {
			ch <- result{nil, fmt.Errorf("fetchChunkSwarmBidi: read header: %w", err)}
			return
		}
		status := hdr[0]
		size := binary.BigEndian.Uint32(hdr[1:5])

		// Fork BEFORE allocating 4MB: error messages are tiny strings.
		if status != 0 {
			errData := make([]byte, size)
			io.ReadFull(stream, errData)
			ch <- result{nil, fmt.Errorf("fetchChunkSwarmBidi: remote error: %s", string(errData))}
			return
		}

		// Success: allocate pooled buffer for chunk data.
		data := getChunkBuf(int(size))
		if _, err := io.ReadFull(stream, data); err != nil {
			ch <- result{nil, fmt.Errorf("fetchChunkSwarmBidi: read data: %w", err)}
			return
		}
		ch <- result{data, nil}
	}()

	select {
	case res := <-ch:
		return res.data, res.err
	case <-ctx.Done():
		return nil, fmt.Errorf("fetchChunkSwarmBidi: timeout for chunk %d", chunkIndex)
	}
}

// handleRegisterProvider stores a provider record from a seeder.
func (s *Server) handleRegisterProvider(from string, msg *MessageRegisterProvider) error {
	if s.StateDB == nil {
		return nil
	}
	rec := swarm.ProviderRecord{
		Addr:         msg.Provider,
		ChunkCount:   msg.ChunkCount,
		TotalChunks:  msg.TotalChunks,
		RegisteredAt: time.Now().UnixNano(),
	}
	if err := s.StateDB.AddProvider(msg.FileKey, rec); err != nil {
		log.Printf("SWARM_PROVIDER: failed to store provider %s for %s: %v", msg.Provider, msg.FileKey, err)
		return err
	}
	log.Printf("SWARM_PROVIDER: registered %s as provider for %s (%d/%d chunks)", msg.Provider, msg.FileKey, msg.ChunkCount, msg.TotalChunks)
	return nil
}

// handleGetProviders responds with the provider list for a file key.
func (s *Server) handleGetProviders(from string, peer peer2peer.Peer, msg *MessageGetProviders) error {
	var providers []ProviderInfo
	if s.StateDB != nil {
		for _, p := range s.StateDB.GetProviders(msg.FileKey) {
			providers = append(providers, ProviderInfo{
				Addr:        p.Addr,
				ChunkCount:  p.ChunkCount,
				TotalChunks: p.TotalChunks,
			})
		}
	}
	resp := &Message{Payload: &MessageProvidersResponse{
		FileKey:   msg.FileKey,
		Providers: providers,
	}}
	buf := new(bytes.Buffer)
	if err := gob.NewEncoder(buf).Encode(resp); err != nil {
		return err
	}
	return peer.SendMsg(peer2peer.IncomingMessage, buf.Bytes())
}

// handleProvidersResponse delivers provider query results to a waiting caller.
func (s *Server) handleProvidersResponse(from string, msg *MessageProvidersResponse) error {
	pendingKey := "__providers__" + msg.FileKey
	s.mu.Lock()
	ch, ok := s.pendingFile[pendingKey]
	s.mu.Unlock()
	if ok {
		// Encode as JSON so the receiver can unmarshal.
		data, err := json.Marshal(msg.Providers)
		if err != nil {
			return err
		}
		select {
		case ch <- data:
		default:
			log.Printf("SWARM_PROVIDER: late response for %s (receiver timed out)", msg.FileKey)
		}
	}
	return nil
}

// getProviders queries hash ring nodes for the seeder list of a file key.
// Returns provider records from the first responding node.
func (s *Server) getProviders(ctx context.Context, fileKey string) []swarm.ProviderRecord {
	// Check local first.
	if s.StateDB != nil {
		local := s.StateDB.GetProviders(fileKey)
		if len(local) > 0 {
			return local
		}
	}

	targets := s.HashRing.GetNodes(fileKey, s.HashRing.ReplicationFactor())
	selfAddr := s.effectiveSelfAddr()

	pendingKey := "__providers__" + fileKey
	ch := make(chan []byte, len(targets))
	s.mu.Lock()
	s.pendingFile[pendingKey] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pendingFile, pendingKey)
		s.mu.Unlock()
	}()

	getMsg := &Message{Payload: &MessageGetProviders{FileKey: fileKey}}
	buf := new(bytes.Buffer)
	if err := gob.NewEncoder(buf).Encode(getMsg); err != nil {
		return nil
	}
	msgBytes := buf.Bytes()

	sent := 0
	s.peerLock.RLock()
	for _, addr := range targets {
		if normalizeAddr(addr) == normalizeAddr(selfAddr) {
			continue
		}
		peer, ok := s.peers[addr]
		if !ok {
			continue
		}
		if err := peer.SendMsg(peer2peer.IncomingMessage, msgBytes); err != nil {
			continue
		}
		sent++
	}
	s.peerLock.RUnlock()

	if sent == 0 {
		return nil
	}

	// Wait for first response.
	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	select {
	case data := <-ch:
		var providers []ProviderInfo
		if err := json.Unmarshal(data, &providers); err != nil {
			return nil
		}
		// Convert to swarm.ProviderRecord.
		result := make([]swarm.ProviderRecord, len(providers))
		for i, p := range providers {
			result[i] = swarm.ProviderRecord{
				Addr:        p.Addr,
				ChunkCount:  p.ChunkCount,
				TotalChunks: p.TotalChunks,
			}
		}
		return result
	case <-queryCtx.Done():
		return nil
	}
}

// AddAccessEntry appends a new AccessEntry to the manifest for the given key
// and replicates the updated manifest to peers. Handles both file manifests
// (ChunkManifest) and directory manifests (DirectoryManifest + per-file manifests).
// If the recipient is already in the access list, this is a no-op.
// Returns (encrypted bool, err error) — encrypted=false means the file is plaintext
// and no access grant is needed.
func (s *Server) AddAccessEntry(fileKey string, isDir bool, entry chunker.AccessEntry, newSignature string) (encrypted bool, err error) {
	selfAddr := normalizeAddr(s.serverOpts.transport.Addr())

	if isDir {
		dm, dmErr := s.GetDirectoryManifest(fileKey)
		if dmErr != nil {
			return false, fmt.Errorf("get directory manifest: %w", dmErr)
		}
		if !dm.Encrypted {
			return false, nil // plaintext — no access grant needed
		}
		// Check if already in access list.
		for _, ae := range dm.AccessList {
			if ae.RecipientPubKey == entry.RecipientPubKey {
				return true, nil // already has access
			}
		}
		// Append to directory manifest.
		dm.AccessList = append(dm.AccessList, entry)
		dm.Signature = newSignature
		dmData, mErr := dirmanifest.Marshal(dm)
		if mErr != nil {
			return true, fmt.Errorf("marshal dir manifest: %w", mErr)
		}
		if err := s.serverOpts.metaData.SetDirManifest(fileKey, dmData); err != nil {
			return true, fmt.Errorf("save dir manifest: %w", err)
		}

		// Update each file's ChunkManifest within the directory.
		for _, f := range dm.Files {
			fm, ok := s.InspectManifest(f.ManifestKey)
			if !ok || fm == nil || !fm.Encrypted {
				continue
			}
			fm.AccessList = append(fm.AccessList, entry)
			fm.Signature = newSignature
			if err := s.serverOpts.metaData.SetManifest(f.ManifestKey, fm); err != nil {
				log.Printf("[grant-access] failed to update file manifest %s: %v", f.ManifestKey, err)
				continue
			}
			s.replicateManifest(f.ManifestKey, selfAddr, fm)
		}
		return true, nil
	}

	// File manifest case.
	manifest, ok := s.InspectManifest(fileKey)
	if !ok || manifest == nil {
		return false, fmt.Errorf("no manifest found for %q", fileKey)
	}
	if !manifest.Encrypted {
		return false, nil
	}
	// Check if already in access list.
	for _, ae := range manifest.AccessList {
		if ae.RecipientPubKey == entry.RecipientPubKey {
			return true, nil
		}
	}
	manifest.AccessList = append(manifest.AccessList, entry)
	manifest.Signature = newSignature
	if err := s.serverOpts.metaData.SetManifest(fileKey, manifest); err != nil {
		return true, fmt.Errorf("save manifest: %w", err)
	}
	s.replicateManifest(fileKey, selfAddr, manifest)
	return true, nil
}

// SendDirectShare notifies a peer that a file has been sent to them.
// The file must already be uploaded and replicated in the cluster.
func (s *Server) SendDirectShare(peerAddr string, key, name string, size int64, isDir bool) error {
	alias := ""
	fp := ""
	if s.identityMeta != nil {
		alias = s.identityMeta["alias"]
		fp = s.identityMeta["fingerprint"]
	}
	msg := &Message{Payload: &MessageDirectShare{
		Key:         key,
		Name:        name,
		Size:        size,
		IsDir:       isDir,
		SenderAlias: alias,
		SenderFP:    fp,
	}}
	return s.sendToAddr(peerAddr, msg)
}

// SendOrQueueDirectShare attempts to send a direct-share notification to a
// peer. If the peer is offline, the notification is queued in the local outbox
// and will be delivered automatically when the peer reconnects.
// Returns queued=true if the message was queued instead of delivered.
func (s *Server) SendOrQueueDirectShare(peerAddr, recipientFP string, key, name string, size int64, isDir bool) (queued bool, err error) {
	err = s.SendDirectShare(peerAddr, key, name, size, isDir)
	if err == nil {
		return false, nil
	}
	// Peer offline — queue for later delivery.
	if s.StateDB == nil {
		return false, err
	}
	if recipientFP == "" {
		return false, err // can't queue without fingerprint
	}
	qErr := s.QueueOutbox(recipientFP, peerAddr, key, name, size, isDir)
	if qErr != nil {
		return false, fmt.Errorf("send failed (%v) and queue failed (%v)", err, qErr)
	}
	return true, nil
}

// QueueOutbox saves a direct-share notification to the local outbox for
// delivery when the recipient peer reconnects.
func (s *Server) QueueOutbox(recipientFP, recipientHint, fileKey, fileName string, fileSize int64, isDir bool) error {
	if s.StateDB == nil {
		return fmt.Errorf("state database not available")
	}
	entry := State.OutboxEntry{
		ID:            fmt.Sprintf("%s/%s/%d", recipientFP, fileKey, time.Now().UnixNano()),
		RecipientFP:   recipientFP,
		RecipientHint: recipientHint,
		FileKey:       fileKey,
		FileName:      fileName,
		FileSize:      fileSize,
		IsDir:         isDir,
	}
	return s.StateDB.AddOutboxEntry(entry)
}

// FlushOutbox delivers any pending outbox messages to a peer that just
// connected. Called from OnPeer (outbound) and handleAnnounce (inbound remap).
func (s *Server) FlushOutbox(peerAddr string) {
	if s.StateDB == nil {
		return
	}
	// Resolve fingerprint from cluster metadata.
	fp := ""
	if s.Cluster != nil {
		if node, ok := s.Cluster.GetNode(peerAddr); ok && node.Metadata != nil {
			fp = node.Metadata["fingerprint"]
		}
	}
	if fp == "" {
		return // can't match outbox entries without fingerprint
	}

	entries, err := s.StateDB.ListOutboxForPeer(fp)
	if err != nil || len(entries) == 0 {
		return
	}

	for _, e := range entries {
		if err := s.SendDirectShare(peerAddr, e.FileKey, e.FileName, e.FileSize, e.IsDir); err != nil {
			log.Printf("[outbox] flush to %s failed for %q: %v", peerAddr, e.FileName, err)
			return // peer went offline again, stop trying
		}
		_ = s.StateDB.RemoveOutboxEntry(e.ID)
		log.Printf("[outbox] delivered %q to %s", e.FileName, peerAddr)
	}
}

// GetInbox returns the local inbox entries (files sent to us by other peers).
func (s *Server) GetInbox() ([]State.InboxEntry, error) {
	if s.StateDB == nil {
		return nil, nil
	}
	return s.StateDB.ListInbox()
}

// DeleteFile removes a file from local storage, records a tombstone, and
// broadcasts the deletion to all connected peers.
func (s *Server) DeleteFile(key string) (int, error) {
	// Verify ownership: key must start with own fingerprint.
	ownFP := ""
	if s.identityMeta != nil {
		ownFP = s.identityMeta["fingerprint"]
	}
	if ownFP == "" {
		return 0, fmt.Errorf("no identity configured")
	}
	if !strings.HasPrefix(key, ownFP+"/") {
		return 0, fmt.Errorf("not authorized: you can only delete your own files")
	}

	// Delete chunks from store.
	manifestKey := chunker.ManifestStorageKey(key)
	if _, rc, err := s.Store.ReadStream(manifestKey); err == nil {
		manifestData, readErr := io.ReadAll(rc)
		rc.Close()
		if readErr == nil {
			var manifest chunker.ChunkManifest
			if json.Unmarshal(manifestData, &manifest) == nil {
				for _, ci := range manifest.Chunks {
					sk := chunker.ChunkStorageKey(ci.EncHash)
					_ = s.Store.Remove(sk)
				}
			}
		}
		_ = s.Store.Remove(manifestKey)
	}

	// Also try to delete directory manifest.
	dirManifestKey := "dirmanifest:" + key
	_ = s.Store.Remove(dirManifestKey)

	// Remove from StateDB.
	if s.StateDB != nil {
		_ = s.StateDB.RemoveUpload(key)
		_ = s.StateDB.RemovePublicFile(key)
		_ = s.StateDB.AddTombstone(State.TombstoneEntry{
			Key:         key,
			DeletedAt:   time.Now().UnixNano(),
			Fingerprint: ownFP,
		})
	}

	// Update gossip metadata.
	s.UpdatePublicCatalogMetadata()

	// Broadcast to all connected peers.
	delMsg := &Message{Payload: &MessageDeleteFile{
		Key:         key,
		Fingerprint: ownFP,
	}}
	propagated := 0
	s.peerLock.RLock()
	for addr, peer := range s.peers {
		buf := new(bytes.Buffer)
		if err := gob.NewEncoder(buf).Encode(delMsg); err != nil {
			continue
		}
		if err := peer.SendMsg(peer2peer.IncomingMessage, buf.Bytes()); err != nil {
			log.Printf("[delete] failed to broadcast to %s: %v", addr, err)
			continue
		}
		propagated++
	}
	s.peerLock.RUnlock()

	return propagated, nil
}

// handleDeleteFile processes a remote tombstone message.
func (s *Server) handleDeleteFile(from string, msg *MessageDeleteFile) error {
	// Verify ownership: key must start with sender's fingerprint.
	if !strings.HasPrefix(msg.Key, msg.Fingerprint+"/") {
		log.Printf("[delete] rejected: key %q doesn't match fingerprint %s from %s", msg.Key, msg.Fingerprint, from)
		return nil
	}

	// Check if already tombstoned.
	if s.StateDB != nil && s.StateDB.IsTombstoned(msg.Key) {
		return nil // already processed
	}

	// Delete local chunks if we have them.
	manifestKey := chunker.ManifestStorageKey(msg.Key)
	if _, rc, err := s.Store.ReadStream(manifestKey); err == nil {
		manifestData, readErr := io.ReadAll(rc)
		rc.Close()
		if readErr == nil {
			var manifest chunker.ChunkManifest
			if json.Unmarshal(manifestData, &manifest) == nil {
				for _, ci := range manifest.Chunks {
					sk := chunker.ChunkStorageKey(ci.EncHash)
					_ = s.Store.Remove(sk)
				}
			}
		}
		_ = s.Store.Remove(manifestKey)
	}

	// Remove directory manifest too.
	_ = s.Store.Remove("dirmanifest:" + msg.Key)

	// Record tombstone locally.
	if s.StateDB != nil {
		_ = s.StateDB.RemoveUpload(msg.Key)
		_ = s.StateDB.RemovePublicFile(msg.Key)
		_ = s.StateDB.AddTombstone(State.TombstoneEntry{
			Key:         msg.Key,
			DeletedAt:   time.Now().UnixNano(),
			Fingerprint: msg.Fingerprint,
		})
	}

	log.Printf("[delete] applied tombstone for %q from %s (via %s)", msg.Key, msg.Fingerprint, from)
	return nil
}

// matchesQuery returns true if name matches all words in query (case-insensitive AND).
func matchesQuery(name, query string) bool {
	lower := strings.ToLower(name)
	for _, w := range strings.Fields(strings.ToLower(query)) {
		if !strings.Contains(lower, w) {
			return false
		}
	}
	return true
}

// SearchFiles searches the local public catalog and floods the query to peers.
// Returns aggregated results after a timeout.
func (s *Server) SearchFiles(query string) ([]SearchResult, error) {
	requestID := fmt.Sprintf("%x", time.Now().UnixNano())
	origin := s.effectiveSelfAddr()

	// Search own public files.
	var localResults []SearchResult
	if s.StateDB != nil {
		files, err := s.StateDB.ListPublicFiles()
		if err == nil {
			alias := ""
			fp := ""
			if s.identityMeta != nil {
				alias = s.identityMeta["alias"]
				fp = s.identityMeta["fingerprint"]
			}
			for _, f := range files {
				if matchesQuery(f.Name, query) {
					localResults = append(localResults, SearchResult{
						Key:        f.Key,
						Name:       f.Name,
						Size:       f.Size,
						IsDir:      f.IsDir,
						OwnerAlias: alias,
						OwnerFP:    fp,
						NodeAddr:   origin,
					})
				}
			}
		}
	}

	// Set up response channel.
	respKey := origin + "#search#" + requestID
	ch := make(chan []byte, 64)
	s.mu.Lock()
	s.pendingFile[respKey] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pendingFile, respKey)
		s.mu.Unlock()
	}()

	// Mark as seen to avoid echo.
	s.searchSeen.SeenOrAdd(requestID)

	// Flood to all peers.
	searchMsg := &Message{Payload: &MessageSearchRequest{
		Query:     query,
		RequestID: requestID,
		Origin:    origin,
		TTL:       5,
	}}
	s.peerLock.RLock()
	for addr, peer := range s.peers {
		buf := new(bytes.Buffer)
		if err := gob.NewEncoder(buf).Encode(searchMsg); err != nil {
			continue
		}
		if err := peer.SendMsg(peer2peer.IncomingMessage, buf.Bytes()); err != nil {
			log.Printf("[search] failed to send to %s: %v", addr, err)
		}
	}
	s.peerLock.RUnlock()

	// Collect responses with timeout.
	allResults := append([]SearchResult{}, localResults...)
	seen := make(map[string]bool)
	for _, r := range localResults {
		seen[r.Key] = true
	}

	timeout := time.After(2 * time.Second)
	for {
		select {
		case data := <-ch:
			var resp MessageSearchResponse
			if json.Unmarshal(data, &resp) != nil {
				continue
			}
			for _, r := range resp.Results {
				if !seen[r.Key] {
					seen[r.Key] = true
					allResults = append(allResults, r)
				}
			}
		case <-timeout:
			return allResults, nil
		}
	}
}

// handleSearchRequest processes an incoming search flood.
func (s *Server) handleSearchRequest(from string, msg *MessageSearchRequest) error {
	// Dedup: drop if already seen.
	if s.searchSeen.SeenOrAdd(msg.RequestID) {
		return nil
	}

	// Search local public files.
	var results []SearchResult
	if s.StateDB != nil {
		files, err := s.StateDB.ListPublicFiles()
		if err == nil {
			alias := ""
			fp := ""
			if s.identityMeta != nil {
				alias = s.identityMeta["alias"]
				fp = s.identityMeta["fingerprint"]
			}
			selfAddr := s.effectiveSelfAddr()
			for _, f := range files {
				if matchesQuery(f.Name, msg.Query) {
					results = append(results, SearchResult{
						Key:        f.Key,
						Name:       f.Name,
						Size:       f.Size,
						IsDir:      f.IsDir,
						OwnerAlias: alias,
						OwnerFP:    fp,
						NodeAddr:   selfAddr,
					})
				}
			}
		}
	}

	// Send results directly to origin.
	if len(results) > 0 {
		resp := &Message{Payload: &MessageSearchResponse{
			RequestID: msg.RequestID,
			Results:   results,
			FromNode:  s.effectiveSelfAddr(),
		}}
		_ = s.sendToAddr(msg.Origin, resp)
	}

	// Forward to other peers with TTL-1.
	if msg.TTL > 1 {
		fwdMsg := &Message{Payload: &MessageSearchRequest{
			Query:     msg.Query,
			RequestID: msg.RequestID,
			Origin:    msg.Origin,
			TTL:       msg.TTL - 1,
		}}
		s.peerLock.RLock()
		for addr, peer := range s.peers {
			if addr == from {
				continue // don't echo back
			}
			buf := new(bytes.Buffer)
			if err := gob.NewEncoder(buf).Encode(fwdMsg); err != nil {
				continue
			}
			_ = peer.SendMsg(peer2peer.IncomingMessage, buf.Bytes())
		}
		s.peerLock.RUnlock()
	}

	return nil
}

// handleSearchResponse routes search results to the waiting SearchFiles call.
func (s *Server) handleSearchResponse(from string, msg *MessageSearchResponse) error {
	selfAddr := s.effectiveSelfAddr()
	respKey := selfAddr + "#search#" + msg.RequestID

	s.mu.Lock()
	ch, ok := s.pendingFile[respKey]
	s.mu.Unlock()

	if ok {
		data, _ := json.Marshal(msg)
		ch <- data
	}
	return nil
}

// GracefulShutdown signals all peers that this node is leaving and waits briefly
// for any in-flight operations to complete before the process exits.
func (s *Server) GracefulShutdown() {
	log.Println("[GracefulShutdown] Signalling peers and shutting down...")

	// Stop gossip FIRST so this node stops sending "I am Alive" digests to peers.
	// If gossip keeps running after we broadcast leaving, peers may receive a
	// stale Alive digest and immediately re-dial us — causing the ring to flicker.
	if s.GossipSvc != nil {
		s.GossipSvc.Stop()
	}
	if s.HeartbeatSvc != nil {
		s.HeartbeatSvc.Stop()
	}

	// Now broadcast MessageLeaving so peers immediately remove this node from
	// their rings (voluntary departure — no phi-accrual timeout needed).
	// Include our current generation so peers can set StateLeft at gen+1,
	// ensuring any stale gossip digest we already sent (at gen) is overridden.
	selfAddr := s.serverOpts.transport.Addr()
	var selfGen uint64
	if s.Cluster != nil {
		if info, ok := s.Cluster.GetNode(selfAddr); ok {
			selfGen = info.Generation
		}
	}
	_ = s.Broadcast(Message{Payload: &MessageLeaving{From: selfAddr, Generation: selfGen}})

	// Pause to let the leaving message propagate before closing connections.
	time.Sleep(200 * time.Millisecond)

	if s.HealthSrv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.HealthSrv.Stop(ctx)
	}
	if s.HandoffSvc != nil {
		s.HandoffSvc.Stop()
	}
	if s.AntiEntropy != nil {
		s.AntiEntropy.Stop()
	}
	if s.MDNSAdvertiser != nil {
		s.MDNSAdvertiser.Stop()
	}
	// Two-Port: close the data transport before closing control.
	if s.dataTransport != nil {
		s.dataTransport.Close()
	}
	if s.casWriteCh != nil {
		close(s.casWriteCh)
	}
	if s.CacheMgr != nil {
		s.CacheMgr.Stop()
	}
	if s.StateDB != nil {
		s.StateDB.Close()
	}

	// Give replication goroutines a moment to finish.
	time.Sleep(300 * time.Millisecond)
	s.Stop()
}

// handleLeaving processes a MessageLeaving from a peer that is voluntarily
// departing. The node is immediately removed from the hash ring and its
// cluster state is updated to StateLeft — no failure detection timeout needed.
func (s *Server) handleLeaving(addr string, msgGen uint64) error {
	self := s.serverOpts.transport.Addr()
	log.Printf("[TRACE][handleLeaving] self=%s received LEAVING from addr=%s msgGen=%d", self, addr, msgGen)

	// Resolve the canonical peer address: the leaving message may carry a
	// bare port (":3001") while the peers map stores "127.0.0.1:3001".
	// Check the peers map directly, then try announceAdded reverse lookup.
	canonicalAddr := addr
	s.peerLock.RLock()
	_, inPeers := s.peers[addr]
	if !inPeers {
		// Try announceAdded: canonical→ephemeral map, but we need the reverse.
		// Also try every peer to match by port when the host is missing.
		_, addrPort, _ := net.SplitHostPort(addr)
		for peerAddr := range s.peers {
			_, peerPort, _ := net.SplitHostPort(peerAddr)
			if peerPort == addrPort {
				canonicalAddr = peerAddr
				inPeers = true
				break
			}
		}
	}
	s.peerLock.RUnlock()
	log.Printf("[TRACE][handleLeaving] self=%s addr=%s canonicalAddr=%s inPeers=%v ring_size_before=%d", self, addr, canonicalAddr, inPeers, s.HashRing.Size())

	s.HashRing.RemoveNode(normalizeAddr(addr))
	s.HashRing.RemoveNode(normalizeAddr(canonicalAddr))
	log.Printf("[TRACE][handleLeaving] self=%s addr=%s ring_size_after=%d", self, addr, s.HashRing.Size())

	if s.Cluster != nil {
		// Try both the raw addr and the canonical for cluster state update.
		lookupAddr := addr
		info, ok := s.Cluster.GetNode(addr)
		if !ok {
			info, ok = s.Cluster.GetNode(canonicalAddr)
			if ok {
				lookupAddr = canonicalAddr
			}
		}
		// Use msgGen+1 so StateLeft beats any stale gossip digest the leaving node
		// already sent (which carries generation == msgGen == its epoch timestamp).
		gen := msgGen + 1
		if gen == 0 || (!ok && msgGen == 0) {
			// Fallback if generation wasn't provided (old peer / zero value).
			gen = uint64(1)
			if ok {
				gen = info.Generation + 1
			}
		}
		log.Printf("[TRACE][handleLeaving] self=%s updating cluster state addr=%s to StateLeft gen=%d (msgGen=%d was_known=%v prev_gen=%d prev_state=%v)",
			self, lookupAddr, gen, msgGen, ok, info.Generation, info.State)
		s.Cluster.UpdateState(lookupAddr, membership.StateLeft, gen)
	}

	// Remove from local peer map so heartbeats stop.
	// Also clean up the announceAdded reverse-mapping so OnPeerDisconnect
	// doesn't attempt a second (redundant) ring removal and doesn't leak memory.
	s.peerLock.Lock()
	if peer, ok := s.peers[canonicalAddr]; ok {
		log.Printf("[TRACE][handleLeaving] self=%s closing peer ptr=%p for addr=%s", self, peer, canonicalAddr)
		_ = peer.Close()
		delete(s.peers, canonicalAddr)
	}
	// Also try the raw addr in case it's stored differently.
	if canonicalAddr != addr {
		if peer, ok := s.peers[addr]; ok {
			_ = peer.Close()
			delete(s.peers, addr)
		}
	}
	delete(s.announceAdded, addr)
	delete(s.announceAdded, canonicalAddr)
	s.peerLock.Unlock()

	metrics.PeerCount.Set(float64(len(s.peers)))
	log.Printf("[TRACE][handleLeaving] self=%s DONE for addr=%s ring_size=%d peer_count=%d", self, addr, s.HashRing.Size(), len(s.peers))
	return nil
}

func (s *Server) Broadcast(d Message) error {
	log.Println("[Broadcast] Encoding message...")

	buf := new(bytes.Buffer)
	if err := gob.NewEncoder(buf).Encode(d); err != nil {
		log.Printf("[Broadcast] Error encoding message: %v\n", err)
		return err
	}

	// Copy peers while holding lock to avoid race condition
	s.peerLock.Lock()
	peersCopy := make(map[string]peer2peer.Peer, len(s.peers))
	for addr, peer := range s.peers {
		peersCopy[addr] = peer
	}
	s.peerLock.Unlock()

	log.Printf("[Broadcast] Broadcasting message to %d peers\n", len(peersCopy))

	var errs []error
	for addr, peer := range peersCopy {
		log.Printf("[Broadcast] Sending message to peer: %s\n", addr)

		if err := peer.SendMsg(peer2peer.IncomingMessage, buf.Bytes()); err != nil {
			log.Printf("[Broadcast] Error sending message to %s: %v\n", addr, err)
			errs = append(errs, fmt.Errorf("peer %s: %w", addr, err))
			continue
		}

		log.Printf("[Broadcast] Successfully sent message to %s\n", addr)
	}

	log.Println("[Broadcast] Message broadcast complete")
	if len(errs) > 0 {
		return fmt.Errorf("broadcast failed for %d/%d peers: %v", len(errs), len(peersCopy), errs[0])
	}
	return nil
}

func NewServer(opts ServerOpts) *Server {
	StoreOpts := storage.StructOpts{
		PathTransformFunc: opts.pathTransform,
		Metadata:          opts.metaData,
		Root:              opts.storageRoot,
	}

	replFactor := opts.ReplicationFactor
	if replFactor <= 0 {
		replFactor = hashring.DefaultReplicationFactor
	}

	bAddrs := make(map[string]struct{}, len(opts.bootstrapNodes))
	for _, a := range opts.bootstrapNodes {
		if norm := NormalizeUserAddr(a); norm != "" {
			bAddrs[norm] = struct{}{}
		}
	}

	return &Server{
		peers:      map[string]peer2peer.Peer{},
		dataPeers:  map[string]peer2peer.Peer{},
		serverOpts: opts,
		Store:      storage.NewStore(StoreOpts),
		HashRing: hashring.New(&hashring.Config{
			ReplicationFactor: replFactor,
		}),
		quitch:         make(chan struct{}),
		pendingFile:    make(map[string]chan []byte),
		pendingOffer:   make(map[string]chan []string),
		casWriteCh:     make(chan casWriteRequest, 32),
		dialingSet:     make(map[string]struct{}),
		announceAdded:  make(map[string]string),
		bootstrapAddrs: bAddrs,
	}
}

// Start binds the transport port, initialises all services, and launches the
// RPC loop in a background goroutine. It returns once the node is fully ready
// to accept peer connections and IPC commands. If the port cannot be bound,
// Start returns an error immediately so the caller can fail fast.
func (s *Server) Start() error {
	if err := s.serverOpts.transport.ListenAndAccept(); err != nil {
		return err
	}

	// Add self to the hash ring so we participate in key ownership.
	selfAddr := s.serverOpts.transport.Addr()
	s.HashRing.AddNode(normalizeAddr(selfAddr))
	log.Printf("[Run] Added self (%s) to hash ring (replication factor: %d)", selfAddr, s.HashRing.ReplicationFactor())

	// Sprint 2: start failure detection and handoff delivery.
	if s.HeartbeatSvc != nil {
		s.HeartbeatSvc.Start()
		log.Println("[Run] HeartbeatService started")
	}
	if s.HandoffSvc != nil {
		s.HandoffSvc.Start()
		log.Println("[Run] HandoffService started")
	}

	// Sprint 3: start gossip dissemination.
	if s.GossipSvc != nil {
		s.GossipSvc.Start()
		log.Println("[Run] GossipService started")
	}

	// Sprint 4: start anti-entropy background sync.
	if s.AntiEntropy != nil {
		s.AntiEntropy.Start()
		log.Println("[Run] AntiEntropyService started")
	}

	// Phase 3: proactive NAT traversal via STUN.
	if s.NATService != nil {
		go s.discoverNAT()
	}

	// mDNS LAN auto-discovery: background scan + advertise.
	go s.mdnsDiscoveryLoop()
	if s.identityMeta != nil {
		selfAddr := s.effectiveSelfAddr()
		host, portStr, _ := net.SplitHostPort(selfAddr)
		port, _ := strconv.Atoi(portStr)
		alias := s.identityMeta["alias"]
		fp := s.identityMeta["fingerprint"]
		if adv, err := peermdns.NewAdvertiser(host, alias, fp, port); err != nil {
			log.Printf("[mDNS] advertiser failed (LAN discovery disabled): %v", err)
		} else {
			s.MDNSAdvertiser = adv
			log.Printf("[mDNS] advertising as %s on %s:%d", alias, host, port)
		}
	}

	// Two-Port: start the data transport BEFORE bootstrap so that DialDual
	// can use it immediately when connecting to bootstrap peers.
	if s.dataTransport != nil {
		if err := s.dataTransport.ListenAndAccept(); err != nil {
			return fmt.Errorf("data transport: %w", err)
		}
		log.Printf("[Run] Data transport listening on %s", s.dataTransport.Addr())
		go s.dataLoop()
	}

	// Always attempt bootstrap — reconnects known peers from StateDB even
	// without --peer flags. BootstrapNetwork handles empty bootstrap lists.
	if err := s.BootstrapNetwork(); err != nil {
		return err
	}

	go s.connectionManagerLoop()
	go s.casWriteLoop()
	go s.loop()
	return nil
}

// Run is kept for backwards compatibility with tests that call it directly.
// It calls Start and then blocks until the server shuts down.
func (s *Server) Run() error {
	if err := s.Start(); err != nil {
		return err
	}
	<-s.quitch
	return nil
}

func (s *Server) loop() {
	defer func() {
		s.serverOpts.transport.Close()
		log.Println("[loop] File server closed due to user quit action")
	}()

	log.Println("[loop] Starting server loop...")

	for {
		select {
		case RPC, ok := <-s.serverOpts.transport.Consume():
			if !ok {
				log.Println("[loop] Channel closed. Exiting loop.")
				return
			}

			if RPC.From == nil {
				log.Println("[loop] Got RPC with nil 'From'. Skipping.")
				continue
			}

			log.Printf("[loop] Received RPC from: %s\n", RPC.From.String())

			if len(RPC.Payload) == 0 {
				log.Println("[loop] Empty payload. Skipping message.")
				continue
			}

			var message Message
			err := gob.NewDecoder(bytes.NewReader(RPC.Payload)).Decode(&message)
			if err != nil {
				log.Printf("[loop] Error decoding message from %s: %v\n", RPC.From.String(), err)
				continue
			}

			log.Printf("[loop] Payload type after decoding: %T\n", message.Payload)

			streamReader := RPC.StreamReader
			if err := s.handleMessage(RPC.From.String(), RPC.Peer, &message, RPC.StreamWg, streamReader); err != nil {
				log.Printf("[loop] Error handling message from %s: %v\n", RPC.From.String(), err)
				continue
			}

		case <-s.quitch:
			log.Println("[loop] Quit channel received, exiting loop")
			return
		}
	}
}

// dataLoop is the dedicated RPC loop for the data-plane transport.
// It only handles data messages (StoreFile, GetFile, LocalFile, GetChunkSwarm).
// All other message types are logged and dropped — they should arrive on the
// control transport instead.
func (s *Server) dataLoop() {
	defer func() {
		s.dataTransport.Close()
		log.Println("[dataLoop] Data transport closed")
	}()

	log.Println("[dataLoop] Starting data-plane loop...")

	for {
		select {
		case RPC, ok := <-s.dataTransport.Consume():
			if !ok {
				log.Println("[dataLoop] Channel closed. Exiting.")
				return
			}
			if RPC.From == nil || len(RPC.Payload) == 0 {
				continue
			}

			var message Message
			if err := gob.NewDecoder(bytes.NewReader(RPC.Payload)).Decode(&message); err != nil {
				log.Printf("[dataLoop] decode error from %s: %v", RPC.From, err)
				continue
			}

			from := RPC.From.String()
			peer := RPC.Peer
			streamReader := RPC.StreamReader

			switch m := message.Payload.(type) {
			case *MessageStoreFile:
				go func() {
					if err := s.handleStoreMessage(from, peer, m, RPC.StreamWg, streamReader); err != nil {
						log.Printf("[dataLoop] StoreFile error from %s: %v", from, err)
					}
				}()
			case *MessageGetFile:
				go func() {
					if err := s.handleGetMessage(from, peer, m); err != nil {
						log.Printf("[dataLoop] GetFile error from %s: %v", from, err)
					}
				}()
			case *MessageLocalFile:
				go func() {
					if err := s.handleLocalMessage(from, peer, m, RPC.StreamWg, streamReader); err != nil {
						log.Printf("[dataLoop] LocalFile error from %s: %v", from, err)
					}
				}()
			case *MessageGetChunkSwarm:
				if RPC.StreamWriter != nil {
					go s.handleGetChunkSwarmBidi(from, m, RPC.StreamWriter, RPC.StreamWg)
				} else {
					go s.handleGetChunkSwarm(from, peer, m)
				}
			default:
				log.Printf("[dataLoop] unexpected message type %T from %s", message.Payload, from)
			}

		case <-s.quitch:
			log.Println("[dataLoop] Quit channel received, exiting")
			return
		}
	}
}

func (s *Server) handleMessage(from string, peer peer2peer.Peer, msg *Message, streamWg *sync.WaitGroup, streamReader io.Reader) error {
	log.Printf("[handleMessage] Handling message from %s: Type=%s\n",
		from, strings.TrimPrefix(reflect.TypeOf(msg.Payload).String(), "main."))

	switch m := msg.Payload.(type) {
	case *MessageStoreFile:
		// Dispatch to a goroutine so the server loop immediately returns to
		// drain the next QUIC stream. Without this, concurrent chunk streams
		// pile up in the QUIC connection receive window while the loop blocks
		// reading one stream, causing a flow-control deadlock.
		go func() {
			if err := s.handleStoreMessage(from, peer, m, streamWg, streamReader); err != nil {
				log.Printf("[handleMessage] async StoreFile error from %s: %v\n", from, err)
			}
		}()
		return nil

	case *MessageGetFile:
		go func() {
			if err := s.handleGetMessage(from, peer, m); err != nil {
				log.Printf("[handleMessage] async GetFile error from %s: %v\n", from, err)
			}
		}()
		return nil

	case *MessageLocalFile:
		// Same async dispatch — this reads stream data from a peer.
		go func() {
			if err := s.handleLocalMessage(from, peer, m, streamWg, streamReader); err != nil {
				log.Printf("[handleMessage] async LocalFile error from %s: %v\n", from, err)
			}
		}()
		return nil

	case *MessageHeartbeat:
		go func(m *MessageHeartbeat) {
			if err := s.handleHeartbeat(from, peer, m); err != nil {
				log.Printf("[handleMessage] async heartbeat error from %s: %v", from, err)
			}
		}(m)
		return nil

	case *MessageHeartbeatAck:
		// An ack is proof the peer is alive — record it so the failure
		// detector doesn't declare the peer dead while it's responding.
		if s.HeartbeatSvc != nil {
			s.HeartbeatSvc.RecordHeartbeat(from)
		}
		return nil

	case *MessageAnnounce:
		return s.handleAnnounce(from, m)

	case *MessageAnnounceAck:
		return s.handleAnnounceAck(m)

	case *MessageGossipDigest:
		// Dispatch gossip digest processing to a goroutine so the RPC loop
		// is immediately free to process the next message (e.g. chunk data).
		go func(m *MessageGossipDigest) {
			if s.HeartbeatSvc != nil {
				s.HeartbeatSvc.RecordHeartbeat(from)
			}
			if s.GossipSvc != nil {
				s.GossipSvc.HandleDigest(from, &gossip.MessageGossipDigest{
					From:    m.From,
					Digests: m.Digests,
				}, func(msg interface{}) error {
					var payload interface{}
					if gr, ok := msg.(*gossip.MessageGossipResponse); ok {
						payload = &MessageGossipResponse{
							From:     gr.From,
							Full:     gr.Full,
							MyDigest: gr.MyDigest,
						}
					} else {
						payload = msg
					}
					buf := new(bytes.Buffer)
					if err := gob.NewEncoder(buf).Encode(&Message{Payload: payload}); err != nil {
						return err
					}
					return peer.SendMsg(peer2peer.IncomingMessage, buf.Bytes())
				})
			}
		}(m)
		return nil

	case *MessageGossipResponse:
		go func(m *MessageGossipResponse) {
			if s.HeartbeatSvc != nil {
				s.HeartbeatSvc.RecordHeartbeat(from)
			}
			if s.GossipSvc != nil {
				s.GossipSvc.HandleResponse(from, &gossip.MessageGossipResponse{
					From:     m.From,
					Full:     m.Full,
					MyDigest: m.MyDigest,
				})
			}
		}(m)
		return nil

	case *MessageQuorumWrite:
		return s.handleQuorumWrite(from, peer, m)

	case *MessageQuorumWriteAck:
		if s.Quorum != nil {
			s.Quorum.HandleWriteAck(quorum.WriteAck{
				NodeAddr: m.From,
				Key:      m.Key,
				Success:  m.Success,
				ErrMsg:   m.ErrMsg,
			})
		}
		return nil

	case *MessageQuorumRead:
		return s.handleQuorumRead(from, peer, m)

	case *MessageQuorumReadResponse:
		if s.Quorum != nil {
			s.Quorum.HandleReadResponse(quorum.ReadResponse{
				NodeAddr:  m.From,
				Key:       m.Key,
				Found:     m.Found,
				Clock:     vclock.VectorClock(m.Clock),
				Timestamp: m.Timestamp,
			})
		}
		return nil

	case *MessageMerkleSync:
		if s.AntiEntropy != nil {
			s.AntiEntropy.HandleSync(from, &merkle.MessageMerkleSync{
				From:     m.From,
				RootHash: m.RootHash,
			})
		}
		return nil

	case *MessageMerkleDiffResponse:
		if s.AntiEntropy != nil {
			s.AntiEntropy.HandleDiffResponse(from, &merkle.MessageMerkleDiffResponse{
				From:    m.From,
				AllKeys: m.AllKeys,
			})
		}
		return nil

	case *MessageLeaving:
		return s.handleLeaving(m.From, m.Generation)

	case *MessageStoreManifest:
		var manifest chunker.ChunkManifest
		if err := json.Unmarshal(m.ManifestJSON, &manifest); err != nil {
			return fmt.Errorf("handleMessage: unmarshal manifest: %w", err)
		}
		if err := s.serverOpts.metaData.SetManifest(m.FileKey, &manifest); err != nil {
			return fmt.Errorf("handleMessage: SetManifest: %w", err)
		}
		// Mark the key as chunked so GetData on this node takes the fast path
		// without needing to fetch the manifest from a peer again.
		fm, _ := s.serverOpts.metaData.Get(m.FileKey)
		fm.Chunked = true
		_ = s.serverOpts.metaData.Set(m.FileKey, fm)
		return nil

	case *MessageGetManifest:
		// Peer is asking for metadata. Unified stat: check dir manifest first,
		// then file manifest — one network round-trip resolves either type.
		resp := &MessageManifestResponse{FileKey: m.FileKey}
		if dirData, ok := s.serverOpts.metaData.GetDirManifest(m.FileKey); ok {
			resp.ManifestJSON = dirData
			resp.IsDirectory = true
		} else if manifest, ok := s.serverOpts.metaData.GetManifest(m.FileKey); ok {
			if data, err := json.Marshal(manifest); err == nil {
				resp.ManifestJSON = data
			}
		}
		buf := new(bytes.Buffer)
		if err := gob.NewEncoder(buf).Encode(&Message{Payload: resp}); err == nil {
			_ = peer.SendMsg(peer2peer.IncomingMessage, buf.Bytes())
		}
		return nil

	case *MessageManifestResponse:
		// Deliver to any pending fetchManifest caller waiting on this key.
		s.mu.Lock()
		ch, ok := s.pendingFile["__manifest__"+m.FileKey]
		s.mu.Unlock()
		if ok && len(m.ManifestJSON) > 0 {
			// Prefix with 1-byte type flag: 0x00=file, 0x01=directory.
			typeByte := byte(0x00)
			if m.IsDirectory {
				typeByte = 0x01
			}
			payload := make([]byte, 1+len(m.ManifestJSON))
			payload[0] = typeByte
			copy(payload[1:], m.ManifestJSON)
			ch <- payload
		}
		return nil

	case *MessageChunkOffer:
		return s.handleChunkOffer(peer, m)

	case *MessageChunkNeed:
		// Deliver to the replicateChunk goroutine waiting on this peer's offer reply.
		s.mu.Lock()
		ch, ok := s.pendingOffer[from]
		s.mu.Unlock()
		if ok {
			ch <- m.Missing
		}
		return nil

	case *MessageIdentityMeta:
		// Store the remote node's identity metadata for alias resolution.
		if s.Cluster != nil && m.Metadata != nil {
			// Resolve the canonical address for this peer. We cannot trust m.From
			// because in Docker/NAT it may be 127.0.0.1:3000 (container-local),
			// which collides with the local node's own address.
			//
			// Strategy:
			// 1. Check if 'from' (connection addr) was remapped by handleAnnounce
			//    to a canonical addr — use the canonical.
			// 2. If 'from' itself is already a canonical peer addr — use it.
			// 3. Fall back to m.From only if it doesn't collide with our own addr.
			// 4. Last resort: use 'from' and ensure the node exists.
			addr := ""
			selfAddr := normalizeAddr(s.serverOpts.transport.Addr())

			// Check announceAdded reverse: is 'from' an ephemeral that was remapped?
			s.peerLock.RLock()
			for canonical, ephemeral := range s.announceAdded {
				if ephemeral == from {
					addr = canonical
					break
				}
			}
			// Also check if 'from' itself is a canonical peer key.
			if addr == "" {
				if _, ok := s.peers[from]; ok {
					addr = from
				}
			}
			s.peerLock.RUnlock()

			// Fall back to m.From only if it doesn't collide with self.
			if addr == "" && m.From != "" && m.From != selfAddr {
				addr = m.From
			}
			if addr == "" {
				addr = from
			}
			// Fingerprint-based blocklist check: even if the peer's address
			// changed (DHCP, NAT), the identity fingerprint stays the same.
			if fp := m.Metadata["fingerprint"]; fp != "" && s.StateDB != nil && s.StateDB.IsIgnoredFingerprint(fp) {
				// If this peer was explicitly requested via --peer flag,
				// clear the blocklist instead of rejecting. The user's
				// intent overrides any previous disconnect/block.
				_, isBootstrap := s.bootstrapAddrs[addr]
				if !isBootstrap {
					_, isBootstrap = s.bootstrapAddrs[from]
				}
				if isBootstrap {
					_ = s.StateDB.RemoveIgnoredByFingerprint(fp)
					log.Printf("[identity] Cleared blocklist for bootstrap peer %s (fp %s)", addr, fp)
				} else {
					log.Printf("[identity] REJECT: peer %s fingerprint %s is blocklisted, disconnecting", addr, fp)
					s.peerLock.Lock()
					if p, ok := s.peers[from]; ok {
						p.Close()
						delete(s.peers, from)
					}
					s.peerLock.Unlock()
					return nil
				}
			}

			// Ensure the node exists in cluster before setting metadata.
			// This handles the case where the identity message arrives before
			// handleAnnounce has added the node to the cluster.
			s.Cluster.AddNode(addr, nil)
			s.Cluster.SetMetadata(addr, m.Metadata)
			log.Printf("[identity] stored metadata for %s (alias=%s fingerprint=%s)", addr, m.Metadata["alias"], m.Metadata["fingerprint"])
		}
		return nil

	case *MessageGetPublicCatalog:
		return s.handleGetPublicCatalog(from)

	case *MessagePublicCatalogResponse:
		// Handled via pendingFile channel (request-response pattern).
		// The response may arrive from an ephemeral address, but the pending
		// key was stored under the canonical address. Try both.
		s.mu.Lock()
		ch, ok := s.pendingFile[from+"#catalog"]
		if !ok {
			// Translate ephemeral → canonical via announceAdded reverse lookup.
			s.peerLock.RLock()
			for canonical, ephemeral := range s.announceAdded {
				if ephemeral == from {
					ch, ok = s.pendingFile[canonical+"#catalog"]
					break
				}
			}
			s.peerLock.RUnlock()
		}
		s.mu.Unlock()
		if ok {
			ch <- m.CatalogJSON
		}
		return nil

	case *MessageDeleteFile:
		return s.handleDeleteFile(from, m)

	case *MessageSearchRequest:
		return s.handleSearchRequest(from, m)

	case *MessageSearchResponse:
		return s.handleSearchResponse(from, m)

	case *MessageDirectShare:
		return s.handleDirectShare(from, m)

	// ── Swarm architecture messages ───────────────────────────────
	case *MessageGetChunkSwarm:
		// Serve chunks concurrently — don't block the message loop while
		// reading 4MB from disk and streaming over QUIC. This allows the
		// seeder to handle multiple chunk requests in parallel.
		go s.handleGetChunkSwarm(from, peer, m)
		return nil

	case *MessageRegisterProvider:
		return s.handleRegisterProvider(from, m)

	case *MessageGetProviders:
		return s.handleGetProviders(from, peer, m)

	case *MessageProvidersResponse:
		return s.handleProvidersResponse(from, m)

	default:
		typeName := strings.TrimPrefix(reflect.TypeOf(msg.Payload).String(), "main.")
		log.Printf("[handleMessage] Unknown message type %s from %s\n", typeName, from)
	}

	return nil
}

// sendToAddr sends a gob-encoded Message to a specific peer by address.
// Returns an error if the peer is not connected or the send fails.
func (s *Server) sendToAddr(addr string, msg *Message) error {
	s.peerLock.RLock()
	peer, ok := s.peers[addr]
	if !ok {
		// Try the ephemeral address from announceAdded (canonical → ephemeral).
		if eph, have := s.announceAdded[addr]; have {
			peer, ok = s.peers[eph]
		}
	}
	if !ok {
		// Reverse lookup: addr might be an ephemeral address whose peer was
		// remapped to a canonical address by handleAnnounce.
		for canonical, ephemeral := range s.announceAdded {
			if ephemeral == addr {
				peer, ok = s.peers[canonical]
				break
			}
		}
	}
	s.peerLock.RUnlock()

	if !ok {
		return fmt.Errorf("sendToAddr: peer %s not connected", addr)
	}

	buf := new(bytes.Buffer)
	if err := gob.NewEncoder(buf).Encode(msg); err != nil {
		return fmt.Errorf("sendToAddr: encode: %w", err)
	}

	if err := peer.SendMsg(peer2peer.IncomingMessage, buf.Bytes()); err != nil {
		return fmt.Errorf("sendToAddr: send to %s: %w", addr, err)
	}
	return nil
}

// handleHeartbeat records the heartbeat arrival and sends an ack.
func (s *Server) handleHeartbeat(from string, peer peer2peer.Peer, msg *MessageHeartbeat) error {
	metrics.RecordHeartbeat("received")
	if s.HeartbeatSvc != nil {
		s.HeartbeatSvc.RecordHeartbeat(from)
		// Also record under the canonical address if this inbound peer was remapped
		// by handleAnnounce. Without this, the liveness detector tracks phi under
		// the ephemeral key while the ring only knows the canonical key.
		s.peerLock.RLock()
		for canonical, ephemeral := range s.announceAdded {
			if ephemeral == from {
				s.HeartbeatSvc.RecordHeartbeat(canonical)
				break
			}
		}
		s.peerLock.RUnlock()
	}
	selfAddr := s.serverOpts.transport.Addr()
	ack := &Message{Payload: &MessageHeartbeatAck{
		From:      selfAddr,
		Timestamp: msg.Timestamp,
	}}
	// Send ack directly on the same connection — avoids peers[from] map lookup
	// which fails after handleAnnounce remaps the ephemeral key to canonical.
	buf := new(bytes.Buffer)
	if err := gob.NewEncoder(buf).Encode(ack); err == nil {
		_ = peer.SendMsg(peer2peer.IncomingMessage, buf.Bytes())
	}
	return nil
}

// handleAnnounce processes a MessageAnnounce from a newly-connected inbound peer.
// The peer's TCP connection is stored under its ephemeral port (e.g. "172.17.0.2:47636").
// This handler remaps it to the canonical listen address (e.g. "172.17.0.2:3000"),
// adds it to the hash ring, and triggers rebalance — all immediately on connect.
func (s *Server) handleAnnounce(from string, msg *MessageAnnounce) error {
	canonical := msg.ListenAddr
	if canonical == "" {
		return nil
	}

	// If the peer announced only a port (e.g. ":3000"), resolve it to a full
	// address using the remote IP we already know from the TCP connection.
	// This handles the case where Node B is inside Docker and its self-address
	// is ":3000" (container-local), but Node A sees it arrive from 172.17.0.2.
	if strings.HasPrefix(canonical, ":") {
		remoteHost, _, err := net.SplitHostPort(from)
		if err == nil && remoteHost != "" {
			canonical = net.JoinHostPort(remoteHost, canonical[1:])
		}
	}

	// Canonicalize loopback variants so the peers map key matches the ring key.
	canonical = normalizeAddr(canonical)

	// If canonical == from, no remap is needed (shared QUIC transport means
	// the peer's source port is already its listen port). But we still need
	// to ensure it's in the ring and cluster — fall through to registration.
	needsRemap := canonical != from

	// Reject self-connections: if the resolved canonical address has the same
	// port as us and its host is one of our own local interfaces, it's a
	// gossip-triggered loopback (e.g. gossip learns "10.145.16.251:3000" from
	// a stale entry, dials us, and we'd add ourselves twice under a different key).
	selfAddr := s.serverOpts.transport.Addr()
	_, selfPort, _ := net.SplitHostPort(selfAddr)
	canonicalHost, canonicalPort, _ := net.SplitHostPort(canonical)
	if canonicalPort == selfPort && isLocalAddr(canonicalHost) {
		log.Printf("[handleAnnounce] ignoring self-connection from %s (canonical %s matches local port %s)", from, canonical, selfPort)
		return nil
	}

	// Reject inbound connections from blocklisted peers. The initial inbound
	// accepted under an ephemeral port, but now we know the canonical address.
	if s.StateDB != nil && s.StateDB.IsIgnoredPeer(canonical) {
		log.Printf("[handleAnnounce] REJECT: peer %s (canonical %s) is blocklisted, disconnecting", from, canonical)
		s.peerLock.Lock()
		if p, ok := s.peers[from]; ok {
			p.Close()
			delete(s.peers, from)
		}
		s.peerLock.Unlock()
		// Clean up any cluster entry that was created under the ephemeral
		// address by MessageIdentityMeta arriving before this announce.
		if s.Cluster != nil {
			// Remove the ephemeral cluster entry — it's an artifact.
			s.Cluster.RemoveNode(from)
		}
		return nil
	}

	s.peerLock.Lock()
	p, ok := s.peers[from]
	if ok && !p.Outbound() && needsRemap {
		// Remap ephemeral entry → canonical listen address.
		delete(s.peers, from)
		s.peers[canonical] = p
		// Record so OnPeerDisconnect can clean up the ring when this inbound drops.
		s.announceAdded[canonical] = from
		log.Printf("[handleAnnounce] inbound peer %s → canonical %s", from, canonical)
	}
	s.peerLock.Unlock()

	if !ok {
		return nil
	}

	if !s.HashRing.HasNode(normalizeAddr(canonical)) {
		s.HashRing.AddNode(normalizeAddr(canonical))
		log.Printf("[handleAnnounce] added %s to hash ring (size=%d)", normalizeAddr(canonical), s.HashRing.Size())
		metrics.SetPeerCount(s.outboundPeerCount())
		metrics.SetRingSize(s.HashRing.Size())

		if s.Cluster != nil {
			s.Cluster.AddNode(canonical, nil)
			// Migrate any identity metadata that was stored under the ephemeral
			// address (if the identity message arrived before the announce).
			// Only when from != canonical — with shared QUIC transport the source
			// port equals the listen port, so from == canonical. Running the
			// migration in that case would delete the node we just created.
			if needsRemap {
				if old, ok := s.Cluster.GetNode(from); ok {
					if old.Metadata != nil && len(old.Metadata) > 0 {
						s.Cluster.SetMetadata(canonical, old.Metadata)
					}
					s.Cluster.RemoveNode(from)
					log.Printf("[handleAnnounce] migrated metadata from %s to %s", from, canonical)
				}
			}
		}
		// Reset failure detector for the inbound peer so stale heartbeat
		// gaps from the previous session don't cause a premature death.
		if s.HeartbeatSvc != nil {
			s.HeartbeatSvc.RecordHeartbeat(canonical)
		}
		if s.HandoffSvc != nil {
			s.HandoffSvc.OnPeerReconnect(canonical)
		}
		if s.Rebalancer != nil {
			s.Rebalancer.OnNodeJoined(canonical)
		}
		// Flush outbox: deliver any pending direct-shares for this inbound peer.
		go s.FlushOutbox(canonical)
	}

	// Send MessageAnnounceAck back to the peer telling it its externally-visible
	// address. This is how nodes behind NAT/Docker discover their routable IP.
	s.peerLock.RLock()
	peer, hasPeer := s.peers[canonical]
	s.peerLock.RUnlock()
	if hasPeer {
		// The peer's externally-visible address is its canonical listen address
		// (derived from the IP we see on the inbound connection + announced port).
		ackMsg := &Message{Payload: &MessageAnnounceAck{YourAddr: canonical}}
		buf := new(bytes.Buffer)
		if err := gob.NewEncoder(buf).Encode(ackMsg); err == nil {
			_ = peer.SendMsg(peer2peer.IncomingMessage, buf.Bytes())
		}

		// Send our identity metadata so the remote peer can resolve our alias.
		if s.identityMeta != nil {
			idFrom := s.effectiveSelfAddr()
			idMsg := &Message{Payload: &MessageIdentityMeta{
				From:     idFrom,
				Metadata: s.identityMeta,
			}}
			buf := new(bytes.Buffer)
			if err := gob.NewEncoder(buf).Encode(idMsg); err == nil {
				_ = peer.SendMsg(peer2peer.IncomingMessage, buf.Bytes())
			}
		}
	}
	return nil
}

// isVirtualInterface returns true for Docker, VM, and container bridge
// interfaces that should be skipped during LAN IP resolution.
func isVirtualInterface(name string) bool {
	lower := strings.ToLower(name)
	for _, prefix := range []string{
		"docker", "veth", "br-",
		"vmnet", "vmware", "virbr", "vnet",
		"vboxnet",
		"lxcbr", "lxdbr",
		"flannel", "cni", "calico", "podman",
		"tailscale", "wg",
	} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// resolveOutboundIP returns this machine's preferred outbound LAN IP.
// Tier 1: UDP dial trick — lets the OS routing table pick the right interface.
// Tier 2: Scored interface enumeration — prefers WiFi/Ethernet over virtual
//
//	bridges (docker0, virbr0, veth*, etc.) for air-gapped campus LANs.
//
// Tier 3: 127.0.0.1 — absolute last resort (single-machine only).
func resolveOutboundIP() string {
	// Tier 1: UDP routing trick. The OS picks the interface that would route
	// to 8.8.8.8. No packet is actually sent (UDP connect, not send).
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err == nil {
		ip := conn.LocalAddr().(*net.UDPAddr).IP.String()
		conn.Close()
		if ip != "" && ip != "0.0.0.0" {
			return ip
		}
	}

	// Tier 2: Scored interface walk.
	// Skips virtual interfaces and prefers RFC-1918 private IPs (LAN addresses).
	ifaces, err := net.Interfaces()
	if err != nil {
		return "127.0.0.1"
	}

	var fallback string
	for _, i := range ifaces {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 || i.Flags&net.FlagPointToPoint != 0 {
			continue
		}
		if isVirtualInterface(i.Name) {
			continue
		}
		addrs, _ := i.Addrs()
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			ip4 := ip.To4()
			if ip4 == nil {
				continue
			}
			// Prefer RFC-1918 private ranges (campus LAN / hotspot).
			if ip4[0] == 10 || (ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31) || (ip4[0] == 192 && ip4[1] == 168) {
				return ip4.String()
			}
			if fallback == "" {
				fallback = ip4.String()
			}
		}
	}
	if fallback != "" {
		return fallback
	}

	// Tier 3: Last resort.
	return "127.0.0.1"
}

// effectiveSelfAddr returns the externally-visible address if known (via
// AnnounceAck or STUN), otherwise resolves the real LAN IP for unspecified
// bind addresses so peer announcements carry a routable address.
func (s *Server) effectiveSelfAddr() string {
	s.externalAddrMu.Lock()
	ext := s.externalAddr
	s.externalAddrMu.Unlock()
	if ext != "" {
		return ext
	}
	addr := s.serverOpts.transport.Addr()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return normalizeAddr(addr)
	}
	ip := net.ParseIP(host)
	// Bound to all interfaces (":3000" or "0.0.0.0:3000") — resolve real LAN IP.
	if host == "" || (ip != nil && ip.IsUnspecified()) {
		return net.JoinHostPort(resolveOutboundIP(), port)
	}
	return normalizeAddr(addr)
}

// SelfAddr returns the node's canonical self address (public API for IPC handlers).
func (s *Server) SelfAddr() string {
	return s.effectiveSelfAddr()
}

// SelfFingerprint returns this node's identity fingerprint, or "" if no identity.
func (s *Server) SelfFingerprint() string {
	if s.identityMeta == nil {
		return ""
	}
	return s.identityMeta["fingerprint"]
}

// GetLANPeers returns the cached mDNS-discovered LAN peers. The daemon
// updates this list every 10 seconds in the background — callers never wait
// for a live mDNS scan. This makes the IPC response instant (~0.1ms vs 3s).
func (s *Server) GetLANPeers() []peermdns.DiscoveredPeer {
	s.lanPeersMu.RLock()
	defer s.lanPeersMu.RUnlock()
	result := make([]peermdns.DiscoveredPeer, len(s.lanPeers))
	copy(result, s.lanPeers)
	return result
}

// mdnsDiscoveryLoop runs in the background and periodically scans the LAN via
// mDNS. Results are cached so IPC handlers return instantly.
func (s *Server) mdnsDiscoveryLoop() {
	selfFP := s.SelfFingerprint()
	selfAddr := s.SelfAddr()

	scan := func() {
		found, err := peermdns.Scan(5 * time.Second)
		if err != nil {
			return
		}
		var filtered []peermdns.DiscoveredPeer
		for _, f := range found {
			if selfFP != "" && f.Fingerprint == selfFP {
				continue
			}
			if f.Addr == selfAddr {
				continue
			}
			filtered = append(filtered, f)
		}
		s.lanPeersMu.Lock()
		s.lanPeers = filtered
		s.lanPeersMu.Unlock()
	}

	// Initial scan immediately on startup.
	scan()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			scan()
		case <-s.quitch:
			return
		}
	}
}

// discoverNAT runs STUN discovery in the background. On success it sets the
// external address and injects the public_addr into gossip metadata, unifying
// with the reactive announce-ack discovery path.
func (s *Server) discoverNAT() {
	pubAddr, err := s.NATService.DiscoverPublicAddr()
	if err != nil {
		log.Printf("[NAT] STUN discovery failed (LAN-only mode): %v", err)
		return
	}
	log.Printf("[NAT] Public address discovered: %s", pubAddr)

	// Store in gossip metadata so peers can reach us across subnets.
	selfAddr := s.effectiveSelfAddr()
	meta := s.NATService.AnnotateGossip()
	s.Cluster.SetMetadata(selfAddr, meta)
}

// handleAnnounceAck processes a MessageAnnounceAck from a peer we connected to.
// The ack tells us our externally-visible address (what the remote sees us as).
// If it differs from our current canonAddr (e.g. we think we're 127.0.0.1:3000
// but the peer sees us as 172.17.0.3:3000), we re-register under the external
// address in the ring, cluster, and gossip so the whole cluster can route to us.
func (s *Server) handleAnnounceAck(msg *MessageAnnounceAck) error {
	if msg.YourAddr == "" {
		return nil
	}

	extAddr := normalizeAddr(msg.YourAddr)
	selfAddr := normalizeAddr(s.serverOpts.transport.Addr())

	// If the external address matches what we already think we are, nothing to do.
	if extAddr == selfAddr {
		log.Printf("[AnnounceAck] external addr %s matches self — no re-registration needed", extAddr)
		return nil
	}

	s.externalAddrMu.Lock()
	alreadySet := s.externalAddr != ""
	s.externalAddr = extAddr
	s.externalAddrMu.Unlock()

	if alreadySet {
		// Already re-registered from a previous ack — don't repeat.
		log.Printf("[AnnounceAck] external addr already set to %s, skipping re-registration", extAddr)
		return nil
	}

	log.Printf("[AnnounceAck] discovered external addr: %s (was: %s) — re-registering", extAddr, selfAddr)

	// 1. Re-register in hash ring: add external, remove old self.
	if !s.HashRing.HasNode(extAddr) {
		s.HashRing.AddNode(extAddr)
	}
	if selfAddr != extAddr {
		s.HashRing.RemoveNode(selfAddr)
	}

	// 2. Re-register in cluster state: copy metadata from old addr to new.
	if s.Cluster != nil {
		oldNode, ok := s.Cluster.GetNode(selfAddr)
		s.Cluster.AddNode(extAddr, nil)
		if ok && oldNode.Metadata != nil {
			s.Cluster.SetMetadata(extAddr, oldNode.Metadata)
		} else if s.identityMeta != nil {
			s.Cluster.SetMetadata(extAddr, s.identityMeta)
		}
		nextGen := s.Cluster.NextGeneration(extAddr)
		s.Cluster.UpdateState(extAddr, membership.StateAlive, nextGen)
	}

	// 3. Update gossip selfAddr so it advertises the routable address.
	if s.GossipSvc != nil {
		s.GossipSvc.SetSelfAddr(extAddr)
	}

	log.Printf("[AnnounceAck] re-registration complete: ring has %s, cluster has %s", extAddr, extAddr)
	return nil
}

func (s *Server) handleGetMessage(from string, peer peer2peer.Peer, msg *MessageGetFile) error {
	log.Printf("HANDLE_GET: Received file request for key '%s' from peer '%s'", msg.Key, from)

	fs, r, err := s.Store.ReadStream(msg.Key)
	if err != nil {
		log.Printf("HANDLE_GET: Error reading file for key '%s' from disk: %v", msg.Key, err)
		return fmt.Errorf("HANDLE_GET: error fetching file from disk: %+v", err)
	}
	defer r.Close()

	p := &Message{
		Payload: MessageLocalFile{
			Key:  msg.Key,
			Size: fs,
		},
	}

	buf := new(bytes.Buffer)
	if err = gob.NewEncoder(buf).Encode(p); err != nil {
		return err
	}

	// Read the file bytes so we can send the entire message+stream atomically.
	fileData, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("HANDLE_GET: reading file data: %w", err)
	}

	err = peer.SendStream(buf.Bytes(), fileData)
	if err != nil {
		log.Printf("[HANDLE_GET] Error sending message+stream to %s: %v\n", from, err)
		return err
	}

	log.Printf("HANDLE_GET: Successfully sent %d bytes to peer '%s' for key '%s'", len(fileData), from, msg.Key)

	return nil
}

func (s *Server) handleStoreMessage(from string, peer peer2peer.Peer, msg *MessageStoreFile, streamWg *sync.WaitGroup, streamReader io.Reader) error {
	log.Printf("HANDLE_STORE: Received store request for key %s from %s", msg.Key, from)

	// Use streamReader (set to quic.Stream for QUIC, peer for TCP) to read file data.
	if streamReader == nil {
		streamReader = peer.(io.Reader) // TCP fallback
	}
	log.Println("HANDLE_STORE: Starting file storage...")
	n, err := s.Store.WriteStream(msg.Key, io.LimitReader(streamReader, msg.Size))
	if err != nil {
		log.Println("HANDLE_STORE: Storage failed:", err)
		return fmt.Errorf("error storing file to disk: %+v", err)
	}

	log.Println("HANDLE_STORE: Closing stream...")
	if streamWg != nil {
		streamWg.Done()
	}

	log.Printf("HANDLE_STORE: Successfully stored [%d] bytes to %s from %s", n, msg.Key, from)
	return nil
}

// handleChunkOffer processes a MessageChunkOffer from an uploading peer.
// It checks which of the offered chunk hashes are already stored locally
// and replies with a MessageChunkNeed listing only the missing ones.
func (s *Server) handleChunkOffer(peer peer2peer.Peer, msg *MessageChunkOffer) error {
	var missing []string
	for _, h := range msg.Hashes {
		storageKey := chunker.ChunkStorageKey(h)
		if !s.Store.Has(storageKey) {
			missing = append(missing, h)
		}
	}
	log.Printf("[CHUNK_OFFER] offered=%d missing=%d", len(msg.Hashes), len(missing))

	reply := &Message{Payload: &MessageChunkNeed{Missing: missing}}
	buf := new(bytes.Buffer)
	if err := gob.NewEncoder(buf).Encode(reply); err != nil {
		return fmt.Errorf("handleChunkOffer: encode reply: %w", err)
	}
	return peer.SendMsg(peer2peer.IncomingMessage, buf.Bytes())
}

func (s *Server) handleLocalMessage(from string, peer peer2peer.Peer, msg *MessageLocalFile, streamWg *sync.WaitGroup, streamReader io.Reader) error {
	if streamReader == nil {
		streamReader = peer.(io.Reader) // TCP fallback
	}

	// Tell quic-go we're done reading — refund MAX_STREAMS quota immediately.
	// Without this, the stream stays half-open (we read the data but never
	// consumed EOF), exhausting the sender's MaxIncomingStreams sliding window.
	type cancelReader interface{ CancelRead(uint64) }

	// Check out a pooled buffer instead of allocating fresh. With MaxParallel=8
	// goroutines each handling 4 MiB chunks, reusing buffers avoids GC thrashing
	// that stutters the TUI. The buffer ownership transfers to the downloader
	// worker via pendingFile; the worker calls PutChunkBuf after WriteAt.
	data := getChunkBuf(int(msg.Size))
	_, err := io.ReadFull(streamReader, data)
	if err != nil {
		log.Printf("[handleLocalMessage] read error from %s for %s: %v", from, msg.Key, err)
		if cr, ok := streamReader.(cancelReader); ok {
			cr.CancelRead(0)
		}
		if streamWg != nil {
			streamWg.Done()
		}
		return err
	}

	if cr, ok := streamReader.(cancelReader); ok {
		cr.CancelRead(0)
	}

	if streamWg != nil {
		streamWg.Done()
	}

	// Deliver immediately to waiting downloader goroutine — zero disk I/O on hot path.
	s.mu.Lock()
	ch, ok := s.pendingFile[msg.Key]
	s.mu.Unlock()

	if ok {
		select {
		case ch <- data:
		default:
		}
	}

	// Async CAS cache write for re-seeding (non-blocking).
	// The chunk is already delivered; CAS write is best-effort for future serving.
	// Use a COPY for the CAS write — the original pooled buffer is now owned by
	// the downloader worker and must not be held hostage during slow disk writes.
	select {
	case s.casWriteCh <- casWriteRequest{key: msg.Key, data: append([]byte(nil), data...)}:
	default:
	}

	return nil
}

// chunkBufPool recycles 4 MiB byte slices used by handleLocalMessage
// and consumed by the downloader workers. Without this, MaxParallel=8
// causes 8 × 4 MiB allocations per round-trip, thrashing the GC and
// stuttering the TUI. Stores *[]byte to avoid interface boxing of large slices.
var chunkBufPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, 4<<20) // 4 MiB — matches default chunk size
		return &b
	},
}

// getChunkBuf checks out a buffer from the pool, sized to exactly n bytes.
// If the pooled buffer is too small (rare — custom chunk size), allocates fresh.
func getChunkBuf(n int) []byte {
	bp := chunkBufPool.Get().(*[]byte)
	b := *bp
	if cap(b) >= n {
		return b[:n]
	}
	// Chunk larger than pool capacity — allocate fresh, let GC handle it.
	return make([]byte, n)
}

// PutChunkBuf returns a buffer to the pool. Only buffers with capacity >= 4 MiB
// are recycled; undersized or oversized buffers are left for GC.
func PutChunkBuf(b []byte) {
	if cap(b) >= 4<<20 {
		b = b[:cap(b)]
		chunkBufPool.Put(&b)
	}
}

// casWriteRequest is a unit of work for the background CAS cache writer.
type casWriteRequest struct {
	key  string
	data []byte
}

// casWriteLoop processes async CAS cache writes in the background.
// Serializes disk writes to reduce seek contention. Started in Server.Start().
func (s *Server) casWriteLoop() {
	for req := range s.casWriteCh {
		if _, err := s.Store.WriteStream(req.key, bytes.NewReader(req.data)); err != nil {
			log.Printf("CAS_CACHE: write failed for %s: %v", req.key, err)
		}
	}
}

func (s *Server) Stop() {
	s.shutdownOnce.Do(func() { close(s.quitch) })
}

func (s *Server) OnPeerDisconnect(p peer2peer.Peer) {
	addr := p.RemoteAddr().String()
	self := s.serverOpts.transport.Addr()
	log.Printf("[TRACE][OnPeerDisconnect] self=%s addr=%s outbound=%v ptr=%p", self, addr, p.Outbound(), p)

	s.peerLock.Lock()
	// Only remove from map if the disconnecting peer is the SAME object
	// currently stored. A duplicate connection that was rejected should
	// not remove the surviving connection's map entry.
	current, ok := s.peers[addr]
	samePtr := ok && current == p
	if samePtr {
		delete(s.peers, addr)
	}
	s.peerLock.Unlock()
	log.Printf("[TRACE][OnPeerDisconnect] self=%s addr=%s map_had_entry=%v same_ptr=%v current_ptr=%p disconnect_ptr=%p",
		self, addr, ok, samePtr, current, p)

	// Outbound peers are always in the ring. Inbound peers are normally NOT in
	// the ring — EXCEPT those added via handleAnnounce (remapped to canonical).
	// Check announceAdded first so those get cleaned up too.
	if !p.Outbound() {
		// Look for a canonical addr that was remapped from this ephemeral addr,
		// OR check if this addr itself is a canonical that was announce-added.
		s.peerLock.Lock()
		var canonicalToRemove string
		for canonical, ephemeral := range s.announceAdded {
			if ephemeral == addr || canonical == addr {
				canonicalToRemove = canonical
				break
			}
		}
		if canonicalToRemove != "" {
			delete(s.announceAdded, canonicalToRemove)
		}
		s.peerLock.Unlock()

		if canonicalToRemove != "" {
			log.Printf("[TRACE][OnPeerDisconnect] INBOUND announce-added closed: self=%s canonical=%s — removing from ring", self, canonicalToRemove)
			s.HashRing.RemoveNode(normalizeAddr(canonicalToRemove))
			metrics.SetPeerCount(s.outboundPeerCount())
			metrics.SetRingSize(s.HashRing.Size())
			if s.Cluster != nil {
				// Ground truth: socket closed = peer is gone. Mark Dead
				// immediately without bumping generation (node owns its own clock).
				s.Cluster.ForceLocalState(canonicalToRemove, membership.StateDead)
			}
		} else {
			log.Printf("[TRACE][OnPeerDisconnect] INBOUND closed: self=%s addr=%s (no ring change)", self, addr)
			// Mark any ephemeral cluster entry as Dead so it doesn't
			// linger in the peer list. This handles the case where
			// MessageIdentityMeta added the node under the ephemeral
			// address before handleAnnounce could remap it.
			if s.Cluster != nil {
				// Remove ephemeral cluster entry — it's an artifact of
				// identity arriving before announce. Not a real node.
				s.Cluster.RemoveNode(addr)
				log.Printf("[TRACE][OnPeerDisconnect] removed ephemeral cluster entry: %s", addr)
			}
		}
		return
	}

	// If this was a duplicate that was rejected (surviving connection is still in map),
	// or if the entry was already cleaned up by handleLeaving (!ok), don't touch the ring.
	if !samePtr {
		if ok {
			log.Printf("[TRACE][OnPeerDisconnect] DUPLICATE outbound closed: self=%s addr=%s ring_size=%d (ring untouched)", self, addr, s.HashRing.Size())
		} else {
			log.Printf("[TRACE][OnPeerDisconnect] ALREADY_CLEANED outbound closed: self=%s addr=%s (handleLeaving already removed, ring untouched)", self, addr)
		}
		return
	}

	log.Printf("[TRACE][OnPeerDisconnect] REMOVING from ring: self=%s addr=%s ring_size_before=%d", self, addr, s.HashRing.Size())
	s.HashRing.RemoveNode(normalizeAddr(addr))
	peerCount := s.outboundPeerCount()
	log.Printf("[TRACE][OnPeerDisconnect] RING UPDATED: self=%s removed=%s ring_size=%d peer_count=%d", self, addr, s.HashRing.Size(), peerCount)
	metrics.SetPeerCount(peerCount)
	metrics.SetRingSize(s.HashRing.Size())

	// Sprint 2: clean up failure detector state for this peer.
	if s.HeartbeatSvc != nil {
		s.HeartbeatSvc.RemovePeer(addr)
	}

	// Ground truth: the QUIC socket closed — this is cryptographic proof
	// the peer is gone. Mark Dead immediately without bumping generation
	// (the remote node owns its own clock). Its next heartbeat will
	// naturally carry a higher generation and override this local state.
	if s.Cluster != nil {
		log.Printf("[TRACE][OnPeerDisconnect] ground-truth Dead: self=%s addr=%s", self, addr)
		s.Cluster.ForceLocalState(addr, membership.StateDead)
	}
}

// OnDataPeer is called when a new peer connects on the data transport.
// Data peers are stored separately — no ring/cluster/gossip interaction.
func (s *Server) OnDataPeer(p peer2peer.Peer) error {
	// Data transport peers are keyed by the CONTROL port address (N),
	// derived from the data port (N+1) by subtracting 1.
	addr := dataAddrToControlAddr(p.RemoteAddr().String())
	log.Printf("[TRACE][OnDataPeer] addr=%s (data remote=%s) outbound=%v", addr, p.RemoteAddr(), p.Outbound())

	s.dataPeerLock.Lock()
	s.dataPeers[addr] = p
	s.dataPeerLock.Unlock()

	return nil
}

// OnDataPeerDisconnect cleans up when a data transport peer disconnects.
func (s *Server) OnDataPeerDisconnect(p peer2peer.Peer) {
	addr := dataAddrToControlAddr(p.RemoteAddr().String())
	log.Printf("[TRACE][OnDataPeerDisconnect] addr=%s (data remote=%s)", addr, p.RemoteAddr())

	s.dataPeerLock.Lock()
	current, ok := s.dataPeers[addr]
	if ok && current == p {
		delete(s.dataPeers, addr)
	}
	s.dataPeerLock.Unlock()
}

// getDataPeer returns the data-plane peer for addr if available,
// falling back to the control-plane peer for single-port backward compat.
func (s *Server) getDataPeer(addr string) (peer2peer.Peer, bool) {
	s.dataPeerLock.RLock()
	dp, ok := s.dataPeers[addr]
	s.dataPeerLock.RUnlock()
	if ok {
		return dp, true
	}
	// Fallback: single-port mode or data connection not yet established.
	s.peerLock.RLock()
	cp, ok := s.peers[addr]
	s.peerLock.RUnlock()
	return cp, ok
}

// dataAddrToControlAddr derives the control-port address (port N) from a
// data-port address (port N+1). Used to key data peers by their control addr.
func dataAddrToControlAddr(dataAddr string) string {
	host, portStr, err := net.SplitHostPort(dataAddr)
	if err != nil {
		return dataAddr // can't parse — return as-is
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 1 {
		return dataAddr
	}
	return net.JoinHostPort(host, strconv.Itoa(port-1))
}

// dialPeer dials a peer on the control transport, and also on the data
// transport if Two-Port mode is active. Uses DialDual for atomic parallel dial.
func (s *Server) dialPeer(addr string) error {
	if s.dataTransport != nil {
		// Both transports must be QUIC for DialDual.
		ctrlQT, ctrlOK := s.serverOpts.transport.(*quicTransport.Transport)
		dataQT, dataOK := s.dataTransport.(*quicTransport.Transport)
		if ctrlOK && dataOK {
			return quicTransport.DialDual(ctrlQT, dataQT, addr)
		}
	}
	return s.dialPeer(addr)
}

// outboundPeerCount returns the number of outbound (real) peer connections.
func (s *Server) outboundPeerCount() int {
	s.peerLock.RLock()
	defer s.peerLock.RUnlock()
	count := 0
	for _, p := range s.peers {
		if p.Outbound() {
			count++
		}
	}
	return count
}

// recordDialFailure bumps the backoff for a peer address.
// Backoff schedule: 1min → 5min → 1hr (cap).
func (s *Server) recordDialFailure(addr string) {
	s.backoffMu.Lock()
	defer s.backoffMu.Unlock()
	exp := s.backoffExp[addr]
	var dur time.Duration
	switch exp {
	case 0:
		dur = 1 * time.Minute
	case 1:
		dur = 5 * time.Minute
	default:
		dur = 1 * time.Hour
	}
	s.backoffMap[addr] = time.Now().Add(dur)
	if exp < 2 {
		s.backoffExp[addr] = exp + 1
	}
}

// clearBackoff resets backoff state for a peer (called on successful connect).
func (s *Server) clearBackoff(addr string) {
	s.backoffMu.Lock()
	defer s.backoffMu.Unlock()
	delete(s.backoffMap, addr)
	delete(s.backoffExp, addr)
}

// isBackedOff returns true if the peer is still in its backoff penalty window.
func (s *Server) isBackedOff(addr string) bool {
	s.backoffMu.Lock()
	defer s.backoffMu.Unlock()
	earliest, ok := s.backoffMap[addr]
	if !ok {
		return false
	}
	if time.Now().After(earliest) {
		// Backoff expired — allow retry.
		delete(s.backoffMap, addr)
		return false
	}
	return true
}

// connectionManagerLoop periodically attempts to reconnect known peers that
// have gone offline. It respects maxPeers, exponential backoff, and the
// ignored peers blocklist.
func (s *Server) connectionManagerLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.quitch:
			return
		case <-ticker.C:
			s.reconnectPass()
		}
	}
}

// reconnectPass dials known peers that are not currently connected, not
// ignored, and not backed off. Limits dials to (maxPeers - active) per pass.
func (s *Server) reconnectPass() {
	active := s.outboundPeerCount()
	if active >= s.maxPeers {
		return // network healthy
	}
	needed := s.maxPeers - active

	if s.StateDB == nil {
		return
	}
	knownPeers, err := s.StateDB.ListKnownPeers()
	if err != nil {
		return
	}

	self := normalizeAddr(s.serverOpts.transport.Addr())

	// Collect currently connected addrs.
	s.peerLock.RLock()
	connected := make(map[string]struct{}, len(s.peers))
	for addr := range s.peers {
		connected[addr] = struct{}{}
	}
	s.peerLock.RUnlock()

	var candidates []string
	for _, addr := range knownPeers {
		addr = normalizeAddr(addr)
		if addr == self {
			continue
		}
		if _, ok := connected[addr]; ok {
			continue
		}
		if s.StateDB.IsIgnoredPeer(addr) {
			continue
		}
		if s.isBackedOff(addr) {
			continue
		}
		// Skip if already dialing.
		s.dialingLock.Lock()
		_, dialing := s.dialingSet[addr]
		s.dialingLock.Unlock()
		if dialing {
			continue
		}
		candidates = append(candidates, addr)
		if len(candidates) >= needed {
			break
		}
	}

	for _, addr := range candidates {
		go func(peerAddr string) {
			log.Printf("[ConnMgr] attempting reconnect to %s", peerAddr)
			if err := s.dialPeer(peerAddr); err != nil {
				log.Printf("[ConnMgr] failed to reconnect %s: %v", peerAddr, err)
				s.recordDialFailure(peerAddr)
			} else {
				log.Printf("[ConnMgr] reconnected to %s", peerAddr)
			}
		}(addr)
	}
}

func (s *Server) BootstrapNetwork() error {
	var wg sync.WaitGroup
	for _, addr := range s.serverOpts.bootstrapNodes {
		addr = NormalizeUserAddr(addr) // bare IPs get :3000 appended
		if addr == "" {
			continue
		}
		// Explicit --peers flag overrides any previous blocklist entry.
		if s.StateDB != nil && s.StateDB.IsIgnoredPeer(addr) {
			_ = s.StateDB.RemoveIgnoredPeer(addr)
			log.Printf("[Bootstrap] Unblocked %s (explicit --peers flag)", addr)
		}
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			log.Printf("[Bootstrap] Attempting to connect with remote: %s", addr)
			if err := s.dialPeer(addr); err != nil {
				log.Printf("[Bootstrap] Failed to dial %s: %v", addr, err)
			} else {
				log.Printf("[Bootstrap] Successfully connected to %s", addr)
				if s.StateDB != nil {
					_ = s.StateDB.AddKnownPeer(addr)
				}
			}
		}(addr)
	}
	wg.Wait()

	// Dial known peers asynchronously (fire-and-forget, don't block startup).
	if s.StateDB != nil {
		knownPeers, err := s.StateDB.ListKnownPeers()
		if err == nil {
			// Build set of bootstrap addrs to skip duplicates.
			bootstrapSet := make(map[string]struct{}, len(s.serverOpts.bootstrapNodes))
			for _, addr := range s.serverOpts.bootstrapNodes {
				bootstrapSet[addr] = struct{}{}
			}
			for _, addr := range knownPeers {
				if _, skip := bootstrapSet[addr]; skip {
					continue
				}
				if s.StateDB.IsIgnoredPeer(addr) {
					log.Printf("[Bootstrap] Skipping ignored peer: %s", addr)
					continue
				}
				go func(peerAddr string) {
					log.Printf("[Bootstrap] Reconnecting known peer: %s", peerAddr)
					if err := s.dialPeer(peerAddr); err != nil {
						log.Printf("[Bootstrap] Known peer %s offline: %v", peerAddr, err)
					} else {
						log.Printf("[Bootstrap] Reconnected to known peer: %s", peerAddr)
					}
				}(addr)
			}
		}
	}

	return nil
}

func (s *Server) OnPeer(p peer2peer.Peer) error {
	addr := normalizeAddr(p.RemoteAddr().String())
	self := s.serverOpts.transport.Addr()
	log.Printf("[TRACE][OnPeer] self=%s addr=%s outbound=%v ptr=%p", self, addr, p.Outbound(), p)

	// Only outbound peers have a RemoteAddr() equal to the real listen
	// address. Inbound connections carry an ephemeral OS-assigned port
	// and must NOT enter the hash ring or cluster membership table.
	if !p.Outbound() {
		s.peerLock.Lock()
		s.peers[addr] = p
		s.peerLock.Unlock()
		// SWIM Refutation: bump our own generation on any new connection
		// so peers that previously marked us Dead will accept our heartbeats.
		if s.Cluster != nil {
			s.Cluster.BumpSelfGeneration()
		}
		log.Printf("[TRACE][OnPeer] INBOUND accepted from %s (not added to ring) self=%s", addr, self)
		return nil
	}

	// For outbound connections, check if we already have an outbound
	// connection to this listen address. If so, skip — the existing
	// connection is fine and we don't want duplicates fighting.
	s.peerLock.Lock()
	existing, alreadyConnected := s.peers[addr]
	if alreadyConnected && existing.Outbound() {
		s.peerLock.Unlock()
		log.Printf("[TRACE][OnPeer] DUPLICATE outbound to %s on self=%s — closing new ptr=%p keeping ptr=%p", addr, self, p, existing)
		p.Close()
		return nil
	}
	log.Printf("[TRACE][OnPeer] STORING outbound to %s on self=%s ptr=%p (prev entry existed=%v prevOutbound=%v)",
		addr, self, p, alreadyConnected, alreadyConnected && existing.Outbound())
	s.peers[addr] = p
	s.peerLock.Unlock()

	s.HashRing.AddNode(normalizeAddr(addr))
	s.clearBackoff(addr) // successful connect — reset any backoff
	peerCount := s.outboundPeerCount()
	log.Printf("[TRACE][OnPeer] RING UPDATED self=%s added=%s ring_size=%d peer_count=%d", self, normalizeAddr(addr), s.HashRing.Size(), peerCount)
	metrics.SetPeerCount(peerCount)
	metrics.SetRingSize(s.HashRing.Size())

	// Tell the peer our canonical listen address so it can remap its inbound
	// connection (stored under our ephemeral port) to our real listen address
	// and add us to its hash ring immediately.
	// Identity metadata is sent in the same goroutine AFTER announce so the
	// remote side processes announce first (remap) and the identity message
	// lands on the canonical address, not the ephemeral one.
	go func() {
		announceMsg := &Message{Payload: &MessageAnnounce{ListenAddr: self}}
		if err := s.sendToAddr(addr, announceMsg); err != nil {
			log.Printf("[OnPeer] announce to %s failed: %v", addr, err)
		}
		// Phase 2: send identity metadata after announce so remote has the
		// canonical mapping when it processes the identity message.
		if s.identityMeta != nil {
			idMsg := &Message{Payload: &MessageIdentityMeta{
				From:     s.effectiveSelfAddr(),
				Metadata: s.identityMeta,
			}}
			if err := s.sendToAddr(addr, idMsg); err != nil {
				log.Printf("[OnPeer] identity meta to %s failed: %v", addr, err)
			}
		}
	}()

	// Sprint 2: deliver any pending hints and trigger rebalance.
	if s.HandoffSvc != nil {
		s.HandoffSvc.OnPeerReconnect(addr)
	}
	if s.Rebalancer != nil {
		s.Rebalancer.OnNodeJoined(addr)
	}

	// Sprint 3: record peer as Alive in membership table.
	// Only insert new nodes here. For reconnecting nodes that were previously
	// Dead/Suspect, we do NOT locally bump their generation — the remote node
	// owns its own generation (unix-nano timestamp). The next gossip heartbeat
	// from the remote will carry a generation higher than any stale local
	// state, and UpdateState will naturally accept it.
	if s.Cluster != nil {
		s.Cluster.AddNode(addr, nil)
		// SWIM Refutation: bump OUR OWN generation so that any peer that
		// previously marked us Dead/Suspect will accept our heartbeats.
		// We only bump self — never touch another node's generation.
		s.Cluster.BumpSelfGeneration()
	}

	// Reset the failure detector for this peer so stale heartbeat gaps
	// from the previous session don't trigger a premature death declaration.
	if s.HeartbeatSvc != nil {
		s.HeartbeatSvc.RecordHeartbeat(addr)
	}

	// Sprint G: auto-persist outbound peers for reconnection on restart.
	if s.StateDB != nil {
		_ = s.StateDB.AddKnownPeer(addr)
	}

	// Flush outbox: deliver any pending direct-shares queued while this peer was offline.
	go s.FlushOutbox(addr)

	return nil
}

// ConnectPeer dials a new peer and persists it in the known peers list.
// If the peer was previously blocklisted, the blocklist entry is removed
// since a manual connect is an explicit user intent to reconnect.
func (s *Server) ConnectPeer(addr string) error {
	// Clear blocklist entry if present — user explicitly wants this peer.
	if s.StateDB != nil && s.StateDB.IsIgnoredPeer(addr) {
		_ = s.StateDB.RemoveIgnoredPeer(addr)
		log.Printf("[ConnectPeer] removed %s from blocklist (manual reconnect)", addr)
	}
	if err := s.dialPeer(addr); err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	if s.StateDB != nil {
		_ = s.StateDB.AddKnownPeer(addr)
	}
	return nil
}

// IsPeerConnected returns true if addr has an active transport connection.
func (s *Server) IsPeerConnected(addr string) bool {
	s.peerLock.RLock()
	_, ok := s.peers[addr]
	s.peerLock.RUnlock()
	return ok
}

// DisconnectPeer closes the connection to a peer and removes it from known peers.
// Also closes any inbound connection from the same node (stored under an
// ephemeral address) so the peer cannot continue sending gossip after disconnect.
func (s *Server) DisconnectPeer(addr string) error {
	s.peerLock.Lock()
	p, ok := s.peers[addr]
	if !ok {
		s.peerLock.Unlock()
		return fmt.Errorf("peer %s not found", addr)
	}
	delete(s.peers, addr)

	// Also close the reverse inbound connection if one exists.
	// announceAdded maps canonical → ephemeral for inbound peers.
	// We also need to check for any inbound peer stored under addr
	// (when the canonical IS the addr used by announceAdded).
	var inboundPeer peer2peer.Peer
	var inboundAddr string
	for canonical, ephemeral := range s.announceAdded {
		if canonical == addr {
			// Our outbound addr matches a canonical that was announce-added.
			// The inbound may be stored under the ephemeral or canonical.
			if ip, bok := s.peers[ephemeral]; bok {
				inboundPeer = ip
				inboundAddr = ephemeral
			}
			delete(s.announceAdded, canonical)
			break
		}
	}
	if inboundPeer != nil {
		delete(s.peers, inboundAddr)
	}
	s.peerLock.Unlock()

	p.Close()
	if inboundPeer != nil {
		inboundPeer.Close()
		log.Printf("[DisconnectPeer] also closed inbound connection from %s (ephemeral %s)", addr, inboundAddr)
	}
	s.HashRing.RemoveNode(addr)

	if s.Cluster != nil {
		// Ground truth: we closed the connection. Mark Dead without
		// stealing the remote node's generation clock.
		s.Cluster.ForceLocalState(addr, membership.StateDead)
	}
	if s.StateDB != nil {
		_ = s.StateDB.RemoveKnownPeer(addr)
	}
	return nil
}

// DisconnectAndIgnorePeer disconnects a peer AND adds it to the blocklist
// so gossip won't re-dial it. Use UnblockPeer to reverse.
func (s *Server) DisconnectAndIgnorePeer(addr string) error {
	// Look up the peer's identity fingerprint before disconnecting,
	// so the blocklist entry survives IP/port changes (DHCP, NAT).
	var fingerprint string
	if s.Cluster != nil {
		if node, ok := s.Cluster.GetNode(addr); ok && node.Metadata != nil {
			fingerprint = node.Metadata["fingerprint"]
		}
	}

	// Best-effort disconnect — peer may already be gone (e.g. remote side
	// closed first).  We still need to add it to the blocklist below.
	_ = s.DisconnectPeer(addr)

	if s.StateDB != nil {
		_ = s.StateDB.AddIgnoredPeer(State.IgnoredPeerEntry{
			Addr:        addr,
			Fingerprint: fingerprint,
			IgnoredAt:   time.Now().UnixNano(),
		})
	}
	return nil
}

// UnblockPeer removes a peer from the ignored list, allowing reconnection.
func (s *Server) UnblockPeer(addr string) error {
	if s.StateDB == nil {
		return fmt.Errorf("no state DB")
	}
	return s.StateDB.RemoveIgnoredPeer(addr)
}

func init() {
	gob.Register(&MessageStoreFile{})
	gob.Register(&MessageGetFile{})
	gob.Register(&MessageLocalFile{})
	gob.Register(&MessageAnnounce{})
	gob.Register(&MessageAnnounceAck{})
	gob.Register(&MessageHeartbeat{})
	gob.Register(&MessageHeartbeatAck{})
	gob.Register(&MessageGossipDigest{})
	gob.Register(&MessageGossipResponse{})
	gob.Register(membership.NodeState(0))
	gob.Register(membership.GossipDigest{})
	gob.Register(membership.NodeInfo{})
	// Sprint 4
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
	gob.Register(&MessageGetPublicCatalog{})
	gob.Register(&MessagePublicCatalogResponse{})
	gob.Register(&MessageDeleteFile{})
	gob.Register(&MessageSearchRequest{})
	gob.Register(&MessageSearchResponse{})
	gob.Register(&MessageDirectShare{})
	// Swarm architecture
	gob.Register(&MessageGetChunkSwarm{})
	gob.Register(&MessageRegisterProvider{})
	gob.Register(&MessageGetProviders{})
	gob.Register(&MessageProvidersResponse{})
}

// MakeServerOpts holds optional configuration for MakeServer.
type MakeServerOpts struct {
	IdentityMeta map[string]string // gossip metadata from identity (alias, fingerprint, keys)
	X25519Priv   []byte            // X25519 private key for DEK unwrapping (swarm on-demand encryption)
	DisableSTUN  bool              // skip STUN-based NAT traversal (default: false = STUN enabled)
	MaxPeers     int               // target active peer connections (0 = default 8, clamped to 100)
	StateDBPath  string            // path to persistent state DB (empty = disabled)
	CacheLimit   int64             // max CAS cache size in bytes (0 = default 50GB)
	DataPort     string            // separate data-plane listen addr (e.g. ":3001"); empty = single-port mode
	DataDir      string            // base directory for storage + metadata (empty = cwd)
}

// sanitizePathAddr replaces characters illegal in Windows file names (e.g. ':')
// so that paths derived from listenAddr work on every OS.
func sanitizePathAddr(addr string) string {
	return strings.ReplaceAll(addr, ":", "_")
}

func MakeServer(listenAddr string, replicationFactor int, makeOpts *MakeServerOpts, node ...string) *Server {
	metaPath := "_metadata.db"
	pathAddr := sanitizePathAddr(listenAddr)

	// Root all runtime data under DataDir (e.g. ~/.hermod/) so the binary
	// can be run from any working directory.
	if makeOpts != nil && makeOpts.DataDir != "" {
		pathAddr = filepath.Join(makeOpts.DataDir, pathAddr)
	}

	// canonAddr is the routable address used for ring membership, gossip,
	// heartbeat, quorum, and cluster identity. When bound to ":3000" or
	// "0.0.0.0:3000", we resolve the real LAN IP so subsystems don't get
	// stuck on "127.0.0.1:3000" — which is unreachable from remote peers.
	// effectiveSelfAddr() is not available yet (transport not started), so
	// we replicate its logic here.
	canonAddr := normalizeAddr(listenAddr)
	if host, port, err := net.SplitHostPort(listenAddr); err == nil {
		ip := net.ParseIP(host)
		if host == "" || (ip != nil && ip.IsUnspecified()) {
			canonAddr = net.JoinHostPort(resolveOutboundIP(), port)
		}
	}

	// Sprint 5: initialise structured logging and Prometheus metrics.
	// Skip if the caller already configured logging (e.g. daemon redirects
	// to a file so stdout stays clean for the TUI).
	if logging.Global == nil {
		logging.Init("server", logging.LevelInfo)
	}
	metrics.Init(prometheus.DefaultRegisterer)

	// Load .env file (ignore error if not found - will use OS env vars)
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, using environment variables")
	}

	// Resolve optional mTLS config (used by both TCP and QUIC transports).
	var tlsCfg *tls.Config
	if os.Getenv("DFS_ENABLE_TLS") == "true" {
		log.Println("mTLS enabled, setting up certificate infrastructure...")

		certDir := ".certs"
		caCertPath := certDir + "/ca.crt"
		caKeyPath := certDir + "/ca.key"
		nodeCertPath := certDir + "/node" + listenAddr + ".crt"
		nodeKeyPath := certDir + "/node" + listenAddr + ".key"

		caCert, caKey, err := crypto.LoadOrGenerateCA(caCertPath, caKeyPath)
		if err != nil {
			log.Fatalf("Failed to setup CA: %v", err)
		}
		log.Printf("CA loaded/generated: %s", caCert.Subject.CommonName)

		nodeOpts := crypto.NodeCertOptions{NodeID: listenAddr}
		if err := crypto.LoadOrGenerateNodeCert(caCert, caKey, nodeCertPath, nodeKeyPath, nodeOpts); err != nil {
			log.Fatalf("Failed to setup node certificate: %v", err)
		}

		tlsCfg, err = crypto.LoadMTLSConfig(nodeCertPath, nodeKeyPath, caCertPath)
		if err != nil {
			log.Fatalf("Failed to setup mTLS config: %v", err)
		}
		log.Println("mTLS configured successfully with CA-signed certificates")
	} else {
		log.Println("TLS disabled (set DFS_ENABLE_TLS=true to enable mTLS)")
	}

	// Sprint 8: use transport factory — defaults to QUIC, override with DFS_TRANSPORT=tcp.
	// OnPeer/OnPeerDisconnect are forwarded via closures so the server pointer
	// (set below) can be captured after construction.
	// Resolve wildcard listen address to the real LAN IP BEFORE creating the
	// transport. On multi-homed hosts (e.g. br0 + WiFi), binding to 0.0.0.0
	// causes QUIC response packets to route out a different interface than
	// where the request came in, breaking connection establishment.
	transportAddr := listenAddr
	if host, port, err := net.SplitHostPort(listenAddr); err == nil {
		ip := net.ParseIP(host)
		if host == "" || (ip != nil && ip.IsUnspecified()) {
			resolved := resolveOutboundIP()
			transportAddr = net.JoinHostPort(resolved, port)
			log.Printf("[MakeServer] resolved wildcard %s → %s", listenAddr, transportAddr)
		}
	}

	var sptr *Server
	// When a data port is configured, the main transport gets RoleControl
	// so it uses tight QUIC windows and a small rpcCh buffer.
	mainRole := quicTransport.RoleUnified
	if makeOpts.DataPort != "" {
		mainRole = quicTransport.RoleControl
	}
	tr, err := factory.NewFromEnv(transportAddr, factory.Options{
		TLSConfig: tlsCfg,
		QUICRole:  mainRole,
		OnPeer: func(p peer2peer.Peer) error {
			return sptr.OnPeer(p)
		},
		OnPeerDisconnect: func(p peer2peer.Peer) {
			sptr.OnPeerDisconnect(p)
		},
	})
	if err != nil {
		log.Fatalf("Failed to create transport: %v", err)
	}
	log.Printf("[MakeServer] transport: %s (protocol=%s, role=%d)", listenAddr, factory.ProtocolFromEnv(), mainRole)

	// Two-Port Architecture: create a separate data-plane transport on DataPort.
	var dataTr peer2peer.Transport
	if makeOpts.DataPort != "" && factory.ProtocolFromEnv() == factory.ProtocolQUIC {
		// Resolve data transport address the same way as the control transport.
		dataTransportAddr := makeOpts.DataPort
		if host, port, err := net.SplitHostPort(makeOpts.DataPort); err == nil {
			ip := net.ParseIP(host)
			if host == "" || (ip != nil && ip.IsUnspecified()) {
				dataTransportAddr = net.JoinHostPort(resolveOutboundIP(), port)
			}
		}
		dataTr, err = factory.New(factory.ProtocolQUIC, dataTransportAddr, factory.Options{
			TLSConfig: tlsCfg,
			QUICRole:  quicTransport.RoleData,
			OnPeer: func(p peer2peer.Peer) error {
				return sptr.OnDataPeer(p)
			},
			OnPeerDisconnect: func(p peer2peer.Peer) {
				sptr.OnDataPeerDisconnect(p)
			},
		})
		if err != nil {
			log.Fatalf("Failed to create data transport: %v", err)
		}
		log.Printf("[MakeServer] data transport: %s (role=Data)", dataTransportAddr)
	}

	metaStore, err := storage.NewBoltMetaStore(pathAddr + metaPath)
	if err != nil {
		log.Fatalf("Failed to open metadata store: %v", err)
	}

	opts := ServerOpts{
		pathTransform:     storage.CASPathTransformFunc,
		transport:         tr,
		metaData:          metaStore,
		bootstrapNodes:    node,
		storageRoot:       pathAddr + "_network",
		ReplicationFactor: replicationFactor,
	}

	s := NewServer(opts)
	sptr = s // wire closure so transport callbacks reach the server

	// Two-Port: wire the data transport into the server.
	if dataTr != nil {
		s.dataTransport = dataTr
	}

	// Sprint F: persistent local state.
	if makeOpts.StateDBPath != "" {
		sdb, err := State.Open(makeOpts.StateDBPath)
		if err != nil {
			log.Printf("[WARN] failed to open state DB at %s: %v (continuing without state)", makeOpts.StateDBPath, err)
		} else {
			s.StateDB = sdb
		}
	}

	// Swarm: CAS cache manager for LRU eviction of secondary seeder chunks.
	if s.StateDB != nil {
		evictFunc := func(fileKey string) error {
			manifest, ok := s.serverOpts.metaData.GetManifest(fileKey)
			if !ok {
				return nil // no manifest = nothing to evict
			}
			var errs []error
			for _, ci := range manifest.Chunks {
				sk := chunker.ChunkStorageKey(ci.EncHash)
				if s.Store.Has(sk) {
					if err := s.Store.Remove(sk); err != nil {
						errs = append(errs, err)
					}
				}
			}
			if len(errs) > 0 {
				return fmt.Errorf("evict %s: %d chunk deletions failed", fileKey, len(errs))
			}
			return nil
		}
		s.CacheMgr = swarm.NewCacheManager(s.StateDB, makeOpts.CacheLimit, evictFunc)
		go s.CacheMgr.RunEvictionLoop(5 * time.Minute)

		// Background provider re-registration: every 10 minutes, re-register
		// all local seed sources with hash ring nodes so provider records
		// stay fresh across ring topology changes and TTL expiry.
		go func() {
			ticker := time.NewTicker(10 * time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-s.CacheMgr.StopCh():
					return
				case <-ticker.C:
					sources, err := s.StateDB.AllSeedSources()
					if err != nil {
						log.Printf("[WARN] provider re-registration: %v", err)
						continue
					}
					for fileKey, src := range sources {
						completed := swarm.CountBits(src.Bitfield)
						s.registerProvider(fileKey, completed, src.TotalChunks)
					}
					if len(sources) > 0 {
						log.Printf("PROVIDER_REREGISTER: refreshed %d provider records", len(sources))
					}
				}
			}
		}()
	}

	// Sprint G: active transfer queue + search dedup.
	s.TransferMgr = transfer.NewManager()
	s.searchSeen = NewSearchRequestCache()

	// Connection Manager: backoff tracking + target peer count.
	s.backoffMap = make(map[string]time.Time)
	s.backoffExp = make(map[string]int)
	const absoluteMaxPeers = 100
	if makeOpts.MaxPeers > 0 {
		s.maxPeers = makeOpts.MaxPeers
	} else {
		s.maxPeers = 8
	}
	if s.maxPeers > absoluteMaxPeers {
		s.maxPeers = absoluteMaxPeers
	}

	// Sprint 2: wire HeartbeatService
	hbCfg := failure.DefaultConfig()
	s.HeartbeatSvc = failure.NewHeartbeatService(
		hbCfg,
		canonAddr,
		// getPeers closure: returns listen addresses of outbound peers only
		func() []string {
			s.peerLock.RLock()
			defer s.peerLock.RUnlock()
			addrs := make([]string, 0, len(s.peers))
			for addr, p := range s.peers {
				if p.Outbound() {
					addrs = append(addrs, addr)
				}
			}
			return addrs
		},
		// sendHeartbeat closure: sends a MessageHeartbeat to the given address
		func(addr string) error {
			return s.sendToAddr(addr, &Message{
				Payload: &MessageHeartbeat{
					From:      canonAddr,
					Timestamp: time.Now().UnixNano(),
				},
			})
		},
		// onSuspect: log warning
		func(addr string) {
			log.Printf("[failure] SUSPECT: peer %s — phi exceeded threshold", addr)
		},
		// onDead: remove from ring and trigger re-replication
		func(addr string) {
			log.Printf("[failure] DEAD: peer %s — removing from ring", addr)
			s.HashRing.RemoveNode(normalizeAddr(addr))
			if s.Rebalancer != nil {
				s.Rebalancer.OnNodeLeft(addr)
			}
		},
	)

	// Sprint 2: wire HandoffService
	hintDir := filepath.Join(pathAddr+"_network", ".hints")
	hintStore, err := handoff.NewStore(hintDir, handoff.DefaultMaxHints, handoff.DefaultMaxAge, handoff.DefaultMaxDataSize)
	if err != nil {
		log.Printf("[MakeServer] failed to create hint store: %v (hinted handoff disabled)", err)
	} else {
		s.HandoffSvc = handoff.NewHandoffService(hintStore,
			// deliver closure: read data from CAS and send to target.
			// Block-waits during active uploads to avoid HoL blocking on the
			// shared QUIC connection. We block (not skip) to preserve Attempts.
			func(h handoff.Hint) error {
				if s.activeUploads.Load() > 0 {
					log.Printf("[handoff] upload active, deferring delivery of key=%s to %s", h.Key, h.TargetAddr)
					for s.activeUploads.Load() > 0 {
						time.Sleep(500 * time.Millisecond)
					}
				}
				data, err := s.readFile(h.Key)
				if err != nil {
					return fmt.Errorf("handoff deliver: read CAS key %s: %w", h.Key, err)
				}
				msg := &Message{
					Payload: &MessageStoreFile{
						Key:  h.Key,
						Size: int64(len(data)),
					},
				}
				buf := new(bytes.Buffer)
				if err := gob.NewEncoder(buf).Encode(msg); err != nil {
					return err
				}
				// Use data-plane peer when available (Two-Port), fallback to control.
				peer, ok := s.getDataPeer(h.TargetAddr)
				if !ok {
					return fmt.Errorf("handoff deliver: peer %s not connected", h.TargetAddr)
				}
				return peer.SendStream(buf.Bytes(), data)
			},
		)
	}

	// Sprint 2: wire Rebalancer
	s.Rebalancer = rebalance.New(
		canonAddr,
		s.HashRing,
		opts.metaData,
		s.readFile,
		// sendFile closure: replicate a file to a target node.
		// Block-waits during active uploads to avoid HoL blocking. We block
		// (not error) because failed migrations aren't retried by the rebalancer.
		func(targetAddr, key string, data []byte) error {
			if s.activeUploads.Load() > 0 {
				log.Printf("[rebalance] upload active, deferring migration of key=%s to %s", key, targetAddr)
				for s.activeUploads.Load() > 0 {
					time.Sleep(500 * time.Millisecond)
				}
			}
			msg := &Message{
				Payload: &MessageStoreFile{
					Key:  key,
					Size: int64(len(data)),
				},
			}
			buf := new(bytes.Buffer)
			if err := gob.NewEncoder(buf).Encode(msg); err != nil {
				return err
			}

			// Use data-plane peer when available (Two-Port), fallback to control.
			peer, ok := s.getDataPeer(targetAddr)
			if !ok {
				return fmt.Errorf("rebalance: peer %s not connected", targetAddr)
			}
			return peer.SendStream(buf.Bytes(), data)
		},
	)
	s.Rebalancer.SetManifestFuncs(
		// readManifest: serialise the manifest from metadata to JSON
		func(fileKey string) ([]byte, bool) {
			manifest, ok := opts.metaData.GetManifest(fileKey)
			if !ok {
				return nil, false
			}
			data, err := json.Marshal(manifest)
			if err != nil {
				return nil, false
			}
			return data, true
		},
		// sendManifest: send MessageStoreManifest to a peer
		func(targetAddr, fileKey string, manifestJSON []byte) error {
			return s.sendToAddr(targetAddr, &Message{Payload: &MessageStoreManifest{
				FileKey:      fileKey,
				ManifestJSON: manifestJSON,
			}})
		},
	)
	s.Rebalancer.SetShouldSkipKey(isSwarmCacheKey)

	// Sprint 4: wire Quorum coordinator.
	s.Quorum = quorum.New(
		quorum.DefaultConfig(),
		canonAddr,
		// getTargets: ask the hash ring for N responsible nodes
		func(key string) []string {
			return s.HashRing.GetNodes(key, s.HashRing.ReplicationFactor())
		},
		// sendMsg: deliver a quorum message to a peer
		func(addr string, msg interface{}) error {
			switch m := msg.(type) {
			case *quorum.MessageQuorumWrite:
				return s.sendToAddr(addr, &Message{Payload: &MessageQuorumWrite{
					Key: m.Key, Data: m.Data, Clock: m.Clock,
				}})
			case *quorum.MessageQuorumRead:
				return s.sendToAddr(addr, &Message{Payload: &MessageQuorumRead{Key: m.Key}})
			default:
				return fmt.Errorf("quorum sendMsg: unknown type %T", msg)
			}
		},
		// localWrite: store bytes + update metadata with vclock
		func(key string, data []byte, clock vclock.VectorClock) error {
			if _, err := s.Store.WriteStream(key, bytes.NewReader(data)); err != nil {
				return err
			}
			fm, _ := s.serverOpts.metaData.Get(key)
			fm.VClock = map[string]uint64(clock)
			fm.Timestamp = time.Now().UnixNano()
			return s.serverOpts.metaData.Set(key, fm)
		},
		// localRead: return metadata for the conflict resolver
		func(key string) (vclock.VectorClock, int64, bool) {
			fm, ok := s.serverOpts.metaData.Get(key)
			if !ok {
				return nil, 0, false
			}
			return vclock.VectorClock(fm.VClock), fm.Timestamp, true
		},
	)

	// Sprint 4: wire AntiEntropyService.
	metaAsKeyer := opts.metaData
	s.AntiEntropy = merkle.NewAntiEntropyService(
		canonAddr,
		10*time.Minute,
		// getKeys: all locally-stored file keys, excluding chunk: keys
		// which are managed by their own replication paths (upload-time
		// replicateChunk or swarm on-demand serving).
		func() []string {
			type keyer interface{ Keys() []string }
			if k, ok := metaAsKeyer.(keyer); ok {
				all := k.Keys()
				filtered := make([]string, 0, len(all))
				for _, key := range all {
					if !isSwarmCacheKey(key) {
						filtered = append(filtered, key)
					}
				}
				return filtered
			}
			return nil
		},
		// getPeers: replica partners from the hash ring
		func() []string {
			return s.HashRing.Members()
		},
		// sendMsg: translate anti-entropy messages to wire protocol
		func(addr string, msg interface{}) error {
			switch m := msg.(type) {
			case *merkle.MessageMerkleSync:
				return s.sendToAddr(addr, &Message{Payload: &MessageMerkleSync{
					From: m.From, RootHash: m.RootHash,
				}})
			case *merkle.MessageMerkleDiffResponse:
				return s.sendToAddr(addr, &Message{Payload: &MessageMerkleDiffResponse{
					From: m.From, AllKeys: m.AllKeys,
				}})
			default:
				return fmt.Errorf("merkle sendMsg: unknown type %T", msg)
			}
		},
		// onNeedKey: peer has a key we're missing — request it.
		// Yields during active uploads to avoid competing for QUIC bandwidth.
		func(addr, key string) {
			if isSwarmCacheKey(key) {
				return // chunk keys handled by swarm/upload replication, not anti-entropy
			}
			if s.activeUploads.Load() > 0 {
				return // yield: foreground upload has priority
			}
			log.Printf("[anti-entropy] requesting missing key %s from %s", key, addr)
			_ = s.sendToAddr(addr, &Message{Payload: &MessageGetFile{Key: key}})
		},
		// onSendKey: we have a key the peer is missing — re-replicate it.
		// Yields entirely when uploads are active to avoid HoL blocking
		// on the shared QUIC connection and rate limiter token bucket.
		func(addr, key string) {
			if isSwarmCacheKey(key) {
				return // chunk keys handled by swarm/upload replication, not anti-entropy
			}
			if s.activeUploads.Load() > 0 {
				return // yield: foreground upload has priority
			}
			log.Printf("[anti-entropy] replicating missing key %s to %s", key, addr)
			data, err := s.readFile(key)
			if err != nil {
				log.Printf("[anti-entropy] failed to read %s: %v", key, err)
				return
			}
			s.storeHint(addr, key, int64(len(data)))
		},
	)

	// Sprint 7: wire Selector + Downloader for parallel chunk downloads.
	s.Selector = selector.New()
	dmCfg := downloader.DefaultConfig()
	s.Downloader = downloader.New(
		dmCfg,
		s.Selector,
		// fetch: swarm-aware — tries local CAS first, then swarm RPC, falls back to hash ring
		func(ctx context.Context, fileKey string, chunkIndex int, storageKey, peerAddr string) ([]byte, error) {
			// Local CAS check (secondary seeder cache).
			if s.Store.Has(storageKey) {
				_, r, err := s.Store.ReadStream(storageKey)
				if err == nil {
					defer r.Close()
					return io.ReadAll(r)
				}
			}
			// Try swarm chunk fetch via bidirectional RPC (single-stream request-response).
			if fileKey != "" {
				manifest, ok := s.serverOpts.metaData.GetManifest(fileKey)
				if ok && manifest != nil && manifest.SeedMode && chunkIndex < len(manifest.Chunks) {
					data, err := s.fetchChunkSwarmBidi(ctx, fileKey, chunkIndex, manifest.Chunks[chunkIndex].EncHash, peerAddr)
					if err == nil {
						// Async CAS cache for re-seeding.
						select {
						case s.casWriteCh <- casWriteRequest{key: storageKey, data: append([]byte(nil), data...)}:
						default:
						}
					}
					return data, err
				}
			}
			// Fallback: legacy hash-ring fetch (MessageGetFile).
			return s.fetchChunkFromPeer(ctx, storageKey, peerAddr)
		},
		// decrypt: decrypt chunk data using the caller-provided DEK
		func(storageKey string, chunkIndex int, data []byte, dek []byte) ([]byte, error) {
			if dek == nil {
				return data, nil // plaintext path — no decryption
			}
			var plain bytes.Buffer
			if err := crypto.DecryptStreamWithDEK(bytes.NewReader(data), &plain, dek, uint64(chunkIndex)); err != nil {
				return nil, err
			}
			return plain.Bytes(), nil
		},
		// getPeers: swarm-aware — queries provider records, falls back to hash ring
		func(fileKey string, chunkIndex int, storageKey string) []string {
			// 1. Query provider records from hash ring nodes.
			if fileKey != "" {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				providers := s.getProviders(ctx, fileKey)
				cancel()
				if len(providers) > 0 {
					addrs := make([]string, len(providers))
					for i, p := range providers {
						addrs[i] = p.Addr
					}
					return addrs
				}
				// 2. Fallback: manifest's OriginalSeeder field.
				if manifest, ok := s.serverOpts.metaData.GetManifest(fileKey); ok && manifest != nil && manifest.OriginalSeeder != "" {
					return []string{manifest.OriginalSeeder}
				}
			}
			// 3. Fallback to hash ring (legacy CAS uploads).
			return s.HashRing.GetNodes(storageKey, s.HashRing.ReplicationFactor())
		},
	)
	s.Downloader.SetReleaseBuf(PutChunkBuf)

	// Phase 3: wire NAT traversal (STUN-based public address discovery).
	if makeOpts == nil || !makeOpts.DisableSTUN {
		s.NATService = nat.New(nat.DefaultConfig())
		log.Println("[MakeServer] STUN-based NAT traversal enabled")
	} else {
		log.Println("[MakeServer] STUN-based NAT traversal disabled (--no-stun)")
	}

	// Sprint 5: wire health server on port+1000 (e.g. :3000 → :4000).
	s.startedAt = time.Now()
	healthPort := deriveHealthPort(listenAddr)
	reg := prometheus.NewRegistry()
	metrics.Reset()
	metrics.Init(reg)
	s.HealthSrv = health.New(healthPort, s.HealthStatus, reg)
	if err := s.HealthSrv.Start(); err != nil {
		log.Printf("[MakeServer] health server on %s failed to start: %v (continuing without health endpoint)", healthPort, err)
		s.HealthSrv = nil
	} else {
		log.Printf("[MakeServer] health server listening on %s", healthPort)
	}

	// Sprint 3: wire ClusterState and GossipService.
	s.Cluster = membership.New(canonAddr)

	// Inject identity metadata into local node's gossip state + server field.
	var selfFingerprint string
	if makeOpts != nil && makeOpts.IdentityMeta != nil {
		s.identityMeta = makeOpts.IdentityMeta
		s.Cluster.SetMetadata(canonAddr, makeOpts.IdentityMeta)
		selfFingerprint = makeOpts.IdentityMeta["fingerprint"]
		if len(makeOpts.X25519Priv) > 0 {
			s.x25519Priv = makeOpts.X25519Priv
		}
	}

	gossipCfg := gossip.DefaultConfig()
	s.GossipSvc = gossip.New(
		gossipCfg,
		canonAddr,
		s.Cluster,
		// getPeers: all nodes known to the hash ring (not just currently connected)
		func() []string {
			return s.HashRing.Members()
		},
		// sendMsg: translate gossip messages into server wire protocol
		func(addr string, msg interface{}) error {
			switch m := msg.(type) {
			case *gossip.MessageGossipDigest:
				return s.sendToAddr(addr, &Message{
					Payload: &MessageGossipDigest{
						From:    m.From,
						Digests: m.Digests,
					},
				})
			case *gossip.MessageGossipResponse:
				return s.sendToAddr(addr, &Message{
					Payload: &MessageGossipResponse{
						From:     m.From,
						Full:     m.Full,
						MyDigest: m.MyDigest,
					},
				})
			default:
				return fmt.Errorf("gossip sendMsg: unknown message type %T", msg)
			}
		},
		// onNewPeer: dial a peer discovered through gossip.
		// Guards against concurrent duplicate dials via dialingSet.
		func(addr string) {
			self := s.serverOpts.transport.Addr()

			// Reject self-connections: if the discovered addr has the same port
			// as us and its host is one of our own network interfaces, skip it.
			// This prevents gossip from triggering loopback dials (e.g. a bare
			// host learning "172.17.0.1:3000" from a Docker-container peer and
			// dialing the Docker bridge gateway which routes back to itself).
			_, selfPort, _ := net.SplitHostPort(self)
			addrHost, addrPort, _ := net.SplitHostPort(addr)
			if addrPort == selfPort && isLocalAddr(addrHost) {
				log.Printf("[TRACE][onNewPeer] SKIP self=%s addr=%s (self-loopback via local interface)", self, addr)
				return
			}

			// Blocklist check — manually disconnected peers stay disconnected
			// (unless they are explicit --peer bootstrap targets).
			_, isBootstrapPeer := s.bootstrapAddrs[addr]
			if s.StateDB != nil && s.StateDB.IsIgnoredPeer(addr) && !isBootstrapPeer {
				log.Printf("[TRACE][onNewPeer] SKIP self=%s addr=%s (ignored/blocklisted by addr)", self, addr)
				return
			}
			// Fingerprint-based blocklist: catches peers that changed IP (DHCP).
			if s.StateDB != nil && s.Cluster != nil && !isBootstrapPeer {
				if node, ok := s.Cluster.GetNode(addr); ok && node.Metadata != nil {
					if fp := node.Metadata["fingerprint"]; fp != "" && s.StateDB.IsIgnoredFingerprint(fp) {
						log.Printf("[TRACE][onNewPeer] SKIP self=%s addr=%s (ignored/blocklisted by fingerprint %s)", self, addr, fp)
						return
					}
				}
			}

			// Check peers map first — fastest path.
			s.peerLock.RLock()
			existing, already := s.peers[addr]
			s.peerLock.RUnlock()
			log.Printf("[TRACE][onNewPeer] self=%s addr=%s already_connected=%v existing_ptr=%p", self, addr, already, existing)
			if already {
				log.Printf("[TRACE][onNewPeer] SKIP self=%s addr=%s (already in peers map)", self, addr)
				return
			}

			// Grab exclusive dialing lock to prevent concurrent dials to the same addr.
			s.dialingLock.Lock()
			if _, dialing := s.dialingSet[addr]; dialing {
				s.dialingLock.Unlock()
				log.Printf("[TRACE][onNewPeer] SKIP self=%s addr=%s (dial already in progress)", self, addr)
				return
			}
			s.dialingSet[addr] = struct{}{}
			s.dialingLock.Unlock()

			// Always remove from set when done (success or failure).
			defer func() {
				s.dialingLock.Lock()
				delete(s.dialingSet, addr)
				s.dialingLock.Unlock()
			}()

			log.Printf("[TRACE][onNewPeer] DIALLING self=%s -> addr=%s ring_size_before=%d", self, addr, s.HashRing.Size())
			if err := s.dialPeer(addr); err != nil {
				log.Printf("[TRACE][onNewPeer] DIAL FAILED self=%s -> addr=%s err=%v", self, addr, err)
			} else {
				log.Printf("[TRACE][onNewPeer] DIAL OK self=%s -> addr=%s", self, addr)
			}
		},
	)

	// Set selfFingerprint on gossip for fingerprint-based self-detection
	// (prevents Docker/NAT address collision from swallowing our own metadata).
	if selfFingerprint != "" {
		s.GossipSvc.SetSelfFingerprint(selfFingerprint)
	}

	return s
}
