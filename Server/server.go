package Server

import (
	"bytes"
	"encoding/gob"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"

	"github.com/Faizan2005/DFS-Go/Cluster/hashring"
	crypto "github.com/Faizan2005/DFS-Go/Crypto"
	peer2peer "github.com/Faizan2005/DFS-Go/Peer2Peer"
	storage "github.com/Faizan2005/DFS-Go/Storage"
)

type ServerOpts struct {
	storageRoot       string
	pathTransform     storage.PathTransform
	tcpTransport      *peer2peer.TCPTransport
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

func (s *Server) GetData(key string) (io.Reader, error) {
	if s.Store.Has(key) {
		log.Printf("GET_DATA: File for key '%s' found on local disk.", key)
		_, r, err := s.Store.ReadStream(key)
		if err != nil {
			log.Printf("GET_DATA: Failed to read file from local disk for key '%s': %v", key, err)
			return nil, err
		}
		defer r.Close()

		// Look up the encrypted key from metadata for decryption
		fm, ok := s.serverOpts.metaData.Get(key)
		if !ok {
			return nil, fmt.Errorf("metadata not found for key '%s'", key)
		}

		decodedKey, err := hex.DecodeString(fm.EncryptedKey)
		if err != nil {
			return nil, fmt.Errorf("failed to decode encrypted key for '%s': %w", key, err)
		}

		// Decrypt the file before returning
		var decryptedBuf bytes.Buffer
		if err := s.serverOpts.Encryption.DecryptStream(r, &decryptedBuf, decodedKey); err != nil {
			return nil, fmt.Errorf("failed to decrypt file for key '%s': %w", key, err)
		}

		return &decryptedBuf, nil
	}

	log.Printf("GET_DATA: File for key '%s' not found on local disk. Requesting from peers...", key)

	// Use consistent hashing to find which nodes should have this key
	selfAddr := s.serverOpts.tcpTransport.Addr()
	replFactor := s.HashRing.ReplicationFactor()
	targetNodes := s.HashRing.GetNodes(key, replFactor)

	log.Printf("GET_DATA: Hash ring targets for key '%s': %v", key, targetNodes)

	ch := make(chan io.Reader, 1)

	s.mu.Lock()
	s.pendingFile[key] = ch
	s.mu.Unlock()

	// Send get request only to target nodes (not broadcast)
	getMsg := &Message{
		Payload: MessageGetFile{
			Key: key,
		},
	}

	buf := new(bytes.Buffer)
	if err := gob.NewEncoder(buf).Encode(getMsg); err != nil {
		s.mu.Lock()
		delete(s.pendingFile, key)
		s.mu.Unlock()
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
			log.Printf("GET_DATA: Target node %s not connected, skipping", nodeAddr)
			continue
		}
		if err := peer.Send([]byte{peer2peer.IncomingMessage}); err != nil {
			log.Printf("GET_DATA: Failed to send control byte to %s: %v", nodeAddr, err)
			continue
		}
		if err := peer.Send(msgBytes); err != nil {
			log.Printf("GET_DATA: Failed to send get message to %s: %v", nodeAddr, err)
			continue
		}
		sent++
		log.Printf("GET_DATA: Sent get request to %s", nodeAddr)
	}
	s.peerLock.RUnlock()

	if sent == 0 {
		s.mu.Lock()
		delete(s.pendingFile, key)
		s.mu.Unlock()
		return nil, fmt.Errorf("no reachable peers for key '%s'", key)
	}

	// Wait for response with timeout (instead of blocking forever)
	timeout := time.After(10 * time.Second)
	select {
	case reader := <-ch:
		// Clean up the map
		s.mu.Lock()
		delete(s.pendingFile, key)
		s.mu.Unlock()

		if reader == nil {
			return nil, fmt.Errorf("received nil reader for key '%s'", key)
		}

		log.Printf("GET_DATA: Received file stream for key '%s' from peer.", key)
		return reader, nil

	case <-timeout:
		// Clean up the map on timeout
		s.mu.Lock()
		delete(s.pendingFile, key)
		s.mu.Unlock()

		log.Printf("GET_DATA: Timeout waiting for file '%s' from peers", key)
		return nil, fmt.Errorf("timeout: file '%s' not available from any peer within 10 seconds", key)
	}
}

