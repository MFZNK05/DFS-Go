package Crypto

import (
	"bytes"
	"crypto/rand"
	"sync"
	"testing"
)

// randomDEK returns a 32-byte random key for testing.
func randomDEK(t *testing.T) []byte {
	t.Helper()
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		t.Fatalf("randomDEK: %v", err)
	}
	return dek
}

func TestEncryptDecryptWithDEK_SmallData(t *testing.T) {
	original := []byte("This is a small test file for streaming encryption.")
	dek := randomDEK(t)

	var encBuf bytes.Buffer
	if err := EncryptStreamWithDEK(bytes.NewReader(original), &encBuf, dek, 0); err != nil {
		t.Fatalf("EncryptStreamWithDEK: %v", err)
	}

	var decBuf bytes.Buffer
	if err := DecryptStreamWithDEK(&encBuf, &decBuf, dek, 0); err != nil {
		t.Fatalf("DecryptStreamWithDEK: %v", err)
	}

	if !bytes.Equal(original, decBuf.Bytes()) {
		t.Errorf("roundtrip mismatch: got %d bytes, want %d", decBuf.Len(), len(original))
	}
}

func TestEncryptDecryptWithDEK_LargeData(t *testing.T) {
	// Two full chunks + partial third.
	dataSize := ChunkSize*2 + 1024*100
	original := make([]byte, dataSize)
	for i := range original {
		original[i] = byte(i % 256)
	}
	dek := randomDEK(t)

	var encBuf bytes.Buffer
	if err := EncryptStreamWithDEK(bytes.NewReader(original), &encBuf, dek, 0); err != nil {
		t.Fatalf("EncryptStreamWithDEK: %v", err)
	}

	var decBuf bytes.Buffer
	if err := DecryptStreamWithDEK(&encBuf, &decBuf, dek, 0); err != nil {
		t.Fatalf("DecryptStreamWithDEK: %v", err)
	}

	if !bytes.Equal(original, decBuf.Bytes()) {
		t.Errorf("large roundtrip mismatch: got %d bytes, want %d", decBuf.Len(), len(original))
	}
}

func TestEncryptDecryptWithDEK_ExactChunkSize(t *testing.T) {
	original := make([]byte, ChunkSize)
	for i := range original {
		original[i] = byte(i % 256)
	}
	dek := randomDEK(t)

	var encBuf bytes.Buffer
	if err := EncryptStreamWithDEK(bytes.NewReader(original), &encBuf, dek, 0); err != nil {
		t.Fatalf("EncryptStreamWithDEK: %v", err)
	}

	var decBuf bytes.Buffer
	if err := DecryptStreamWithDEK(&encBuf, &decBuf, dek, 0); err != nil {
		t.Fatalf("DecryptStreamWithDEK: %v", err)
	}

	if !bytes.Equal(original, decBuf.Bytes()) {
		t.Errorf("exact chunk roundtrip mismatch: got %d bytes, want %d", decBuf.Len(), len(original))
	}
}

func TestEncryptDecryptWithDEK_EmptyData(t *testing.T) {
	original := []byte{}
	dek := randomDEK(t)

	var encBuf bytes.Buffer
	if err := EncryptStreamWithDEK(bytes.NewReader(original), &encBuf, dek, 0); err != nil {
		t.Fatalf("EncryptStreamWithDEK: %v", err)
	}

	var decBuf bytes.Buffer
	if err := DecryptStreamWithDEK(&encBuf, &decBuf, dek, 0); err != nil {
		t.Fatalf("DecryptStreamWithDEK: %v", err)
	}

	if !bytes.Equal(original, decBuf.Bytes()) {
		t.Errorf("empty roundtrip: expected empty, got %d bytes", decBuf.Len())
	}
}

func TestEncryptDecryptWithDEK_WrongKey(t *testing.T) {
	original := []byte("secret data that should not decrypt with wrong key")
	dek := randomDEK(t)

	var encBuf bytes.Buffer
	if err := EncryptStreamWithDEK(bytes.NewReader(original), &encBuf, dek, 0); err != nil {
		t.Fatalf("EncryptStreamWithDEK: %v", err)
	}

	wrongDEK := randomDEK(t)
	var decBuf bytes.Buffer
	err := DecryptStreamWithDEK(&encBuf, &decBuf, wrongDEK, 0)
	if err == nil {
		t.Error("expected error when decrypting with wrong key")
	}
}

// ---------------------------------------------------------------------------
// EncryptStreamWithDEKPool
// ---------------------------------------------------------------------------

func TestEncryptStreamWithDEKPool(t *testing.T) {
	pool := &sync.Pool{
		New: func() any { b := make([]byte, 0, ChunkSize+64); return &b },
	}
	original := make([]byte, ChunkSize/2)
	for i := range original {
		original[i] = byte(i % 251)
	}
	dek := randomDEK(t)

	var encBuf bytes.Buffer
	if err := EncryptStreamWithDEKPool(bytes.NewReader(original), &encBuf, dek, pool, 0); err != nil {
		t.Fatalf("EncryptStreamWithDEKPool: %v", err)
	}

	var decBuf bytes.Buffer
	if err := DecryptStreamWithDEK(&encBuf, &decBuf, dek, 0); err != nil {
		t.Fatalf("DecryptStreamWithDEK: %v", err)
	}
	if !bytes.Equal(decBuf.Bytes(), original) {
		t.Errorf("pool roundtrip mismatch: got %d bytes, want %d", decBuf.Len(), len(original))
	}
}

