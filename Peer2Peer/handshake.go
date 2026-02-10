package peer2peer

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"log"
	"net"
)

// HandshakeFunc is a function type for performing handshakes with peers.
type HandshakeFunc func(Peer) error

// NOPEHandshakeFunc is a no-op handshake that always succeeds.
// WARNING: This does not verify peer identity. Only use for testing/development.
func NOPEHandshakeFunc(Peer) error { return nil }

// TLSVerifyHandshakeFunc verifies that the peer presented a valid TLS certificate.
// This should be used with mTLS configuration where the TLS library already
// verified the certificate chain. This function logs the verified peer identity.
func TLSVerifyHandshakeFunc(p Peer) error {
	// The Peer interface embeds net.Conn, so we can type assert directly
	var conn net.Conn = p

	// Check if it's a TLS connection
	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		log.Printf("HANDSHAKE: Connection is not TLS (plain TCP), skipping certificate verification")
		return nil // Allow non-TLS connections (if TLS is disabled)
	}

	// Force the TLS handshake to complete (if not already)
	if err := tlsConn.Handshake(); err != nil {
		return fmt.Errorf("TLS handshake failed: %w", err)
	}

	// Get connection state after handshake
	state := tlsConn.ConnectionState()

	// Verify we have peer certificates (should be present with mTLS)
	if len(state.PeerCertificates) == 0 {
		return fmt.Errorf("peer did not present a certificate")
	}

	// Log peer identity from certificate
	peerCert := state.PeerCertificates[0]
	fingerprint := sha256.Sum256(peerCert.Raw)
	fingerprintHex := hex.EncodeToString(fingerprint[:])

	log.Printf("HANDSHAKE: Verified peer certificate:")
	log.Printf("  Subject: %s", peerCert.Subject.CommonName)
	log.Printf("  Issuer: %s", peerCert.Issuer.CommonName)
	log.Printf("  Fingerprint (SHA256): %s", fingerprintHex)
	log.Printf("  Valid from: %s to %s", peerCert.NotBefore, peerCert.NotAfter)
	log.Printf("  TLS Version: %s", tlsVersionString(state.Version))
	log.Printf("  Cipher Suite: %s", tls.CipherSuiteName(state.CipherSuite))

	return nil
}

// MakeFingerprintVerifyHandshake creates a handshake function that verifies
// peer certificates against a set of allowed fingerprints.
// This is useful when you don't have a CA but want to pin specific peer certs.
func MakeFingerprintVerifyHandshake(allowedFingerprints map[string]bool) HandshakeFunc {
	return func(p Peer) error {
		var conn net.Conn = p

		tlsConn, ok := conn.(*tls.Conn)
		if !ok {
			return fmt.Errorf("expected TLS connection for fingerprint verification")
		}

		if err := tlsConn.Handshake(); err != nil {
			return fmt.Errorf("TLS handshake failed: %w", err)
		}

		state := tlsConn.ConnectionState()
		if len(state.PeerCertificates) == 0 {
			return fmt.Errorf("peer did not present a certificate")
		}

		peerCert := state.PeerCertificates[0]
		fingerprint := sha256.Sum256(peerCert.Raw)
		fingerprintHex := hex.EncodeToString(fingerprint[:])

		if !allowedFingerprints[fingerprintHex] {
			return fmt.Errorf("peer certificate fingerprint %s not in allowed list", fingerprintHex)
		}

		log.Printf("HANDSHAKE: Verified peer fingerprint: %s (Subject: %s)",
			fingerprintHex, peerCert.Subject.CommonName)
		return nil
	}
}

// tlsVersionString returns a human-readable TLS version string.
func tlsVersionString(version uint16) string {
	switch version {
	case tls.VersionTLS10:
		return "TLS 1.0"
	case tls.VersionTLS11:
		return "TLS 1.1"
	case tls.VersionTLS12:
		return "TLS 1.2"
	case tls.VersionTLS13:
		return "TLS 1.3"
	default:
		return fmt.Sprintf("Unknown (0x%04x)", version)
	}
}