func (s *Server) StoreData(key string, w io.Reader) error {
	log.Println("STORE_DATA: Starting storage process for key:", key)

	// Create temp file for encrypted data (streaming approach)
	tempFile, err := os.CreateTemp("", "dfs-encrypt-*")
	if err != nil {
		log.Println("STORE_DATA: Failed to create temp file:", err)
		return err
	}
	tempPath := tempFile.Name()

	// Ensure cleanup
	defer func() {
		tempFile.Close()
		os.Remove(tempPath)
	}()

	// Encrypt directly to temp file using streaming (no full buffering!)
	log.Println("STORE_DATA: Encrypting file data with streaming...")
	encryptedKey, err := s.serverOpts.Encryption.EncryptStream(w, tempFile)
	if err != nil {
		log.Println("STORE_DATA: Encryption failed:", err)
		return err
	}
	log.Println("STORE_DATA: Encryption complete")

	// Get file size and seek back to start
	fileInfo, err := tempFile.Stat()
	if err != nil {
		log.Println("STORE_DATA: Failed to stat temp file:", err)
		return err
	}
	encryptedSize := fileInfo.Size()

	if _, err := tempFile.Seek(0, 0); err != nil {
		log.Println("STORE_DATA: Failed to seek temp file:", err)
		return err
	}

	// Store encrypted key in metadata
	err = s.serverOpts.metaData.Set(key, storage.FileMeta{EncryptedKey: hex.EncodeToString(encryptedKey)})
	if err != nil {
		log.Println("STORE_DATA: Metadata store failed:", err)
		return err
	}
	log.Println("STORE_DATA: Metadata stored successfully")

	// Store encrypted data locally by streaming from temp file
	log.Println("STORE_DATA: Storing encrypted file locally...")
	fs, err := s.Store.WriteStream(key, tempFile)
	if err != nil {
		log.Println("STORE_DATA: Local storage failed:", err)
		return err
	}
	log.Printf("STORE_DATA: Local storage successful (%d bytes)", fs)

	// Use consistent hashing to determine which nodes should store this key
	selfAddr := s.serverOpts.tcpTransport.Addr()
	replFactor := s.HashRing.ReplicationFactor()
	targetNodes := s.HashRing.GetNodes(key, replFactor)

	log.Printf("STORE_DATA: Hash ring targets for key '%s' (N=%d): %v", key, replFactor, targetNodes)

	// Filter to only remote peers (exclude self)
	s.peerLock.RLock()
	var targetPeers []struct {
		addr string
		peer peer2peer.Peer
	}
	for _, nodeAddr := range targetNodes {
		if nodeAddr == selfAddr {
			continue // we already stored locally
		}
		if p, ok := s.peers[nodeAddr]; ok {
			targetPeers = append(targetPeers, struct {
				addr string
				peer peer2peer.Peer
			}{nodeAddr, p})
		} else {
			log.Printf("STORE_DATA: Target node %s not connected, skipping", nodeAddr)
		}
	}
	s.peerLock.RUnlock()

	if len(targetPeers) == 0 {
		log.Println("STORE_DATA: No remote peers to replicate to")
		return nil
	}

	// Send store message to target peers
	encKeyHex := hex.EncodeToString(encryptedKey)
	msg := &Message{
		Payload: MessageStoreFile{
			Key:          key,
			Size:         encryptedSize,
			EncryptedKey: encKeyHex,
		},
	}

	buf := new(bytes.Buffer)
	if err := gob.NewEncoder(buf).Encode(msg); err != nil {
		log.Println("STORE_DATA: Failed to encode store message:", err)
		return err
	}
	msgBytes := buf.Bytes()

	// Seek temp file back to start for peer distribution
	if _, err := tempFile.Seek(0, 0); err != nil {
		log.Println("STORE_DATA: Failed to seek temp file for peers:", err)
		return err
	}

	// Buffer encrypted data for parallel sends
	encryptedData, err := io.ReadAll(tempFile)
	if err != nil {
		log.Println("STORE_DATA: Failed to read encrypted data:", err)
		return err
	}

	log.Printf("STORE_DATA: Replicating to %d target peers...", len(targetPeers))

	var wg sync.WaitGroup
	for _, tp := range targetPeers {
		wg.Add(1)
		go func(addr string, p peer2peer.Peer) {
			defer wg.Done()

			log.Printf("STORE_DATA: [Replicate] Sending to peer %s", addr)

			// Send control byte + gob message
			if err := p.Send([]byte{peer2peer.IncomingMessage}); err != nil {
				log.Printf("STORE_DATA: [Replicate] Failed to send control byte to %s: %v", addr, err)
				return
			}
			if err := p.Send(msgBytes); err != nil {
				log.Printf("STORE_DATA: [Replicate] Failed to send message to %s: %v", addr, err)
				return
			}

			time.Sleep(time.Millisecond * 50)

			// Send stream signal + encrypted data
			if err := p.Send([]byte{peer2peer.IncomingStream}); err != nil {
				log.Printf("STORE_DATA: [Replicate] Failed to signal stream to %s: %v", addr, err)
				return
			}

			n, err := io.Copy(p, bytes.NewReader(encryptedData))
			if err != nil {
				log.Printf("STORE_DATA: [Replicate] Failed to send data to %s: %v", addr, err)
				return
			}

			log.Printf("STORE_DATA: [Replicate] Successfully sent %d bytes to %s", n, addr)
		}(tp.addr, tp.peer)
	}

	wg.Wait()
	log.Printf("STORE_DATA: Replication complete for key '%s'", key)
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
	err := s.serverOpts.tcpTransport.ListenAndAccept()
	if err != nil {
		return err
	}

	// Add self to the hash ring so we participate in key ownership
	selfAddr := s.serverOpts.tcpTransport.Addr()
	s.HashRing.AddNode(selfAddr)
	log.Printf("[Run] Added self (%s) to hash ring (replication factor: %d)", selfAddr, s.HashRing.ReplicationFactor())

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
		s.serverOpts.tcpTransport.Close()
		log.Println("[loop] File server closed due to user quit action")
	}()

	log.Println("[loop] Starting server loop...")

	for {
		select {
		case RPC, ok := <-s.serverOpts.tcpTransport.Consume():
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
		log.Printf("[handleMessage] Detected MessageGetFile from %s\n", from)
		return s.handleLocalMessage(from, m)

	default:
		typeName := strings.TrimPrefix(reflect.TypeOf(msg.Payload).String(), "main.")
		log.Printf("[handleMessage] Unknown message type %s from %s\n", typeName, from)
	}

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
	s.peerLock.Lock()
	defer s.peerLock.Unlock()

	addr := p.RemoteAddr().String()
	delete(s.peers, addr)
	s.HashRing.RemoveNode(addr)
	log.Printf("[OnPeerDisconnect] Removed peer: %s (ring size: %d)\n", addr, s.HashRing.Size())
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
			if err := s.serverOpts.tcpTransport.Dial(addr); err != nil {
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
	s.peerLock.Lock()
	defer s.peerLock.Unlock()

	addr := p.RemoteAddr().String()
	s.peers[addr] = p
	s.HashRing.AddNode(addr)
	log.Printf("[OnPeer] Connected with remote peer: %s (ring size: %d)\n", addr, s.HashRing.Size())
	return nil
}

func init() {
	gob.Register(&MessageStoreFile{})
	gob.Register(&MessageGetFile{})
	gob.Register(&MessageLocalFile{})
}

func MakeServer(listenAddr string, replicationFactor int, node ...string) *Server {
	metaPath := "_metadata.json"

	// Load .env file (ignore error if not found - will use OS env vars)
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, using environment variables")
	}

	// Get encryption key from environment (loaded from .env or OS)
	EncryptionServiceKey := os.Getenv("DFS_ENCRYPTION_KEY")
	if EncryptionServiceKey == "" {
		log.Fatal("CRITICAL: DFS_ENCRYPTION_KEY not set. Please set it in .env file or as environment variable.")
	}

	tcpOpts := peer2peer.TCPTransportOpts{
		ListenAddr:    listenAddr,
		HandshakeFunc: peer2peer.NOPEHandshakeFunc,
		Decoder:       peer2peer.DefaultDecoder{},
	}

	// Check if TLS is enabled
	if os.Getenv("DFS_ENABLE_TLS") == "true" {
		log.Println("mTLS enabled, setting up certificate infrastructure...")

		certDir := ".certs"
		caCertPath := certDir + "/ca.crt"
		caKeyPath := certDir + "/ca.key"
		nodeCertPath := certDir + "/node" + listenAddr + ".crt"
		nodeKeyPath := certDir + "/node" + listenAddr + ".key"

		// Step 1: Load or generate CA (shared across all nodes in the cluster)
		caCert, caKey, err := crypto.LoadOrGenerateCA(caCertPath, caKeyPath)
		if err != nil {
			log.Fatalf("Failed to setup CA: %v", err)
		}
		log.Printf("CA loaded/generated: %s (Issuer: %s)", caCert.Subject.CommonName, caCert.Issuer.CommonName)

		// Step 2: Generate node certificate signed by CA
		nodeOpts := crypto.NodeCertOptions{
			NodeID:   listenAddr,
			IPs:      nil, // defaults to localhost
			DNSNames: nil, // defaults to localhost
		}
		if err := crypto.LoadOrGenerateNodeCert(caCert, caKey, nodeCertPath, nodeKeyPath, nodeOpts); err != nil {
			log.Fatalf("Failed to setup node certificate: %v", err)
		}
		log.Printf("Node certificate ready: %s", nodeCertPath)

		// Step 3: Load mTLS config (mutual authentication)
		tlsConfig, err := crypto.LoadMTLSConfig(nodeCertPath, nodeKeyPath, caCertPath)
		if err != nil {
			log.Fatalf("Failed to setup mTLS config: %v", err)
		}
		tcpOpts.TLSConfig = tlsConfig

		// Step 4: Use TLS-aware handshake function
		tcpOpts.HandshakeFunc = peer2peer.TLSVerifyHandshakeFunc

		log.Println("mTLS configured successfully with CA-signed certificates")
	} else {
		log.Println("TLS disabled (set DFS_ENABLE_TLS=true to enable mTLS)")
	}

	tcpTransport := peer2peer.NewTCPTransport(tcpOpts)

	opts := ServerOpts{
		pathTransform:     storage.CASPathTransformFunc,
		tcpTransport:      tcpTransport,
		metaData:          storage.NewMetaFile(listenAddr + metaPath),
		bootstrapNodes:    node,
		storageRoot:       listenAddr + "_network",
		Encryption:        crypto.NewEncryptionService(EncryptionServiceKey),
		ReplicationFactor: replicationFactor,
	}

	s := NewServer(opts)
	// Wire callbacks after server is fully constructed so they
	// operate on the correct server instance's peers map.
	tcpTransport.OnPeer = s.OnPeer
	tcpTransport.OnPeerDisconnect = s.OnPeerDisconnect
	return s
}
