package Storage

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Faizan2005/DFS-Go/Storage/chunker"
	bolt "go.etcd.io/bbolt"
)

const defaultRoot = "DFSNetworkRoot"

type Store struct {
	structOpts StructOpts
}

type PathTransform func(string) PathKey

type StructOpts struct {
	PathTransformFunc PathTransform
	Metadata          MetadataStore
	Root              string
}

type FileMeta struct {
	Path      string
	VClock    map[string]uint64      // vector clock at time of write (Sprint 4)
	Timestamp int64                  // UnixNano wall clock — LWW tiebreaker (Sprint 4)
	Chunked   bool                   // true when the file was split into chunks (Sprint 6)
	Manifest  *chunker.ChunkManifest // non-nil iff Chunked == true (Sprint 6)
}

type PathKey struct {
	pathname string
	filename string
}

// Interface for metadata operations
type MetadataStore interface {
	Get(key string) (FileMeta, bool)
	Set(key string, meta FileMeta) error
	GetManifest(fileKey string) (*chunker.ChunkManifest, bool)
	SetManifest(fileKey string, manifest *chunker.ChunkManifest) error
}

func NewStore(opts StructOpts) *Store {
	if opts.PathTransformFunc == nil {
		opts.PathTransformFunc = DefaultPathTransformFunc
	}
	if opts.Root == "" {
		opts.Root = defaultRoot
	}
	return &Store{structOpts: opts}
}

func DefaultPathTransformFunc(key string) PathKey {
	return PathKey{
		pathname: key,
		filename: key,
	}
}

func CASPathTransformFunc(key string) PathKey {
	hash := sha256.Sum256([]byte(key))
	hashStr := hex.EncodeToString(hash[:])

	blockSize := 5
	sliceLen := len(hashStr) / blockSize

	paths := make([]string, sliceLen)
	for i := 0; i < sliceLen; i++ {
		from, to := i*blockSize, (i+1)*blockSize
		if to > len(hashStr) {
			to = len(hashStr)
		}
		paths[i] = hashStr[from:to]
	}

	pathKey := strings.Join(paths, "/")

	return PathKey{
		pathname: pathKey,
		filename: hashStr,
	}
}

// ReadStream returns a reader for the file content.
// IMPORTANT: The returned io.ReadCloser MUST be closed by the caller.
func (s *Store) ReadStream(key string) (int64, io.ReadCloser, error) {
	meta, ok := s.structOpts.Metadata.Get(key)
	if !ok {
		return 0, nil, os.ErrNotExist
	}

	file, err := os.Open(meta.Path)
	if err != nil {
		return 0, nil, err
	}

	info, err := file.Stat()
	if err != nil {
		file.Close()
		return 0, nil, err
	}

	// Return the file handle directly - caller must close it
	return info.Size(), file, nil
}

