package main

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	peer2peer "github.com/Faizan2005/DFS-Go/Peer2Peer"
)

type ServerOpts struct {
	storageRoot    string
	pathTransform  pathTransform
	tcpTransport   peer2peer.TCPTransport
	metaData       Metadata
	bootstrapNodes []string
}

type Server struct {
	peerLock sync.Mutex
	peers    map[string]peer2peer.Peer

	serverOpts  ServerOpts
	Store       *Store
	quitch      chan struct{}
	pendingFile map[string]chan io.Reader
}

type Message struct {
	Payload any
}

type MessageStoreFile struct {
	Key string
	//size int64
}

type MessageGetFile struct {
	Key string
}

func (s *Server) GetData(key string) (io.Reader, error) {
	if s.Store.Has(key) {
		fmt.Print("reading from disk")
		w, err := s.Store.ReadStream(key)
		if err != nil {
			return nil, err
		}
		return w, nil
	}

	fmt.Print("file not available on disk")

	p := &Message{
		Payload: MessageGetFile{
			Key: key,
		},
	}

	ch := make(chan io.Reader, 1)
	s.pendingFile[key] = ch

	if err := s.Broadcast(*p); err != nil {
		return nil, err
	}

	time.Sleep(time.Millisecond * 50)

	r := <-ch
	delete(s.pendingFile, key)

	return r, nil
}

func (s *Server) StoreData(key string, w io.Reader) error {
	log.Println("STORE_DATA: Starting storage process for key:", key)

	// First read ALL data into buffer
	buff := new(bytes.Buffer)
	if _, err := io.Copy(buff, w); err != nil {
		log.Println("STORE_DATA: Error buffering data:", err)
		return err
	}

	// Store from buffer
	log.Println("STORE_DATA: Storing locally...")
	if err := s.Store.WriteStream(key, bytes.NewReader(buff.Bytes())); err != nil {
		log.Println("STORE_DATA: Local storage failed:", err)
		return err
	}
	log.Println("STORE_DATA: Local storage successful")

	// Rest of the function remains the same...
	p := &Message{
		Payload: MessageStoreFile{Key: key},
	}

	log.Println("STORE_DATA: Broadcasting store message...")
	if err := s.Broadcast(*p); err != nil {
		log.Println("STORE_DATA: Broadcast failed:", err)
		return err
	}

	time.Sleep(time.Millisecond * 50)
	log.Println("STORE_DATA: Starting peer distribution...")

	for addr, peer := range s.peers {
		log.Printf("STORE_DATA: Processing peer %s", addr)

		log.Printf("STORE_DATA: Sending stream signal to %s", addr)
		if err := peer.Send([]byte{peer2peer.IncomingStream}); err != nil {
			log.Printf("STORE_DATA: Failed to signal stream to %s: %v", addr, err)
			continue
		}

		log.Printf("STORE_DATA: Sending file data to %s", addr)
		n, err := io.Copy(peer, bytes.NewReader(buff.Bytes()))
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
	buf := new(bytes.Buffer)
	if err := gob.NewEncoder(buf).Encode(d); err != nil {
		return err
	}

	for _, peer := range s.peers {
		peer.Send([]byte{peer2peer.IncomingMessage})
		if err := peer.Send(buf.Bytes()); err != nil {
			return err
		}
	}

	return nil
}

func NewServer(opts ServerOpts) *Server {
	StoreOpts := StructOpts{
		PathTransformFunc: opts.pathTransform,
		Metadata:          &opts.metaData,
		Root:              opts.storageRoot,
	}

	return &Server{
		peers:       map[string]peer2peer.Peer{},
		serverOpts:  opts,
		Store:       NewStore(StoreOpts),
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
		log.Println("file server closed due to user quit action")
	}()

	for {
		select {
		case RPC := <-s.serverOpts.tcpTransport.Consume():
			var message Message
			err := gob.NewDecoder(bytes.NewReader(RPC.Payload)).Decode(&message)
			log.Print(err)
			fmt.Println(message)

			if err := s.handleMessage(RPC.From.String(), &message); err != nil {
				return
			}

		case <-s.quitch:
			return
		}
	}
}

func (s *Server) handleMessage(from string, msg *Message) error {
	switch m := msg.Payload.(type) {
	case *MessageStoreFile:
		return s.handleStoreMessage(from, m)

	case *MessageGetFile:
		return s.handleGetMessage(from, m)
	}

	return nil
}

func (s *Server) handleGetMessage(from string, msg *MessageGetFile) error {
	peer, ok := s.peers[from]
	if !ok {
		return fmt.Errorf("peer (%s) not found", from)
	}

	r, err := s.Store.ReadStream(msg.Key)
	if err != nil {
		return fmt.Errorf("error fetching file from disk: %+v", err)
	}

	// buff := new(bytes.Buffer)
	// _, err = io.Copy(buff, r)
	// if err != nil {
	// 	return fmt.Errorf("error copying file to buffer: %w", err)
	// }

	if err = peer.Send([]byte{peer2peer.IncomingStream}); err != nil {
		log.Printf("failed to signal stream to %s: %v", from, err)
	}

	if ch, ok := s.pendingFile[msg.Key]; ok {
		ch <- r
	} else {
		fmt.Println("Received file but nobody waiting for it")
	}

	return nil
	// err = peer.Send(buff.Bytes())
	// if err != nil {
	// 	return fmt.Errorf("error sending data to peer: %w", err)
	// }

	// return nil
}

// func (s *Server) handleStoreMessage(from string, msg *MessageStoreFile) error {
// 	log.Printf("HANDLE_STORE: Received store request for key %s from %s", msg.Key, from)

// 	peer, ok := s.peers[from]
// 	if !ok {
// 		return fmt.Errorf("peer not found")
// 	}

// 	// Get the TCPPeer to access connection
// 	tcpPeer, ok := peer.(*peer2peer.TCPPeer)
// 	if !ok {
// 		return fmt.Errorf("peer type assertion failed")
// 	}

// 	log.Println("HANDLE_STORE: Starting file storage...")
// 	err := s.Store.WriteStream(msg.Key, tcpPeer.Conn)
// 	if err != nil {
// 		log.Println("HANDLE_STORE: Storage failed:", err)
// 		return err
// 	}

// 	log.Println("HANDLE_STORE: Closing stream...")
// 	//tcpPeer.Wg.Done()

// 	log.Printf("HANDLE_STORE: Successfully stored file %s", msg.Key)
// 	return nil
// }

func (s *Server) handleStoreMessage(from string, msg *MessageStoreFile) error {
	log.Printf("HANDLE_STORE: Received store request for key %s from %s", msg.Key, from)

	peer, ok := s.peers[from]
	if !ok {
		err := fmt.Errorf("peer (%s) not found", from)
		log.Println("HANDLE_STORE: Error:", err)
		return err
	}

	log.Println("HANDLE_STORE: Starting file storage...")
	err := s.Store.WriteStream(msg.Key, peer.(io.Reader))
	if err != nil {
		log.Println("HANDLE_STORE: Storage failed:", err)
		return fmt.Errorf("error storing file to disk: %+v", err)
	}

	log.Println("HANDLE_STORE: Closing stream...")
	peer.CloseStream()

	log.Printf("HANDLE_STORE: Successfully stored file %s from %s", msg.Key, from)
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
	gob.Register(MessageStoreFile{})
	gob.Register(MessageGetFile{})
}
