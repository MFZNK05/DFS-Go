package Crypto

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestCAGeneration verifies CA certificate generation
func TestCAGeneration(t *testing.T) {
	// Create temp directory for certs
	tempDir, err := os.MkdirTemp("", "tls_test_ca")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	caCertPath := filepath.Join(tempDir, "ca.crt")
	caKeyPath := filepath.Join(tempDir, "ca.key")

	// Generate CA
	caCert, caKey, err := GenerateCA(caCertPath, caKeyPath)
	if err != nil {
		t.Fatalf("GenerateCA failed: %v", err)
	}

	// Verify CA certificate properties
	if !caCert.IsCA {
		t.Error("CA certificate should have IsCA = true")
	}
	if caCert.Subject.CommonName != "DFS-Go Root CA" {
		t.Errorf("Unexpected CA CommonName: %s", caCert.Subject.CommonName)
	}
	if caKey == nil {
		t.Error("CA private key should not be nil")
	}

	// Verify files were created
	if _, err := os.Stat(caCertPath); os.IsNotExist(err) {
		t.Error("CA certificate file was not created")
	}
	if _, err := os.Stat(caKeyPath); os.IsNotExist(err) {
		t.Error("CA key file was not created")
	}

	// Verify key permissions (should be 0600)
	info, err := os.Stat(caKeyPath)
	if err != nil {
		t.Fatalf("Failed to stat CA key: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("CA key should have 0600 permissions, got %o", info.Mode().Perm())
	}

	t.Logf("CA generated successfully: %s", caCert.Subject.CommonName)
}

// TestCALoadExisting verifies loading an existing CA
func TestCALoadExisting(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "tls_test_ca_load")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	caCertPath := filepath.Join(tempDir, "ca.crt")
	caKeyPath := filepath.Join(tempDir, "ca.key")

	// Generate CA first
	origCert, _, err := GenerateCA(caCertPath, caKeyPath)
	if err != nil {
		t.Fatalf("GenerateCA failed: %v", err)
	}

	// Load the same CA
	loadedCert, loadedKey, err := LoadCA(caCertPath, caKeyPath)
	if err != nil {
		t.Fatalf("LoadCA failed: %v", err)
	}

	// Verify loaded cert matches original
	if loadedCert.Subject.CommonName != origCert.Subject.CommonName {
		t.Errorf("Loaded CA CommonName mismatch: got %s, want %s",
			loadedCert.Subject.CommonName, origCert.Subject.CommonName)
	}
	if loadedKey == nil {
		t.Error("Loaded CA key should not be nil")
	}
}

// TestLoadOrGenerateCA verifies the idempotent CA loading
func TestLoadOrGenerateCA(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "tls_test_ca_idempotent")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	caCertPath := filepath.Join(tempDir, "ca.crt")
	caKeyPath := filepath.Join(tempDir, "ca.key")

	// First call should generate
	cert1, _, err := LoadOrGenerateCA(caCertPath, caKeyPath)
	if err != nil {
		t.Fatalf("First LoadOrGenerateCA failed: %v", err)
	}
	serial1 := cert1.SerialNumber

	// Second call should load existing (same serial)
	cert2, _, err := LoadOrGenerateCA(caCertPath, caKeyPath)
	if err != nil {
		t.Fatalf("Second LoadOrGenerateCA failed: %v", err)
	}
	serial2 := cert2.SerialNumber

	if serial1.Cmp(serial2) != 0 {
		t.Error("LoadOrGenerateCA should return same CA on second call")
	}
}