func (s *Store) WriteStream(key string, r io.Reader) (int64, error) {
	log.Printf("WRITE_STREAM: Starting to write stream for key: %s", key)

	// Transform the key to path
	pathKey := s.structOpts.PathTransformFunc(key)
	log.Printf("WRITE_STREAM: Transformed key to path: %s", pathKey.pathname)

	// Create directory
	fullPath := filepath.Join(s.structOpts.Root, pathKey.pathname)
	log.Printf("WRITE_STREAM: Creating directory at: %s", fullPath)
	err := os.MkdirAll(fullPath, os.ModePerm)
	if err != nil {
		log.Printf("WRITE_STREAM: Failed to create directory: %v", err)
		return 0, err
	}

	// Create a temporary file to stream data to
	tempFile, err := os.CreateTemp(fullPath, "temp-*")
	if err != nil {
		log.Printf("WRITE_STREAM: Failed to create temp file: %v", err)
		return 0, err
	}
	tempPath := tempFile.Name()

	// Ensure cleanup on error
	defer func() {
		tempFile.Close()
		// Remove temp file if it still exists (wasn't renamed)
		os.Remove(tempPath)
	}()

	// Create MD5 hasher for content-addressing
	hasher := md5.New()

	// TeeReader: reads from r, writes to hasher, returns data for file write
	teeReader := io.TeeReader(r, hasher)

	// Stream directly to temp file while computing hash
	log.Println("WRITE_STREAM: Streaming data to temp file while computing hash...")
	n, err := io.Copy(tempFile, teeReader)
	if err != nil {
		log.Printf("WRITE_STREAM: Failed to write to temp file: %v", err)
		return 0, err
	}
	log.Printf("WRITE_STREAM: Successfully streamed %d bytes", n)

	// Get the computed hash
	hashBytes := hasher.Sum(nil)
	hashStr := hex.EncodeToString(hashBytes)
	pathKey.filename = hashStr
	log.Printf("WRITE_STREAM: Calculated MD5 hash: %s", hashStr)

	// Close temp file before rename
	if err := tempFile.Close(); err != nil {
		log.Printf("WRITE_STREAM: Failed to close temp file: %v", err)
		return 0, err
	}

	// Final file path (content-addressed)
	finalPath := filepath.Join(fullPath, pathKey.filename)
	log.Printf("WRITE_STREAM: Final path for file: %s", finalPath)

	// Rename temp file to final content-addressed path
	if err := os.Rename(tempPath, finalPath); err != nil {
		log.Printf("WRITE_STREAM: Failed to rename temp file: %v", err)
		return 0, err
	}
	log.Println("WRITE_STREAM: File renamed successfully")

	// Update metadata
	log.Printf("WRITE_STREAM: Updating metadata for key: %s", key)
	fm, _ := s.structOpts.Metadata.Get(key)
	fm.Path = finalPath

	if err := s.structOpts.Metadata.Set(key, fm); err != nil {
		log.Printf("WRITE_STREAM: Failed to update metadata: %v", err)
		return 0, err
	}
	log.Printf("WRITE_STREAM: Metadata updated successfully")

	return n, nil
}

func (s *Store) Remove(key string) error {
	meta, ok := s.structOpts.Metadata.Get(key)
	if !ok {
		return os.ErrNotExist
	}
	return os.Remove(meta.Path)
}

func (s *Store) Has(key string) bool {
	meta, ok := s.structOpts.Metadata.Get(key)
	if !ok {
		return false
	}

	_, err := os.Stat(meta.Path)
	return !errors.Is(err, os.ErrNotExist)
}

func (s *Store) TearDown() error {
	return os.RemoveAll(s.structOpts.Root)
}

type MetaFile struct {
	path  string
	store map[string]FileMeta
	mu    sync.Mutex
}

func NewMetaFile(path string) *MetaFile {
	m := &MetaFile{
		path:  path,
		store: make(map[string]FileMeta),
	}

	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &m.store)
	} else {
		_ = os.WriteFile(path, []byte("{}"), 0600)
	}

	return m
}

func (m *MetaFile) Get(key string) (FileMeta, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	val, ok := m.store[key]
	return val, ok
}

func (m *MetaFile) Set(key string, meta FileMeta) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.store[key] = meta
	data, err := json.MarshalIndent(m.store, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(m.path, data, 0600)
}

// Keys returns all keys currently tracked in the metadata store.
// Used by Rebalancer to enumerate locally-held files.
func (m *MetaFile) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	keys := make([]string, 0, len(m.store))
	for k := range m.store {
		keys = append(keys, k)
	}
	return keys
}

// GetManifest returns the ChunkManifest for a chunked file, stored inline in
// the FileMeta entry under the manifest storage key.
func (m *MetaFile) GetManifest(fileKey string) (*chunker.ChunkManifest, bool) {
	mkey := chunker.ManifestStorageKey(fileKey)
	fm, ok := m.Get(mkey)
	if !ok || fm.Manifest == nil {
		return nil, false
	}
	return fm.Manifest, true
}

