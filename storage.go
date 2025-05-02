package main

import (
	"bytes"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
)

const defaultRoot = "DFSNetworkRoot"

type Store struct {
	structOpts StructOpts
}

type pathTransform func(string) PathKey

type StructOpts struct {
	PathTransformFunc pathTransform
	Metadata          *Metadata
	Root              string
}

type PathKey struct {
	pathname string
	filename string
}

func NewStore(opts StructOpts) *Store {
	store := &Store{
		structOpts: opts,
	}

	if store.structOpts.PathTransformFunc == nil {
		store.structOpts.PathTransformFunc = DefaultPathTransformFunc
	}

	if store.structOpts.Root == "" {
		store.structOpts.Root = defaultRoot
	}

	return store
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
		from, to := i*blockSize, (i*blockSize)+blockSize
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
	// PathKey := s.structOpts.pathTransformFunc(key)

	// filePath := PathKey.pathname

	filePath, ok := s.structOpts.Metadata.Get(key)
	if !ok {
		return 0, nil, os.ErrNotExist
	}

	file, err := os.Open(filePath)
	if err != nil {
		return 0, nil, err
	}

	fi, _ := file.Stat()
	fs := fi.Size()

	buff := new(bytes.Buffer)

	if _, err = io.Copy(buff, file); err != nil {
		return 0, nil, err
	}

	file.Close()

	return fs, buff, nil
}

func (s *Store) WriteStream(key string, w io.Reader) (int64, error) {
	pathKey := s.structOpts.PathTransformFunc(key)
	//pathKey := s.CASPathTransformFunc(key)

	err := os.MkdirAll(s.structOpts.Root+"/"+pathKey.pathname, os.ModePerm)
	if err != nil {
		return 0, err
	}

	buff := new(bytes.Buffer)
	_, err = io.Copy(buff, w)
	if err != nil {
		return 0, err
	}

	hash := md5.Sum(buff.Bytes())
	hashStr := hex.EncodeToString(hash[:])
	pathKey.filename = hashStr

	filePath := s.structOpts.Root + "/" + pathKey.pathname + "/" + pathKey.filename

	f, err := os.Create(filePath)
	if err != nil {
		return 0, err
	}

	defer f.Close()

	n, err := io.Copy(f, buff)
	if err != nil {
		return 0, err
	}

	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}

	fs := fi.Size()
	err = s.structOpts.Metadata.Set(key, filePath)
	if err != nil {
		return 0, err
	}

	log.Printf("written (%d) bytes to disk: %s", n, filePath)
	return fs, nil
}

func (s *Store) Remove(key string) error {
	filePath, ok := s.structOpts.Metadata.Get(key)
	if !ok {
		return os.ErrNotExist
	}

	fmt.Println(filePath)
	paths := strings.Split(filePath, "/")

	err := os.RemoveAll(paths[1])
	if err != nil {
		return err
	}

	return nil
}

func (s *Store) TearDown() error {
	return os.RemoveAll(s.structOpts.Root)
}

func (s *Store) Has(key string) bool {
	filePath, _ := s.structOpts.Metadata.Get(key)

	_, err := os.Stat(filePath)
	return !errors.Is(err, os.ErrNotExist)
}