// TestNodeCertGeneration verifies node certificate generation
func TestNodeCertGeneration(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "tls_test_node")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	caCertPath := filepath.Join(tempDir, "ca.crt")
	caKeyPath := filepath.Join(tempDir, "ca.key")
	nodeCertPath := filepath.Join(tempDir, "node.crt")
	nodeKeyPath := filepath.Join(tempDir, "node.key")

	// Generate CA
	caCert, caKey, err := GenerateCA(caCertPath, caKeyPath)
	if err != nil {
		t.Fatalf("GenerateCA failed: %v", err)
	}

	// Generate node certificate
	opts := NodeCertOptions{
		NodeID:   "test-node-1",
		IPs:      nil, // use defaults
		DNSNames: nil, // use defaults
	}
	err = GenerateNodeCert(caCert, caKey, nodeCertPath, nodeKeyPath, opts)
	if err != nil {
		t.Fatalf("GenerateNodeCert failed: %v", err)
	}

	// Verify node cert was created
	if _, err := os.Stat(nodeCertPath); os.IsNotExist(err) {
		t.Error("Node certificate file was not created")
	}
	if _, err := os.Stat(nodeKeyPath); os.IsNotExist(err) {
		t.Error("Node key file was not created")
	}

	// Verify node cert is signed by CA
	err = verifyNodeCert(nodeCertPath, caCert)
	if err != nil {
		t.Errorf("Node cert verification failed: %v", err)
	}

	t.Log("Node certificate generated and verified successfully")
}

// TestMTLSConfigLoading verifies mTLS config loading
func TestMTLSConfigLoading(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "tls_test_mtls_config")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Setup CA and node cert
	caCertPath := filepath.Join(tempDir, "ca.crt")
	caKeyPath := filepath.Join(tempDir, "ca.key")
	nodeCertPath := filepath.Join(tempDir, "node.crt")
	nodeKeyPath := filepath.Join(tempDir, "node.key")

	caCert, caKey, err := GenerateCA(caCertPath, caKeyPath)
	if err != nil {
		t.Fatalf("GenerateCA failed: %v", err)
	}

	err = GenerateNodeCert(caCert, caKey, nodeCertPath, nodeKeyPath, NodeCertOptions{NodeID: "test"})
	if err != nil {
		t.Fatalf("GenerateNodeCert failed: %v", err)
	}

	// Load mTLS config
	config, err := LoadMTLSConfig(nodeCertPath, nodeKeyPath, caCertPath)
	if err != nil {
		t.Fatalf("LoadMTLSConfig failed: %v", err)
	}

	// Verify config properties
	if len(config.Certificates) == 0 {
		t.Error("mTLS config should have certificates")
	}
	if config.ClientCAs == nil {
		t.Error("mTLS config should have ClientCAs for server-side verification")
	}
	if config.RootCAs == nil {
		t.Error("mTLS config should have RootCAs for client-side verification")
	}
	if config.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Error("mTLS config should require and verify client certs")
	}
	if config.InsecureSkipVerify {
		t.Error("mTLS config should NOT have InsecureSkipVerify")
	}
	if config.MinVersion != tls.VersionTLS12 {
		t.Error("mTLS config should have MinVersion TLS 1.2")
	}

	t.Log("mTLS config loaded successfully with proper security settings")
}

