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
	payload any
}

type MessageStoreFile struct {
	key string
	//size int64
}

type MessageGetFile struct {
	key string
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
		payload: MessageGetFile{
			key: key,
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
	buff := new(bytes.Buffer)
	tee := io.TeeReader(w, buff)

	err := s.Store.WriteStream(key, tee)
	if err != nil {
		return err
	}

	p := &Message{
		payload: MessageStoreFile{
			key: key,
		},
		//size: ,
	}

	err = s.Broadcast(*p)
	if err != nil {
		return err
	}

	time.Sleep(time.Millisecond * 50)

	peerList := []io.Writer{}
	for _, peer := range s.peers {
		peerList = append(peerList, peer)
	}

	mw := io.MultiWriter(peerList...)
	err = gob.NewEncoder(mw).Encode(buff)
	if err != nil {
		return err
	}

	return nil
}

func (s *Server) Broadcast(d Message) error {
	// peerList := []io.Writer{}
	// for _, peer := range s.peers {
	// 	peerList = append(peerList, peer)
	// }

	// mw := io.MultiWriter(peerList...)

	// msg := Message{
	// 	from:    "self",
	// 	payload: &d,
	// }

	// return gob.NewEncoder(mw).Encode(msg)
	buff := new(bytes.Buffer)
	err := gob.NewEncoder(buff).Encode(d)
	if err != nil {
		return err
	}

	for _, peer := range s.peers {
		if err := peer.Send(buff.Bytes()); err != nil {
			fmt.Errorf("error sending to peer: %s", peer.RemoteAddr())
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
	switch m := msg.payload.(type) {
	case *MessageStoreFile:
		return s.handleStoreMessage(from, m)

	case *MessageGetFile:
		return s.handleGetMessage(from, m)
	}

	return nil
}

func (s *Server) handleGetMessage(from string, msg *MessageGetFile) error {
	_, ok := s.peers[from]
	if !ok {
		return fmt.Errorf("peer (%s) not found", from)
	}

	r, err := s.Store.ReadStream(msg.key)
	if err != nil {
		return fmt.Errorf("error fetching file from disk: %+v", err)
	}

	buff := new(bytes.Buffer)
	_, err = io.Copy(buff, r)
	if err != nil {
		return fmt.Errorf("error copying file to buffer: %w", err)
	}

	if ch, ok := s.pendingFile[msg.key]; ok {
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

func (s *Server) handleStoreMessage(from string, msg *MessageStoreFile) error {

	peer, ok := s.peers[from]
	if !ok {
		return fmt.Errorf("peer (%s) not found", from)
	}

	r := peer.(io.Reader)

	err := s.Store.WriteStream(msg.key, r)
	if err != nil {
		return fmt.Errorf("error storing file to disk: %+v", err)
	}

	buff := new(bytes.Buffer)
	n, err := io.Copy(buff, r)
	if err != nil {
		return err
	}

	fmt.Printf("written (%d) bytes to disk", n)
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
