package rebalance

import (
	"log"
	"sync"

	"github.com/Faizan2005/DFS-Go/Cluster/hashring"
	storage "github.com/Faizan2005/DFS-Go/Storage"
)

// SendFileFunc is a closure provided by server.go that sends an already-encrypted
// file (identified by key) to the given target address.
// encKey is the hex-encoded per-file encrypted key.
// data is the raw encrypted bytes.
type SendFileFunc func(targetAddr, key, encKey string, data []byte) error

// ReadFileFunc reads an encrypted file from local storage.
// Returns the hex-encoded encrypted key and the raw encrypted bytes.
type ReadFileFunc func(key string) (encKey string, data []byte, err error)

// Rebalancer migrates data when the hash ring topology changes:
//   - On node join:  keys the new node should own are copied to it.
//   - On node leave: keys that now have fewer than N replicas are re-replicated.
type Rebalancer struct {
	selfAddr string
	ring     *hashring.HashRing
	meta     storage.MetadataStore
	readFile ReadFileFunc
	sendFile SendFileFunc

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
		selfAddr: selfAddr,
		ring:     ring,
		meta:     meta,
		readFile: readFile,
		sendFile: sendFile,
		stopCh:   make(chan struct{}),
	}
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

		encKey, data, err := r.readFile(key)
		if err != nil {
			log.Printf("[rebalance] failed to read key=%s for migration: %v", key, err)
			continue
		}

		if err := r.sendFile(newAddr, key, encKey, data); err != nil {
			log.Printf("[rebalance] failed to send key=%s to %s: %v", key, newAddr, err)
			continue
		}

		migrated++
		log.Printf("[rebalance] migrated key=%s to %s", key, newAddr)
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

		encKey, data, err := r.readFile(key)
		if err != nil {
			log.Printf("[rebalance] failed to read key=%s for rereplicate: %v", key, err)
			continue
		}

		// Send to each node in the responsible set that we haven't already sent to.
		for _, target := range responsible {
			if target == r.selfAddr || target == deadAddr {
				continue
			}
			if err := r.sendFile(target, key, encKey, data); err != nil {
				log.Printf("[rebalance] failed to rereplicate key=%s to %s: %v", key, target, err)
				continue
			}
			replicated++
			log.Printf("[rebalance] rereplicated key=%s to %s", key, target)
		}
	}

	log.Printf("[rebalance] rereplicate after %s complete: %d keys re-sent", deadAddr, replicated)
}

// localKeys returns all file keys stored locally by iterating the metadata store.
// This relies on MetaFile.Keys() which is added to storage.go in this sprint.
func (r *Rebalancer) localKeys() []string {
	type keyer interface {
		Keys() []string
	}
	if k, ok := r.meta.(keyer); ok {
		return k.Keys()
	}
	return nil
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
