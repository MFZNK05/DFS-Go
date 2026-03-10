package State

import (
	"os"
	"path/filepath"
	"testing"
)

func tempDB(t *testing.T) *StateDB {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestRecordAndListUploads(t *testing.T) {
	db := tempDB(t)

	err := db.RecordUpload(UploadEntry{
		Key:  "abc123/myfile",
		Name: "myfile",
		Path: "/tmp/myfile.txt",
		Size: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}

	err = db.RecordUpload(UploadEntry{
		Key:    "abc123/secret",
		Name:   "secret",
		Path:   "/tmp/secret.txt",
		Size:   2048,
		Public: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	uploads, err := db.ListUploads()
	if err != nil {
		t.Fatal(err)
	}
	if len(uploads) != 2 {
		t.Fatalf("expected 2 uploads, got %d", len(uploads))
	}
}

func TestRecordAndListDownloads(t *testing.T) {
	db := tempDB(t)

	err := db.RecordDownload(DownloadEntry{
		Key:        "abc123/myfile",
		Name:       "myfile",
		OutputPath: "/tmp/out.txt",
		Size:       1024,
	})
	if err != nil {
		t.Fatal(err)
	}

	downloads, err := db.ListDownloads()
	if err != nil {
		t.Fatal(err)
	}
	if len(downloads) != 1 {
		t.Fatalf("expected 1 download, got %d", len(downloads))
	}
	if downloads[0].OutputPath != "/tmp/out.txt" {
		t.Fatalf("unexpected output path: %s", downloads[0].OutputPath)
	}
}

func TestPublicFiles(t *testing.T) {
	db := tempDB(t)

	err := db.AddPublicFile(PublicFileEntry{Key: "abc/file1", Name: "file1", Size: 100})
	if err != nil {
		t.Fatal(err)
	}
	err = db.AddPublicFile(PublicFileEntry{Key: "abc/file2", Name: "file2", Size: 200, IsDir: true})
	if err != nil {
		t.Fatal(err)
	}

	files, err := db.ListPublicFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 public files, got %d", len(files))
	}

	// Summary
	count, totalSize, hash := db.PublicCatalogSummary()
	if count != 2 {
		t.Fatalf("expected count 2, got %d", count)
	}
	if totalSize != 300 {
		t.Fatalf("expected totalSize 300, got %d", totalSize)
	}
	if hash == "" {
		t.Fatal("expected non-empty hash")
	}

	// Remove one
	err = db.RemovePublicFile("abc/file1")
	if err != nil {
		t.Fatal(err)
	}
	files, err = db.ListPublicFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 public file after remove, got %d", len(files))
	}

	// Hash should change after removal
	_, _, hash2 := db.PublicCatalogSummary()
	if hash2 == hash {
		t.Fatal("hash should change after removing a file")
	}
}

func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "persist.db")

	// Write
	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	err = db.RecordUpload(UploadEntry{Key: "fp/test", Name: "test", Size: 42})
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Reopen
	db2, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()

	uploads, err := db2.ListUploads()
	if err != nil {
		t.Fatal(err)
	}
	if len(uploads) != 1 || uploads[0].Name != "test" {
		t.Fatalf("expected persisted upload 'test', got %v", uploads)
	}
}

func TestOpenInvalidPath(t *testing.T) {
	_, err := Open("/nonexistent/path/state.db")
	if err == nil {
		t.Fatal("expected error for invalid path")
	}
}

func TestTimestampAutoSet(t *testing.T) {
	db := tempDB(t)

	err := db.RecordUpload(UploadEntry{Key: "fp/auto", Name: "auto", Size: 1})
	if err != nil {
		t.Fatal(err)
	}

	uploads, err := db.ListUploads()
	if err != nil {
		t.Fatal(err)
	}
	if uploads[0].Timestamp == 0 {
		t.Fatal("expected auto-set timestamp")
	}
}

func TestEmptyDB(t *testing.T) {
	db := tempDB(t)

	uploads, err := db.ListUploads()
	if err != nil {
		t.Fatal(err)
	}
	if len(uploads) != 0 {
		t.Fatalf("expected 0 uploads, got %d", len(uploads))
	}

	downloads, err := db.ListDownloads()
	if err != nil {
		t.Fatal(err)
	}
	if len(downloads) != 0 {
		t.Fatalf("expected 0 downloads, got %d", len(downloads))
	}

	count, totalSize, _ := db.PublicCatalogSummary()
	if count != 0 || totalSize != 0 {
		t.Fatalf("expected empty summary, got count=%d size=%d", count, totalSize)
	}
}

// Suppress unused import warning
var _ = os.Remove
