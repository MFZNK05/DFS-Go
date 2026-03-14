package rebalance

import (
	"log"
	"strings"
	"sync"
	"time"

	"github.com/Faizan2005/DFS-Go/Cluster/hashring"
	storage "github.com/Faizan2005/DFS-Go/Storage"
)

// DefaultMigrationDelay is the pause between consecutive chunk migrations.
// This prevents rebalance from saturating the peer connection during active uploads.
// At 50ms, 466 chunks take ~23s total but each individual migration is interleaved
// with upload replication rather than monopolising the connection in bursts.
const DefaultMigrationDelay = 50 * time.Millisecond

// SendFileFunc is a closure provided by server.go that sends a file
// (identified by key) to the given target address.
// data is the raw (possibly encrypted) bytes.
type SendFileFunc func(targetAddr, key string, data []byte) error

// ReadFileFunc reads a file from local storage.
// Returns the raw bytes (possibly encrypted — opaque to rebalancer).
type ReadFileFunc func(key string) (data []byte, err error)

// SendManifestFunc sends a ChunkManifest (already serialised as JSON) to a peer.
// fileKey is the original file key (not the "manifest:" prefixed key).
type SendManifestFunc func(targetAddr, fileKey string, manifestJSON []byte) error

// ReadManifestFunc reads the JSON-serialised ChunkManifest for a chunked file.
// Returns (nil, false) when the manifest is not held locally.
type ReadManifestFunc func(fileKey string) (manifestJSON []byte, ok bool)

// ShouldSkipKeyFunc returns true if a key should be skipped during migration.
// Used to prevent rebalancing CAS data for SeedMode files (swarm handles payload).
type ShouldSkipKeyFunc func(key string) bool

// Rebalancer migrates data when the hash ring topology changes:
//   - On node join:  keys the new node should own are copied to it.
//   - On node leave: keys that now have fewer than N replicas are re-replicated.
type Rebalancer struct {
	selfAddr     string
	ring         *hashring.HashRing
	meta         storage.MetadataStore
	readFile     ReadFileFunc
	sendFile     SendFileFunc
	readManifest ReadManifestFunc
	sendManifest SendManifestFunc
	shouldSkip   ShouldSkipKeyFunc

	// MigrationDelay is the pause inserted between consecutive chunk migrations.
	// Zero means no delay (full speed). Defaults to DefaultMigrationDelay.
	MigrationDelay time.Duration

	mu         sync.Mutex
	inProgress bool

	stopCh chan struct{}
}

// New creates a Rebalancer. readFile and sendFile are closures wired by server.go.
func New(
	selfAddr string,
	ring *hashring.HashRing,
	meta storage.MetadataStore,
	readFile ReadFileFunc,
	sendFile SendFileFunc,
) *Rebalancer {
	return &Rebalancer{
		selfAddr:       selfAddr,
		ring:           ring,
		meta:           meta,
		readFile:       readFile,
		sendFile:       sendFile,
		MigrationDelay: DefaultMigrationDelay,
		stopCh:         make(chan struct{}),
	}
}

// SetManifestFuncs wires optional manifest read/send closures.
// Call this after New() before the first OnNodeJoined fires.
func (r *Rebalancer) SetManifestFuncs(read ReadManifestFunc, send SendManifestFunc) {
	r.readManifest = read
	r.sendManifest = send
}

// SetShouldSkipKey wires a filter that excludes keys from migration/rereplicate.
// Used to prevent background rebalancing of chunk: keys (swarm handles payload).
func (r *Rebalancer) SetShouldSkipKey(fn ShouldSkipKeyFunc) {
	r.shouldSkip = fn
}

// OnNodeJoined triggers key migration when a new node has been added to the ring.
// It runs asynchronously so it doesn't block OnPeer.
func (r *Rebalancer) OnNodeJoined(newAddr string) {
	go r.migrateToNewNode(newAddr)
}

// OnNodeLeft triggers re-replication when a node has been removed from the ring.
// It runs asynchronously.
func (r *Rebalancer) OnNodeLeft(deadAddr string) {
	go r.rereplicate(deadAddr)
}

