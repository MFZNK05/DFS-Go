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

	serverOpts  ServerOpts
	Store       *storage.Store
	HashRing    *hashring.HashRing
	quitch      chan struct{}
	pendingFile map[string]chan io.Reader
	mu          sync.Mutex

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

func (s *Server) GetData(key string) (io.Reader, error) {
	t0 := time.Now()

	// Check whether this key is stored as chunked.
	fm, hasMeta := s.serverOpts.metaData.Get(key)
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

	ch := make(chan io.Reader, 1)
	s.mu.Lock()
	s.pendingFile[storageKey] = ch
	s.mu.Unlock()
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
		if err := peer.Send([]byte{peer2peer.IncomingMessage}); err != nil {
			continue
		}
		if err := peer.Send(msgBytes); err != nil {
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
func (s *Server) fetchChunkFromPeer(storageKey, peerAddr string) ([]byte, error) {
	ch := make(chan io.Reader, 1)
	s.mu.Lock()
	s.pendingFile[storageKey] = ch
	s.mu.Unlock()
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
	if err := peer.Send([]byte{peer2peer.IncomingMessage}); err != nil {
		return nil, fmt.Errorf("fetchChunkFromPeer: send control byte: %w", err)
	}
	if err := peer.Send(buf.Bytes()); err != nil {
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
// This is used when a node doesn't have the manifest locally yet.
func (s *Server) fetchManifestFromPeers(key string) (*chunker.ChunkManifest, error) {
	// The manifest is stored under "manifest:<key>" as a regular metadata entry.
	// We request it via the normal get path so peers respond with their copy.
	manifestKey := chunker.ManifestStorageKey(key)
	selfAddr := s.serverOpts.transport.Addr()
	targetNodes := s.HashRing.GetNodes(manifestKey, s.HashRing.ReplicationFactor())

	ch := make(chan io.Reader, 1)
	s.mu.Lock()
	s.pendingFile[manifestKey] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pendingFile, manifestKey)
		s.mu.Unlock()
	}()

	getMsg := &Message{Payload: &MessageGetFile{Key: manifestKey}}
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
		if err := peer.Send([]byte{peer2peer.IncomingMessage}); err != nil {
			continue
		}
		if err := peer.Send(msgBytes); err != nil {
			continue
		}
		sent++
	}
	s.peerLock.RUnlock()

	if sent == 0 {
		return nil, fmt.Errorf("no reachable peers for manifest of '%s'", key)
	}

	select {
	case r := <-ch:
		if r == nil {
			return nil, fmt.Errorf("nil reader for manifest of '%s'", key)
		}
		// The manifest was stored as a JSON-encoded FileMeta.Manifest field.
		// The peer sends back the raw encrypted manifest bytes; we decode them.
		data, err := io.ReadAll(r)
		if err != nil {
			return nil, err
		}
		// Peer sends it as encrypted bytes — decrypt using local key if available,
		// otherwise the manifest is stored plaintext in the metadata JSON.
		// Since manifests are stored via SetManifest (which uses FileMeta.Manifest,
		// a JSON-serialised struct), the peer's handleGetMessage returns the raw
		// store bytes. We decode it as a FileMeta to extract the Manifest field.
		var fm storage.FileMeta
		if err := json.Unmarshal(data, &fm); err != nil {
			return nil, fmt.Errorf("fetchManifest: unmarshal FileMeta: %w", err)
		}
		if fm.Manifest == nil {
			return nil, fmt.Errorf("fetchManifest: nil manifest in FileMeta for '%s'", key)
		}
		return fm.Manifest, nil
	case <-time.After(10 * time.Second):
		return nil, fmt.Errorf("timeout fetching manifest for '%s'", key)
	}
}

func (s *Server) StoreData(key string, w io.Reader) error {
	t0 := time.Now()
	log.Println("STORE_DATA: Starting chunked storage for key:", key)
	defer func() { metrics.RecordStore("store", nil, time.Since(t0)) }()

	selfAddr := s.serverOpts.transport.Addr()
	replFactor := s.HashRing.ReplicationFactor()

	chunkCh, errCh := chunker.ChunkReader(w, chunker.DefaultChunkSize)

	var chunkInfos []chunker.ChunkInfo

	for chunk := range chunkCh {
		// Sprint 7: compress before encryption when data is compressible.
		toEncrypt := chunk.Data
		wasCompressed := false
		if compression.ShouldCompress(chunk.Data) {
			if c, wc, err := compression.CompressChunk(chunk.Data, compression.LevelFastest); err == nil && wc {
				toEncrypt = c
				wasCompressed = true
			}
		}

		// Encrypt the (possibly compressed) chunk into a temp file.
		tempFile, err := os.CreateTemp("", "dfs-chunk-*")
		if err != nil {
			return fmt.Errorf("STORE_DATA: create temp file: %w", err)
		}
		tempPath := tempFile.Name()

		encKey, err := s.serverOpts.Encryption.EncryptStream(bytes.NewReader(toEncrypt), tempFile)
		if err != nil {
			tempFile.Close()
			os.Remove(tempPath)
			return fmt.Errorf("STORE_DATA: encrypt chunk %d: %w", chunk.Index, err)
		}

		// Read encrypted bytes back for CAS key + replication.
		if _, err := tempFile.Seek(0, 0); err != nil {
			tempFile.Close()
			os.Remove(tempPath)
			return fmt.Errorf("STORE_DATA: seek chunk %d: %w", chunk.Index, err)
		}
		encData, err := io.ReadAll(tempFile)
		tempFile.Close()
		os.Remove(tempPath)
		if err != nil {
			return fmt.Errorf("STORE_DATA: read chunk %d: %w", chunk.Index, err)
		}

		// CAS storage key = "chunk:" + sha256(encrypted bytes).
		encHashRaw := sha256.Sum256(encData)
		encHashHex := hex.EncodeToString(encHashRaw[:])
		storageKey := chunker.ChunkStorageKey(encHashHex)
		encKeyHex := hex.EncodeToString(encKey)

		// Store locally (skip if already present — dedup).
		if !s.Store.Has(storageKey) {
			if _, err := s.Store.WriteStream(storageKey, bytes.NewReader(encData)); err != nil {
				return fmt.Errorf("STORE_DATA: local store chunk %d: %w", chunk.Index, err)
			}
			if err := s.serverOpts.metaData.Set(storageKey, storage.FileMeta{
				EncryptedKey: encKeyHex,
				Timestamp:    time.Now().UnixNano(),
			}); err != nil {
				return fmt.Errorf("STORE_DATA: metadata chunk %d: %w", chunk.Index, err)
			}
		}

		// Replicate to ring-responsible peers.
		targetNodes := s.HashRing.GetNodes(storageKey, replFactor)
		s.replicateChunk(selfAddr, storageKey, encKeyHex, encData, targetNodes)

		chunkInfos = append(chunkInfos, chunker.ChunkInfo{
			Index:      chunk.Index,
			Hash:       hex.EncodeToString(chunk.Hash[:]),
			Size:       chunk.Size,
			EncHash:    encHashHex,
			Compressed: wasCompressed,
		})

		log.Printf("STORE_DATA: chunk %d stored (compressed=%v storageKey=%s)", chunk.Index, wasCompressed, storageKey)
	}

	// Check for read error from chunker goroutine.
	if err := <-errCh; err != nil {
		return fmt.Errorf("STORE_DATA: chunker read error: %w", err)
	}

	// Build and persist the manifest.
	manifest := chunker.BuildManifest(key, chunkInfos, time.Now().UnixNano())
	if err := s.serverOpts.metaData.SetManifest(key, manifest); err != nil {
		return fmt.Errorf("STORE_DATA: store manifest: %w", err)
	}

	// Update top-level FileMeta so Has(key) works and GetData knows it's chunked.
	if err := s.serverOpts.metaData.Set(key, storage.FileMeta{
		Chunked:   true,
		Timestamp: time.Now().UnixNano(),
	}); err != nil {
		return fmt.Errorf("STORE_DATA: update file meta: %w", err)
	}

	log.Printf("STORE_DATA: key '%s' stored as %d chunks", key, len(chunkInfos))
	return nil
}

// replicateChunk sends storageKey's encrypted bytes to all targetNodes that
// are not selfAddr. Unreachable nodes get a hint entry.
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

	var wg sync.WaitGroup
	for _, nodeAddr := range targetNodes {
		if nodeAddr == selfAddr {
			continue
		}
		p, ok := peers[nodeAddr]
		if !ok {
			s.storeHint(nodeAddr, storageKey, encKeyHex, encData)
			metrics.RecordReplication("hint")
			continue
		}
		wg.Add(1)
		go func(addr string, peer peer2peer.Peer) {
			defer wg.Done()
			if err := peer.Send([]byte{peer2peer.IncomingMessage}); err != nil {
				s.storeHint(addr, storageKey, encKeyHex, encData)
				metrics.RecordReplication("hint")
				return
			}
			if err := peer.Send(msgBytes); err != nil {
				s.storeHint(addr, storageKey, encKeyHex, encData)
				metrics.RecordReplication("hint")
				return
			}
			time.Sleep(50 * time.Millisecond)
			if err := peer.Send([]byte{peer2peer.IncomingStream}); err != nil {
				s.storeHint(addr, storageKey, encKeyHex, encData)
				metrics.RecordReplication("hint")
				return
			}
			if _, err := io.Copy(peer, bytes.NewReader(encData)); err != nil {
				s.storeHint(addr, storageKey, encKeyHex, encData)
				metrics.RecordReplication("hint")
				return
			}
			metrics.RecordReplication("ok")
		}(nodeAddr, p)
	}
	wg.Wait()
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
func (s *Server) handleQuorumWrite(from string, msg *MessageQuorumWrite) error {
	selfAddr := s.serverOpts.transport.Addr()

	// Write the data into local store via the existing WriteStream path.
	// We store the raw encrypted bytes directly (already encrypted by sender).
	_, err := s.Store.WriteStream(msg.Key, bytes.NewReader(msg.Data))
	if err == nil {
		// Update metadata with the vclock and timestamp from the write.
		_ = s.serverOpts.metaData.Set(msg.Key, storage.FileMeta{
			EncryptedKey: msg.EncryptedKey,
			VClock:       msg.Clock,
			Timestamp:    time.Now().UnixNano(),
		})
	}

	ack := &Message{Payload: &MessageQuorumWriteAck{
		Key:     msg.Key,
		From:    selfAddr,
		Success: err == nil,
		ErrMsg:  func() string {
			if err != nil {
				return err.Error()
			}
			return ""
		}(),
	}}
	_ = s.sendToAddr(from, ack) // best-effort
	return nil
}

// handleQuorumRead responds with local metadata for the requested key.
func (s *Server) handleQuorumRead(from string, msg *MessageQuorumRead) error {
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
	_ = s.sendToAddr(from, resp)
	return nil
}

// deriveHealthPort returns the health-server listen address by adding 1000 to
// the port in listenAddr (e.g. ":3000" → ":4000", "127.0.0.1:3000" → "127.0.0.1:4000").
// Falls back to ":14000" if parsing fails.
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
	s.peerLock.RLock()
	peerCount := len(s.peers)
	s.peerLock.RUnlock()

	ringSize := s.HashRing.Size()
	status := "ok"
	if peerCount == 0 && ringSize <= 1 {
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

// GracefulShutdown signals all peers that this node is leaving and waits briefly
// for any in-flight operations to complete before the process exits.
func (s *Server) GracefulShutdown() {
	log.Println("[GracefulShutdown] Signalling peers and shutting down...")

	if s.HealthSrv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.HealthSrv.Stop(ctx)
	}
	if s.HeartbeatSvc != nil {
		s.HeartbeatSvc.Stop()
	}
	if s.HandoffSvc != nil {
		s.HandoffSvc.Stop()
	}
	if s.GossipSvc != nil {
		s.GossipSvc.Stop()
	}
	if s.AntiEntropy != nil {
		s.AntiEntropy.Stop()
	}

	// Give replication goroutines a moment to finish.
	time.Sleep(500 * time.Millisecond)
	s.Stop()
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

		err := peer.Send([]byte{peer2peer.IncomingMessage})
		if err != nil {
			log.Printf("[Broadcast] Error sending control byte to %s: %v\n", addr, err)
			errs = append(errs, fmt.Errorf("peer %s: %w", addr, err))
			continue
		}

		if err := peer.Send(buf.Bytes()); err != nil {
			log.Printf("[Broadcast] Error sending actual message to %s: %v\n", addr, err)
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
		pendingFile: make(map[string]chan io.Reader),
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

			if err := s.handleMessage(RPC.From.String(), &message); err != nil {
				log.Printf("[loop] Error handling message from %s: %v\n", RPC.From.String(), err)
				continue
			}

		case <-s.quitch:
			log.Println("[loop] Quit channel received, exiting loop")
			return
		}
	}
}

