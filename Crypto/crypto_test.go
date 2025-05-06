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