// TestMTLSConnection tests an actual mTLS connection between two peers
func TestMTLSConnection(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "tls_test_mtls_conn")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Setup shared CA
	caCertPath := filepath.Join(tempDir, "ca.crt")
	caKeyPath := filepath.Join(tempDir, "ca.key")
	caCert, caKey, err := GenerateCA(caCertPath, caKeyPath)
	if err != nil {
		t.Fatalf("GenerateCA failed: %v", err)
	}

	// Setup server node cert
	serverCertPath := filepath.Join(tempDir, "server.crt")
	serverKeyPath := filepath.Join(tempDir, "server.key")
	err = GenerateNodeCert(caCert, caKey, serverCertPath, serverKeyPath, NodeCertOptions{NodeID: "server"})
	if err != nil {
		t.Fatalf("GenerateNodeCert (server) failed: %v", err)
	}

	// Setup client node cert
	clientCertPath := filepath.Join(tempDir, "client.crt")
	clientKeyPath := filepath.Join(tempDir, "client.key")
	err = GenerateNodeCert(caCert, caKey, clientCertPath, clientKeyPath, NodeCertOptions{NodeID: "client"})
	if err != nil {
		t.Fatalf("GenerateNodeCert (client) failed: %v", err)
	}

	// Load server and client configs
	serverConfig, err := LoadServerTLSConfig(serverCertPath, serverKeyPath, caCertPath)
	if err != nil {
		t.Fatalf("LoadServerTLSConfig failed: %v", err)
	}

	clientConfig, err := LoadClientTLSConfig(clientCertPath, clientKeyPath, caCertPath)
	if err != nil {
		t.Fatalf("LoadClientTLSConfig failed: %v", err)
	}

	// Start TLS server
	listener, err := tls.Listen("tcp", "127.0.0.1:0", serverConfig)
	if err != nil {
		t.Fatalf("Failed to start TLS listener: %v", err)
	}
	defer listener.Close()

	serverAddr := listener.Addr().String()
	testMessage := []byte("Hello mTLS!")

	// Server goroutine
	var serverErr error
	var receivedData []byte
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		conn, err := listener.Accept()
		if err != nil {
			serverErr = fmt.Errorf("accept failed: %w", err)
			return
		}
		defer conn.Close()

		// Force TLS handshake
		tlsConn := conn.(*tls.Conn)
		if err := tlsConn.Handshake(); err != nil {
			serverErr = fmt.Errorf("server handshake failed: %w", err)
			return
		}

		// Verify client presented certificate
		state := tlsConn.ConnectionState()
		if len(state.PeerCertificates) == 0 {
			serverErr = fmt.Errorf("client did not present certificate")
			return
		}

		t.Logf("Server: Connected to client with cert CN=%s", state.PeerCertificates[0].Subject.CommonName)

		// Read message
		receivedData = make([]byte, 1024)
		n, err := conn.Read(receivedData)
		if err != nil && err != io.EOF {
			serverErr = fmt.Errorf("read failed: %w", err)
			return
		}
		receivedData = receivedData[:n]
	}()

	// Give server time to start
	time.Sleep(50 * time.Millisecond)

	// Client connects
	conn, err := tls.Dial("tcp", serverAddr, clientConfig)
	if err != nil {
		t.Fatalf("Client dial failed: %v", err)
	}
	defer conn.Close()

	// Verify server certificate
	state := conn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		t.Error("Server did not present certificate")
	} else {
		t.Logf("Client: Connected to server with cert CN=%s", state.PeerCertificates[0].Subject.CommonName)
	}

	// Send message
	_, err = conn.Write(testMessage)
	if err != nil {
		t.Fatalf("Client write failed: %v", err)
	}
	conn.Close()

	// Wait for server
	wg.Wait()

	if serverErr != nil {
		t.Fatalf("Server error: %v", serverErr)
	}

	if string(receivedData) != string(testMessage) {
		t.Errorf("Message mismatch: got %q, want %q", receivedData, testMessage)
	}

	t.Log("mTLS connection test passed: bidirectional certificate verification works")
}

