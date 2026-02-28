// Package compression provides zstd compress/decompress helpers for chunk data.
//
// Upload path:  plaintext chunk → ShouldCompress? → CompressChunk → encrypt
// Download path: decrypt → DecompressChunk (if Compressed==true) → verify sha256
package compression

import (
	"bytes"
	"math"

	"github.com/klauspost/compress/zstd"
)

// Level maps to zstd encoder speed presets.
type Level int

const (
	LevelFastest Level = iota // zstd SpeedFastest — best for real-time uploads
	LevelDefault              // zstd SpeedDefault — good balance
	LevelBest                 // zstd SpeedBestCompression — for batch/archive
)

// minCompressSize is the smallest chunk worth attempting compression on.
// Compressing tiny chunks adds framing overhead for no benefit.
const minCompressSize = 1024 // 1 KiB

// ShouldCompress returns false for data that is already compressed or has high
// entropy (random-looking bytes won't compress). It checks magic-byte prefixes
// for common formats first, then falls back to a Shannon entropy estimate on
// the first 4 KiB.
func ShouldCompress(data []byte) bool {
	if len(data) < minCompressSize {
		return false
	}

	// Magic-byte check for known compressed/opaque formats.
	if isAlreadyCompressed(data) {
		return false
	}

	// Shannon entropy check on first 4 KiB sample.
	sample := data
	if len(sample) > 4096 {
		sample = sample[:4096]
	}
	return shannonEntropy(sample) < 7.0 // bits per byte; truly random ≈ 8.0
}

// CompressChunk compresses data using zstd at the given level.
// If the compressed output is larger than the input, the original is returned
// unchanged with wasCompressed=false (never grow the data).
func CompressChunk(data []byte, level Level) (out []byte, wasCompressed bool, err error) {
	encLevel := zstd.SpeedFastest
	switch level {
	case LevelDefault:
		encLevel = zstd.SpeedDefault
	case LevelBest:
		encLevel = zstd.SpeedBestCompression
	}

	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(encLevel))
	if err != nil {
		return data, false, err
	}
	defer enc.Close()

	compressed := enc.EncodeAll(data, make([]byte, 0, len(data)/2))
	if len(compressed) >= len(data) {
		// Compression made it bigger — not worth it.
		return data, false, nil
	}
	return compressed, true, nil
}

// DecompressChunk decompresses a zstd-compressed chunk.
func DecompressChunk(data []byte) ([]byte, error) {
	dec, err := zstd.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer dec.Close()

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(dec); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// isAlreadyCompressed checks common magic-byte prefixes for formats that are
// already compressed and would not benefit from a second pass.
func isAlreadyCompressed(data []byte) bool {
	if len(data) < 4 {
		return false
	}
	magic := data[:4]
	switch {
	case bytes.HasPrefix(magic, []byte{0xFF, 0xD8, 0xFF}): // JPEG
		return true
	case bytes.Equal(magic, []byte{0x89, 0x50, 0x4E, 0x47}): // PNG
		return true
	case bytes.HasPrefix(magic, []byte{0x50, 0x4B}): // ZIP / DOCX / XLSX / PPTX
		return true
	case bytes.HasPrefix(magic, []byte{0x1F, 0x8B}): // gzip
		return true
	case bytes.Equal(magic, []byte{0x28, 0xB5, 0x2F, 0xFD}): // zstd
		return true
	case bytes.HasPrefix(magic, []byte{0x00, 0x00, 0x00}) && len(data) > 7 &&
		(data[4] == 0x66 || data[4] == 0x6D): // MP4 / MOV ftyp box
		return true
	case bytes.HasPrefix(magic, []byte{0x1A, 0x45, 0xDF, 0xA3}): // MKV / WebM
		return true
	}
	return false
}

// shannonEntropy computes the Shannon entropy (bits per byte) of data.
// A value >= 7.5 indicates highly random / already-compressed content.
func shannonEntropy(data []byte) float64 {
	if len(data) == 0 {
		return 0
	}
	var freq [256]int
	for _, b := range data {
		freq[b]++
	}
	n := float64(len(data))
	var h float64
	for _, f := range freq {
		if f == 0 {
			continue
		}
		p := float64(f) / n
		h -= p * math.Log2(p)
	}
	return h
}