// SetManifest stores manifest in the metadata store under the manifest key for
// fileKey. The manifest is serialised as part of the FileMeta JSON.
func (m *MetaFile) SetManifest(fileKey string, manifest *chunker.ChunkManifest) error {
	mkey := chunker.ManifestStorageKey(fileKey)
	return m.Set(mkey, FileMeta{Manifest: manifest})
}

// ---------------------------------------------------------------------------
// BoltMetaStore — bbolt-backed MetadataStore implementation.
// Each Set() writes only the affected key (O(1)), replacing the O(N) full
// JSON rewrite of MetaFile. Crash-safe via bbolt's WAL.
// ---------------------------------------------------------------------------

const (
	boltBucketMeta     = "filemeta"
	boltBucketManifest = "manifests"
)

// BoltMetaStore implements MetadataStore using an embedded bbolt database.
type BoltMetaStore struct {
	db *bolt.DB
}

// NewBoltMetaStore opens (or creates) a bbolt database at dbPath and returns
// a ready-to-use BoltMetaStore.
func NewBoltMetaStore(dbPath string) (*BoltMetaStore, error) {
	db, err := bolt.Open(dbPath, 0600, nil)
	if err != nil {
		return nil, fmt.Errorf("bolt open %s: %w", dbPath, err)
	}
	// Ensure both buckets exist.
	err = db.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists([]byte(boltBucketMeta)); err != nil {
			return err
		}
		_, err := tx.CreateBucketIfNotExists([]byte(boltBucketManifest))
		return err
	})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("bolt init buckets: %w", err)
	}
	return &BoltMetaStore{db: db}, nil
}

// Close releases the database file. Call on server shutdown.
func (b *BoltMetaStore) Close() error {
	return b.db.Close()
}

func (b *BoltMetaStore) Get(key string) (FileMeta, bool) {
	var fm FileMeta
	err := b.db.View(func(tx *bolt.Tx) error {
		bkt := tx.Bucket([]byte(boltBucketMeta))
		v := bkt.Get([]byte(key))
		if v == nil {
			return os.ErrNotExist
		}
		return json.Unmarshal(v, &fm)
	})
	if err != nil {
		return FileMeta{}, false
	}
	return fm, true
}

func (b *BoltMetaStore) Set(key string, meta FileMeta) error {
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return b.db.Update(func(tx *bolt.Tx) error {
		bkt := tx.Bucket([]byte(boltBucketMeta))
		return bkt.Put([]byte(key), data)
	})
}

// Keys returns all keys in the filemeta bucket.
// Used by Rebalancer via type assertion (not part of MetadataStore interface).
func (b *BoltMetaStore) Keys() []string {
	var keys []string
	b.db.View(func(tx *bolt.Tx) error {
		bkt := tx.Bucket([]byte(boltBucketMeta))
		return bkt.ForEach(func(k, _ []byte) error {
			keys = append(keys, string(k))
			return nil
		})
	})
	return keys
}

func (b *BoltMetaStore) GetManifest(fileKey string) (*chunker.ChunkManifest, bool) {
	var manifest chunker.ChunkManifest
	err := b.db.View(func(tx *bolt.Tx) error {
		bkt := tx.Bucket([]byte(boltBucketManifest))
		v := bkt.Get([]byte(fileKey))
		if v == nil {
			return os.ErrNotExist
		}
		return json.Unmarshal(v, &manifest)
	})
	if err != nil {
		return nil, false
	}
	return &manifest, true
}

func (b *BoltMetaStore) SetManifest(fileKey string, manifest *chunker.ChunkManifest) error {
	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	return b.db.Update(func(tx *bolt.Tx) error {
		bkt := tx.Bucket([]byte(boltBucketManifest))
		return bkt.Put([]byte(fileKey), data)
	})
}