func (s *Server) handleMessage(from string, msg *Message) error {
	log.Printf("[handleMessage] Handling message from %s: Type=%s\n",
		from, strings.TrimPrefix(reflect.TypeOf(msg.Payload).String(), "main."))

	switch m := msg.Payload.(type) {
	case *MessageStoreFile:
		log.Printf("[handleMessage] Detected MessageStoreFile from %s\n", from)
		return s.handleStoreMessage(from, m)

	case *MessageGetFile:
		log.Printf("[handleMessage] Detected MessageGetFile from %s\n", from)
		return s.handleGetMessage(from, m)

	case *MessageLocalFile:
		log.Printf("[handleMessage] Detected MessageLocalFile from %s\n", from)
		return s.handleLocalMessage(from, m)

	case *MessageHeartbeat:
		return s.handleHeartbeat(from, m)

	case *MessageHeartbeatAck:
		// RTT measurement could be recorded here in future; for now just log.
		log.Printf("[handleMessage] HeartbeatAck from %s (rtt origin ts=%d)\n", from, m.Timestamp)
		return nil

	case *MessageGossipDigest:
		if s.GossipSvc != nil {
			s.GossipSvc.HandleDigest(from, &gossip.MessageGossipDigest{
				From:    m.From,
				Digests: m.Digests,
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
		return s.handleQuorumWrite(from, m)

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
		return s.handleQuorumRead(from, m)

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

	if err := peer.Send([]byte{peer2peer.IncomingMessage}); err != nil {
		return fmt.Errorf("sendToAddr: send control byte to %s: %w", addr, err)
	}
	if err := peer.Send(buf.Bytes()); err != nil {
		return fmt.Errorf("sendToAddr: send payload to %s: %w", addr, err)
	}
	return nil
}

// handleHeartbeat records the arrival and sends an ack back.
func (s *Server) handleHeartbeat(from string, msg *MessageHeartbeat) error {
	metrics.RecordHeartbeat("received")
	if s.HeartbeatSvc != nil {
		s.HeartbeatSvc.RecordHeartbeat(msg.From)
	}

	selfAddr := s.serverOpts.transport.Addr()
	ack := &Message{Payload: &MessageHeartbeatAck{
		From:      selfAddr,
		Timestamp: msg.Timestamp,
	}}
	// Best-effort ack — ignore error (peer may have already disconnected).
	_ = s.sendToAddr(from, ack)
	return nil
}

func (s *Server) handleGetMessage(from string, msg *MessageGetFile) error {
	log.Printf("HANDLE_GET: Received file request for key '%s' from peer '%s'", msg.Key, from)

	s.peerLock.RLock()
	peer, ok := s.peers[from]
	s.peerLock.RUnlock()
	if !ok {
		return fmt.Errorf("peer (%s) not found", from)
	}

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

	if err = peer.Send([]byte{peer2peer.IncomingMessage}); err != nil {
		return err
	}

	if err := peer.Send(buf.Bytes()); err != nil {
		log.Printf("[HANDLE_GET] Error sending actual message to %s: %v\n", from, err)
		return err
	}

	log.Printf("[HANDLE_GET] Successfully sent message to %s\n", from)

	time.Sleep(time.Millisecond * 50)

	log.Printf("HANDLE_GET: Sending stream signal to peer '%s'", from)
	if err = peer.Send([]byte{peer2peer.IncomingStream}); err != nil {
		log.Printf("HANDLE_GET: Failed to signal stream to '%s': %v", from, err)
		// Not returning error here so we still attempt to send file
	}

	log.Printf("HANDLE_GET: Sending file data for key '%s' to peer '%s'", msg.Key, from)
	n, err := io.Copy(peer, r)
	if err != nil {
		log.Printf("HANDLE_GET: Failed to send file data to peer '%s': %v", from, err)
		return err
	}
	log.Printf("HANDLE_GET: Successfully sent %d bytes to peer '%s' for key '%s'", n, from, msg.Key)

	return nil
}

func (s *Server) handleStoreMessage(from string, msg *MessageStoreFile) error {
	log.Printf("HANDLE_STORE: Received store request for key %s from %s", msg.Key, from)

	s.peerLock.RLock()
	peer, ok := s.peers[from]
	s.peerLock.RUnlock()
	if !ok {
		err := fmt.Errorf("peer (%s) not found", from)
		log.Println("HANDLE_STORE: Error:", err)
		return err
	}

	log.Println("HANDLE_STORE: Starting file storage...")
	n, err := s.Store.WriteStream(msg.Key, io.LimitReader(peer.(io.Reader), msg.Size))
	if err != nil {
		log.Println("HANDLE_STORE: Storage failed:", err)
		return fmt.Errorf("error storing file to disk: %+v", err)
	}

	log.Println("HANDLE_STORE: Closing stream...")
	peer.CloseStream()

	// Save the encrypted key to local metadata so this node can decrypt the file
	if msg.EncryptedKey != "" {
		if err := s.serverOpts.metaData.Set(msg.Key, storage.FileMeta{EncryptedKey: msg.EncryptedKey}); err != nil {
			log.Printf("HANDLE_STORE: Failed to save metadata for key %s: %v", msg.Key, err)
			return err
		}
	}

	log.Printf("HANDLE_STORE: Successfully stored [%d] bytes to %s from %s", n, msg.Key, from)
	return nil
}

func (s *Server) handleLocalMessage(from string, msg *MessageLocalFile) error {
	s.peerLock.RLock()
	peer, ok := s.peers[from]
	s.peerLock.RUnlock()
	if !ok {
		return fmt.Errorf("peer (%s) not found", from)
	}

	n, err := s.Store.WriteStream(msg.Key, io.LimitReader(peer, msg.Size))
	if err != nil {
		log.Printf("HANDLE_LOCAL: Storage error from %s: %v", from, err)
		return err
	}
	log.Printf("HANDLE_LOCAL: Stored %d bytes from %s", n, from)

	// Get metadata
	fm, ok := s.serverOpts.metaData.Get(msg.Key)
	if !ok {
		log.Printf("HANDLE_LOCAL: Missing metadata for %s", msg.Key)
		return fmt.Errorf("missing metadata")
	}

	// Read encrypted data
	_, r, err := s.Store.ReadStream(msg.Key)
	if err != nil {
		log.Printf("HANDLE_LOCAL: Read error for %s: %v", msg.Key, err)
		return err
	}
	defer r.Close()

	log.Printf("HANDLE_LOCAL: Key = %s, ExpectedSize = %d, WrittenSize = %d", msg.Key, msg.Size, n)

	decodedKey, err := hex.DecodeString(fm.EncryptedKey)
	if err != nil {
		return fmt.Errorf("failed to decode hex key: %w", err)
	}

	log.Printf("HANDLE_LOCAL: EncryptedKey = %x", decodedKey)

	// Decrypt using streaming (reads/decrypts in chunks)
	var decryptedBuf bytes.Buffer
	err = s.serverOpts.Encryption.DecryptStream(r, &decryptedBuf, decodedKey)
	if err != nil {
		log.Printf("HANDLE_LOCAL: Decryption error: %v", err)
		return err
	}

	peer.CloseStream()
	log.Printf("HANDLE_LOCAL: Successfully retrieved '%s' from %s", msg.Key, from)

	s.mu.Lock()
	ch, ok := s.pendingFile[msg.Key]
	s.mu.Unlock()

	if ok {
		ch <- &decryptedBuf
	} else {
		log.Printf("HANDLE_LOCAL: No waiting channel for key %s", msg.Key)
	}

	return nil
}

func (s *Server) Stop() {
	close(s.quitch)
}

func (s *Server) OnPeerDisconnect(p peer2peer.Peer) {
	addr := p.RemoteAddr().String()

	s.peerLock.Lock()
	delete(s.peers, addr)
	peerCount := len(s.peers)
	s.peerLock.Unlock()

	s.HashRing.RemoveNode(addr)
	log.Printf("[OnPeerDisconnect] Removed peer: %s (ring size: %d)\n", addr, s.HashRing.Size())
	metrics.SetPeerCount(peerCount)
	metrics.SetRingSize(s.HashRing.Size())

	// Sprint 2: clean up failure detector state for this peer.
	if s.HeartbeatSvc != nil {
		s.HeartbeatSvc.RemovePeer(addr)
	}

	// Sprint 3: mark as suspect in membership table.
	if s.Cluster != nil {
		nextGen := s.Cluster.NextGeneration(addr)
		s.Cluster.UpdateState(addr, membership.StateSuspect, nextGen)
	}
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

	s.peerLock.Lock()
	s.peers[addr] = p
	peerCount := len(s.peers)
	s.peerLock.Unlock()

	s.HashRing.AddNode(addr)
	log.Printf("[OnPeer] Connected with remote peer: %s (ring size: %d)\n", addr, s.HashRing.Size())
	metrics.SetPeerCount(peerCount)
	metrics.SetRingSize(s.HashRing.Size())

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
		// getPeers closure: returns all currently connected peer addresses
		func() []string {
			s.peerLock.RLock()
			defer s.peerLock.RUnlock()
			addrs := make([]string, 0, len(s.peers))
			for addr := range s.peers {
				addrs = append(addrs, addr)
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
				return s.sendToAddr(h.TargetAddr, &Message{
					Payload: &MessageStoreFile{
						Key:          h.Key,
						Size:         int64(len(h.Data)),
						EncryptedKey: h.EncryptedKey,
					},
				})
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
			if err := s.sendToAddr(targetAddr, msg); err != nil {
				return err
			}
			// Give the peer a moment to process the metadata message
			time.Sleep(50 * time.Millisecond)

			s.peerLock.RLock()
			peer, ok := s.peers[targetAddr]
			s.peerLock.RUnlock()
			if !ok {
				return fmt.Errorf("rebalance: peer %s disconnected before stream", targetAddr)
			}
			if err := peer.Send([]byte{peer2peer.IncomingStream}); err != nil {
				return err
			}
			_, err := io.Copy(peer, bytes.NewReader(data))
			return err
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
			return s.serverOpts.metaData.Set(key, storage.FileMeta{
				EncryptedKey: encKey,
				VClock:       map[string]uint64(clock),
				Timestamp:    time.Now().UnixNano(),
			})
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
		// onNewPeer: dial a peer discovered through gossip
		func(addr string) {
			log.Printf("[gossip] dialling newly discovered peer %s", addr)
			if err := s.serverOpts.transport.Dial(addr); err != nil {
				log.Printf("[gossip] failed to dial %s: %v", addr, err)
			}
		},
	)

	return s
}
