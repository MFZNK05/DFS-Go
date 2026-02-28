package chunker_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Faizan2005/DFS-Go/Storage/chunker"
)

// drainChunker collects all chunks from ChunkReader and returns them along
// with any error. It blocks until both channels are closed.
func drainChunker(r io.Reader, chunkSize int) ([]chunker.Chunk, error) {
	ch, errCh := chunker.ChunkReader(r, chunkSize)
	var chunks []chunker.Chunk
	for c := range ch {
		chunks = append(chunks, c)
	}
	if err := <-errCh; err != nil {
		return chunks, err
	}
	return chunks, nil
}

// ---------------------------------------------------------------------------
// ChunkReader
// ---------------------------------------------------------------------------

func TestChunkSmallFile(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 100)
	chunks, err := drainChunker(bytes.NewReader(data), chunker.DefaultChunkSize)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk for 100-byte file, got %d", len(chunks))
	}
	if chunks[0].Index != 0 {
		t.Errorf("expected index 0, got %d", chunks[0].Index)
	}
	if chunks[0].Size != 100 {
		t.Errorf("expected size 100, got %d", chunks[0].Size)
	}
}

func TestChunkExactBoundary(t *testing.T) {
	// Exactly one chunk size — should produce exactly 1 chunk with no empty tail.
	chunkSize := 64
	data := bytes.Repeat([]byte("a"), chunkSize)
	chunks, err := drainChunker(bytes.NewReader(data), chunkSize)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	if chunks[0].Size != int64(chunkSize) {
		t.Errorf("expected size %d, got %d", chunkSize, chunks[0].Size)
	}
}

func TestChunkMultiple(t *testing.T) {
	chunkSize := 4
	// 10 bytes → 3 chunks: 4+4+2
	data := []byte("0123456789")
	chunks, err := drainChunker(bytes.NewReader(data), chunkSize)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(chunks))
	}
	if chunks[0].Size != 4 {
		t.Errorf("chunk 0: expected size 4, got %d", chunks[0].Size)
	}
	if chunks[1].Size != 4 {
		t.Errorf("chunk 1: expected size 4, got %d", chunks[1].Size)
	}
	if chunks[2].Size != 2 {
		t.Errorf("chunk 2: expected size 2, got %d", chunks[2].Size)
	}
	// Indexes must be 0,1,2
	for i, c := range chunks {
		if c.Index != i {
			t.Errorf("chunk %d: wrong index %d", i, c.Index)
		}
	}
}

func TestChunkHashCorrect(t *testing.T) {
	data := []byte("hello world")
	chunks, err := drainChunker(bytes.NewReader(data), chunker.DefaultChunkSize)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := sha256.Sum256(data)
	if chunks[0].Hash != expected {
		t.Errorf("hash mismatch: want %x got %x", expected, chunks[0].Hash)
	}
}

func TestChunkEmptyReader(t *testing.T) {
	chunks, err := drainChunker(bytes.NewReader(nil), chunker.DefaultChunkSize)
	if err != nil {
		t.Fatalf("unexpected error on empty reader: %v", err)
	}
	if len(chunks) != 0 {
		t.Errorf("expected 0 chunks for empty reader, got %d", len(chunks))
	}
}

// ---------------------------------------------------------------------------
// Reassemble
// ---------------------------------------------------------------------------

