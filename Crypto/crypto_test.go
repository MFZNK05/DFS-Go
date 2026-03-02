package Crypto

import (
	"bytes"
	"sync"
	"testing"
)

func TestEncryptDecryptFile(t *testing.T) {
	originalData := []byte("This is some data to encrypt and decrypt")
	src := bytes.NewReader(originalData)
	key := "this is a secret key"

	es := NewEncryptionService(key)
	cipherText, encryptedKey, err := es.EncryptFile(src)
	if err != nil {
		t.Fatalf("Encryption failed: %v", err)
	}

	// Decrypt
	plainText, err := es.DecryptFile(cipherText, encryptedKey)
	if err != nil {
		t.Fatalf("Decryption failed: %v", err)
	}

	if !bytes.Equal(originalData, plainText) {
		t.Errorf("Decrypted data does not match original.\nExpected: %s\nGot: %s", originalData, plainText)
	}
}

func TestEncryptDecryptKey(t *testing.T) {
	key := "this is a secret key"
	es := NewEncryptionService(key)

	fileKey := make([]byte, 32)
	for i := range fileKey {
		fileKey[i] = byte(i)
	}

	encKey, err := es.EncryptKey(fileKey)
	if err != nil {
		t.Fatalf("EncryptKey failed: %v", err)
	}

	decKey, err := es.DecryptKey(encKey)
	if err != nil {
		t.Fatalf("DecryptKey failed: %v", err)
	}

	if !bytes.Equal(fileKey, decKey) {
		t.Errorf("Decrypted key does not match original.\nExpected: %x\nGot: %x", fileKey, decKey)
	}
}

func TestEncryptDecryptStream_SmallData(t *testing.T) {
	originalData := []byte("This is a small test file for streaming encryption.")
	key := "streaming-test-key"

	es := NewEncryptionService(key)

	// Encrypt
	var encryptedBuf bytes.Buffer
	encryptedKey, err := es.EncryptStream(bytes.NewReader(originalData), &encryptedBuf)
	if err != nil {
		t.Fatalf("EncryptStream failed: %v", err)
	}

	// Decrypt
	var decryptedBuf bytes.Buffer
	err = es.DecryptStream(&encryptedBuf, &decryptedBuf, encryptedKey)
	if err != nil {
		t.Fatalf("DecryptStream failed: %v", err)
	}

	if !bytes.Equal(originalData, decryptedBuf.Bytes()) {
		t.Errorf("Decrypted data does not match original.\nExpected: %s\nGot: %s", originalData, decryptedBuf.Bytes())
	}
}

func TestEncryptDecryptStream_LargeData(t *testing.T) {
	// Create data larger than one chunk (4MB + some)
	dataSize := ChunkSize + 1024*100 // 4MB + 100KB
	originalData := make([]byte, dataSize)
	for i := range originalData {
		originalData[i] = byte(i % 256)
	}
	key := "large-file-test-key"

	es := NewEncryptionService(key)

	// Encrypt
	var encryptedBuf bytes.Buffer
	encryptedKey, err := es.EncryptStream(bytes.NewReader(originalData), &encryptedBuf)
	if err != nil {
		t.Fatalf("EncryptStream failed: %v", err)
	}

	// Decrypt
	var decryptedBuf bytes.Buffer
	err = es.DecryptStream(&encryptedBuf, &decryptedBuf, encryptedKey)
	if err != nil {
		t.Fatalf("DecryptStream failed: %v", err)
	}

	if !bytes.Equal(originalData, decryptedBuf.Bytes()) {
		t.Errorf("Decrypted data does not match original. Sizes: expected %d, got %d", len(originalData), len(decryptedBuf.Bytes()))
	}
}

func TestEncryptDecryptStream_ExactChunkSize(t *testing.T) {
	// Test with data exactly equal to chunk size
	originalData := make([]byte, ChunkSize)
	for i := range originalData {
		originalData[i] = byte(i % 256)
	}
	key := "exact-chunk-test-key"

	es := NewEncryptionService(key)

	// Encrypt
	var encryptedBuf bytes.Buffer
	encryptedKey, err := es.EncryptStream(bytes.NewReader(originalData), &encryptedBuf)
	if err != nil {
		t.Fatalf("EncryptStream failed: %v", err)
	}

	// Decrypt
	var decryptedBuf bytes.Buffer
	err = es.DecryptStream(&encryptedBuf, &decryptedBuf, encryptedKey)
	if err != nil {
		t.Fatalf("DecryptStream failed: %v", err)
	}

	if !bytes.Equal(originalData, decryptedBuf.Bytes()) {
		t.Errorf("Decrypted data does not match original. Sizes: expected %d, got %d", len(originalData), len(decryptedBuf.Bytes()))
	}
}