// TestMTLSRejectsUntrustedClient verifies that mTLS rejects clients without valid certs
func TestMTLSRejectsUntrustedClient(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "tls_test_mtls_reject")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Setup CA 1 (trusted by server)
	ca1CertPath := filepath.Join(tempDir, "ca1.crt")
	ca1KeyPath := filepath.Join(tempDir, "ca1.key")
	ca1Cert, ca1Key, err := GenerateCA(ca1CertPath, ca1KeyPath)
	if err != nil {
		t.Fatalf("GenerateCA (CA1) failed: %v", err)
	}

	// Setup CA 2 (untrusted, used by rogue client)
	ca2CertPath := filepath.Join(tempDir, "ca2.crt")
	ca2KeyPath := filepath.Join(tempDir, "ca2.key")
	ca2Cert, ca2Key, err := GenerateCA(ca2CertPath, ca2KeyPath)
	if err != nil {
		t.Fatalf("GenerateCA (CA2) failed: %v", err)
	}

	// Server uses CA1
	serverCertPath := filepath.Join(tempDir, "server.crt")
	serverKeyPath := filepath.Join(tempDir, "server.key")
	err = GenerateNodeCert(ca1Cert, ca1Key, serverCertPath, serverKeyPath, NodeCertOptions{NodeID: "server"})
	if err != nil {
		t.Fatalf("GenerateNodeCert (server) failed: %v", err)
	}

	// Rogue client uses CA2 (not trusted by server)
	rogueCertPath := filepath.Join(tempDir, "rogue.crt")
	rogueKeyPath := filepath.Join(tempDir, "rogue.key")
	err = GenerateNodeCert(ca2Cert, ca2Key, rogueCertPath, rogueKeyPath, NodeCertOptions{NodeID: "rogue"})
	if err != nil {
		t.Fatalf("GenerateNodeCert (rogue) failed: %v", err)
	}

	// Server config trusts only CA1
	serverConfig, err := LoadServerTLSConfig(serverCertPath, serverKeyPath, ca1CertPath)
	if err != nil {
		t.Fatalf("LoadServerTLSConfig failed: %v", err)
	}

	// Rogue client config uses CA2 cert but points to CA2 for root (won't verify server)
	// However, the server should reject the client's cert regardless
	rogueCert, err := tls.LoadX509KeyPair(rogueCertPath, rogueKeyPath)
	if err != nil {
		t.Fatalf("Failed to load rogue cert: %v", err)
	}
	rogueConfig := &tls.Config{
		Certificates:       []tls.Certificate{rogueCert},
		InsecureSkipVerify: true, // Client doesn't verify server (to focus on server rejecting client)
	}

	// Start server
	listener, err := tls.Listen("tcp", "127.0.0.1:0", serverConfig)
	if err != nil {
		t.Fatalf("Failed to start listener: %v", err)
	}
	defer listener.Close()

	serverAddr := listener.Addr().String()

	// Server goroutine - expects handshake to fail
	var serverHandshakeErr error
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		tlsConn := conn.(*tls.Conn)
		serverHandshakeErr = tlsConn.Handshake()
	}()

	// Wait for server to be ready
	time.Sleep(50 * time.Millisecond)

	// Rogue client tries to connect
	conn, err := tls.Dial("tcp", serverAddr, rogueConfig)
	if err != nil {
		// Expected: connection may fail during dial
		t.Logf("Rogue client dial failed (expected): %v", err)
	} else {
		// If dial succeeded, try to use the connection
		_, err = conn.Write([]byte("test"))
		if err != nil {
			t.Logf("Rogue client write failed (expected): %v", err)
		}
		conn.Close()
	}

	wg.Wait()

	// Server should have rejected the handshake
	if serverHandshakeErr == nil {
		t.Error("Server should have rejected untrusted client certificate")
	} else {
		t.Logf("Server correctly rejected untrusted client: %v", serverHandshakeErr)
	}
}

// TestCertificateFingerprint verifies fingerprint calculation
func TestCertificateFingerprint(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "tls_test_fingerprint")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	caCertPath := filepath.Join(tempDir, "ca.crt")
	caKeyPath := filepath.Join(tempDir, "ca.key")

	caCert, _, err := GenerateCA(caCertPath, caKeyPath)
	if err != nil {
		t.Fatalf("GenerateCA failed: %v", err)
	}

	// Calculate fingerprint
	fingerprint := sha256.Sum256(caCert.Raw)
	fingerprintHex := hex.EncodeToString(fingerprint[:])

	// Fingerprint should be 64 hex characters (256 bits = 32 bytes = 64 hex)
	if len(fingerprintHex) != 64 {
		t.Errorf("Fingerprint should be 64 hex chars, got %d", len(fingerprintHex))
	}

	t.Logf("Certificate fingerprint (SHA256): %s", fingerprintHex)
}

