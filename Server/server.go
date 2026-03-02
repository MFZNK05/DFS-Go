package Server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
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
	"strings"
	"sync"
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
	"github.com/Faizan2005/DFS-Go/Observability/health"
	"github.com/Faizan2005/DFS-Go/Observability/logging"
	"github.com/Faizan2005/DFS-Go/Observability/metrics"
	peer2peer "github.com/Faizan2005/DFS-Go/Peer2Peer"
	"github.com/Faizan2005/DFS-Go/Server/downloader"
	storage "github.com/Faizan2005/DFS-Go/Storage"
	"github.com/Faizan2005/DFS-Go/Storage/chunker"
	"github.com/Faizan2005/DFS-Go/Storage/compression"
	"github.com/Faizan2005/DFS-Go/factory"
	"github.com/prometheus/client_golang/prometheus"
)

// Package-level sync.Pools for the StoreData upload pipeline (Fix 3).
// Reusing these large slabs across chunks eliminates GC stop-the-world pauses
// (~100-200 ms each) that dominated small-file upload latency.
var (
	// chunkReadBufPool pools the 4 MiB read buffer used by ChunkReaderWithPool.
	chunkReadBufPool = sync.Pool{
		New: func() any { b := make([]byte, chunker.DefaultChunkSize); return &b },
	}
	// compBufPool pools the compression output slab used by CompressChunkWithPool.
	compBufPool = sync.Pool{
		New: func() any { b := make([]byte, 0, chunker.DefaultChunkSize); return &b },
	}
	// encBufPool pools the GCM ciphertext slab used by EncryptStreamWithPool.
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
	Encryption        *crypto.EncryptionService
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
	pendingFile  map[string]chan io.Reader
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
}

type Message struct {
	Payload any
}

