package swarm

import (
	"log"
	"sort"
	"sync"
	"time"
)

// DefaultCacheLimit is the default maximum CAS cache size (50 GB).
const DefaultCacheLimit int64 = 50 * 1024 * 1024 * 1024

// CacheStore is the interface that CacheManager needs from StateDB.
// This avoids a circular dependency with the State package.
type CacheStore interface {
	SetCacheEntry(entry CacheEntry) error
	GetCacheEntry(fileKey string) *CacheEntry
	TouchCache(fileKey string)
	RemoveCacheEntry(fileKey string) error
	ListCacheEntries() ([]CacheEntry, error)
	TotalCacheSize() int64
	GetSeedSource(key string) *SeedSource
	RemoveSeedSource(key string) error
}

// CacheEntry tracks CAS cache usage for a file (canonical type lives here in swarm).
type CacheEntry struct {
	FileKey      string `json:"file_key"`
	Size         int64  `json:"size"`
	LastAccessed int64  `json:"last_accessed"`
}

// EvictFunc is called when a file's cached chunks should be removed from CAS.
// It receives the file key and should delete the CAS-stored chunks.
type EvictFunc func(fileKey string) error

// CacheManager tracks CAS cache usage and evicts LRU entries when the cache
// exceeds the configured limit. Only CAS-backed seed sources (secondary seeders)
// are evictable — original seeders with OriginalPath are never evicted.
type CacheManager struct {
	store    CacheStore
	limit    int64
	evict    EvictFunc
	stopCh   chan struct{}
	stopOnce sync.Once
}

// NewCacheManager creates a CacheManager with the given limit (bytes).
// If limit <= 0, DefaultCacheLimit is used.
func NewCacheManager(store CacheStore, limit int64, evict EvictFunc) *CacheManager {
	if limit <= 0 {
		limit = DefaultCacheLimit
	}
	return &CacheManager{
		store:  store,
		limit:  limit,
		evict:  evict,
		stopCh: make(chan struct{}),
	}
}

// Track records that a file's chunks are now cached in CAS.
func (cm *CacheManager) Track(fileKey string, size int64) {
	_ = cm.store.SetCacheEntry(CacheEntry{
		FileKey:      fileKey,
		Size:         size,
		LastAccessed: time.Now().UnixNano(),
	})
}

// Touch updates the last-accessed timestamp for a cached file.
func (cm *CacheManager) Touch(fileKey string) {
	cm.store.TouchCache(fileKey)
}

// RunEvictionLoop periodically checks cache usage and evicts LRU entries.
// Runs until Stop() is called. Typically launched as a goroutine.
func (cm *CacheManager) RunEvictionLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-cm.stopCh:
			return
		case <-ticker.C:
			cm.EvictIfNeeded()
		}
	}
}

// EvictIfNeeded checks total cache size and evicts LRU entries until under limit.
func (cm *CacheManager) EvictIfNeeded() {
	total := cm.store.TotalCacheSize()
	if total <= cm.limit {
		return
	}

	entries, err := cm.store.ListCacheEntries()
	if err != nil || len(entries) == 0 {
		return
	}

	// Sort by LastAccessed ascending (oldest first).
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].LastAccessed < entries[j].LastAccessed
	})

	for _, e := range entries {
		if total <= cm.limit {
			break
		}

		// Never evict original seeders (those with OriginalPath).
		src := cm.store.GetSeedSource(e.FileKey)
		if src != nil && src.OriginalPath != "" {
			continue
		}

		log.Printf("CACHE_EVICT: evicting %s (size=%d, last_accessed=%d)", e.FileKey, e.Size, e.LastAccessed)

		if cm.evict != nil {
			if err := cm.evict(e.FileKey); err != nil {
				log.Printf("CACHE_EVICT: failed to evict chunks for %s: %v", e.FileKey, err)
				continue
			}
		}

		// Remove cache entry and seed source.
		_ = cm.store.RemoveCacheEntry(e.FileKey)
		_ = cm.store.RemoveSeedSource(e.FileKey)
		total -= e.Size
	}
}

// StopCh returns the channel that is closed when Stop() is called.
// Useful for external goroutines that should stop alongside the CacheManager.
func (cm *CacheManager) StopCh() <-chan struct{} {
	return cm.stopCh
}

// Stop halts the eviction loop.
func (cm *CacheManager) Stop() {
	cm.stopOnce.Do(func() { close(cm.stopCh) })
}
