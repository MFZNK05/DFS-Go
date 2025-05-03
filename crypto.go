package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"io"
)

type EncryptionService struct {
	FileKey []byte
}

func NewEncryptionService(Key string) *EncryptionService {
	return &EncryptionService{
		FileKey: []byte(Key),
	}
}

func (e *EncryptionService) fileKey() ([]byte, error) {
	key := make([]byte, 32)
	_, err := rand.Read(key)
	if err != nil {
		return nil, err
	}

	return key, nil
}

func (e *EncryptionService) EncryptFile(src io.Reader) ([]byte, []byte, error) {
	key, err := e.fileKey()
	if err != nil {
		return nil, nil, err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, err
	}

	buff := new(bytes.Buffer)
	if _, err = io.Copy(buff, src); err != nil {
		return nil, nil, err
	}

	cipherText := gcm.Seal(nonce, nonce, buff.Bytes(), nil)

	encryptedKey, err := e.EncryptKey(key)
	if err != nil {
		return nil, nil, err
	}

	return cipherText, encryptedKey, nil
}

func (e *EncryptionService) EncryptKey(fileKey []byte) ([]byte, error) {
	block, err := aes.NewCipher(e.FileKey)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}

	cypherKey := gcm.Seal(nonce, nonce, fileKey, nil)

	return cypherKey, nil
}