// migrateToNewNode scans all locally-held keys.
// For each key where newAddr is now a responsible replica, it sends a copy there.
// Manifest keys ("manifest:*") are sent via sendManifest when available.
func (r *Rebalancer) migrateToNewNode(newAddr string) {
	r.mu.Lock()
	if r.inProgress {
		r.mu.Unlock()
		log.Printf("[rebalance] rebalance already in progress, skipping migration to %s", newAddr)
		return
	}
	r.inProgress = true
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		r.inProgress = false
		r.mu.Unlock()
	}()

	keys := r.localKeys()
	if len(keys) == 0 {
		return
	}

	log.Printf("[rebalance] node joined %s — checking %d local keys for migration", newAddr, len(keys))

	migrated := 0
	for _, key := range keys {
		responsible := r.ring.GetNodes(key, r.ring.ReplicationFactor())

		if !contains(responsible, newAddr) {
			continue // new node is not responsible for this key
		}
		if !contains(responsible, r.selfAddr) {
			continue // we are not responsible either — not our job to migrate
		}

		// Manifest keys are metadata-only (no CAS file). Send via dedicated path.
		if strings.HasPrefix(key, "manifest:") {
			fileKey := strings.TrimPrefix(key, "manifest:")
			if r.readManifest != nil && r.sendManifest != nil {
				if data, ok := r.readManifest(fileKey); ok {
					if err := r.sendManifest(newAddr, fileKey, data); err != nil {
						log.Printf("[rebalance] failed to send manifest key=%s to %s: %v", fileKey, newAddr, err)
					} else {
						migrated++
						log.Printf("[rebalance] migrated manifest key=%s to %s", fileKey, newAddr)
					}
				}
			}
			continue
		}

		data, err := r.readFile(key)
		if err != nil {
			log.Printf("[rebalance] failed to read key=%s for migration: %v", key, err)
			continue
		}

		if err := r.sendFile(newAddr, key, data); err != nil {
			log.Printf("[rebalance] failed to send key=%s to %s: %v", key, newAddr, err)
			continue
		}

		migrated++
		log.Printf("[rebalance] migrated key=%s to %s", key, newAddr)

		// Throttle: yield the connection so active uploads can interleave.
		if r.MigrationDelay > 0 {
			select {
			case <-r.stopCh:
				log.Printf("[rebalance] migration to %s interrupted at %d/%d keys", newAddr, migrated, len(keys))
				return
			case <-time.After(r.MigrationDelay):
			}
		}
	}

	log.Printf("[rebalance] migration to %s complete: %d/%d keys sent", newAddr, migrated, len(keys))
}

// rereplicate scans local keys and restores the replication factor for any key
// that was stored on deadAddr (and therefore now has fewer than N live copies).
func (r *Rebalancer) rereplicate(deadAddr string) {
	r.mu.Lock()
	if r.inProgress {
		r.mu.Unlock()
		log.Printf("[rebalance] rebalance already in progress, skipping rereplicate for %s", deadAddr)
		return
	}
	r.inProgress = true
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		r.inProgress = false
		r.mu.Unlock()
	}()

	keys := r.localKeys()
	if len(keys) == 0 {
		return
	}

	log.Printf("[rebalance] node left %s — checking %d local keys for re-replication", deadAddr, len(keys))

	replicated := 0
	n := r.ring.ReplicationFactor()

	for _, key := range keys {
		// After removing deadAddr from the ring, GetNodes returns the new responsible set.
		// If its size is < N, we need to send to additional nodes.
		responsible := r.ring.GetNodes(key, n)

		if len(responsible) >= n {
			continue // enough replicas remain
		}

		// We only act if self is currently responsible for this key.
		if !contains(responsible, r.selfAddr) {
			continue
		}

		data, err := r.readFile(key)
		if err != nil {
			log.Printf("[rebalance] failed to read key=%s for rereplicate: %v", key, err)
			continue
		}

		// Send to each node in the responsible set that we haven't already sent to.
		for _, target := range responsible {
			if target == r.selfAddr || target == deadAddr {
				continue
			}
			if err := r.sendFile(target, key, data); err != nil {
				log.Printf("[rebalance] failed to rereplicate key=%s to %s: %v", key, target, err)
				continue
			}
			replicated++
			log.Printf("[rebalance] rereplicated key=%s to %s", key, target)
		}
	}

	log.Printf("[rebalance] rereplicate after %s complete: %d keys re-sent", deadAddr, replicated)
}

// localKeys returns all file keys stored locally by iterating the metadata store,
// excluding any keys that shouldSkip returns true for (e.g., chunk: keys that
// are managed by swarm on-demand serving or upload-time replication).
func (r *Rebalancer) localKeys() []string {
	type keyer interface {
		Keys() []string
	}
	k, ok := r.meta.(keyer)
	if !ok {
		return nil
	}
	all := k.Keys()
	if r.shouldSkip == nil {
		return all
	}
	filtered := make([]string, 0, len(all))
	for _, key := range all {
		if !r.shouldSkip(key) {
			filtered = append(filtered, key)
		}
	}
	return filtered
}

// contains reports whether slice s contains elem.
func contains(s []string, elem string) bool {
	for _, v := range s {
		if v == elem {
			return true
		}
	}
	return false
}
