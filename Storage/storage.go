package Storage

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	Path         string
	EncryptedKey string // Optional encrypted key (unused currently)
}

type PathKey struct {
	pathname string
	filename string
}

// Interface for metadata operations
type MetadataStore interface {
	Get(key string) (FileMeta, bool)
	Set(key string, meta FileMeta) error
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
		_ = os.WriteFile(path, []byte("{}"), os.ModePerm)
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

	return os.WriteFile(m.path, data, os.ModePerm)
}
