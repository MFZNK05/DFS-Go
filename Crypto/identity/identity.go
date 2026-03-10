// Package identity manages per-node Ed25519 + X25519 keypairs for ECDH sharing.
//
// Each node generates an identity once with Generate and stores it at
// ~/.hermond/identity.json. The Ed25519 key signs manifests (proves authorship),
// the X25519 key is used for ECDH key agreement (wraps DEKs for recipients).
//
// Public keys are injected into gossip via GossipMetadata so other nodes can
// look up recipients by alias.
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/curve25519"
)

// Identity holds both keypairs and a human-readable alias.
type Identity struct {
	Alias       string `json:"alias"`
	Ed25519Priv []byte `json:"ed25519_priv"` // 64 bytes
	Ed25519Pub  []byte `json:"ed25519_pub"`  // 32 bytes
	X25519Priv  []byte `json:"x25519_priv"`  // 32 bytes
	X25519Pub   []byte `json:"x25519_pub"`   // 32 bytes
}

// DefaultPath returns the identity file path.
// If HERMOND_IDENTITY_PATH is set, it is used; otherwise ~/.hermond/identity.json.
// Falls back to DFS_IDENTITY_PATH for backward compatibility.
func DefaultPath() string {
	if p := os.Getenv("HERMOND_IDENTITY_PATH"); p != "" {
		return p
	}
	if p := os.Getenv("DFS_IDENTITY_PATH"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".hermond", "identity.json")
	}
	return filepath.Join(home, ".hermond", "identity.json")
}

// Generate creates a new identity with the given alias.
func Generate(alias string) (*Identity, error) {
	if alias == "" {
		return nil, fmt.Errorf("alias must not be empty")
	}

	edPub, edPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("ed25519 keygen: %w", err)
	}

	var x25519Priv [32]byte
	if _, err := rand.Read(x25519Priv[:]); err != nil {
		return nil, fmt.Errorf("x25519 keygen: %w", err)
	}
	x25519Pub, err := curve25519.X25519(x25519Priv[:], curve25519.Basepoint)
	if err != nil {
		return nil, fmt.Errorf("x25519 scalar mult: %w", err)
	}

	return &Identity{
		Alias:       alias,
		Ed25519Priv: edPriv,
		Ed25519Pub:  edPub,
		X25519Priv:  x25519Priv[:],
		X25519Pub:   x25519Pub,
	}, nil
}

// Save writes the identity to path as JSON with 0600 permissions.
// Parent directories are created with 0700 if they don't exist.
func (id *Identity) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create identity dir: %w", err)
	}
	data, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal identity: %w", err)
	}
	return os.WriteFile(path, data, 0600)
}

// Load reads an identity from path.
func Load(path string) (*Identity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var id Identity
	if err := json.Unmarshal(data, &id); err != nil {
		return nil, fmt.Errorf("parse identity: %w", err)
	}
	return &id, nil
}

// Fingerprint returns the first 16 hex chars of SHA-256(Ed25519Pub).
// This is used as the storage namespace prefix for all files owned by this identity.
func (id *Identity) Fingerprint() string {
	h := sha256.Sum256(id.Ed25519Pub)
	return hex.EncodeToString(h[:8])
}

// GossipMetadata returns the map to inject into ClusterState.SetMetadata.
// Public keys are hex-encoded. Private keys are never included.
func (id *Identity) GossipMetadata() map[string]string {
	return map[string]string{
		"alias":       id.Alias,
		"x25519_pub":  hex.EncodeToString(id.X25519Pub),
		"ed25519_pub": hex.EncodeToString(id.Ed25519Pub),
		"fingerprint": id.Fingerprint(),
	}
}