type MessageStoreFile struct {
	Key          string
	Size         int64
	EncryptedKey string
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
	Key          string
	EncryptedKey string
	Data         []byte
	Clock        map[string]uint64
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
	Key          string
	From         string
	Found        bool
	Clock        map[string]uint64
	Timestamp    int64
	EncryptedKey string
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
// ManifestJSON is empty when the peer does not have the manifest.
type MessageManifestResponse struct {
	FileKey      string
	ManifestJSON []byte // nil/empty if not found
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
	return 0
}

func (s *Server) GetData(key string) (io.Reader, error) {
	t0 := time.Now()

	// Check whether this key is stored as chunked.
	fm, hasMeta := s.serverOpts.metaData.Get(key)
	log.Printf("[LIFECYCLE] GET_DATA: key=%s hasMeta=%v Chunked=%v", key, hasMeta, fm.Chunked)
	if hasMeta && fm.Chunked {
		defer func() { metrics.RecordGet("local", nil, time.Since(t0)) }()
		return s.getChunked(key)
	}

	// Legacy single-blob path (backwards compatibility with pre-Sprint-6 data).
	if s.Store.Has(key) {
		log.Printf("GET_DATA: key '%s' found locally (legacy blob)", key)
		defer func() { metrics.RecordGet("local", nil, time.Since(t0)) }()
		_, r, err := s.Store.ReadStream(key)
		if err != nil {
			return nil, err
		}
		defer r.Close()

		if !hasMeta {
			return nil, fmt.Errorf("metadata not found for key '%s'", key)
		}
		decodedKey, err := hex.DecodeString(fm.EncryptedKey)
		if err != nil {
			return nil, fmt.Errorf("failed to decode encrypted key for '%s': %w", key, err)
		}
		var decryptedBuf bytes.Buffer
		if err := s.serverOpts.Encryption.DecryptStream(r, &decryptedBuf, decodedKey); err != nil {
			return nil, fmt.Errorf("failed to decrypt file for key '%s': %w", key, err)
		}
		return &decryptedBuf, nil
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

	return s.getChunked(key)
}

// getChunked reassembles a chunked file. When a Downloader is wired in
// (Sprint 7) it fetches all chunks in parallel; otherwise it falls back to a
// serial loop. Either path decrypts + decompresses + verifies each chunk.
func (s *Server) getChunked(key string) (io.Reader, error) {
	manifest, ok := s.serverOpts.metaData.GetManifest(key)
	if !ok {
		return nil, fmt.Errorf("getChunked: no manifest for key '%s'", key)
	}
	log.Printf("[LIFECYCLE] GET_CHUNKED: key=%s numChunks=%d usingDownloader=%v", key, len(manifest.Chunks), s.Downloader != nil)

	// Sprint 7: parallel path via Downloader.
	if s.Downloader != nil {
		var out bytes.Buffer
		if err := s.Downloader.Download(manifest, &out, nil); err != nil {
			return nil, fmt.Errorf("getChunked: %w", err)
		}
		go s.readRepair(key)
		return &out, nil
	}

	// Serial fallback (single-node or Downloader not yet wired).
	chunks := make([]chunker.Chunk, len(manifest.Chunks))

	for i, info := range manifest.Chunks {
		storageKey := chunker.ChunkStorageKey(info.EncHash)

		var encData []byte

		if s.Store.Has(storageKey) {
			_, r, err := s.Store.ReadStream(storageKey)
			if err != nil {
				return nil, fmt.Errorf("getChunked: read local chunk %d: %w", info.Index, err)
			}
			encData, err = io.ReadAll(r)
			r.Close()
			if err != nil {
				return nil, fmt.Errorf("getChunked: read bytes chunk %d: %w", info.Index, err)
			}
		} else {
			data, err := s.fetchChunkFromPeers(storageKey)
			if err != nil {
				return nil, fmt.Errorf("getChunked: fetch chunk %d from peers: %w", info.Index, err)
			}
			encData = data
			chunkMeta, _ := s.serverOpts.metaData.Get(storageKey)
			_, _ = s.Store.WriteStream(storageKey, bytes.NewReader(encData))
			_ = s.serverOpts.metaData.Set(storageKey, chunkMeta)
		}

		chunkMeta, ok := s.serverOpts.metaData.Get(storageKey)
		if !ok {
			return nil, fmt.Errorf("getChunked: no metadata for storageKey %s", storageKey)
		}
		encKey, err := hex.DecodeString(chunkMeta.EncryptedKey)
		if err != nil {
			return nil, fmt.Errorf("getChunked: decode enc key chunk %d: %w", info.Index, err)
		}

		var plain bytes.Buffer
		if err := s.serverOpts.Encryption.DecryptStream(bytes.NewReader(encData), &plain, encKey); err != nil {
			return nil, fmt.Errorf("getChunked: decrypt chunk %d: %w", info.Index, err)
		}

		plainBytes := plain.Bytes()

		// Sprint 7: decompress if the chunk was compressed at upload time.
		if info.Compressed {
			plainBytes, err = compression.DecompressChunk(plainBytes)
			if err != nil {
				return nil, fmt.Errorf("getChunked: decompress chunk %d: %w", info.Index, err)
			}
		}

		got := sha256.Sum256(plainBytes)
		gotHex := hex.EncodeToString(got[:])
		if gotHex != info.Hash {
			return nil, fmt.Errorf("getChunked: integrity check failed for chunk %d (want %s got %s)",
				info.Index, info.Hash, gotHex)
		}

		chunks[i] = chunker.Chunk{
			Index: info.Index,
			Hash:  got,
			Size:  int64(len(plainBytes)),
			Data:  plainBytes,
		}
	}

	var out bytes.Buffer
	if err := chunker.Reassemble(chunks, &out); err != nil {
		return nil, fmt.Errorf("getChunked: reassemble: %w", err)
	}

	go s.readRepair(key)
	return &out, nil
}

// fetchChunkFromPeers requests a chunk's encrypted bytes from the ring-
// responsible peers and returns the raw encrypted data.
func (s *Server) fetchChunkFromPeers(storageKey string) ([]byte, error) {
	selfAddr := s.serverOpts.transport.Addr()
	targetNodes := s.HashRing.GetNodes(storageKey, s.HashRing.ReplicationFactor())

	// If another goroutine is already fetching this key, share its channel
	// rather than overwriting it. Overwriting would orphan the first caller's
	// select, causing it to time out even when a response arrives.
	s.mu.Lock()
	ch, alreadyFetching := s.pendingFile[storageKey]
	if !alreadyFetching {
		ch = make(chan io.Reader, 1)
		s.pendingFile[storageKey] = ch
	}
	s.mu.Unlock()

	if alreadyFetching {
		// Another goroutine owns this fetch — wait for its result.
		select {
		case r := <-ch:
			if r == nil {
				return nil, fmt.Errorf("fetchChunkFromPeers: nil reader for '%s'", storageKey)
			}
			// Put it back so any other waiter also gets it (best-effort).
			select {
			case ch <- r:
			default:
			}
			return io.ReadAll(r)
		case <-time.After(10 * time.Second):
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
	case r := <-ch:
		if r == nil {
			return nil, fmt.Errorf("nil reader for chunk '%s'", storageKey)
		}
		return io.ReadAll(r)
	case <-time.After(10 * time.Second):
		return nil, fmt.Errorf("timeout fetching chunk '%s'", storageKey)
	}
}

// fetchChunkFromPeer fetches the encrypted bytes for storageKey from one
// specific peer. Used by the Downloader which has already selected the best
// peer via the Selector — so we send to that peer only.
// When peerAddr == selfAddr the chunk is read directly from local storage.
func (s *Server) fetchChunkFromPeer(storageKey, peerAddr string) ([]byte, error) {
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
		ch = make(chan io.Reader, 1)
		s.pendingFile[storageKey] = ch
	}
	s.mu.Unlock()

	if alreadyFetching {
		select {
		case r := <-ch:
			if r == nil {
				return nil, fmt.Errorf("fetchChunkFromPeer: nil reader for '%s'", storageKey)
			}
			select {
			case ch <- r:
			default:
			}
			return io.ReadAll(r)
		case <-time.After(10 * time.Second):
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
	case r := <-ch:
		if r == nil {
			return nil, fmt.Errorf("fetchChunkFromPeer: nil reader for '%s'", storageKey)
		}
		return io.ReadAll(r)
	case <-time.After(10 * time.Second):
		return nil, fmt.Errorf("fetchChunkFromPeer: timeout for '%s'", storageKey)
	}
}

// fetchManifestFromPeers retrieves a ChunkManifest from peers for key.
// Uses MessageGetManifest / MessageManifestResponse — a direct JSON exchange
// that does NOT go through the encrypted-stream path.
func (s *Server) fetchManifestFromPeers(key string) (*chunker.ChunkManifest, error) {
	selfAddr := s.serverOpts.transport.Addr()
	targetNodes := s.HashRing.GetNodes(key, s.HashRing.ReplicationFactor())

	// Use a sentinel key in pendingFile to receive the async response.
	pendingKey := "__manifest__" + key
	ch := make(chan io.Reader, len(targetNodes))
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
		return nil, fmt.Errorf("fetchManifest: no reachable peers for '%s'", key)
	}

	select {
	case r := <-ch:
		data, err := io.ReadAll(r)
		if err != nil {
			return nil, fmt.Errorf("fetchManifest: read response: %w", err)
		}
		var manifest chunker.ChunkManifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			return nil, fmt.Errorf("fetchManifest: unmarshal: %w", err)
		}
		return &manifest, nil
	case <-time.After(10 * time.Second):
		return nil, fmt.Errorf("timeout fetching manifest for '%s'", key)
	}
}

// encryptedChunk carries all per-chunk results from Stage 1 (compress+encrypt)
// to Stage 2 (local store + replicate) of the StoreData pipeline.
type encryptedChunk struct {
	info       chunker.ChunkInfo
	encData    []byte
	encKeyHex  string
	storageKey string
	targets    []string
}

func (s *Server) StoreData(key string, w io.Reader) error {
	t0 := time.Now()
	log.Println("STORE_DATA: Starting chunked storage for key:", key)
	defer func() { metrics.RecordStore("store", nil, time.Since(t0)) }()

	selfAddr := s.serverOpts.transport.Addr()
	replFactor := s.HashRing.ReplicationFactor()

	// encCh connects Stage 1 (compress+encrypt) with Stage 2 (store+replicate).
	// Buffered=2 lets Stage 1 stay up to 1 chunk ahead of Stage 2, overlapping
	// CPU work (encryption) with network I/O (replication).
	encCh := make(chan encryptedChunk, 2)
	stage1ErrCh := make(chan error, 1)

	// Stage 1: read → compress → encrypt → push to encCh.
	go func() {
		defer close(encCh)
		chunkCh, errCh := chunker.ChunkReaderWithPool(w, chunker.DefaultChunkSize, &chunkReadBufPool)
		for chunk := range chunkCh {
			toEncrypt := chunk.Data
			wasCompressed := false
			if compression.ShouldCompress(chunk.Data) {
				if c, wc, err := compression.CompressChunkWithPool(chunk.Data, compression.LevelFastest, &compBufPool); err == nil && wc {
					toEncrypt = c
					wasCompressed = true
				}
			}

			// Encrypt into an in-memory buffer using the pooled ciphertext slab.
			var encBuf bytes.Buffer
			encKey, err := s.serverOpts.Encryption.EncryptStreamWithPool(bytes.NewReader(toEncrypt), &encBuf, &encBufPool)
			if err != nil {
				stage1ErrCh <- fmt.Errorf("STORE_DATA: encrypt chunk %d: %w", chunk.Index, err)
				return
			}
			encData := encBuf.Bytes()

			encHashRaw := sha256.Sum256(encData)
			encHashHex := hex.EncodeToString(encHashRaw[:])
			storageKey := chunker.ChunkStorageKey(encHashHex)
			encKeyHex := hex.EncodeToString(encKey)
			targets := s.HashRing.GetNodes(storageKey, replFactor)

			log.Printf("STORE_DATA: chunk %d encrypted (compressed=%v storageKey=%s)", chunk.Index, wasCompressed, storageKey)

			encCh <- encryptedChunk{
				info: chunker.ChunkInfo{
					Index:      chunk.Index,
					Hash:       hex.EncodeToString(chunk.Hash[:]),
					Size:       chunk.Size,
					EncHash:    encHashHex,
					Compressed: wasCompressed,
				},
				encData:    encData,
				encKeyHex:  encKeyHex,
				storageKey: storageKey,
				targets:    targets,
			}
		}
		stage1ErrCh <- <-errCh
	}()

	// Stage 2: local store + replicate. Runs concurrently with Stage 1.
	var chunkInfos []chunker.ChunkInfo
	for ec := range encCh {
		// Store locally (skip if already present — dedup).
		if !s.Store.Has(ec.storageKey) {
			if _, err := s.Store.WriteStream(ec.storageKey, bytes.NewReader(ec.encData)); err != nil {
				return fmt.Errorf("STORE_DATA: local store chunk %d: %w", ec.info.Index, err)
			}
			fm, _ := s.serverOpts.metaData.Get(ec.storageKey)
			fm.EncryptedKey = ec.encKeyHex
			fm.Timestamp = time.Now().UnixNano()
			if err := s.serverOpts.metaData.Set(ec.storageKey, fm); err != nil {
				return fmt.Errorf("STORE_DATA: metadata chunk %d: %w", ec.info.Index, err)
			}
		}

		log.Printf("[LIFECYCLE] STORE_DATA: chunk stored locally storageKey=%s encKeyHex=%q targets=%v",
			ec.storageKey, ec.encKeyHex, ec.targets)
		s.replicateChunk(selfAddr, ec.storageKey, ec.encKeyHex, ec.encData, ec.targets)
		chunkInfos = append(chunkInfos, ec.info)
		log.Printf("STORE_DATA: chunk %d replicated (storageKey=%s)", ec.info.Index, ec.storageKey)
	}

	// Check for error from Stage 1.
	if err := <-stage1ErrCh; err != nil {
		return fmt.Errorf("STORE_DATA: stage1: %w", err)
	}

	// Build and persist the manifest locally.
	manifest := chunker.BuildManifest(key, chunkInfos, time.Now().UnixNano())
	if err := s.serverOpts.metaData.SetManifest(key, manifest); err != nil {
		return fmt.Errorf("STORE_DATA: store manifest: %w", err)
	}

	// Manifest replication is fire-and-forget (non-blocking).
	s.replicateManifest(key, selfAddr, manifest)

	// Update top-level FileMeta.
	topFm, _ := s.serverOpts.metaData.Get(key)
	if topFm.VClock == nil {
		topFm.VClock = make(map[string]uint64)
	}
	topFm.VClock[selfAddr]++
	topFm.Chunked = true
	topFm.Timestamp = time.Now().UnixNano()
	if err := s.serverOpts.metaData.Set(key, topFm); err != nil {
		return fmt.Errorf("STORE_DATA: update file meta: %w", err)
	}

	log.Printf("STORE_DATA: key '%s' stored as %d chunks", key, len(chunkInfos))
	return nil
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

func (s *Server) replicateChunk(selfAddr, storageKey, encKeyHex string, encData []byte, targetNodes []string) {
	msg := &Message{
		Payload: &MessageStoreFile{
			Key:          storageKey,
			Size:         int64(len(encData)),
			EncryptedKey: encKeyHex,
		},
	}
	buf := new(bytes.Buffer)
	if err := gob.NewEncoder(buf).Encode(msg); err != nil {
		log.Printf("replicateChunk: encode: %v", err)
		return
	}
	msgBytes := buf.Bytes()

	s.peerLock.RLock()
	peers := make(map[string]peer2peer.Peer, len(s.peers))
	for a, p := range s.peers {
		peers[a] = p
	}
	s.peerLock.RUnlock()

	log.Printf("REPLICATE_CHUNK: key=%s selfAddr=%s targets=%v", storageKey, selfAddr, targetNodes)

	type result struct{ err error }
	needed := 0
	// Buffer = len(targetNodes) so goroutines never block on send after caller returns.
	resultCh := make(chan result, len(targetNodes))

	for _, nodeAddr := range targetNodes {
		if nodeAddr == selfAddr {
			continue
		}
		p, ok := peers[nodeAddr]
		if !ok {
			log.Printf("REPLICATE_CHUNK: peer %s NOT FOUND, storing hint", nodeAddr)
			s.storeHint(nodeAddr, storageKey, encKeyHex, encData)
			metrics.RecordReplication("hint")
			continue
		}
		needed++
		go func(addr string, peer peer2peer.Peer) {
			err := peer.SendStream(msgBytes, encData)
			if err != nil {
				log.Printf("REPLICATE_CHUNK: SendStream to %s failed: %v, storing hint", addr, err)
				s.storeHint(addr, storageKey, encKeyHex, encData)
				metrics.RecordReplication("hint")
			} else {
				log.Printf("REPLICATE_CHUNK: SendStream to %s OK for key=%s", addr, storageKey)
				metrics.RecordReplication("ok")
			}
			resultCh <- result{err}
		}(nodeAddr, p)
	}

	// Return as soon as writeQuorum(needed) successes arrive.
	// Remaining goroutines drain into the buffered channel without leaking.
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
		s.pendingFile[probeKey] = make(chan io.Reader, 1)
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
			encKey, data, err := s.readEncryptedFile(key)
			if err != nil {
				log.Printf("[readRepair] failed to read key=%s for repair: %v", key, err)
				continue
			}
			s.storeHint(nodeAddr, key, encKey, data)
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
		fm.EncryptedKey = msg.EncryptedKey
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
		Key:          msg.Key,
		From:         selfAddr,
		Found:        found,
		Clock:        fm.VClock,
		Timestamp:    fm.Timestamp,
		EncryptedKey: fm.EncryptedKey,
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

// storeHint saves a hinted handoff entry for a key that could not be delivered.
func (s *Server) storeHint(targetAddr, key, encKeyHex string, data []byte) {
	if s.HandoffSvc == nil {
		return
	}
	s.HandoffSvc.StoreHint(handoff.Hint{
		Key:          key,
		TargetAddr:   targetAddr,
		EncryptedKey: encKeyHex,
		Data:         data,
		CreatedAt:    time.Now(),
	})
}

// readEncryptedFile reads the raw encrypted bytes for key from local storage.
// Used by the Rebalancer's ReadFileFunc closure.
func (s *Server) readEncryptedFile(key string) (encKey string, data []byte, err error) {
	fm, ok := s.serverOpts.metaData.Get(key)
	if !ok {
		return "", nil, fmt.Errorf("readEncryptedFile: no metadata for key %s", key)
	}

	_, r, err := s.Store.ReadStream(key)
	if err != nil {
		return "", nil, fmt.Errorf("readEncryptedFile: open %s: %w", key, err)
	}
	defer r.Close()

	data, err = io.ReadAll(r)
	if err != nil {
		return "", nil, fmt.Errorf("readEncryptedFile: read %s: %w", key, err)
	}

	return fm.EncryptedKey, data, nil
}

// healthStatus returns the current health snapshot for /health endpoint.
func (s *Server) healthStatus() health.Status {
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
	return s.serverOpts.metaData.GetManifest(key)
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

	s.peerLock.RLock()
	_, inPeers := s.peers[addr]
	s.peerLock.RUnlock()
	log.Printf("[TRACE][handleLeaving] self=%s addr=%s inPeers=%v ring_size_before=%d", self, addr, inPeers, s.HashRing.Size())

	s.HashRing.RemoveNode(addr)
	log.Printf("[TRACE][handleLeaving] self=%s addr=%s ring_size_after=%d", self, addr, s.HashRing.Size())

	if s.Cluster != nil {
		info, ok := s.Cluster.GetNode(addr)
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
			self, addr, gen, msgGen, ok, info.Generation, info.State)
		s.Cluster.UpdateState(addr, membership.StateLeft, gen)
	}

	// Remove from local peer map so heartbeats stop.
	// Also clean up the announceAdded reverse-mapping so OnPeerDisconnect
	// doesn't attempt a second (redundant) ring removal and doesn't leak memory.
	s.peerLock.Lock()
	if peer, ok := s.peers[addr]; ok {
		log.Printf("[TRACE][handleLeaving] self=%s closing peer ptr=%p for addr=%s", self, peer, addr)
		_ = peer.Close()
		delete(s.peers, addr)
	}
	delete(s.announceAdded, addr)
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

	return &Server{
		peers:      map[string]peer2peer.Peer{},
		serverOpts: opts,
		Store:      storage.NewStore(StoreOpts),
		HashRing: hashring.New(&hashring.Config{
			ReplicationFactor: replFactor,
		}),
		quitch:      make(chan struct{}),
		pendingFile:   make(map[string]chan io.Reader),
		dialingSet:    make(map[string]struct{}),
		announceAdded: make(map[string]string),
	}
}

func (s *Server) Run() error {
	err := s.serverOpts.transport.ListenAndAccept()
	if err != nil {
		return err
	}

	// Add self to the hash ring so we participate in key ownership
	selfAddr := s.serverOpts.transport.Addr()
	s.HashRing.AddNode(selfAddr)
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

	if len(s.serverOpts.bootstrapNodes) != 0 {
		err := s.BootstrapNetwork()
		if err != nil {
			return err
		}
	}

	s.loop()
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

			log.Printf("[loop] Decoded message: %+v\n", message)
			log.Printf("[loop] Payload type after decoding: %T\n", message.Payload)

			if err := s.handleMessage(RPC.From.String(), RPC.Peer, &message, RPC.StreamWg, RPC.StreamReader); err != nil {
				log.Printf("[loop] Error handling message from %s: %v\n", RPC.From.String(), err)
				continue
			}

		case <-s.quitch:
			log.Println("[loop] Quit channel received, exiting loop")
			return
		}
	}
}