func TestEncryptStreamWithDEKPool_LargeMultiChunk(t *testing.T) {
	pool := &sync.Pool{
		New: func() any { b := make([]byte, 0, ChunkSize+64); return &b },
	}
	original := make([]byte, ChunkSize*2+1024*100)
	for i := range original {
		original[i] = byte(i % 199)
	}
	dek := randomDEK(t)

	var encBuf bytes.Buffer
	if err := EncryptStreamWithDEKPool(bytes.NewReader(original), &encBuf, dek, pool, 0); err != nil {
		t.Fatalf("EncryptStreamWithDEKPool: %v", err)
	}

	var decBuf bytes.Buffer
	if err := DecryptStreamWithDEK(&encBuf, &decBuf, dek, 0); err != nil {
		t.Fatalf("DecryptStreamWithDEK: %v", err)
	}
	if !bytes.Equal(decBuf.Bytes(), original) {
		t.Errorf("pool large roundtrip mismatch: got %d bytes, want %d", decBuf.Len(), len(original))
	}
}

func TestEncryptStreamWithDEKPool_NilPool(t *testing.T) {
	original := []byte("nil pool fallback test data")
	dek := randomDEK(t)

	var encBuf bytes.Buffer
	if err := EncryptStreamWithDEKPool(bytes.NewReader(original), &encBuf, dek, nil, 0); err != nil {
		t.Fatalf("EncryptStreamWithDEKPool(nil): %v", err)
	}

	var decBuf bytes.Buffer
	if err := DecryptStreamWithDEK(&encBuf, &decBuf, dek, 0); err != nil {
		t.Fatalf("DecryptStreamWithDEK: %v", err)
	}
	if !bytes.Equal(decBuf.Bytes(), original) {
		t.Error("nil pool roundtrip mismatch")
	}
}

// ---------------------------------------------------------------------------
// NonceOffset — verifies the two-time pad fix
// ---------------------------------------------------------------------------

func TestNonceOffset_RoundTrip(t *testing.T) {
	original := []byte("chunk data at offset 42")
	dek := randomDEK(t)

	var encBuf bytes.Buffer
	if err := EncryptStreamWithDEK(bytes.NewReader(original), &encBuf, dek, 42); err != nil {
		t.Fatalf("encrypt with offset 42: %v", err)
	}

	var decBuf bytes.Buffer
	if err := DecryptStreamWithDEK(&encBuf, &decBuf, dek, 42); err != nil {
		t.Fatalf("decrypt with offset 42: %v", err)
	}

	if !bytes.Equal(original, decBuf.Bytes()) {
		t.Error("roundtrip with nonceOffset=42 failed")
	}
}

func TestNonceOffset_DifferentCiphertext(t *testing.T) {
	// Same plaintext + same DEK but different offsets must produce different ciphertext.
	data := []byte("identical data for both chunks")
	dek := randomDEK(t)

	var enc0, enc1 bytes.Buffer
	if err := EncryptStreamWithDEK(bytes.NewReader(data), &enc0, dek, 0); err != nil {
		t.Fatal(err)
	}
	if err := EncryptStreamWithDEK(bytes.NewReader(data), &enc1, dek, 1); err != nil {
		t.Fatal(err)
	}

	if bytes.Equal(enc0.Bytes(), enc1.Bytes()) {
		t.Fatal("same DEK + same plaintext with different nonceOffset produced identical ciphertext — two-time pad NOT fixed")
	}
}

func TestNonceOffset_WrongOffsetFails(t *testing.T) {
	original := []byte("encrypted at offset 5")
	dek := randomDEK(t)

	var encBuf bytes.Buffer
	if err := EncryptStreamWithDEK(bytes.NewReader(original), &encBuf, dek, 5); err != nil {
		t.Fatal(err)
	}

	// Decrypt with wrong offset — nonce mismatch should error.
	var decBuf bytes.Buffer
	err := DecryptStreamWithDEK(&encBuf, &decBuf, dek, 0)
	if err == nil {
		t.Error("expected error when decrypting with wrong nonceOffset")
	}
}

func TestNonceOffset_PoolVariant(t *testing.T) {
	pool := &sync.Pool{
		New: func() any { b := make([]byte, 0, ChunkSize+64); return &b },
	}
	original := make([]byte, ChunkSize/2)
	for i := range original {
		original[i] = byte(i % 131)
	}
	dek := randomDEK(t)

	var encBuf bytes.Buffer
	if err := EncryptStreamWithDEKPool(bytes.NewReader(original), &encBuf, dek, pool, 7); err != nil {
		t.Fatal(err)
	}

	var decBuf bytes.Buffer
	if err := DecryptStreamWithDEK(&encBuf, &decBuf, dek, 7); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, decBuf.Bytes()) {
		t.Error("pool roundtrip with nonceOffset=7 failed")
	}
}