// TestUniqueSerialNumbers verifies that each cert gets a unique serial
func TestUniqueSerialNumbers(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "tls_test_serial")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	serials := make(map[string]bool)

	// Generate multiple CAs and check serial uniqueness
	for i := 0; i < 5; i++ {
		caCertPath := filepath.Join(tempDir, fmt.Sprintf("ca%d.crt", i))
		caKeyPath := filepath.Join(tempDir, fmt.Sprintf("ca%d.key", i))

		caCert, _, err := GenerateCA(caCertPath, caKeyPath)
		if err != nil {
			t.Fatalf("GenerateCA %d failed: %v", i, err)
		}

		serialHex := caCert.SerialNumber.Text(16)
		if serials[serialHex] {
			t.Errorf("Duplicate serial number detected: %s", serialHex)
		}
		serials[serialHex] = true
	}

	t.Log("All certificates have unique serial numbers")
}

// TestP2PMTLSConfig verifies combined P2P mTLS config works for both roles
func TestP2PMTLSConfig(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "tls_test_p2p")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Setup shared CA
	caCertPath := filepath.Join(tempDir, "ca.crt")
	caKeyPath := filepath.Join(tempDir, "ca.key")
	caCert, caKey, err := GenerateCA(caCertPath, caKeyPath)
	if err != nil {
		t.Fatalf("GenerateCA failed: %v", err)
	}

	// Setup two peer nodes
	peer1CertPath := filepath.Join(tempDir, "peer1.crt")
	peer1KeyPath := filepath.Join(tempDir, "peer1.key")
	err = GenerateNodeCert(caCert, caKey, peer1CertPath, peer1KeyPath, NodeCertOptions{NodeID: "peer1"})
	if err != nil {
		t.Fatalf("GenerateNodeCert (peer1) failed: %v", err)
	}

	peer2CertPath := filepath.Join(tempDir, "peer2.crt")
	peer2KeyPath := filepath.Join(tempDir, "peer2.key")
	err = GenerateNodeCert(caCert, caKey, peer2CertPath, peer2KeyPath, NodeCertOptions{NodeID: "peer2"})
	if err != nil {
		t.Fatalf("GenerateNodeCert (peer2) failed: %v", err)
	}

	// Both peers use combined mTLS config (works as both server and client)
	peer1Config, err := LoadMTLSConfig(peer1CertPath, peer1KeyPath, caCertPath)
	if err != nil {
		t.Fatalf("LoadMTLSConfig (peer1) failed: %v", err)
	}

	peer2Config, err := LoadMTLSConfig(peer2CertPath, peer2KeyPath, caCertPath)
	if err != nil {
		t.Fatalf("LoadMTLSConfig (peer2) failed: %v", err)
	}

	// Peer1 acts as server, Peer2 as client
	listener, err := tls.Listen("tcp", "127.0.0.1:0", peer1Config)
	if err != nil {
		t.Fatalf("Peer1 listen failed: %v", err)
	}
	defer listener.Close()

	var wg sync.WaitGroup
	var serverConnState tls.ConnectionState

	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		tlsConn := conn.(*tls.Conn)
		if err := tlsConn.Handshake(); err != nil {
			t.Errorf("Peer1 (server) handshake failed: %v", err)
			return
		}
		serverConnState = tlsConn.ConnectionState()
	}()

	time.Sleep(50 * time.Millisecond)

	// Peer2 connects to Peer1
	conn, err := tls.Dial("tcp", listener.Addr().String(), peer2Config)
	if err != nil {
		t.Fatalf("Peer2 dial failed: %v", err)
	}
	defer conn.Close()

	clientConnState := conn.ConnectionState()
	wg.Wait()

	// Verify both sides saw valid peer certificates
	if len(serverConnState.PeerCertificates) == 0 {
		t.Error("Peer1 (server) did not see peer2's certificate")
	} else {
		t.Logf("Peer1 saw: %s", serverConnState.PeerCertificates[0].Subject.CommonName)
	}

	if len(clientConnState.PeerCertificates) == 0 {
		t.Error("Peer2 (client) did not see peer1's certificate")
	} else {
		t.Logf("Peer2 saw: %s", clientConnState.PeerCertificates[0].Subject.CommonName)
	}

	t.Log("P2P mTLS connection test passed: both peers verified each other")
}