func TestEncryptDecryptStream_EmptyData(t *testing.T) {
	originalData := []byte{}
	key := "empty-test-key"

	es := NewEncryptionService(key)

	// Encrypt empty data
	var encryptedBuf bytes.Buffer
	encryptedKey, err := es.EncryptStream(bytes.NewReader(originalData), &encryptedBuf)
	if err != nil {
		t.Fatalf("EncryptStream failed: %v", err)
	}

	// Decrypt
	var decryptedBuf bytes.Buffer
	err = es.DecryptStream(&encryptedBuf, &decryptedBuf, encryptedKey)
	if err != nil {
		t.Fatalf("DecryptStream failed: %v", err)
	}

	if !bytes.Equal(originalData, decryptedBuf.Bytes()) {
		t.Errorf("Decrypted data does not match original. Expected empty, got %d bytes", len(decryptedBuf.Bytes()))
	}
}

// ---------------------------------------------------------------------------
// EncryptStreamWithPool — decrypted output must match EncryptStream exactly
// ---------------------------------------------------------------------------

func TestEncryptStreamWithPool(t *testing.T) {
	pool := &sync.Pool{
		New: func() any { b := make([]byte, 0, ChunkSize+64); return &b },
	}
	original := make([]byte, ChunkSize/2) // half a chunk, single iteration
	for i := range original {
		original[i] = byte(i % 251)
	}
	es := NewEncryptionService("pool-test-key")

	var encBuf bytes.Buffer
	encKey, err := es.EncryptStreamWithPool(bytes.NewReader(original), &encBuf, pool)
	if err != nil {
		t.Fatalf("EncryptStreamWithPool: %v", err)
	}

	var decBuf bytes.Buffer
	if err := es.DecryptStream(&encBuf, &decBuf, encKey); err != nil {
		t.Fatalf("DecryptStream: %v", err)
	}
	if !bytes.Equal(decBuf.Bytes(), original) {
		t.Errorf("pool roundtrip mismatch: got %d bytes, want %d", decBuf.Len(), len(original))
	}
}

func TestEncryptStreamWithPool_LargeMultiChunk(t *testing.T) {
	pool := &sync.Pool{
		New: func() any { b := make([]byte, 0, ChunkSize+64); return &b },
	}
	// Two full chunks + a partial third to exercise the inner loop.
	original := make([]byte, ChunkSize*2+1024*100)
	for i := range original {
		original[i] = byte(i % 199)
	}
	es := NewEncryptionService("pool-large-key")

	var encBuf bytes.Buffer
	encKey, err := es.EncryptStreamWithPool(bytes.NewReader(original), &encBuf, pool)
	if err != nil {
		t.Fatalf("EncryptStreamWithPool: %v", err)
	}

	var decBuf bytes.Buffer
	if err := es.DecryptStream(&encBuf, &decBuf, encKey); err != nil {
		t.Fatalf("DecryptStream: %v", err)
	}
	if !bytes.Equal(decBuf.Bytes(), original) {
		t.Errorf("pool large roundtrip mismatch: got %d bytes, want %d", decBuf.Len(), len(original))
	}
}

func TestEncryptStreamWithPool_NilPool(t *testing.T) {
	// nil pool must fall back to standard EncryptStream.
	original := []byte("nil pool fallback test data")
	es := NewEncryptionService("nil-pool-key")

	var encBuf bytes.Buffer
	encKey, err := es.EncryptStreamWithPool(bytes.NewReader(original), &encBuf, nil)
	if err != nil {
		t.Fatalf("EncryptStreamWithPool(nil): %v", err)
	}

	var decBuf bytes.Buffer
	if err := es.DecryptStream(&encBuf, &decBuf, encKey); err != nil {
		t.Fatalf("DecryptStream: %v", err)
	}
	if !bytes.Equal(decBuf.Bytes(), original) {
		t.Error("nil pool roundtrip mismatch")
	}
}
