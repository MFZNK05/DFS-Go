package main

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

type pathTransform func(string) PathKey

type StructOpts struct {
	PathTransformFunc pathTransform
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
	pathKey := s.structOpts.PathTransformFunc(key)

	fullPath := filepath.Join(s.structOpts.Root, pathKey.pathname)
	err := os.MkdirAll(fullPath, os.ModePerm)
	if err != nil {
		return 0, err
	}

	buf := new(bytes.Buffer)
	_, err = io.Copy(buf, r)
	if err != nil {
		return 0, err
	}

	hash := md5.Sum(buf.Bytes())
	hashStr := hex.EncodeToString(hash[:])
	pathKey.filename = hashStr

	finalPath := filepath.Join(fullPath, pathKey.filename)

	f, err := os.Create(finalPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	n, err := io.Copy(f, buf)
	if err != nil {
		return 0, err
	}

	fm, ok := s.structOpts.Metadata.Get(key)
	if !ok {
		return 0, os.ErrNotExist
	}

	fm.Path = finalPath

	if err := s.structOpts.Metadata.Set(key, fm); err != nil {
		return 0, err
	}

	log.Printf("Written %d bytes to %s\n", n, finalPath)
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
