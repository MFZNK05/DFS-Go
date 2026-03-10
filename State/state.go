// Package State provides persistent local state for the DFS daemon.
// It tracks upload history, download history, and public file listings
// using a BoltDB database at ~/.dfs/state.db.
package State

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	bucketUploads     = []byte("uploads")
	bucketDownloads   = []byte("downloads")
	bucketPublicFiles = []byte("public_files")
	bucketConfig      = []byte("config")
	bucketTombstones  = []byte("tombstones")
	bucketKnownPeers   = []byte("known_peers")
	bucketInbox        = []byte("inbox")
	bucketIgnoredPeers    = []byte("ignored_peers")
	bucketTransferHistory = []byte("transfer_history")
	bucketOutbox          = []byte("outbox")
)

// TombstoneEntry records a soft-deleted file for gossip propagation.
type TombstoneEntry struct {
	Key         string `json:"key"`
	DeletedAt   int64  `json:"deletedAt"`
	Fingerprint string `json:"fingerprint"`
}

// UploadEntry records a file or directory uploaded by this node.
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

// DownloadEntry records a file or directory downloaded by this node.
type DownloadEntry struct {
	Key        string `json:"key"`
	Name       string `json:"name"`
	OutputPath string `json:"outputPath"`
	Size       int64  `json:"size"`
	Encrypted  bool   `json:"encrypted"`
	IsDir      bool   `json:"isDir"`
	Timestamp  int64  `json:"timestamp"`
}

// TransferHistoryEntry records a completed transfer for persistent history.
type TransferHistoryEntry struct {
	ID        string  `json:"id"`
	Direction int     `json:"direction"` // 0=upload, 1=download
	Key       string  `json:"key"`
	Name      string  `json:"name"`
	Size      int64   `json:"size"`
	IsDir     bool    `json:"isDir"`
	Encrypted bool    `json:"encrypted"`
	Public    bool    `json:"public"`
	Speed     float64 `json:"speed"`     // average bytes/sec
	StartedAt int64   `json:"startedAt"` // unix nano
	DoneAt    int64   `json:"doneAt"`    // unix nano
	Status    int     `json:"status"`    // 3=completed, 4=failed
	Error     string  `json:"error,omitempty"`
}

