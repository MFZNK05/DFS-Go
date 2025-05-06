package Storage

import (
	"bytes"
	"crypto/md5"
	"crypto/sha1"
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
	hash := sha1.Sum([]byte(key))
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

func (s *Store) ReadStream(key string) (int64, io.Reader, error) {
	meta, ok := s.structOpts.Metadata.Get(key)
	if !ok {
		return 0, nil, os.ErrNotExist
	}

	file, err := os.Open(meta.Path)
	if err != nil {
		return 0, nil, err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return 0, nil, err
	}

	buf := new(bytes.Buffer)
	if _, err := io.Copy(buf, file); err != nil {
		return 0, nil, err
	}

	return info.Size(), buf, nil
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

	// Read all data into buffer
	buf := new(bytes.Buffer)
	log.Println("WRITE_STREAM: Copying data into buffer")
	_, err = io.Copy(buf, r)
	if err != nil {
		log.Printf("WRITE_STREAM: Error while copying data to buffer: %v", err)
		return 0, err
	}
	log.Printf("WRITE_STREAM: Data copied into buffer (%d bytes)", buf.Len())

	// Calculate hash
	hash := md5.Sum(buf.Bytes())
	hashStr := hex.EncodeToString(hash[:])
	pathKey.filename = hashStr
	log.Printf("WRITE_STREAM: Calculated MD5 hash: %s", hashStr)

	// Final file path
	finalPath := filepath.Join(fullPath, pathKey.filename)
	log.Printf("WRITE_STREAM: Final path for file: %s", finalPath)

	// Create file
	f, err := os.Create(finalPath)
	if err != nil {
		log.Printf("WRITE_STREAM: Failed to create file: %v", err)
		return 0, err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			log.Printf("WRITE_STREAM: Warning - failed to close file: %v", cerr)
		}
	}()
	log.Println("WRITE_STREAM: File created successfully")

	// Write buffer to file
	log.Println("WRITE_STREAM: Writing buffer data to file")
	n, err := io.Copy(f, buf)
	if err != nil {
		log.Printf("WRITE_STREAM: Failed to write to file: %v", err)
		return 0, err
	}
	log.Printf("WRITE_STREAM: Successfully wrote %d bytes to file", n)

	// Get and update metadata
	log.Printf("WRITE_STREAM: Fetching metadata for key: %s", key)
	fm, ok := s.structOpts.Metadata.Get(key)
	if !ok {
		log.Printf("WRITE_STREAM: Metadata for key '%s' does not exist. Creating new metadata entry.", key)
		fm := FileMeta{}
		if err := s.structOpts.Metadata.Set(key, fm); err != nil {
			return 0, err
		}
	}

	fm.Path = finalPath
	log.Printf("WRITE_STREAM: Setting file path in metadata: %s", finalPath)

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
