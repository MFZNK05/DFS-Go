package e2e_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/Faizan2005/DFS-Go/Storage/compression"
)

// compressionCase describes a file-type corpus entry for compression tests.
type compressionCase struct {
	name            string
	payloadType     string
	size            int
	expectCompress  bool // does ShouldCompress return true for this type?
}

var compressionCorpus = []compressionCase{
	// Compressible types
	{"text-2MB", "text", 2 * 1024 * 1024, true},
	{"html-3MB", "html", 3 * 1024 * 1024, true},
	{"csv-5MB", "csv", 5 * 1024 * 1024, true},
	// Incompressible types (magic bytes or high entropy)
	{"jpeg-500KB", "jpeg", 500 * 1024, false},
	{"png-1MB", "png", 1 * 1024 * 1024, false},
	{"zip-2MB", "zip", 2 * 1024 * 1024, false},
	{"gzip-1MB", "gzip", 1 * 1024 * 1024, false},
	{"binary-4MB", "binary", 4 * 1024 * 1024, false},
}

// TestShouldCompressDetection verifies that ShouldCompress returns the expected
// value for each file type based on magic bytes / entropy heuristic.
func TestShouldCompressDetection(t *testing.T) {
	for _, tc := range compressionCorpus {
		t.Run(tc.name, func(t *testing.T) {
			data := makePayload(tc.payloadType, tc.size)
			got := compression.ShouldCompress(data)
			if got != tc.expectCompress {
				t.Errorf("ShouldCompress(%s): got %v, want %v", tc.name, got, tc.expectCompress)
			}
		})
	}
}

// TestRoundtrip_Text verifies repeated ASCII text (2MB) round-trips correctly.
func TestRoundtrip_Text(t *testing.T) {
	c := newCluster(t, 3, 3)
	data := makePayload("text", 2*1024*1024)
	assertRoundtrip(t, c.nodes[0], c.nodes[0], "compress-text-key", data)
}

// TestRoundtrip_HTML verifies HTML markup (3MB) round-trips correctly.
func TestRoundtrip_HTML(t *testing.T) {
	c := newCluster(t, 3, 3)
	data := makePayload("html", 3*1024*1024)
	assertRoundtrip(t, c.nodes[0], c.nodes[0], "compress-html-key", data)
}

// TestRoundtrip_CSV verifies CSV data (5MB) round-trips correctly.
func TestRoundtrip_CSV(t *testing.T) {
	c := newCluster(t, 3, 3)
	data := makePayload("csv", 5*1024*1024)
	assertRoundtrip(t, c.nodes[0], c.nodes[0], "compress-csv-key", data)
}

// TestRoundtrip_JPEG verifies JPEG-headered data (500KB) is stored and
// retrieved without data corruption (compression should be skipped).
func TestRoundtrip_JPEG(t *testing.T) {
	c := newCluster(t, 3, 3)
	data := makePayload("jpeg", 500*1024)
	assertRoundtrip(t, c.nodes[0], c.nodes[0], "compress-jpeg-key", data)
}

// TestRoundtrip_PNG verifies PNG-headered data (1MB) round-trips correctly.
func TestRoundtrip_PNG(t *testing.T) {
	c := newCluster(t, 3, 3)
	data := makePayload("png", 1*1024*1024)
	assertRoundtrip(t, c.nodes[0], c.nodes[0], "compress-png-key", data)
}

// TestRoundtrip_ZIP verifies ZIP-headered data (2MB) round-trips correctly.
func TestRoundtrip_ZIP(t *testing.T) {
	c := newCluster(t, 3, 3)
	data := makePayload("zip", 2*1024*1024)
	assertRoundtrip(t, c.nodes[0], c.nodes[0], "compress-zip-key", data)
}

// TestRoundtrip_Gzip verifies gzip-headered data (1MB) round-trips correctly.
func TestRoundtrip_Gzip(t *testing.T) {
	c := newCluster(t, 3, 3)
	data := makePayload("gzip", 1*1024*1024)
	assertRoundtrip(t, c.nodes[0], c.nodes[0], "compress-gzip-key", data)
}

// TestRoundtrip_RandomBinary verifies high-entropy random data (4MB)
// round-trips without corruption.
func TestRoundtrip_RandomBinary(t *testing.T) {
	c := newCluster(t, 3, 3)
	data := makePayload("binary", 4*1024*1024)
	assertRoundtrip(t, c.nodes[0], c.nodes[0], "compress-binary-key", data)
}

// TestMixedChunks uploads a 12MB file whose chunks alternate between
// compressible (text) and incompressible (binary) content, verifying that
// the entire file round-trips correctly regardless of per-chunk decisions.
func TestMixedChunks(t *testing.T) {
	c := newCluster(t, 3, 3)

	// Build 12MB of alternating 2MB blocks: text, binary, text, binary, text, binary.
	var buf []byte
	for i := 0; i < 6; i++ {
		if i%2 == 0 {
			buf = append(buf, makePayload("text", 2*1024*1024)...)
		} else {
			buf = append(buf, makePayload("binary", 2*1024*1024)...)
		}
	}

	assertRoundtrip(t, c.nodes[0], c.nodes[0], "mixed-chunks-key", buf)
}

// TestAllTypesRoundtripOnAllNodes uploads one file of each major type and
// verifies all 3 nodes return the correct bytes.
func TestAllTypesRoundtripOnAllNodes(t *testing.T) {
	c := newCluster(t, 3, 3)

	types := []string{"text", "html", "csv", "jpeg", "png", "zip", "binary"}
	payloads := make(map[string][]byte, len(types))

	for _, typ := range types {
		key := fmt.Sprintf("allnodes-%s", typ)
		payloads[key] = makePayload(typ, 512*1024)
		if err := c.nodes[0].StoreData(key, readerOf(payloads[key])); err != nil {
			t.Fatalf("StoreData(%s): %v", typ, err)
		}
	}

	time.Sleep(500 * time.Millisecond)

	for key, expected := range payloads {
		wantHash := sha256hex(expected)
		for i, nd := range c.nodes {
			got := mustGetData(t, nd, key)
			if sha256hex(got) != wantHash {
				t.Errorf("node[%d] key=%q: sha256 mismatch", i, key)
			}
		}
	}
}

// TestCompressChunkAPI directly exercises the CompressChunk helper to confirm
// compressible data shrinks and incompressible data is returned as-is.
func TestCompressChunkAPI(t *testing.T) {
	compressible := makePayload("text", 128*1024)
	out, wasCompressed, err := compression.CompressChunk(compressible, compression.LevelDefault)
	if err != nil {
		t.Fatalf("CompressChunk(text): %v", err)
	}
	if !wasCompressed {
		t.Error("expected text data to be compressed")
	}
	if len(out) >= len(compressible) {
		t.Errorf("compressed size (%d) should be < original (%d)", len(out), len(compressible))
	}

	incompressible := makePayload("binary", 128*1024)
	out2, wasCompressed2, err := compression.CompressChunk(incompressible, compression.LevelDefault)
	if err != nil {
		t.Fatalf("CompressChunk(binary): %v", err)
	}
	if wasCompressed2 {
		t.Error("expected binary data NOT to be compressed")
	}
	if len(out2) != len(incompressible) {
		t.Errorf("uncompressed output size (%d) should equal input (%d)", len(out2), len(incompressible))
	}
}
