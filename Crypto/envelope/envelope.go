// Package envelope provides ECDH-based DEK wrapping and Ed25519 manifest signing.
//
// WrapDEKForRecipient performs X25519 ECDH key agreement, derives a wrapping key
// via HKDF-SHA256, and AES-GCM wraps the DEK for a specific recipient.
// UnwrapDEK reverses the process on the recipient side.
//
// SignManifest/VerifyManifest provide Ed25519 signatures over a canonical
// manifest payload to prove who authorized access.
package envelope

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"

	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
)

// AccessEntry represents a DEK wrapped for a specific recipient.
type AccessEntry struct {
	RecipientPubKey string `json:"recipient_pub_key"` // hex X25519 pub
	WrappedDEK      string `json:"wrapped_dek"`       // hex [12B nonce][ciphertext+tag]
}

// WrapDEKForRecipient performs X25519 ECDH with recipientPub, derives a
// wrapping key via HKDF-SHA256, and AES-GCM wraps the DEK.
func WrapDEKForRecipient(senderX25519Priv []byte, recipientX25519PubHex string, dek []byte) (*AccessEntry, error) {
	recipientPub, err := hex.DecodeString(recipientX25519PubHex)
	if err != nil {
		return nil, errors.New("invalid recipient pub hex")
	}
	if len(recipientPub) != 32 {
		return nil, errors.New("recipient pub key must be 32 bytes")
	}

	shared, err := curve25519.X25519(senderX25519Priv, recipientPub)
	if err != nil {
		return nil, err
	}

	wrapKey, err := deriveWrapKey(shared)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(wrapKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	wrapped := gcm.Seal(nonce, nonce, dek, nil) // [nonce][ciphertext+tag]

	return &AccessEntry{
		RecipientPubKey: recipientX25519PubHex,
		WrappedDEK:      hex.EncodeToString(wrapped),
	}, nil
}

// UnwrapDEK performs ECDH with senderPub and unwraps the DEK.
func UnwrapDEK(recipientX25519Priv []byte, senderX25519PubHex string, wrappedDEKHex string) ([]byte, error) {
	senderPub, err := hex.DecodeString(senderX25519PubHex)
	if err != nil {
		return nil, errors.New("invalid sender pub hex")
	}
	if len(senderPub) != 32 {
		return nil, errors.New("sender pub key must be 32 bytes")
	}
	wrappedDEK, err := hex.DecodeString(wrappedDEKHex)
	if err != nil {
		return nil, errors.New("invalid wrapped DEK hex")
	}

	shared, err := curve25519.X25519(recipientX25519Priv, senderPub)
	if err != nil {
		return nil, err
	}

	wrapKey, err := deriveWrapKey(shared)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(wrapKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonceSize := gcm.NonceSize()
	if len(wrappedDEK) < nonceSize {
		return nil, errors.New("wrapped DEK too short")
	}
	return gcm.Open(nil, wrappedDEK[:nonceSize], wrappedDEK[nonceSize:], nil)
}

// deriveWrapKey derives a 32-byte AES wrapping key from the ECDH shared secret.
func deriveWrapKey(shared []byte) ([]byte, error) {
	hkdfReader := hkdf.New(sha256.New, shared, nil, []byte("dfs-dek-wrap"))
	wrapKey := make([]byte, 32)
	if _, err := io.ReadFull(hkdfReader, wrapKey); err != nil {
		return nil, err
	}
	return wrapKey, nil
}

// ManifestSigningPayload is the canonical struct signed by Ed25519.
// Fields are alphabetically ordered for deterministic JSON serialization.
// Only access-control fields are signed — MerkleRoot provides separate integrity.
type ManifestSigningPayload struct {
	AccessList  []AccessEntry `json:"access_list"`
	Encrypted   bool          `json:"encrypted"`
	FileKey     string        `json:"file_key"`
	OwnerPubKey string        `json:"owner_pub_key"`
}

// SignManifest signs the canonical payload with the Ed25519 private key.
// Returns the hex-encoded signature.
func SignManifest(edPriv ed25519.PrivateKey, payload ManifestSigningPayload) (string, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(data)
	sig := ed25519.Sign(edPriv, hash[:])
	return hex.EncodeToString(sig), nil
}

// VerifyManifest verifies the Ed25519 signature over the canonical payload.
func VerifyManifest(edPubHex string, payload ManifestSigningPayload, sigHex string) (bool, error) {
	edPub, err := hex.DecodeString(edPubHex)
	if err != nil {
		return false, err
	}
	if len(edPub) != ed25519.PublicKeySize {
		return false, errors.New("invalid ed25519 pub key length")
	}
	sig, err := hex.DecodeString(sigHex)
	if err != nil {
		return false, err
	}
	if len(sig) != ed25519.SignatureSize {
		return false, errors.New("invalid signature length")
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return false, err
	}
	hash := sha256.Sum256(data)
	return ed25519.Verify(edPub, hash[:], sig), nil
}
