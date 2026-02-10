package Crypto

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
)

// ChunkSize is the size of each chunk for streaming encryption (4MB)
const ChunkSize = 4 * 1024 * 1024

type EncryptionService struct {
	FileKey []byte
}

func NewEncryptionService(Key string) *EncryptionService {
	hash := sha256.Sum256([]byte(Key))
	return &EncryptionService{
		FileKey: hash[:],
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

	cipherKey := gcm.Seal(nonce, nonce, fileKey, nil)

	return cipherKey, nil
}

func (e *EncryptionService) DecryptFile(cipherText, encryptedKey []byte) ([]byte, error) {
	decryptedKey, err := e.DecryptKey(encryptedKey)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(decryptedKey)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonceSize := gcm.NonceSize()
	if len(cipherText) < nonceSize {
		return nil, errors.New("ciphertext too short")
	}

	nonce, ciphertext := cipherText[:nonceSize], cipherText[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, err
	}

	return plaintext, nil
}

func (e *EncryptionService) DecryptKey(encryptedKey []byte) ([]byte, error) {
	block, err := aes.NewCipher(e.FileKey)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonceSize := gcm.NonceSize()
	if len(encryptedKey) < nonceSize {
		return nil, errors.New("encrypted key too short")
	}

	nonce, ciphertext := encryptedKey[:nonceSize], encryptedKey[nonceSize:]
	return gcm.Open(nil, nonce, ciphertext, nil)
}

// EncryptStream encrypts data from src and writes to dst in chunks.
// Returns the encrypted file key. Each chunk is independently encrypted
// with a unique nonce derived from the chunk index.
// Format per chunk: [4 bytes: chunk size][12 bytes: nonce][N bytes: ciphertext + auth tag]
func (e *EncryptionService) EncryptStream(src io.Reader, dst io.Writer) ([]byte, error) {
	// Generate a random file key for this file
	fileKey, err := e.fileKey()
	if err != nil {
		return nil, err
	}

	// Create AES-GCM cipher
	block, err := aes.NewCipher(fileKey)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonceSize := gcm.NonceSize()
	buffer := make([]byte, ChunkSize)
	chunkIndex := uint64(0)

	for {
		// Read a chunk
		n, readErr := io.ReadFull(src, buffer)

		if n > 0 {
			// Generate nonce from chunk index (deterministic but unique per chunk)
			nonce := make([]byte, nonceSize)
			binary.LittleEndian.PutUint64(nonce, chunkIndex)

			// Encrypt the chunk
			ciphertext := gcm.Seal(nil, nonce, buffer[:n], nil)

			// Write chunk size (4 bytes)
			chunkSize := uint32(nonceSize + len(ciphertext))
			if err := binary.Write(dst, binary.LittleEndian, chunkSize); err != nil {
				return nil, err
			}

			// Write nonce + ciphertext
			if _, err := dst.Write(nonce); err != nil {
				return nil, err
			}
			if _, err := dst.Write(ciphertext); err != nil {
				return nil, err
			}

			chunkIndex++
		}

		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}

	// Encrypt the file key with master key
	encryptedKey, err := e.EncryptKey(fileKey)
	if err != nil {
		return nil, err
	}

	return encryptedKey, nil
}

// DecryptStream decrypts data from src and writes plaintext to dst.
// The encrypted file key must be provided (from EncryptStream).
func (e *EncryptionService) DecryptStream(src io.Reader, dst io.Writer, encryptedKey []byte) error {
	// Decrypt the file key
	fileKey, err := e.DecryptKey(encryptedKey)
	if err != nil {
		return err
	}

	// Create AES-GCM cipher
	block, err := aes.NewCipher(fileKey)
	if err != nil {
		return err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}

	nonceSize := gcm.NonceSize()
	chunkIndex := uint64(0)

	for {
		// Read chunk size (4 bytes)
		var chunkSize uint32
		if err := binary.Read(src, binary.LittleEndian, &chunkSize); err != nil {
			if err == io.EOF {
				break // Normal end of file
			}
			return err
		}

		if chunkSize < uint32(nonceSize) {
			return errors.New("invalid chunk: size too small")
		}

		// Read nonce + ciphertext
		chunkData := make([]byte, chunkSize)
		if _, err := io.ReadFull(src, chunkData); err != nil {
			return err
		}

		nonce := chunkData[:nonceSize]
		ciphertext := chunkData[nonceSize:]

		// Verify nonce matches expected chunk index
		expectedNonce := make([]byte, nonceSize)
		binary.LittleEndian.PutUint64(expectedNonce, chunkIndex)
		if !bytes.Equal(nonce, expectedNonce) {
			return errors.New("invalid chunk: nonce mismatch (possible tampering or corruption)")
		}

		// Decrypt the chunk
		plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
		if err != nil {
			return err
		}

		// Write plaintext to destination
		if _, err := dst.Write(plaintext); err != nil {
			return err
		}

		chunkIndex++
	}

	return nil
}
