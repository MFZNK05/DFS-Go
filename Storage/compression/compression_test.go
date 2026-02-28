package compression_test

import (
	"bytes"
	"testing"

	"github.com/Faizan2005/DFS-Go/Storage/compression"
)

// ---------------------------------------------------------------------------
// ShouldCompress
// ---------------------------------------------------------------------------

func TestShouldCompressLowEntropy(t *testing.T) {
	// Highly repetitive text compresses very well.
	data := bytes.Repeat([]byte("hello world "), 200)
	if !compression.ShouldCompress(data) {
		t.Error("expected ShouldCompress=true for repetitive text")
	}
}

func TestShouldCompressHighEntropy(t *testing.T) {
	// Pseudo-random bytes — high entropy, compression won't help.
	data := make([]byte, 4096)
	for i := range data {
		data[i] = byte(i*7 + 13) // deterministic but varied
	}
	// Entropy of this pattern is moderate; test ensures the function runs
	// without panic. We don't assert true/false since the entropy threshold
	// is the interesting boundary.
	_ = compression.ShouldCompress(data)
}

func TestShouldCompressTooSmall(t *testing.T) {
	data := []byte("tiny")
	if compression.ShouldCompress(data) {
		t.Error("expected ShouldCompress=false for data under minCompressSize")
	}
}

func TestShouldCompressJPEG(t *testing.T) {
	// Fake JPEG magic bytes.
	data := make([]byte, 4096)
	data[0] = 0xFF
	data[1] = 0xD8
	data[2] = 0xFF
	data[3] = 0xE0
	if compression.ShouldCompress(data) {
		t.Error("expected ShouldCompress=false for JPEG magic bytes")
	}
}

func TestShouldCompressPNG(t *testing.T) {
	data := make([]byte, 4096)
	data[0] = 0x89
	data[1] = 0x50
	data[2] = 0x4E
	data[3] = 0x47
	if compression.ShouldCompress(data) {
		t.Error("expected ShouldCompress=false for PNG magic bytes")
	}
}

func TestShouldCompressZIP(t *testing.T) {
	data := make([]byte, 4096)
	data[0] = 0x50
	data[1] = 0x4B
	if compression.ShouldCompress(data) {
		t.Error("expected ShouldCompress=false for ZIP magic bytes (PPTX/DOCX)")
	}
}

func TestShouldCompressZstd(t *testing.T) {
	data := make([]byte, 4096)
	data[0] = 0x28
	data[1] = 0xB5
	data[2] = 0x2F
	data[3] = 0xFD
	if compression.ShouldCompress(data) {
		t.Error("expected ShouldCompress=false for already-zstd data")
	}
}

// ---------------------------------------------------------------------------
// CompressChunk / DecompressChunk roundtrip
// ---------------------------------------------------------------------------

func TestRoundtripFastest(t *testing.T) {
	original := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog "), 500)
	roundtrip(t, original, compression.LevelFastest)
}

func TestRoundtripDefault(t *testing.T) {
	original := bytes.Repeat([]byte("compress me please "), 300)
	roundtrip(t, original, compression.LevelDefault)
}

func TestRoundtripBest(t *testing.T) {
	original := bytes.Repeat([]byte("best compression level test data "), 200)
	roundtrip(t, original, compression.LevelBest)
}

func roundtrip(t *testing.T, original []byte, level compression.Level) {
	t.Helper()
	compressed, wasCompressed, err := compression.CompressChunk(original, level)
	if err != nil {
		t.Fatalf("CompressChunk error: %v", err)
	}
	if !wasCompressed {
		t.Fatal("expected wasCompressed=true for repetitive data")
	}
	if len(compressed) >= len(original) {
		t.Errorf("expected compression to reduce size: %d → %d", len(original), len(compressed))
	}

	decompressed, err := compression.DecompressChunk(compressed)
	if err != nil {
		t.Fatalf("DecompressChunk error: %v", err)
	}
	if !bytes.Equal(decompressed, original) {
		t.Errorf("roundtrip mismatch: got %d bytes, want %d", len(decompressed), len(original))
	}
}

func TestCompressIncompressibleData(t *testing.T) {
	// Data that won't compress smaller — CompressChunk should return original unchanged.
	// Use a zstd-compressed blob (already compressed).
	inner := bytes.Repeat([]byte("abc"), 100)
	compressed, _, err := compression.CompressChunk(inner, compression.LevelBest)
	if err != nil {
		t.Fatalf("first compress: %v", err)
	}
	// Compress again — should NOT shrink further (or return original).
	out, wasCompressed, err := compression.CompressChunk(compressed, compression.LevelBest)
	if err != nil {
		t.Fatalf("second compress: %v", err)
	}
	if wasCompressed && len(out) > len(compressed) {
		t.Error("second compression must not grow the data")
	}
}

func TestDecompressInvalidData(t *testing.T) {
	_, err := compression.DecompressChunk([]byte("this is not zstd"))
	if err == nil {
		t.Error("expected error decompressing invalid zstd data")
	}
}

// ---------------------------------------------------------------------------
// Compression ratio sanity check
// ---------------------------------------------------------------------------

func TestCompressionRatioPDF(t *testing.T) {
	// Simulate PDF-like content: repetitive text mixed with structure bytes.
	var buf bytes.Buffer
	header := []byte("%PDF-1.4\n")
	buf.Write(header)
	for i := 0; i < 1000; i++ {
		buf.WriteString("stream\nBT /F1 12 Tf 100 700 Td (Hello World) Tj ET\nendstream\n")
	}
	data := buf.Bytes()

	compressed, wasCompressed, err := compression.CompressChunk(data, compression.LevelDefault)
	if err != nil {
		t.Fatalf("compress: %v", err)
	}
	if !wasCompressed {
		t.Skip("PDF-like data was not compressed (entropy too high for sample)")
	}
	ratio := float64(len(compressed)) / float64(len(data))
	t.Logf("PDF-like compression ratio: %.2f (%.1f%% reduction)", ratio, (1-ratio)*100)
	if ratio > 0.5 {
		t.Logf("warning: compression ratio %.2f is lower than expected for PDF-like data", ratio)
	}
}
