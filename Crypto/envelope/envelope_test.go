package envelope

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"testing"

	"golang.org/x/crypto/curve25519"
)

// genX25519 generates a random X25519 keypair for testing.
func genX25519(t *testing.T) (priv, pub []byte) {
	t.Helper()
	priv = make([]byte, 32)
	if _, err := rand.Read(priv); err != nil {
		t.Fatalf("rand: %v", err)
	}
	pub, err := curve25519.X25519(priv, curve25519.Basepoint)
	if err != nil {
		t.Fatalf("x25519: %v", err)
	}
	return priv, pub
}

func TestWrapUnwrapRoundtrip(t *testing.T) {
	alicePriv, alicePub := genX25519(t)
	bobPriv, bobPub := genX25519(t)

	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		t.Fatal(err)
	}

	entry, err := WrapDEKForRecipient(alicePriv, hex.EncodeToString(bobPub), dek)
	if err != nil {
		t.Fatalf("WrapDEKForRecipient: %v", err)
	}

	if entry.RecipientPubKey != hex.EncodeToString(bobPub) {
		t.Error("recipient pub mismatch")
	}

	recovered, err := UnwrapDEK(bobPriv, hex.EncodeToString(alicePub), entry.WrappedDEK)
	if err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}

	if hex.EncodeToString(recovered) != hex.EncodeToString(dek) {
		t.Error("DEK roundtrip mismatch")
	}
}

func TestUnwrapWrongKey(t *testing.T) {
	alicePriv, _ := genX25519(t)
	_, bobPub := genX25519(t)
	charliePriv, charliePub := genX25519(t)

	dek := make([]byte, 32)
	rand.Read(dek)

	entry, err := WrapDEKForRecipient(alicePriv, hex.EncodeToString(bobPub), dek)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}

	// Charlie tries to unwrap — should fail.
	_, err = UnwrapDEK(charliePriv, hex.EncodeToString(charliePub), entry.WrappedDEK)
	if err == nil {
		t.Error("expected error when unwrapping with wrong key")
	}
}

func TestWrapInvalidPubHex(t *testing.T) {
	priv := make([]byte, 32)
	rand.Read(priv)
	dek := make([]byte, 32)

	_, err := WrapDEKForRecipient(priv, "not-hex", dek)
	if err == nil {
		t.Error("expected error for invalid hex")
	}

	_, err = WrapDEKForRecipient(priv, hex.EncodeToString([]byte("short")), dek)
	if err == nil {
		t.Error("expected error for wrong pub key length")
	}
}

func TestUnwrapTooShort(t *testing.T) {
	priv := make([]byte, 32)
	rand.Read(priv)

	_, err := UnwrapDEK(priv, hex.EncodeToString(make([]byte, 32)), hex.EncodeToString([]byte("short")))
	if err == nil {
		t.Error("expected error for too-short wrapped DEK")
	}
}

func TestSignVerifyManifest(t *testing.T) {
	edPub, edPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	payload := ManifestSigningPayload{
		FileKey:     "test-file",
		OwnerPubKey: "abc123",
		Encrypted:   true,
		AccessList: []AccessEntry{
			{RecipientPubKey: "def456", WrappedDEK: "789abc"},
		},
	}

	sig, err := SignManifest(edPriv, payload)
	if err != nil {
		t.Fatalf("SignManifest: %v", err)
	}
	if sig == "" {
		t.Error("empty signature")
	}

	ok, err := VerifyManifest(hex.EncodeToString(edPub), payload, sig)
	if err != nil {
		t.Fatalf("VerifyManifest: %v", err)
	}
	if !ok {
		t.Error("valid signature rejected")
	}
}

func TestVerifyTamperedPayload(t *testing.T) {
	edPub, edPriv, _ := ed25519.GenerateKey(rand.Reader)

	payload := ManifestSigningPayload{
		FileKey:     "test-file",
		OwnerPubKey: "abc123",
		Encrypted:   true,
	}

	sig, _ := SignManifest(edPriv, payload)

	// Tamper with the payload.
	payload.FileKey = "tampered"
	ok, err := VerifyManifest(hex.EncodeToString(edPub), payload, sig)
	if err != nil {
		t.Fatalf("VerifyManifest: %v", err)
	}
	if ok {
		t.Error("tampered payload should not verify")
	}
}

func TestVerifyWrongKey(t *testing.T) {
	_, edPriv, _ := ed25519.GenerateKey(rand.Reader)
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)

	payload := ManifestSigningPayload{FileKey: "f", Encrypted: true}
	sig, _ := SignManifest(edPriv, payload)

	ok, err := VerifyManifest(hex.EncodeToString(otherPub), payload, sig)
	if err != nil {
		t.Fatalf("VerifyManifest: %v", err)
	}
	if ok {
		t.Error("wrong key should not verify")
	}
}

func TestSelfWrapUnwrap(t *testing.T) {
	// Alice wraps DEK for herself (always done in ECDH upload).
	alicePriv, alicePub := genX25519(t)

	dek := make([]byte, 32)
	rand.Read(dek)

	entry, err := WrapDEKForRecipient(alicePriv, hex.EncodeToString(alicePub), dek)
	if err != nil {
		t.Fatalf("self wrap: %v", err)
	}

	recovered, err := UnwrapDEK(alicePriv, hex.EncodeToString(alicePub), entry.WrappedDEK)
	if err != nil {
		t.Fatalf("self unwrap: %v", err)
	}
	if hex.EncodeToString(recovered) != hex.EncodeToString(dek) {
		t.Error("self wrap/unwrap mismatch")
	}
}