// PublicFileEntry is a public file listing entry (subset of UploadEntry).
type PublicFileEntry struct {
	Key       string `json:"key"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	IsDir     bool   `json:"isDir"`
	Timestamp int64  `json:"timestamp,omitempty"`
}

// StateDB wraps a BoltDB for persistent local state.
type StateDB struct {
	db *bolt.DB
}

// Open opens or creates a StateDB at the given path.
func Open(path string) (*StateDB, error) {
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("state db open %s: %w", path, err)
	}

	// Create buckets.
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bucketUploads, bucketDownloads, bucketPublicFiles, bucketConfig, bucketTombstones, bucketKnownPeers, bucketInbox, bucketIgnoredPeers, bucketTransferHistory, bucketOutbox} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}

	return &StateDB{db: db}, nil
}

// Close closes the underlying BoltDB.
func (s *StateDB) Close() error {
	return s.db.Close()
}

// RecordUpload stores an upload entry.
func (s *StateDB) RecordUpload(entry UploadEntry) error {
	if entry.Timestamp == 0 {
		entry.Timestamp = time.Now().UnixNano()
	}
	return s.put(bucketUploads, entry.Key, entry)
}

// ListUploads returns all upload entries.
func (s *StateDB) ListUploads() ([]UploadEntry, error) {
	var entries []UploadEntry
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketUploads)
		return b.ForEach(func(k, v []byte) error {
			var e UploadEntry
			if err := json.Unmarshal(v, &e); err != nil {
				return err
			}
			entries = append(entries, e)
			return nil
		})
	})
	return entries, err
}

// RecordDownload stores a download entry.
func (s *StateDB) RecordDownload(entry DownloadEntry) error {
	if entry.Timestamp == 0 {
		entry.Timestamp = time.Now().UnixNano()
	}
	return s.put(bucketDownloads, entry.Key, entry)
}

// ListDownloads returns all download entries.
func (s *StateDB) ListDownloads() ([]DownloadEntry, error) {
	var entries []DownloadEntry
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketDownloads)
		return b.ForEach(func(k, v []byte) error {
			var e DownloadEntry
			if err := json.Unmarshal(v, &e); err != nil {
				return err
			}
			entries = append(entries, e)
			return nil
		})
	})
	return entries, err
}

// AddPublicFile adds a file to the public catalog.
func (s *StateDB) AddPublicFile(entry PublicFileEntry) error {
	return s.put(bucketPublicFiles, entry.Key, entry)
}

// RemovePublicFile removes a file from the public catalog.
func (s *StateDB) RemovePublicFile(key string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketPublicFiles).Delete([]byte(key))
	})
}

// ListPublicFiles returns all public file entries.
func (s *StateDB) ListPublicFiles() ([]PublicFileEntry, error) {
	var entries []PublicFileEntry
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketPublicFiles)
		return b.ForEach(func(k, v []byte) error {
			var e PublicFileEntry
			if err := json.Unmarshal(v, &e); err != nil {
				return err
			}
			entries = append(entries, e)
			return nil
		})
	})
	return entries, err
}

// PublicCatalogSummary returns the count, total size, and content hash
// of all public files. The hash is a SHA-256 of sorted keys, used for
// change detection in gossip metadata.
func (s *StateDB) PublicCatalogSummary() (count int, totalSize int64, hash string) {
	var keys []string
	_ = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketPublicFiles)
		return b.ForEach(func(k, v []byte) error {
			keys = append(keys, string(k))
			var e PublicFileEntry
			if err := json.Unmarshal(v, &e); err == nil {
				totalSize += e.Size
			}
			count++
			return nil
		})
	})
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
	}
	hash = hex.EncodeToString(h.Sum(nil))[:16]
	return
}

// ── Filtered upload lists ────────────────────────────────────────────

// ListPublicUploads returns uploads where Public == true.
func (s *StateDB) ListPublicUploads() ([]UploadEntry, error) {
	all, err := s.ListUploads()
	if err != nil {
		return nil, err
	}
	var result []UploadEntry
	for _, e := range all {
		if e.Public {
			result = append(result, e)
		}
	}
	return result, nil
}

// ListPrivateUploads returns uploads where Encrypted == true.
func (s *StateDB) ListPrivateUploads() ([]UploadEntry, error) {
	all, err := s.ListUploads()
	if err != nil {
		return nil, err
	}
	var result []UploadEntry
	for _, e := range all {
		if e.Encrypted {
			result = append(result, e)
		}
	}
	return result, nil
}

// ── Remove methods ──────────────────────────────────────────────────

// RemoveUpload removes an upload entry by key.
func (s *StateDB) RemoveUpload(key string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketUploads).Delete([]byte(key))
	})
}

// RemoveDownload removes a download entry by key.
func (s *StateDB) RemoveDownload(key string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketDownloads).Delete([]byte(key))
	})
}

// ── Tombstones ──────────────────────────────────────────────────────

// AddTombstone records a soft-deleted file.
func (s *StateDB) AddTombstone(entry TombstoneEntry) error {
	if entry.DeletedAt == 0 {
		entry.DeletedAt = time.Now().UnixNano()
	}
	return s.put(bucketTombstones, entry.Key, entry)
}

// ListTombstones returns all tombstone entries.
func (s *StateDB) ListTombstones() ([]TombstoneEntry, error) {
	var entries []TombstoneEntry
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketTombstones)
		return b.ForEach(func(k, v []byte) error {
			var e TombstoneEntry
			if err := json.Unmarshal(v, &e); err != nil {
				return err
			}
			entries = append(entries, e)
			return nil
		})
	})
	return entries, err
}

// IsTombstoned checks if a key has been soft-deleted.
func (s *StateDB) IsTombstoned(key string) bool {
	var found bool
	_ = s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketTombstones).Get([]byte(key))
		found = v != nil
		return nil
	})
	return found
}

// RemoveTombstone removes a tombstone (for GC after expiry).
func (s *StateDB) RemoveTombstone(key string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketTombstones).Delete([]byte(key))
	})
}

// ── Known Peers ──────────────────────────────────────────────────────

// KnownPeerEntry records a peer address for auto-reconnect on restart.
type KnownPeerEntry struct {
	Addr    string `json:"addr"`
	Alias   string `json:"alias,omitempty"`
	AddedAt int64  `json:"addedAt"`
}

// AddKnownPeer persists a peer address for future reconnection.
func (s *StateDB) AddKnownPeer(addr string) error {
	existing := s.getKnownPeer(addr)
	if existing != nil {
		return nil // already known
	}
	return s.put(bucketKnownPeers, addr, KnownPeerEntry{
		Addr:    addr,
		AddedAt: time.Now().UnixNano(),
	})
}

// RemoveKnownPeer removes a peer from the known peers list.
func (s *StateDB) RemoveKnownPeer(addr string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketKnownPeers).Delete([]byte(addr))
	})
}

// ListKnownPeers returns all known peer addresses.
func (s *StateDB) ListKnownPeers() ([]string, error) {
	var addrs []string
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketKnownPeers)
		return b.ForEach(func(k, v []byte) error {
			addrs = append(addrs, string(k))
			return nil
		})
	})
	return addrs, err
}

func (s *StateDB) getKnownPeer(addr string) *KnownPeerEntry {
	var entry KnownPeerEntry
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketKnownPeers).Get([]byte(addr))
		if v == nil {
			return fmt.Errorf("not found")
		}
		return json.Unmarshal(v, &entry)
	})
	if err != nil {
		return nil
	}
	return &entry
}

// ── Ignored Peers (Blocklist) ────────────────────────────────────────

// IgnoredPeerEntry records a peer that was manually disconnected.
// The connection manager and gossip auto-dial skip ignored peers.
type IgnoredPeerEntry struct {
	Addr        string `json:"addr"`
	Fingerprint string `json:"fingerprint,omitempty"`
	IgnoredAt   int64  `json:"ignoredAt"`
}

// AddIgnoredPeer adds a peer to the blocklist.
func (s *StateDB) AddIgnoredPeer(entry IgnoredPeerEntry) error {
	if entry.IgnoredAt == 0 {
		entry.IgnoredAt = time.Now().UnixNano()
	}
	return s.put(bucketIgnoredPeers, entry.Addr, entry)
}

// RemoveIgnoredPeer unblocks a peer.
func (s *StateDB) RemoveIgnoredPeer(addr string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketIgnoredPeers).Delete([]byte(addr))
	})
}

// IsIgnoredPeer checks if a peer address is in the blocklist.
func (s *StateDB) IsIgnoredPeer(addr string) bool {
	var found bool
	_ = s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketIgnoredPeers).Get([]byte(addr))
		found = v != nil
		return nil
	})
	return found
}

// IsIgnoredFingerprint checks if any blocklist entry matches the given
// identity fingerprint. This catches peers that changed their IP/port
// (e.g. DHCP reassignment on campus WiFi) but kept the same identity.
func (s *StateDB) IsIgnoredFingerprint(fingerprint string) bool {
	if fingerprint == "" {
		return false
	}
	var found bool
	_ = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketIgnoredPeers)
		return b.ForEach(func(k, v []byte) error {
			var e IgnoredPeerEntry
			if err := json.Unmarshal(v, &e); err == nil && e.Fingerprint == fingerprint {
				found = true
			}
			return nil
		})
	})
	return found
}

// ListIgnoredPeers returns all ignored peer entries.
func (s *StateDB) ListIgnoredPeers() ([]IgnoredPeerEntry, error) {
	var entries []IgnoredPeerEntry
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketIgnoredPeers)
		return b.ForEach(func(k, v []byte) error {
			var e IgnoredPeerEntry
			if err := json.Unmarshal(v, &e); err != nil {
				return err
			}
			entries = append(entries, e)
			return nil
		})
	})
	return entries, err
}

// ── Inbox ────────────────────────────────────────────────────────────

// InboxEntry records a file sent directly to this node by another peer.
type InboxEntry struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	IsDir       bool   `json:"isDir"`
	SenderAlias string `json:"senderAlias"`
	SenderFP    string `json:"senderFingerprint"`
	ReceivedAt  int64  `json:"receivedAt"`
}

// AddInboxEntry records a direct-share file in the inbox.
func (s *StateDB) AddInboxEntry(entry InboxEntry) error {
	if entry.ReceivedAt == 0 {
		entry.ReceivedAt = time.Now().UnixNano()
	}
	return s.put(bucketInbox, entry.Key, entry)
}

// ListInbox returns all inbox entries.
func (s *StateDB) ListInbox() ([]InboxEntry, error) {
	var entries []InboxEntry
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketInbox)
		return b.ForEach(func(k, v []byte) error {
			var e InboxEntry
			if err := json.Unmarshal(v, &e); err != nil {
				return err
			}
			entries = append(entries, e)
			return nil
		})
	})
	return entries, err
}

// RemoveInboxEntry removes an inbox entry by key.
func (s *StateDB) RemoveInboxEntry(key string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketInbox).Delete([]byte(key))
	})
}

// ── Transfer History ─────────────────────────────────────────────────

// RecordTransfer stores a completed transfer in persistent history.
func (s *StateDB) RecordTransfer(entry TransferHistoryEntry) error {
	if entry.DoneAt == 0 {
		entry.DoneAt = time.Now().UnixNano()
	}
	// Use timestamp-based key for chronological ordering.
	key := fmt.Sprintf("%020d_%s", entry.DoneAt, entry.ID)
	return s.put(bucketTransferHistory, key, entry)
}

// ListTransferHistory returns all transfer history entries (oldest first).
func (s *StateDB) ListTransferHistory() ([]TransferHistoryEntry, error) {
	var entries []TransferHistoryEntry
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketTransferHistory)
		return b.ForEach(func(k, v []byte) error {
			var e TransferHistoryEntry
			if err := json.Unmarshal(v, &e); err != nil {
				return err
			}
			entries = append(entries, e)
			return nil
		})
	})
	return entries, err
}

// MigrateZeroSizes scans download and transfer history records with Size==0
// and backfills them using the provided getSize callback (which should look up
// the manifest). Returns the number of records fixed. Idempotent.
func (s *StateDB) MigrateZeroSizes(getSize func(key string, isDir bool) int64) (int, error) {
	fixed := 0

	// Fix download entries.
	downloads, err := s.ListDownloads()
	if err != nil {
		return 0, fmt.Errorf("migrate: list downloads: %w", err)
	}
	for _, d := range downloads {
		if d.Size != 0 {
			continue
		}
		newSize := getSize(d.Key, d.IsDir)
		if newSize > 0 {
			d.Size = newSize
			if err := s.RecordDownload(d); err == nil {
				fixed++
			}
		}
	}

	// Fix transfer history entries (downloads only).
	history, err := s.ListTransferHistory()
	if err != nil {
		return fixed, fmt.Errorf("migrate: list transfer history: %w", err)
	}
	for _, h := range history {
		if h.Size != 0 || h.Direction != 1 {
			continue
		}
		newSize := getSize(h.Key, h.IsDir)
		if newSize > 0 {
			h.Size = newSize
			if err := s.RecordTransfer(h); err == nil {
				fixed++
			}
		}
	}

	return fixed, nil
}

// ── Outbox (offline send queue) ───────────────────────────────────────

// OutboxEntry records a direct-share notification queued for a peer that
// was offline at send time. Keyed by recipient fingerprint so delivery
// works even if the peer reconnects with a different address.
type OutboxEntry struct {
	ID            string `json:"id"`            // "<recipientFP>/<fileKey>/<nanoTimestamp>"
	RecipientFP   string `json:"recipientFP"`   // fingerprint of target peer
	RecipientHint string `json:"recipientHint"` // last known addr or alias (for logging)
	FileKey       string `json:"fileKey"`
	FileName      string `json:"fileName"`
	FileSize      int64  `json:"fileSize"`
	IsDir         bool   `json:"isDir"`
	CreatedAt     int64  `json:"createdAt"`
}

// AddOutboxEntry queues a direct-share notification for later delivery.
func (s *StateDB) AddOutboxEntry(entry OutboxEntry) error {
	if entry.CreatedAt == 0 {
		entry.CreatedAt = time.Now().UnixNano()
	}
	return s.put(bucketOutbox, entry.ID, entry)
}

// ListOutboxForPeer returns all pending outbox entries for a given fingerprint.
func (s *StateDB) ListOutboxForPeer(recipientFP string) ([]OutboxEntry, error) {
	var entries []OutboxEntry
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketOutbox)
		return b.ForEach(func(k, v []byte) error {
			var e OutboxEntry
			if err := json.Unmarshal(v, &e); err != nil {
				return err
			}
			if e.RecipientFP == recipientFP {
				entries = append(entries, e)
			}
			return nil
		})
	})
	return entries, err
}

// RemoveOutboxEntry deletes a delivered outbox entry by ID.
func (s *StateDB) RemoveOutboxEntry(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketOutbox).Delete([]byte(id))
	})
}

// ListOutbox returns all pending outbox entries.
func (s *StateDB) ListOutbox() ([]OutboxEntry, error) {
	var entries []OutboxEntry
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketOutbox)
		return b.ForEach(func(k, v []byte) error {
			var e OutboxEntry
			if err := json.Unmarshal(v, &e); err != nil {
				return err
			}
			entries = append(entries, e)
			return nil
		})
	})
	return entries, err
}

// put is a helper that JSON-encodes val and stores it under key in the given bucket.
func (s *StateDB) put(bucket []byte, key string, val interface{}) error {
	data, err := json.Marshal(val)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucket).Put([]byte(key), data)
	})
}
