package Crypto

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// =============================================================================
// CA (Certificate Authority) Management
// =============================================================================

// GenerateCA creates a new Certificate Authority keypair and saves to disk.
// The CA is used to sign node certificates for mTLS.
// Returns the CA certificate and private key for immediate use.
func GenerateCA(certPath, keyPath string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	// Generate CA private key (P-256 curve)
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate CA private key: %w", err)
	}

	// Generate random serial number (required for uniqueness)
	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate serial number: %w", err)
	}

	// CA certificate template (10 year validity)
	caTemplate := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization:       []string{"DFS-Go"},
			OrganizationalUnit: []string{"Certificate Authority"},
			CommonName:         "DFS-Go Root CA",
		},
		NotBefore:             time.Now().Add(-1 * time.Hour), // backdate to avoid clock skew issues
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            1, // can sign end-entity certs only
		MaxPathLenZero:        false,
	}

	// Self-sign the CA certificate
	caDER, err := x509.CreateCertificate(rand.Reader, &caTemplate, &caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create CA certificate: %w", err)
	}

	// Parse back to get the certificate object
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse CA certificate: %w", err)
	}

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(certPath), 0755); err != nil {
		return nil, nil, fmt.Errorf("failed to create CA directory: %w", err)
	}

	// Save CA certificate
	if err := saveCertToFile(certPath, caDER); err != nil {
		return nil, nil, err
	}

	// Save CA private key
	if err := saveKeyToFile(keyPath, caKey); err != nil {
		return nil, nil, err
	}

	return caCert, caKey, nil
}

// LoadCA loads an existing CA certificate and private key from disk.
func LoadCA(certPath, keyPath string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	// Load CA certificate
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read CA cert: %w", err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, nil, fmt.Errorf("failed to decode CA cert PEM")
	}
	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse CA cert: %w", err)
	}

	// Load CA private key
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read CA key: %w", err)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil || keyBlock.Type != "EC PRIVATE KEY" {
		return nil, nil, fmt.Errorf("failed to decode CA key PEM")
	}
	caKey, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse CA key: %w", err)
	}

	return caCert, caKey, nil
}

// LoadOrGenerateCA tries to load existing CA, or generates a new one if not found.
func LoadOrGenerateCA(certPath, keyPath string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	if _, err := os.Stat(certPath); err == nil {
		if _, err := os.Stat(keyPath); err == nil {
			return LoadCA(certPath, keyPath)
		}
	}
	return GenerateCA(certPath, keyPath)
}

// =============================================================================
// Node Certificate Generation (signed by CA)
// =============================================================================

// NodeCertOptions contains options for generating a node certificate.
type NodeCertOptions struct {
	NodeID   string   // unique node identifier (used in CommonName)
	IPs      []net.IP // IP addresses for SAN
	DNSNames []string // DNS names for SAN
}

// GenerateNodeCert creates a new node certificate signed by the CA.
// The certificate is valid for both server and client authentication (mTLS).
func GenerateNodeCert(caCert *x509.Certificate, caKey *ecdsa.PrivateKey, certPath, keyPath string, opts NodeCertOptions) error {
	// Generate node private key
	nodeKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("failed to generate node private key: %w", err)
	}

	// Generate random serial number
	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("failed to generate serial number: %w", err)
	}

	// Default SANs if not provided
	ips := opts.IPs
	if len(ips) == 0 {
		ips = []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	}
	dnsNames := opts.DNSNames
	if len(dnsNames) == 0 {
		dnsNames = []string{"localhost"}
	}

	// Node certificate template (1 year validity)
	nodeTemplate := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization:       []string{"DFS-Go"},
			OrganizationalUnit: []string{"Node"},
			CommonName:         fmt.Sprintf("DFS-Go Node %s", opts.NodeID),
		},
		NotBefore:             time.Now().Add(-1 * time.Hour), // backdate for clock skew
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
		IPAddresses:           ips,
		DNSNames:              dnsNames,
	}

	// Sign node certificate with CA
	nodeDER, err := x509.CreateCertificate(rand.Reader, &nodeTemplate, caCert, &nodeKey.PublicKey, caKey)
	if err != nil {
		return fmt.Errorf("failed to create node certificate: %w", err)
	}

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(certPath), 0755); err != nil {
		return fmt.Errorf("failed to create cert directory: %w", err)
	}

	// Save node certificate
	if err := saveCertToFile(certPath, nodeDER); err != nil {
		return err
	}

	// Save node private key
	if err := saveKeyToFile(keyPath, nodeKey); err != nil {
		return err
	}

	return nil
}

// LoadOrGenerateNodeCert tries to load existing node cert, or generates a new one.
func LoadOrGenerateNodeCert(caCert *x509.Certificate, caKey *ecdsa.PrivateKey, certPath, keyPath string, opts NodeCertOptions) error {
	if _, err := os.Stat(certPath); err == nil {
		if _, err := os.Stat(keyPath); err == nil {
			// Cert exists, verify it's signed by our CA
			return verifyNodeCert(certPath, caCert)
		}
	}
	return GenerateNodeCert(caCert, caKey, certPath, keyPath, opts)
}

// verifyNodeCert checks that a node cert is signed by the given CA.
func verifyNodeCert(certPath string, caCert *x509.Certificate) error {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return err
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return fmt.Errorf("failed to decode cert PEM")
	}
	nodeCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return err
	}

	// Verify the node cert was signed by CA
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	_, err = nodeCert.Verify(x509.VerifyOptions{
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	})
	return err
}