func TestReassembleInOrder(t *testing.T) {
	data := []byte("abcdefghij")
	chunkSize := 4
	chunks, err := drainChunker(bytes.NewReader(data), chunkSize)
	if err != nil {
		t.Fatalf("chunk error: %v", err)
	}

	var buf bytes.Buffer
	if err := chunker.Reassemble(chunks, &buf); err != nil {
		t.Fatalf("reassemble error: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), data) {
		t.Errorf("reassembled %q, want %q", buf.Bytes(), data)
	}
}

func TestReassembleOutOfOrder(t *testing.T) {
	data := []byte("0123456789ab")
	chunkSize := 4
	chunks, err := drainChunker(bytes.NewReader(data), chunkSize)
	if err != nil {
		t.Fatalf("chunk error: %v", err)
	}

	// Reverse the order.
	for i, j := 0, len(chunks)-1; i < j; i, j = i+1, j-1 {
		chunks[i], chunks[j] = chunks[j], chunks[i]
	}

	var buf bytes.Buffer
	if err := chunker.Reassemble(chunks, &buf); err != nil {
		t.Fatalf("reassemble error: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), data) {
		t.Errorf("reassembled %q, want %q", buf.Bytes(), data)
	}
}

func TestReassembleEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := chunker.Reassemble(nil, &buf); err != nil {
		t.Errorf("expected nil error for empty chunks, got %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected empty output, got %d bytes", buf.Len())
	}
}

func TestReassembleMissingChunk(t *testing.T) {
	chunks := []chunker.Chunk{
		{Index: 0, Data: []byte("aaaa")},
		{Index: 2, Data: []byte("cccc")}, // index 1 is missing
	}
	var buf bytes.Buffer
	err := chunker.Reassemble(chunks, &buf)
	if err == nil {
		t.Error("expected error for missing chunk index, got nil")
	}
}

// ---------------------------------------------------------------------------
// VerifyChunk
// ---------------------------------------------------------------------------

func TestVerifyChunkOk(t *testing.T) {
	data := []byte("test data")
	c := chunker.Chunk{
		Index: 0,
		Hash:  sha256.Sum256(data),
		Size:  int64(len(data)),
		Data:  data,
	}
	if !chunker.VerifyChunk(c) {
		t.Error("expected VerifyChunk to return true for valid chunk")
	}
}

func TestVerifyChunkCorrupt(t *testing.T) {
	data := []byte("test data")
	c := chunker.Chunk{
		Index: 0,
		Hash:  sha256.Sum256(data),
		Size:  int64(len(data)),
		Data:  append([]byte{}, data...),
	}
	// Flip one bit.
	c.Data[0] ^= 0xFF
	if chunker.VerifyChunk(c) {
		t.Error("expected VerifyChunk to return false for corrupted chunk")
	}
}

// ---------------------------------------------------------------------------
// ChunkStorageKey / ManifestStorageKey
// ---------------------------------------------------------------------------

func TestChunkStorageKeyPrefix(t *testing.T) {
	key := chunker.ChunkStorageKey("abc123")
	if !strings.HasPrefix(key, "chunk:") {
		t.Errorf("expected 'chunk:' prefix, got %q", key)
	}
}

func TestManifestStorageKeyPrefix(t *testing.T) {
	key := chunker.ManifestStorageKey("myfile.pdf")
	if !strings.HasPrefix(key, "manifest:") {
		t.Errorf("expected 'manifest:' prefix, got %q", key)
	}
}

func TestStorageKeyNoCollision(t *testing.T) {
	hash := "deadbeef"
	chunkKey := chunker.ChunkStorageKey(hash)
	manifestKey := chunker.ManifestStorageKey(hash)
	if chunkKey == manifestKey {
		t.Errorf("chunk and manifest keys must not collide: both = %q", chunkKey)
	}
}

// ---------------------------------------------------------------------------
// BuildManifest / MerkleRoot
// ---------------------------------------------------------------------------

func TestBuildManifestDeterministic(t *testing.T) {
	ha := sha256.Sum256([]byte("a"))
	hb := sha256.Sum256([]byte("b"))
	chunks := []chunker.ChunkInfo{
		{Index: 0, Hash: hex.EncodeToString(ha[:]), Size: 1, EncHash: "e0"},
		{Index: 1, Hash: hex.EncodeToString(hb[:]), Size: 1, EncHash: "e1"},
	}
	m1 := chunker.BuildManifest("key1", chunks, time.Now().UnixNano())
	m2 := chunker.BuildManifest("key1", chunks, time.Now().UnixNano())
	if m1.MerkleRoot != m2.MerkleRoot {
		t.Errorf("root not deterministic: %q vs %q", m1.MerkleRoot, m2.MerkleRoot)
	}
}

func TestBuildManifestTotalSize(t *testing.T) {
	chunks := []chunker.ChunkInfo{
		{Index: 0, Size: 100},
		{Index: 1, Size: 200},
		{Index: 2, Size: 50},
	}
	m := chunker.BuildManifest("key", chunks, 0)
	if m.TotalSize != 350 {
		t.Errorf("expected TotalSize 350, got %d", m.TotalSize)
	}
}

func TestBuildManifestEmptyChunks(t *testing.T) {
	m := chunker.BuildManifest("empty", nil, 0)
	if m == nil {
		t.Fatal("expected non-nil manifest for empty chunks")
	}
	if m.TotalSize != 0 {
		t.Errorf("expected TotalSize 0, got %d", m.TotalSize)
	}
	if m.MerkleRoot == "" {
		t.Error("MerkleRoot should not be empty even for empty chunk list")
	}
}

// ---------------------------------------------------------------------------
// Benchmark
// ---------------------------------------------------------------------------

func BenchmarkChunk10MB(b *testing.B) {
	data := bytes.Repeat([]byte("x"), 10*1024*1024)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		chunks, err := drainChunker(bytes.NewReader(data), chunker.DefaultChunkSize)
		if err != nil {
			b.Fatal(err)
		}
		_ = chunks
	}
}
