// Package Crypto provides AES-256-GCM streaming encryption using externally
// provided DEKs. Key derivation and wrapping live in the cse sub-package.
//
// Streaming format per chunk: [4B chunk size][12B nonce][ciphertext + auth tag]
// Nonces are deterministic from the chunk index (safe because each DEK is unique).
package Crypto

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"io"
	"sync"
)

// ChunkSize is the size of each chunk for streaming encryption (4MB).
const ChunkSize = 4 * 1024 * 1024

// EncryptStreamWithDEK encrypts data from src to dst using the provided DEK directly.
// Same streaming format: [4B chunk size][12B nonce][ciphertext+tag].
// Nonces are deterministic from chunk index. Does NOT wrap the key.
func EncryptStreamWithDEK(src io.Reader, dst io.Writer, dek []byte) error {
	block, err := aes.NewCipher(dek)
	if err != nil {
		return err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}

	nonceSize := gcm.NonceSize()
	buffer := make([]byte, ChunkSize)
	chunkIndex := uint64(0)

	for {
		n, readErr := io.ReadFull(src, buffer)
		if n > 0 {
			nonce := make([]byte, nonceSize)
			binary.LittleEndian.PutUint64(nonce, chunkIndex)

			ciphertext := gcm.Seal(nil, nonce, buffer[:n], nil)

			chunkSize := uint32(nonceSize + len(ciphertext))
			if err := binary.Write(dst, binary.LittleEndian, chunkSize); err != nil {
				return err
			}
			if _, err := dst.Write(nonce); err != nil {
				return err
			}
			if _, err := dst.Write(ciphertext); err != nil {
				return err
			}
			chunkIndex++
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	return nil
}

// EncryptStreamWithDEKPool is like EncryptStreamWithDEK but borrows a slab
// from pool for gcm.Seal output, eliminating per-chunk heap allocations.
func EncryptStreamWithDEKPool(src io.Reader, dst io.Writer, dek []byte, pool *sync.Pool) error {
	if pool == nil {
		return EncryptStreamWithDEK(src, dst, dek)
	}

	block, err := aes.NewCipher(dek)
	if err != nil {
		return err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}

	nonceSize := gcm.NonceSize()
	readBuf := make([]byte, ChunkSize)

	pb := pool.Get().(*[]byte)
	defer pool.Put(pb)

	chunkIndex := uint64(0)
	for {
		n, readErr := io.ReadFull(src, readBuf)
		if n > 0 {
			nonce := make([]byte, nonceSize)
			binary.LittleEndian.PutUint64(nonce, chunkIndex)

			*pb = (*pb)[:0]
			ciphertext := gcm.Seal(*pb, nonce, readBuf[:n], nil)

			chunkSize := uint32(nonceSize + len(ciphertext))
			if err := binary.Write(dst, binary.LittleEndian, chunkSize); err != nil {
				return err
			}
			if _, err := dst.Write(nonce); err != nil {
				return err
			}
			if _, err := dst.Write(ciphertext); err != nil {
				return err
			}
			chunkIndex++
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	return nil
}

// DecryptStreamWithDEK decrypts data from src to dst using the raw DEK directly.
func DecryptStreamWithDEK(src io.Reader, dst io.Writer, dek []byte) error {
	block, err := aes.NewCipher(dek)
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
		var chunkSize uint32
		if err := binary.Read(src, binary.LittleEndian, &chunkSize); err != nil {
			if err == io.EOF {
				break
			}
			return err
		}

		if chunkSize < uint32(nonceSize) {
			return errors.New("invalid chunk: size too small")
		}

		chunkData := make([]byte, chunkSize)
		if _, err := io.ReadFull(src, chunkData); err != nil {
			return err
		}

		nonce := chunkData[:nonceSize]
		ciphertext := chunkData[nonceSize:]

		expectedNonce := make([]byte, nonceSize)
		binary.LittleEndian.PutUint64(expectedNonce, chunkIndex)
		if !bytes.Equal(nonce, expectedNonce) {
			return errors.New("invalid chunk: nonce mismatch (possible tampering or corruption)")
		}

		plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
		if err != nil {
			return err
		}

		if _, err := dst.Write(plaintext); err != nil {
			return err
		}

		chunkIndex++
	}

	return nil
}