// =============================================================================
// mTLS Configuration
// =============================================================================

// LoadServerTLSConfig creates a TLS config for server-side mTLS.
// It requires client certificates and verifies them against the CA.
func LoadServerTLSConfig(certPath, keyPath, caPath string) (*tls.Config, error) {
	// Load server certificate
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load server certificate: %w", err)
	}

	// Load CA certificate for client verification
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read CA certificate: %w", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("failed to parse CA certificate")
	}

	config := &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    caPool,
		ClientAuth:   tls.RequireAndVerifyClientCert, // enforce mTLS
		MinVersion:   tls.VersionTLS12,
		CurvePreferences: []tls.CurveID{
			tls.X25519,
			tls.CurveP256,
		},
	}

	return config, nil
}

// LoadClientTLSConfig creates a TLS config for client-side mTLS.
// It presents a client certificate and verifies server certificate against the CA.
func LoadClientTLSConfig(certPath, keyPath, caPath string) (*tls.Config, error) {
	// Load client certificate
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load client certificate: %w", err)
	}

	// Load CA certificate for server verification
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read CA certificate: %w", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("failed to parse CA certificate")
	}

	config := &tls.Config{
		Certificates:       []tls.Certificate{cert},
		RootCAs:            caPool,
		InsecureSkipVerify: false, // VERIFY server certificate
		MinVersion:         tls.VersionTLS12,
		CurvePreferences: []tls.CurveID{
			tls.X25519,
			tls.CurveP256,
		},
	}

	return config, nil
}

// LoadMTLSConfig creates a combined TLS config that works for both server and client.
// In P2P scenarios, each node acts as both server (accepting connections) and client (dialing peers).
func LoadMTLSConfig(certPath, keyPath, caPath string) (*tls.Config, error) {
	// Load node certificate
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load certificate: %w", err)
	}

	// Load CA certificate
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read CA certificate: %w", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("failed to parse CA certificate")
	}

	config := &tls.Config{
		Certificates: []tls.Certificate{cert},
		// Server-side: verify client certs
		ClientCAs:  caPool,
		ClientAuth: tls.RequireAndVerifyClientCert,
		// Client-side: verify server certs
		RootCAs:            caPool,
		InsecureSkipVerify: false,
		// Best practices
		MinVersion: tls.VersionTLS12,
		CurvePreferences: []tls.CurveID{
			tls.X25519,
			tls.CurveP256,
		},
	}

	return config, nil
}

// =============================================================================
// Helper Functions
// =============================================================================

func saveCertToFile(path string, derBytes []byte) error {
	certOut, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("failed to create cert file: %w", err)
	}
	defer certOut.Close()
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes}); err != nil {
		return fmt.Errorf("failed to write cert: %w", err)
	}
	return nil
}

func saveKeyToFile(path string, key *ecdsa.PrivateKey) error {
	keyOut, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("failed to create key file: %w", err)
	}
	defer keyOut.Close()

	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("failed to marshal private key: %w", err)
	}
	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}); err != nil {
		return fmt.Errorf("failed to write key: %w", err)
	}
	return nil
}

// =============================================================================
// Legacy Functions (kept for backward compatibility, marked deprecated)
// =============================================================================

// Deprecated: Use LoadOrGenerateCA + GenerateNodeCert + LoadMTLSConfig instead.
// GenerateSelfSignedCert generates a self-signed certificate (not CA-signed).
// This is insecure for production - use CA-signed certs for mTLS.
func GenerateSelfSignedCert(certPath, keyPath string) (*tls.Config, error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate private key: %w", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("failed to generate serial number: %w", err)
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"DFS-Go"},
			CommonName:   "DFS-Go Node",
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:              []string{"localhost"},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create certificate: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(certPath), 0755); err != nil {
		return nil, fmt.Errorf("failed to create cert directory: %w", err)
	}

	if err := saveCertToFile(certPath, derBytes); err != nil {
		return nil, err
	}
	if err := saveKeyToFile(keyPath, privateKey); err != nil {
		return nil, err
	}

	return LoadTLSConfigInsecure(certPath, keyPath)
}

// Deprecated: Use LoadMTLSConfig instead.
// LoadTLSConfig loads certs without CA verification (insecure).
func LoadTLSConfig(certPath, keyPath string) (*tls.Config, error) {
	return LoadTLSConfigInsecure(certPath, keyPath)
}

// LoadTLSConfigInsecure loads a certificate without CA verification.
// Only use for development/testing. Vulnerable to MITM attacks.
func LoadTLSConfigInsecure(certPath, keyPath string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load certificate: %w", err)
	}

	config := &tls.Config{
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: true, // WARNING: insecure!
		MinVersion:         tls.VersionTLS12,
	}

	return config, nil
}

// Deprecated: Use LoadOrGenerateCA + LoadOrGenerateNodeCert + LoadMTLSConfig instead.
func LoadOrGenerateTLSConfig(certPath, keyPath string) (*tls.Config, error) {
	if _, err := os.Stat(certPath); err == nil {
		if _, err := os.Stat(keyPath); err == nil {
			return LoadTLSConfigInsecure(certPath, keyPath)
		}
	}
	return GenerateSelfSignedCert(certPath, keyPath)
}