// BenchmarkCAGeneration benchmarks CA generation
func BenchmarkCAGeneration(b *testing.B) {
	tempDir, err := os.MkdirTemp("", "tls_bench_ca")
	if err != nil {
		b.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		caCertPath := filepath.Join(tempDir, fmt.Sprintf("ca%d.crt", i))
		caKeyPath := filepath.Join(tempDir, fmt.Sprintf("ca%d.key", i))
		_, _, err := GenerateCA(caCertPath, caKeyPath)
		if err != nil {
			b.Fatalf("GenerateCA failed: %v", err)
		}
	}
}

// BenchmarkNodeCertGeneration benchmarks node cert generation
func BenchmarkNodeCertGeneration(b *testing.B) {
	tempDir, err := os.MkdirTemp("", "tls_bench_node")
	if err != nil {
		b.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	caCertPath := filepath.Join(tempDir, "ca.crt")
	caKeyPath := filepath.Join(tempDir, "ca.key")
	caCert, caKey, err := GenerateCA(caCertPath, caKeyPath)
	if err != nil {
		b.Fatalf("GenerateCA failed: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		nodeCertPath := filepath.Join(tempDir, fmt.Sprintf("node%d.crt", i))
		nodeKeyPath := filepath.Join(tempDir, fmt.Sprintf("node%d.key", i))
		err := GenerateNodeCert(caCert, caKey, nodeCertPath, nodeKeyPath, NodeCertOptions{NodeID: fmt.Sprintf("node%d", i)})
		if err != nil {
			b.Fatalf("GenerateNodeCert failed: %v", err)
		}
	}
}

// BenchmarkTLSHandshake benchmarks the mTLS handshake
func BenchmarkTLSHandshake(b *testing.B) {
	tempDir, err := os.MkdirTemp("", "tls_bench_handshake")
	if err != nil {
		b.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Setup certs
	caCertPath := filepath.Join(tempDir, "ca.crt")
	caKeyPath := filepath.Join(tempDir, "ca.key")
	caCert, caKey, _ := GenerateCA(caCertPath, caKeyPath)

	serverCertPath := filepath.Join(tempDir, "server.crt")
	serverKeyPath := filepath.Join(tempDir, "server.key")
	GenerateNodeCert(caCert, caKey, serverCertPath, serverKeyPath, NodeCertOptions{NodeID: "server"})

	clientCertPath := filepath.Join(tempDir, "client.crt")
	clientKeyPath := filepath.Join(tempDir, "client.key")
	GenerateNodeCert(caCert, caKey, clientCertPath, clientKeyPath, NodeCertOptions{NodeID: "client"})

	serverConfig, _ := LoadServerTLSConfig(serverCertPath, serverKeyPath, caCertPath)
	clientConfig, _ := LoadClientTLSConfig(clientCertPath, clientKeyPath, caCertPath)

	listener, _ := tls.Listen("tcp", "127.0.0.1:0", serverConfig)
	defer listener.Close()

	// Server goroutine
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			tlsConn := conn.(*tls.Conn)
			tlsConn.Handshake()
			conn.Close()
		}
	}()

	time.Sleep(50 * time.Millisecond)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		conn, err := tls.Dial("tcp", listener.Addr().String(), clientConfig)
		if err != nil {
			b.Fatalf("Dial failed: %v", err)
		}
		conn.Close()
	}
}

// Helper to create a simple TCP connection for testing
func dialTCP(addr string) (net.Conn, error) {
	return net.Dial("tcp", addr)
}
