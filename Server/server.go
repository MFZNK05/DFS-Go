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

	crypto "github.com/Faizan2005/DFS-Go/Crypto"
	peer2peer "github.com/Faizan2005/DFS-Go/Peer2Peer"
	storage "github.com/Faizan2005/DFS-Go/Storage"
)

type ServerOpts struct {
	storageRoot    string
	pathTransform  storage.PathTransform
	tcpTransport   peer2peer.TCPTransport
	metaData       storage.MetadataStore
	bootstrapNodes []string
	Encryption     *crypto.EncryptionService
}

type Server struct {
	peerLock sync.Mutex
	peers    map[string]peer2peer.Peer

	serverOpts  ServerOpts
	Store       *storage.Store
	quitch      chan struct{}
	pendingFile map[string]chan io.Reader
	mu          sync.Mutex
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

func (s *Server) GetData(key string) (io.Reader, error) {
	if s.Store.Has(key) {
		log.Printf("GET_DATA: File for key '%s' found on local disk.", key)
		_, w, err := s.Store.ReadStream(key)
		if err != nil {
			log.Printf("GET_DATA: Failed to read file from local disk for key '%s': %v", key, err)
			return nil, err
		}
		return w, nil
	}

	log.Printf("GET_DATA: File for key '%s' not found on local disk. Requesting from peers...", key)

	ch := make(chan io.Reader, 1)

	s.mu.Lock()
	s.pendingFile[key] = ch
	s.mu.Unlock()

	p := &Message{
		Payload: MessageGetFile{
			Key: key,
		},
	}

	if err := s.Broadcast(*p); err != nil {
		log.Printf("GET_DATA: Broadcast to peers failed for key '%s': %v", key, err)

		return nil, err
	}

	time.Sleep(time.Millisecond * 100)

	reader := <-ch

	// Clean up the map
	s.mu.Lock()
	delete(s.pendingFile, key)
	s.mu.Unlock()

	log.Printf("GET_DATA: Received file stream for key '%s' from peer.", key)

	// for addr, peer := range s.peers {
	// 	log.Printf("GET_DATA: Trying peer %s", addr)

	// 	// Get file size
	// 	var fileSize int64
	// 	if err := binary.Read(peer, binary.LittleEndian, &fileSize); err != nil {
	// 		log.Printf("GET_DATA: Size read error from %s: %v", addr, err)
	// 		continue
	// 	}

	// 	buff := new(bytes.Buffer)
	// 	_, err := io.CopyN(buff, peer, fileSize)
	// 	if err != nil {
	// 		return nil, err
	// 	}

	// 	// Stream the file
	// 	n, err := s.Store.WriteStream(key, buff)
	// 	if err != nil {
	// 		log.Printf("GET_DATA: Storage error from %s: %v", addr, err)
	// 		continue
	// 	}
	// 	log.Printf("GET_DATA: Stored %d bytes from %s", n, addr)

	// 	// Get metadata
	// 	fm, ok := s.serverOpts.metaData.Get(key)
	// 	if !ok {
	// 		log.Printf("GET_DATA: Missing metadata for %s", key)
	// 		return nil, fmt.Errorf("missing metadata")
	// 	}

	// 	// Read and decrypt
	// 	_, r, err := s.Store.ReadStream(key)
	// 	if err != nil {
	// 		log.Printf("GET_DATA: Read error for %s: %v", key, err)
	// 		continue
	// 	}

	// 	buf := new(bytes.Buffer)
	// 	if _, err := io.Copy(buf, r); err != nil {
	// 		log.Printf("GET_DATA: Copy error: %v", err)
	// 		continue
	// 	}

	// 	plainData, err := s.serverOpts.Encryption.DecryptFile(buf.Bytes(), []byte(fm.EncryptedKey))
	// 	if err != nil {
	// 		log.Printf("GET_DATA: Decryption error: %v", err)
	// 		continue
	// 	}

	// 	peer.CloseStream()
	// 	log.Printf("GET_DATA: Successfully retrieved '%s' from %s", key, addr)
	// 	return bytes.NewReader(plainData), nil
	// }

	return reader, nil
}

// func (s *Server) GetData(key string) (io.Reader, error) {
// 	if s.Store.Has(key) {
// 		fmt.Print("reading from disk")
// 		_, w, err := s.Store.ReadStream(key)
// 		if err != nil {
// 			return nil, err
// 		}
// 		return w, nil
// 	}

// 	fmt.Print("file not available on disk")

// 	p := &Message{
// 		Payload: MessageGetFile{
// 			Key: key,
// 		},
// 	}

// 	ch := make(chan io.Reader, 1)
// 	s.pendingFile[key] = ch

// 	if err := s.Broadcast(*p); err != nil {
// 		return nil, err
// 	}

// 	time.Sleep(50 * time.Millisecond)

// 	r := <-ch
// 	delete(s.pendingFile, key)

// 	// Get metadata
// 	fm, ok := s.serverOpts.metaData.Get(key)
// 	if !ok {
// 		return nil, os.ErrNotExist
// 	}

// 	// Read from the `io.Reader` into a buffer
// 	buf := new(bytes.Buffer)
// 	if _, err := io.Copy(buf, r); err != nil {
// 		return nil, err
// 	}

// 	// Decrypt the data
// 	plainData, err := s.serverOpts.Encryption.DecryptFile(buf.Bytes(), []byte(fm.EncryptedKey))
// 	if err != nil {
// 		return nil, err
// 	}

// 	// Return a reader to the decrypted data
// 	return bytes.NewReader(plainData), nil
// }

// func (s *Server) GetData(key string) (io.Reader, error) {
// 	if s.Store.Has(key) {
// 		fmt.Print("reading from disk")
// 		_, w, err := s.Store.ReadStream(key)
// 		if err != nil {
// 			return nil, err
// 		}
// 		return w, nil
// 	}

// 	fmt.Print("file not available on disk")

// 	p := &Message{
// 		Payload: MessageGetFile{
// 			Key: key,
// 		},
// 	}

// 	ch := make(chan io.Reader, 1)
// 	s.pendingFile[key] = ch

// 	if err := s.Broadcast(*p); err != nil {
// 		return nil, err
// 	}

// 	time.Sleep(time.Millisecond * 50)

// 	r := <-ch
// 	delete(s.pendingFile, key)
// 	fm, ok := s.serverOpts.metaData.Get(key)
// 	if !ok {
// 		return nil, os.ErrNotExist
// 	}

// 	fileData, err := s.serverOpts.Encryption.DecryptFile([]byte(r), []byte(fm.EncryptedKey))
// 	if err != nil {
// 		return nil, err
// 	}

// 	return fileData, nil
// }

func (s *Server) StoreData(key string, w io.Reader) error {
	log.Println("STORE_DATA: Starting storage process for key:", key)

	// First read ALL data into buffer
	buff := new(bytes.Buffer)
	if _, err := io.Copy(buff, w); err != nil {
		log.Println("STORE_DATA: Error buffering data:", err)
		return err
	}

	// Encrypt once before sending to peers
	log.Println("STORE_DATA: Encrypting file data...")
	cipherText, encryptedKey, err := s.serverOpts.Encryption.EncryptFile(bytes.NewReader(buff.Bytes()))
	if err != nil {
		log.Println("STORE_DATA: Encryption failed:", err)
		return err
	}

	// Store encrypted key in metadata
	err = s.serverOpts.metaData.Set(key, storage.FileMeta{EncryptedKey: hex.EncodeToString(encryptedKey)})
	if err != nil {
		log.Println("STORE_DATA: Metadata store failed:", err)
		return err
	}
	log.Println("STORE_DATA: Metadata stored successfully")

	// Store encrypted data locally
	log.Println("STORE_DATA: Storing encrypted file locally...")
	fs, err := s.Store.WriteStream(key, bytes.NewReader(cipherText))
	if err != nil {
		log.Println("STORE_DATA: Local storage failed:", err)
		return err
	}
	log.Println("STORE_DATA: Local storage successful")

	// Broadcast metadata info
	p := &Message{
		Payload: MessageStoreFile{
			Key:  key,
			Size: fs,
		},
	}

	log.Println("STORE_DATA: Broadcasting store message...")
	if err := s.Broadcast(*p); err != nil {
		log.Println("STORE_DATA: Broadcast failed:", err)
		return err
	}

	time.Sleep(time.Millisecond * 50)
	log.Println("STORE_DATA: Starting peer distribution...")

	// Now send encrypted data to peers
	for addr, peer := range s.peers {
		log.Printf("STORE_DATA: Processing peer %s", addr)

		log.Printf("STORE_DATA: Sending stream signal to %s", addr)
		if err := peer.Send([]byte{peer2peer.IncomingStream}); err != nil {
			log.Printf("STORE_DATA: Failed to signal stream to %s: %v", addr, err)
			continue
		}

		log.Printf("STORE_DATA: Sending file data to %s", addr)
		n, err := io.Copy(peer, bytes.NewReader(cipherText))
		if err != nil {
			log.Printf("STORE_DATA: Failed to send data to %s: %v", addr, err)
			continue
		}

		log.Printf("STORE_DATA: Successfully sent %d bytes to %s", n, addr)
	}

	log.Println("STORE_DATA: Completed peer distribution")
	return nil
}

func (s *Server) Broadcast(d Message) error {
	log.Println("[Broadcast] Encoding message...")

	buf := new(bytes.Buffer)
	if err := gob.NewEncoder(buf).Encode(d); err != nil {
		log.Printf("[Broadcast] Error encoding message: %v\n", err)
		return err
	}

	log.Printf("[Broadcast] Broadcasting message to %d peers\n", len(s.peers))

	for addr, peer := range s.peers {
		log.Printf("[Broadcast] Sending message to peer: %s\n", addr)

		err := peer.Send([]byte{peer2peer.IncomingMessage})
		if err != nil {
			return err
		}

		if err := peer.Send(buf.Bytes()); err != nil {
			log.Printf("[Broadcast] Error sending actual message to %s: %v\n", addr, err)
			return err
		}

		log.Printf("[Broadcast] Successfully sent message to %s\n", addr)
	}

	log.Println("[Broadcast] Message broadcast complete")
	return nil
}

func NewServer(opts ServerOpts) *Server {
	StoreOpts := storage.StructOpts{
		PathTransformFunc: opts.pathTransform,
		Metadata:          opts.metaData,
		Root:              opts.storageRoot,
	}

	return &Server{
		peers:       map[string]peer2peer.Peer{},
		serverOpts:  opts,
		Store:       storage.NewStore(StoreOpts),
		quitch:      make(chan struct{}),
		pendingFile: make(map[string]chan io.Reader),
	}
}

func (s *Server) Run() error {
	err := s.serverOpts.tcpTransport.ListenAndAccept()
	if err != nil {
		return err
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
				return
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

	peer, ok := s.peers[from]
	if !ok {
		return os.ErrNotExist
	}

	fs, r, err := s.Store.ReadStream(msg.Key)
	if err != nil {
		log.Printf("HANDLE_GET: Error reading file for key '%s' from disk: %v", msg.Key, err)
		return fmt.Errorf("HANDLE_GET: error fetching file from disk: %+v", err)
	}

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

	// if ch, ok := s.pendingFile[msg.Key]; ok {
	// 	log.Printf("HANDLE_GET: Forwarding file stream to requester for key '%s'", msg.Key)
	// 	ch <- r
	// } else {
	// 	log.Printf("HANDLE_GET: File '%s' sent to peer '%s' but no one waiting in pendingFile", msg.Key, from)
	// }

	return nil
}

func (s *Server) handleStoreMessage(from string, msg *MessageStoreFile) error {
	log.Printf("HANDLE_STORE: Received store request for key %s from %s", msg.Key, from)

	peer, ok := s.peers[from]
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

	log.Printf("HANDLE_STORE: Successfully stored [%d] bytes to %s from %s", n, msg.Key, from)
	return nil
}

func (s *Server) handleLocalMessage(from string, msg *MessageLocalFile) error {
	peer := s.peers[from]
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

	// Read and decrypt
	_, r, err := s.Store.ReadStream(msg.Key)
	if err != nil {
		log.Printf("HANDLE_LOCAL: Read error for %s: %v", msg.Key, err)
		return err
	}

	buf := new(bytes.Buffer)
	if _, err := io.Copy(buf, r); err != nil {
		log.Printf("HANDLE_LOCAL: Copy error: %v", err)
		return err
	}

	log.Printf("HANDLE_LOCAL: Key = %s, ExpectedSize = %d, WrittenSize = %d", msg.Key, msg.Size, n)

	decodedKey, err := hex.DecodeString(fm.EncryptedKey)
	if err != nil {
		return fmt.Errorf("failed to decode hex key: %w", err)
	}

	log.Printf("HANDLE_LOCAL: EncryptedKey = %x", decodedKey)

	plainData, err := s.serverOpts.Encryption.DecryptFile(buf.Bytes(), []byte(decodedKey))
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
		ch <- bytes.NewReader(plainData)
	} else {
		log.Printf("HANDLE_LOCAL: No waiting channel for key %s", msg.Key)
	}

	return nil
}

func (s *Server) Stop() {
	close(s.quitch)
}

func (s *Server) BootstrapNetwork() error {
	for _, addr := range s.serverOpts.bootstrapNodes {

		if addr == "" {
			continue
		}
		go func() {
			fmt.Println("attempting to connect with remote: ", addr)
			if err := s.serverOpts.tcpTransport.Dial(addr); err != nil {
				log.Println("Dial error:", err)
			}
		}()
	}

	return nil
}

func (s *Server) OnPeer(p peer2peer.Peer) error {
	s.peerLock.Lock()
	defer s.peerLock.Unlock()

	addr := p.RemoteAddr()

	s.peers[addr.String()] = p
	log.Printf("[OnPeer] Connected with remote peer: %s\n", addr.String())
	return nil
}

func init() {
	gob.Register(&MessageStoreFile{})
	gob.Register(&MessageGetFile{})
	gob.Register(&MessageLocalFile{})
}

func MakeServer(listenAddr string, node ...string) *Server {
	metaPath := "_metadata.json"
	EncryptionServiceKey := "qwerty12345"

	tcpOpts := peer2peer.TCPTransportOpts{
		ListenAddr:    listenAddr,
		HandshakeFunc: peer2peer.NOPEHandshakeFunc,
		Decoder:       peer2peer.DefaultDecoder{},
	}
	tcpTransport := peer2peer.NewTCPTransport(tcpOpts)

	s := &Server{} // create server first to use its OnPeer
	tcpTransport.OnPeer = s.OnPeer

	opts := ServerOpts{
		pathTransform:  storage.CASPathTransformFunc,
		tcpTransport:   *tcpTransport,
		metaData:       storage.NewMetaFile(listenAddr + metaPath),
		bootstrapNodes: node,
		storageRoot:    listenAddr + "_network",
		Encryption:     crypto.NewEncryptionService(EncryptionServiceKey),
	}

	*s = *NewServer(opts)
	return s
}
