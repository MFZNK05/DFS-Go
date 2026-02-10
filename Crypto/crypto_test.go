package Crypto

import (
	"bytes"
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