func (s *Server) handleMessage(from string, peer peer2peer.Peer, msg *Message, streamWg *sync.WaitGroup, streamReader io.Reader) error {
	log.Printf("[handleMessage] Handling message from %s: Type=%s\n",
		from, strings.TrimPrefix(reflect.TypeOf(msg.Payload).String(), "main."))

	switch m := msg.Payload.(type) {
	case *MessageStoreFile:
		log.Printf("[handleMessage] Detected MessageStoreFile from %s\n", from)
		return s.handleStoreMessage(from, peer, m, streamWg, streamReader)

	case *MessageGetFile:
		log.Printf("[handleMessage] Detected MessageGetFile from %s\n", from)
		return s.handleGetMessage(from, peer, m)

	case *MessageLocalFile:
		log.Printf("[handleMessage] Detected MessageLocalFile from %s\n", from)
		return s.handleLocalMessage(from, peer, m, streamWg, streamReader)

	case *MessageHeartbeat:
		return s.handleHeartbeat(from, peer, m)

	case *MessageHeartbeatAck:
		// RTT measurement could be recorded here in future; for now just log.
		log.Printf("[handleMessage] HeartbeatAck from %s (rtt origin ts=%d)\n", from, m.Timestamp)
		return nil

	case *MessageAnnounce:
		return s.handleAnnounce(from, m)

	case *MessageGossipDigest:
		if s.GossipSvc != nil {
			s.GossipSvc.HandleDigest(from, &gossip.MessageGossipDigest{
				From:    m.From,
				Digests: m.Digests,
			}, func(msg interface{}) error {
				// Convert *gossip.MessageGossipResponse → *MessageGossipResponse
				// (the registered gob type) before encoding.
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
		return nil

	case *MessageGossipResponse:
		if s.GossipSvc != nil {
			s.GossipSvc.HandleResponse(from, &gossip.MessageGossipResponse{
				From:     m.From,
				Full:     m.Full,
				MyDigest: m.MyDigest,
			})
		}
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
				NodeAddr:     m.From,
				Key:          m.Key,
				Found:        m.Found,
				Clock:        vclock.VectorClock(m.Clock),
				Timestamp:    m.Timestamp,
				EncryptedKey: m.EncryptedKey,
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
		// Peer is asking for our copy of the manifest. Reply immediately.
		resp := &MessageManifestResponse{FileKey: m.FileKey}
		if manifest, ok := s.serverOpts.metaData.GetManifest(m.FileKey); ok {
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
			// Wrap JSON in a bytes.Buffer and send on the channel.
			ch <- bytes.NewReader(m.ManifestJSON)
		}
		return nil

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

	if canonical == from {
		return nil
	}

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

	s.peerLock.Lock()
	p, ok := s.peers[from]
	if ok && !p.Outbound() {
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

	if !s.HashRing.HasNode(canonical) {
		s.HashRing.AddNode(canonical)
		log.Printf("[handleAnnounce] added %s to hash ring (size=%d)", canonical, s.HashRing.Size())
		metrics.SetPeerCount(s.outboundPeerCount())
		metrics.SetRingSize(s.HashRing.Size())

		if s.Cluster != nil {
			s.Cluster.AddNode(canonical, nil)
		}
		if s.HandoffSvc != nil {
			s.HandoffSvc.OnPeerReconnect(canonical)
		}
		if s.Rebalancer != nil {
			s.Rebalancer.OnNodeJoined(canonical)
		}
	}
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

	if err = peer.SendStream(buf.Bytes(), fileData); err != nil {
		log.Printf("[HANDLE_GET] Error sending message+stream to %s: %v\n", from, err)
		return err
	}

	log.Printf("HANDLE_GET: Successfully sent %d bytes to peer '%s' for key '%s'", len(fileData), from, msg.Key)

	return nil
}

func (s *Server) handleStoreMessage(from string, peer peer2peer.Peer, msg *MessageStoreFile, streamWg *sync.WaitGroup, streamReader io.Reader) error {
	log.Printf("HANDLE_STORE: Received store request for key %s from %s — EncryptedKey=%q encKeyLen=%d", msg.Key, from, msg.EncryptedKey, len(msg.EncryptedKey))

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

	// Merge EncryptedKey into existing FileMeta (preserves Path set by WriteStream).
	if msg.EncryptedKey != "" {
		fm, _ := s.serverOpts.metaData.Get(msg.Key)
		fm.EncryptedKey = msg.EncryptedKey
		if err := s.serverOpts.metaData.Set(msg.Key, fm); err != nil {
			log.Printf("HANDLE_STORE: Failed to save metadata for key %s: %v", msg.Key, err)
			return err
		}
		// Immediately verify the write persisted.
		if verified, ok := s.serverOpts.metaData.Get(msg.Key); ok {
			log.Printf("[LIFECYCLE] HANDLE_STORE: metadata saved key=%s EncryptedKey=%q", msg.Key, verified.EncryptedKey)
		} else {
			log.Printf("[LIFECYCLE] HANDLE_STORE: WARNING metadata GET failed immediately after SET for key=%s", msg.Key)
		}
	} else {
		log.Printf("[LIFECYCLE] HANDLE_STORE: WARNING EncryptedKey is EMPTY in received message for key=%s from=%s", msg.Key, from)
	}

	log.Printf("HANDLE_STORE: Successfully stored [%d] bytes to %s from %s", n, msg.Key, from)
	return nil
}

func (s *Server) handleLocalMessage(from string, peer peer2peer.Peer, msg *MessageLocalFile, streamWg *sync.WaitGroup, streamReader io.Reader) error {
	if streamReader == nil {
		streamReader = peer.(io.Reader) // TCP fallback
	}
	n, err := s.Store.WriteStream(msg.Key, io.LimitReader(streamReader, msg.Size))
	if err != nil {
		log.Printf("HANDLE_LOCAL: Storage error from %s: %v", from, err)
		return err
	}
	log.Printf("HANDLE_LOCAL: Stored %d bytes from %s", n, from)

	// Read the raw encrypted bytes back from the store.
	// Callers (fetchChunkFromPeers / fetchChunkFromPeer) expect encrypted bytes
	// and perform decryption themselves — do NOT decrypt here.
	_, r, err := s.Store.ReadStream(msg.Key)
	if err != nil {
		log.Printf("HANDLE_LOCAL: Read error for %s: %v", msg.Key, err)
		return err
	}
	defer r.Close()

	log.Printf("HANDLE_LOCAL: Key = %s, ExpectedSize = %d, WrittenSize = %d", msg.Key, msg.Size, n)

	var encBuf bytes.Buffer
	if _, err = io.Copy(&encBuf, r); err != nil {
		log.Printf("HANDLE_LOCAL: Copy error for %s: %v", msg.Key, err)
		return err
	}

	if streamWg != nil {
		streamWg.Done()
	}
	log.Printf("HANDLE_LOCAL: Successfully retrieved '%s' from %s (enc bytes=%d)", msg.Key, from, encBuf.Len())

	s.mu.Lock()
	ch, ok := s.pendingFile[msg.Key]
	s.mu.Unlock()

	if ok {
		ch <- &encBuf
	} else {
		log.Printf("HANDLE_LOCAL: No waiting channel for key %s", msg.Key)
	}

	return nil
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
			s.HashRing.RemoveNode(canonicalToRemove)
			metrics.SetPeerCount(s.outboundPeerCount())
			metrics.SetRingSize(s.HashRing.Size())
			if s.Cluster != nil {
				nextGen := s.Cluster.NextGeneration(canonicalToRemove)
				s.Cluster.UpdateState(canonicalToRemove, membership.StateSuspect, nextGen)
			}
		} else {
			log.Printf("[TRACE][OnPeerDisconnect] INBOUND closed: self=%s addr=%s (no ring change)", self, addr)
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
	s.HashRing.RemoveNode(addr)
	peerCount := s.outboundPeerCount()
	log.Printf("[TRACE][OnPeerDisconnect] RING UPDATED: self=%s removed=%s ring_size=%d peer_count=%d", self, addr, s.HashRing.Size(), peerCount)
	metrics.SetPeerCount(peerCount)
	metrics.SetRingSize(s.HashRing.Size())

	// Sprint 2: clean up failure detector state for this peer.
	if s.HeartbeatSvc != nil {
		s.HeartbeatSvc.RemovePeer(addr)
	}

	// Sprint 3: mark as suspect in membership table.
	if s.Cluster != nil {
		nextGen := s.Cluster.NextGeneration(addr)
		log.Printf("[TRACE][OnPeerDisconnect] updating cluster: self=%s addr=%s to StateSuspect gen=%d", self, addr, nextGen)
		s.Cluster.UpdateState(addr, membership.StateSuspect, nextGen)
	}
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

func (s *Server) BootstrapNetwork() error {
	var wg sync.WaitGroup
	for _, addr := range s.serverOpts.bootstrapNodes {
		if addr == "" {
			continue
		}
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			log.Printf("[Bootstrap] Attempting to connect with remote: %s", addr)
			if err := s.serverOpts.transport.Dial(addr); err != nil {
				log.Printf("[Bootstrap] Failed to dial %s: %v", addr, err)
			} else {
				log.Printf("[Bootstrap] Successfully connected to %s", addr)
			}
		}(addr)
	}
	wg.Wait()
	return nil
}

func (s *Server) OnPeer(p peer2peer.Peer) error {
	addr := p.RemoteAddr().String()
	self := s.serverOpts.transport.Addr()
	log.Printf("[TRACE][OnPeer] self=%s addr=%s outbound=%v ptr=%p", self, addr, p.Outbound(), p)

	// Only outbound peers have a RemoteAddr() equal to the real listen
	// address. Inbound connections carry an ephemeral OS-assigned port
	// and must NOT enter the hash ring or cluster membership table.
	if !p.Outbound() {
		s.peerLock.Lock()
		s.peers[addr] = p
		s.peerLock.Unlock()
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

	s.HashRing.AddNode(addr)
	peerCount := s.outboundPeerCount()
	log.Printf("[TRACE][OnPeer] RING UPDATED self=%s added=%s ring_size=%d peer_count=%d", self, addr, s.HashRing.Size(), peerCount)
	metrics.SetPeerCount(peerCount)
	metrics.SetRingSize(s.HashRing.Size())

	// Tell the peer our canonical listen address so it can remap its inbound
	// connection (stored under our ephemeral port) to our real listen address
	// and add us to its hash ring immediately.
	go func() {
		announceMsg := &Message{Payload: &MessageAnnounce{ListenAddr: self}}
		if err := s.sendToAddr(addr, announceMsg); err != nil {
			log.Printf("[OnPeer] announce to %s failed: %v", addr, err)
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
	if s.Cluster != nil {
		s.Cluster.AddNode(addr, nil)
		// If it was previously suspect/dead (reconnect), restore to Alive.
		nextGen := s.Cluster.NextGeneration(addr)
		s.Cluster.UpdateState(addr, membership.StateAlive, nextGen)
	}

	return nil
}

func init() {
	gob.Register(&MessageStoreFile{})
	gob.Register(&MessageGetFile{})
	gob.Register(&MessageLocalFile{})
	gob.Register(&MessageAnnounce{})
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
}

func MakeServer(listenAddr string, replicationFactor int, node ...string) *Server {
	metaPath := "_metadata.json"

	// Sprint 5: initialise structured logging and Prometheus metrics.
	logging.Init("server", logging.LevelInfo)
	metrics.Init(prometheus.DefaultRegisterer)

	// Load .env file (ignore error if not found - will use OS env vars)
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, using environment variables")
	}

	// Get encryption key from environment (loaded from .env or OS)
	EncryptionServiceKey := os.Getenv("DFS_ENCRYPTION_KEY")
	if EncryptionServiceKey == "" {
		log.Fatal("CRITICAL: DFS_ENCRYPTION_KEY not set. Please set it in .env file or as environment variable.")
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
	var sptr *Server
	tr, err := factory.NewFromEnv(listenAddr, factory.Options{
		TLSConfig: tlsCfg,
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
	log.Printf("[MakeServer] transport: %s (protocol=%s)", listenAddr, factory.ProtocolFromEnv())

	opts := ServerOpts{
		pathTransform:     storage.CASPathTransformFunc,
		transport:         tr,
		metaData:          storage.NewMetaFile(listenAddr + metaPath),
		bootstrapNodes:    node,
		storageRoot:       listenAddr + "_network",
		Encryption:        crypto.NewEncryptionService(EncryptionServiceKey),
		ReplicationFactor: replicationFactor,
	}

	s := NewServer(opts)
	sptr = s // wire closure so transport callbacks reach the server

	// Sprint 2: wire HeartbeatService
	hbCfg := failure.DefaultConfig()
	s.HeartbeatSvc = failure.NewHeartbeatService(
		hbCfg,
		listenAddr,
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
					From:      listenAddr,
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
			s.HashRing.RemoveNode(addr)
			if s.Rebalancer != nil {
				s.Rebalancer.OnNodeLeft(addr)
			}
		},
	)

	// Sprint 2: wire HandoffService
	hintDir := filepath.Join(listenAddr+"_network", ".hints")
	hintStore, err := handoff.NewStore(hintDir, handoff.DefaultMaxHints, handoff.DefaultMaxAge)
	if err != nil {
		log.Printf("[MakeServer] failed to create hint store: %v (hinted handoff disabled)", err)
	} else {
		s.HandoffSvc = handoff.NewHandoffService(hintStore,
			// deliver closure: resend a hint's encrypted data to its target
			func(h handoff.Hint) error {
				msg := &Message{
					Payload: &MessageStoreFile{
						Key:          h.Key,
						Size:         int64(len(h.Data)),
						EncryptedKey: h.EncryptedKey,
					},
				}
				buf := new(bytes.Buffer)
				if err := gob.NewEncoder(buf).Encode(msg); err != nil {
					return err
				}
				s.peerLock.RLock()
				peer, ok := s.peers[h.TargetAddr]
				s.peerLock.RUnlock()
				if !ok {
					return fmt.Errorf("handoff deliver: peer %s not connected", h.TargetAddr)
				}
				return peer.SendStream(buf.Bytes(), h.Data)
			},
		)
	}

	// Sprint 2: wire Rebalancer
	s.Rebalancer = rebalance.New(
		listenAddr,
		s.HashRing,
		opts.metaData,
		s.readEncryptedFile,
		// sendFile closure: replicate a file to a target node
		func(targetAddr, key, encKey string, data []byte) error {
			msg := &Message{
				Payload: &MessageStoreFile{
					Key:          key,
					Size:         int64(len(data)),
					EncryptedKey: encKey,
				},
			}
			buf := new(bytes.Buffer)
			if err := gob.NewEncoder(buf).Encode(msg); err != nil {
				return err
			}

			s.peerLock.RLock()
			peer, ok := s.peers[targetAddr]
			s.peerLock.RUnlock()
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

	// Sprint 4: wire Quorum coordinator.
	s.Quorum = quorum.New(
		quorum.DefaultConfig(),
		listenAddr,
		// getTargets: ask the hash ring for N responsible nodes
		func(key string) []string {
			return s.HashRing.GetNodes(key, s.HashRing.ReplicationFactor())
		},
		// sendMsg: deliver a quorum message to a peer
		func(addr string, msg interface{}) error {
			switch m := msg.(type) {
			case *quorum.MessageQuorumWrite:
				return s.sendToAddr(addr, &Message{Payload: &MessageQuorumWrite{
					Key: m.Key, EncryptedKey: m.EncryptedKey,
					Data: m.Data, Clock: m.Clock,
				}})
			case *quorum.MessageQuorumRead:
				return s.sendToAddr(addr, &Message{Payload: &MessageQuorumRead{Key: m.Key}})
			default:
				return fmt.Errorf("quorum sendMsg: unknown type %T", msg)
			}
		},
		// localWrite: store encrypted bytes + update metadata with vclock
		func(key, encKey string, data []byte, clock vclock.VectorClock) error {
			if _, err := s.Store.WriteStream(key, bytes.NewReader(data)); err != nil {
				return err
			}
			fm, _ := s.serverOpts.metaData.Get(key)
			fm.EncryptedKey = encKey
			fm.VClock = map[string]uint64(clock)
			fm.Timestamp = time.Now().UnixNano()
			return s.serverOpts.metaData.Set(key, fm)
		},
		// localRead: return metadata for the conflict resolver
		func(key string) (vclock.VectorClock, int64, string, bool) {
			fm, ok := s.serverOpts.metaData.Get(key)
			if !ok {
				return nil, 0, "", false
			}
			return vclock.VectorClock(fm.VClock), fm.Timestamp, fm.EncryptedKey, true
		},
	)

	// Sprint 4: wire AntiEntropyService.
	metaAsKeyer := opts.metaData
	s.AntiEntropy = merkle.NewAntiEntropyService(
		listenAddr,
		10*time.Minute,
		// getKeys: all locally-stored file keys
		func() []string {
			type keyer interface{ Keys() []string }
			if k, ok := metaAsKeyer.(keyer); ok {
				return k.Keys()
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
		// onNeedKey: peer has a key we're missing — request it
		func(addr, key string) {
			log.Printf("[anti-entropy] requesting missing key %s from %s", key, addr)
			_ = s.sendToAddr(addr, &Message{Payload: &MessageGetFile{Key: key}})
		},
		// onSendKey: we have a key the peer is missing — re-replicate it
		func(addr, key string) {
			log.Printf("[anti-entropy] replicating missing key %s to %s", key, addr)
			encKey, data, err := s.readEncryptedFile(key)
			if err != nil {
				log.Printf("[anti-entropy] failed to read %s: %v", key, err)
				return
			}
			s.storeHint(addr, key, encKey, data)
		},
	)

	// Sprint 7: wire Selector + Downloader for parallel chunk downloads.
	s.Selector = selector.New()
	dmCfg := downloader.DefaultConfig()
	s.Downloader = downloader.New(
		dmCfg,
		s.Selector,
		// fetch: get raw encrypted bytes for storageKey from peerAddr
		func(storageKey, peerAddr string) ([]byte, error) {
			return s.fetchChunkFromPeer(storageKey, peerAddr)
		},
		// decrypt: look up per-chunk key and decrypt
		func(storageKey string, encData []byte) ([]byte, error) {
			fm, ok := s.serverOpts.metaData.Get(storageKey)
			log.Printf("[LIFECYCLE] DECRYPT: storageKey=%s metaFound=%v EncryptedKey=%q", storageKey, ok, fm.EncryptedKey)
			if !ok {
				return nil, fmt.Errorf("no metadata for chunk %s", storageKey)
			}
			encKey, err := hex.DecodeString(fm.EncryptedKey)
			if err != nil {
				return nil, err
			}
			var plain bytes.Buffer
			if err := s.serverOpts.Encryption.DecryptStream(bytes.NewReader(encData), &plain, encKey); err != nil {
				return nil, err
			}
			return plain.Bytes(), nil
		},
		// getPeers: ring-responsible nodes for this storageKey
		func(storageKey string) []string {
			return s.HashRing.GetNodes(storageKey, s.HashRing.ReplicationFactor())
		},
	)

	// Sprint 5: wire health server on port+1000 (e.g. :3000 → :4000).
	s.startedAt = time.Now()
	healthPort := deriveHealthPort(listenAddr)
	reg := prometheus.NewRegistry()
	metrics.Reset()
	metrics.Init(reg)
	s.HealthSrv = health.New(healthPort, s.healthStatus, reg)
	if err := s.HealthSrv.Start(); err != nil {
		log.Printf("[MakeServer] health server on %s failed to start: %v (continuing without health endpoint)", healthPort, err)
		s.HealthSrv = nil
	} else {
		log.Printf("[MakeServer] health server listening on %s", healthPort)
	}

	// Sprint 3: wire ClusterState and GossipService.
	s.Cluster = membership.New(listenAddr)

	gossipCfg := gossip.DefaultConfig()
	s.GossipSvc = gossip.New(
		gossipCfg,
		listenAddr,
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
			if err := s.serverOpts.transport.Dial(addr); err != nil {
				log.Printf("[TRACE][onNewPeer] DIAL FAILED self=%s -> addr=%s err=%v", self, addr, err)
			} else {
				log.Printf("[TRACE][onNewPeer] DIAL OK self=%s -> addr=%s", self, addr)
			}
		},
	)

	return s
}
