package ipc

import (
	"time"

	"github.com/Faizan2005/DFS-Go/Storage/chunker"
)

// ECDHUploadRequest holds all fields for an ECDH upload IPC message.
type ECDHUploadRequest struct {
	StorageKey    string
	DEK           []byte
	OwnerPubKey   string
	OwnerEdPubKey string
	AccessList    []chunker.AccessEntry
	Signature     string
	FileSize      int64
	Public        bool
}

// ECDHDirUploadRequest holds all fields for an ECDH directory upload.
type ECDHDirUploadRequest struct {
	StorageKey    string
	DirPath       string
	DEK           []byte
	OwnerPubKey   string
	OwnerEdPubKey string
	AccessList    []chunker.AccessEntry
	Signature     string
	Public        bool
}

// ManifestInfoResponse holds ECDH manifest fields returned to clients.
type ManifestInfoResponse struct {
	Encrypted   bool
	IsDirectory bool
	OwnerPub    string
	AccessList  []chunker.AccessEntry
}

// PeerInfo is the JSON structure for peer listing responses.
type PeerInfo struct {
	Addr        string `json:"addr"`
	State       string `json:"state"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Alias       string `json:"alias,omitempty"`
	PublicCount string `json:"publicCount,omitempty"`
}

// NodeStatusInfo is the JSON structure for node status responses.
type NodeStatusInfo struct {
	Addr      string `json:"addr"`
	Status    string `json:"status"`
	Uptime    string `json:"uptime"`
	PeerCount int    `json:"peerCount"`
	Uploads   int    `json:"uploads"`
	Downloads int    `json:"downloads"`
}

// TransferInfo mirrors Server/transfer.TransferInfo for JSON responses.
type TransferInfo struct {
	ID        string    `json:"id"`
	Direction int       `json:"direction"`
	Status    int       `json:"status"`
	Key       string    `json:"key"`
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	IsDir     bool      `json:"isDir"`
	Encrypted bool      `json:"encrypted"`
	Public    bool      `json:"public"`
	Completed int       `json:"completed"`
	Total     int       `json:"total"`
	BytesDone int64     `json:"bytesDone"`
	Speed     float64   `json:"speed"`
	StartedAt time.Time `json:"startedAt"`
	Error     string    `json:"error,omitempty"`
	FilePath  string    `json:"filePath"`
	OutputDir string    `json:"outputDir"`
}

// SearchResult mirrors Server.SearchResult for JSON responses.
type SearchResult struct {
	Key        string `json:"key"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	IsDir      bool   `json:"isDir"`
	OwnerAlias string `json:"ownerAlias"`
	OwnerFP    string `json:"ownerFingerprint"`
	NodeAddr   string `json:"nodeAddr"`
}

// AliasResult holds resolved alias information.
type AliasResult struct {
	Fingerprint   string `json:"fingerprint"`
	X25519PubHex  string `json:"x25519PubHex"`
	Ed25519PubHex string `json:"ed25519PubHex"`
	NodeAddr      string `json:"nodeAddr"`
}

// UploadEntry mirrors State.UploadEntry for JSON responses.
type UploadEntry struct {
	Key       string `json:"key"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Encrypted bool   `json:"encrypted"`
	Public    bool   `json:"public"`
	IsDir     bool   `json:"isDir"`
	Timestamp int64  `json:"timestamp"`
}

// DownloadEntry mirrors State.DownloadEntry for JSON responses.
type DownloadEntry struct {
	Key        string `json:"key"`
	Name       string `json:"name"`
	OutputPath string `json:"outputPath"`
	Size       int64  `json:"size"`
	Encrypted  bool   `json:"encrypted"`
	IsDir      bool   `json:"isDir"`
	Timestamp  int64  `json:"timestamp"`
}

// PublicFileEntry mirrors State.PublicFileEntry for JSON responses.
type PublicFileEntry struct {
	Key       string `json:"key"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	IsDir     bool   `json:"isDir"`
	Timestamp int64  `json:"timestamp,omitempty"`
}

// InboxEntry represents a file sent directly to this node by another peer.
type InboxEntry struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	IsDir       bool   `json:"isDir"`
	SenderAlias string `json:"senderAlias"`
	SenderFP    string `json:"senderFingerprint"`
	ReceivedAt  int64  `json:"receivedAt"`
}

// BrowseEntry is a labeled file entry for the combined browse view.
type BrowseEntry struct {
	Key        string `json:"key"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	IsDir      bool   `json:"isDir"`
	OwnerAlias string `json:"ownerAlias,omitempty"`
	OwnerFP    string `json:"ownerFingerprint,omitempty"`
	Label      string `json:"label"`              // "public", "shared", "inbox"
	Timestamp  int64  `json:"timestamp,omitempty"` // unix nano, for sorting
}
