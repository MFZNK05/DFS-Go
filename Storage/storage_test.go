package Storage

import (
	"bytes"
	"fmt"
	"io"
	"testing"
)

var Key = "test.txt"
var Data = []byte("whassup ma boy!")

func TestPathTransformFunc(t *testing.T) {
	// SHA-256 produces 64 hex characters (vs SHA-1's 40)
	expectedPathname := "a6ed0/c785d/4590b/c95c2/16bcf/51438/4eee6/765b1/c2b73/2d0b0/a1ad7/e14d3"
	expectedFilename := "a6ed0c785d4590bc95c216bcf514384eee6765b1c2b732d0b0a1ad7e14d3"

	pathKey := CASPathTransformFunc(Key)

	// Note: With SHA-256 we get more path segments
	if pathKey.filename[:60] != expectedFilename {
		t.Errorf("unexpected filename:\ngot: %s\nwant prefix: %s",
			pathKey.filename, expectedFilename)
	}
	if pathKey.pathname != expectedPathname {
		t.Errorf("unexpected pathname:\ngot: %s\nwant: %s",
			pathKey.pathname, expectedPathname)
	}
	t.Logf("pathKey correctly transformed: filename=%s, pathname=%s", pathKey.filename, pathKey.pathname)
}

func TestWriteStream(t *testing.T) {
	reader := bytes.NewReader(Data)

	store := NewStore(StructOpts{
		PathTransformFunc: CASPathTransformFunc,
		Metadata:          NewMetaFile("metadata_test.json"),
	})

	_, err := store.WriteStream(Key, reader)
	if err != nil {
		t.Fatalf("WriteStream failed: %v", err)
	}

	t.Logf("WriteStream succeeded for key: %s", Key)
}

func TestReadStream(t *testing.T) {
	store := NewStore(StructOpts{
		PathTransformFunc: CASPathTransformFunc,
		Metadata:          NewMetaFile("metadata_test.json"),
	})

	for i := 0; i < 50; i++ {
		key := fmt.Sprintf("file%d", i)

		// Write
		reader := bytes.NewReader(Data)
		_, err := store.WriteStream(key, reader)
		if err != nil {
			t.Fatalf("WriteStream failed: %v", err)
		}

		// Read
		_, readStream, err := store.ReadStream(key)
		if err != nil {
			t.Fatalf("ReadStream failed: %v", err)
		}

		readData, err := io.ReadAll(readStream)
		readStream.Close() // Close the file handle
		if err != nil {
			t.Fatalf("Reading data failed: %v", err)
		}

		if !bytes.Equal(readData, Data) {
			t.Errorf("ReadStream data mismatch:\ngot: %s\nwant: %s", string(readData), string(Data))
		} else {
			t.Logf("ReadStream succeeded for key: %s", key)
		}
	}

	if err := store.TearDown(); err != nil {
		t.Error(err)
	}
}
