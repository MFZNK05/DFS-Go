package handoff

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	DefaultMaxHints = 1000
	DefaultMaxAge   = 24 * time.Hour
)

// Hint represents a pending write that could not be delivered to its intended
// target node because the target was unreachable at write time.
// The data stored here is already encrypted — safe to persist to disk.
type Hint struct {
	Key          string    `json:"key"`
	TargetAddr   string    `json:"target_addr"`
	EncryptedKey string    `json:"encrypted_key"` // hex-encoded per-file AES key
	Data         []byte    `json:"data"`          // encrypted file bytes
	CreatedAt    time.Time `json:"created_at"`
	Attempts     int       `json:"attempts"`
}

// Store persists hints to disk so they survive process restarts.
// Hints are keyed by target address and stored as JSON files.
type Store struct {
	mu       sync.Mutex
	hints    map[string][]Hint // targetAddr → pending hints
	dir      string            // directory for hint JSON files
	maxHints int               // per-target cap
	maxAge   time.Duration     // discard hints older than this
}

// NewStore creates (or loads) a hint store rooted at dir.
// Expired hints are purged on load.
func NewStore(dir string, maxHints int, maxAge time.Duration) (*Store, error) {
	if maxHints <= 0 {
		maxHints = DefaultMaxHints
	}
	if maxAge <= 0 {
		maxAge = DefaultMaxAge
	}

	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("handoff: create hint dir %s: %w", dir, err)
	}

	s := &Store{
		hints:    make(map[string][]Hint),
		dir:      dir,
		maxHints: maxHints,
		maxAge:   maxAge,
	}

	if err := s.load(); err != nil {
		return nil, err
	}

	s.purgeExpiredLocked()
	return s, nil
}

// AddHint stores a hint for later delivery.
// If the per-target cap is exceeded the oldest hint is evicted.
func (s *Store) AddHint(h Hint) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	hints := s.hints[h.TargetAddr]

	if len(hints) >= s.maxHints {
		// Evict the oldest (index 0 after chronological ordering).
		log.Printf("[handoff] hint cap reached for %s, evicting oldest hint (key=%s)", h.TargetAddr, hints[0].Key)
		hints = hints[1:]
	}

	hints = append(hints, h)
	s.hints[h.TargetAddr] = hints

	return s.persistLocked(h.TargetAddr)
}

// GetHints returns a copy of all pending hints for targetAddr.
func (s *Store) GetHints(targetAddr string) []Hint {
	s.mu.Lock()
	defer s.mu.Unlock()

	src := s.hints[targetAddr]
	if len(src) == 0 {
		return nil
	}
	cp := make([]Hint, len(src))
	copy(cp, src)
	return cp
}

// DeleteHint removes the hint with the given key for targetAddr.
func (s *Store) DeleteHint(targetAddr, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	hints := s.hints[targetAddr]
	filtered := hints[:0]
	for _, h := range hints {
		if h.Key != key {
			filtered = append(filtered, h)
		}
	}
	s.hints[targetAddr] = filtered
	return s.persistLocked(targetAddr)
}

// UpdateHint replaces the stored hint for (targetAddr, key) with the new value.
// Used to increment the Attempts counter after a failed delivery.
func (s *Store) UpdateHint(h Hint) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	hints := s.hints[h.TargetAddr]
	for i, existing := range hints {
		if existing.Key == h.Key {
			hints[i] = h
			s.hints[h.TargetAddr] = hints
			return s.persistLocked(h.TargetAddr)
		}
	}
	return nil // not found — already delivered
}

// PurgeExpired removes all hints older than maxAge and persists the changes.
func (s *Store) PurgeExpired() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeExpiredLocked()
}

// HasPending reports whether there are any hints for the given address.
func (s *Store) HasPending(targetAddr string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.hints[targetAddr]) > 0
}

// --- internal helpers ---

// purgeExpiredLocked must be called with s.mu held.
func (s *Store) purgeExpiredLocked() {
	cutoff := time.Now().Add(-s.maxAge)
	for addr, hints := range s.hints {
		fresh := hints[:0]
		for _, h := range hints {
			if h.CreatedAt.After(cutoff) {
				fresh = append(fresh, h)
			}
		}
		if len(fresh) != len(hints) {
			s.hints[addr] = fresh
			_ = s.persistLocked(addr) // best-effort
		}
	}
}

// persistLocked writes hints for targetAddr to disk. Must be called with s.mu held.
func (s *Store) persistLocked(targetAddr string) error {
	filename := s.hintFile(targetAddr)
	hints := s.hints[targetAddr]

	if len(hints) == 0 {
		// Remove the file if no hints remain.
		_ = os.Remove(filename)
		return nil
	}

	data, err := json.Marshal(hints)
	if err != nil {
		return fmt.Errorf("handoff: marshal hints for %s: %w", targetAddr, err)
	}

	if err := os.WriteFile(filename, data, 0600); err != nil {
		return fmt.Errorf("handoff: write hints file %s: %w", filename, err)
	}
	return nil
}

// load reads all hint files from disk into memory. Called once on NewStore.
func (s *Store) load() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("handoff: read hint dir: %w", err)
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".hints.json") {
			continue
		}

		path := filepath.Join(s.dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			log.Printf("[handoff] failed to read hint file %s: %v (skipping)", path, err)
			continue
		}

		var hints []Hint
		if err := json.Unmarshal(data, &hints); err != nil {
			log.Printf("[handoff] failed to parse hint file %s: %v (skipping)", path, err)
			continue
		}

		if len(hints) == 0 {
			continue
		}

		// Use TargetAddr from the hint itself — avoids filename sanitisation issues.
		addr := hints[0].TargetAddr
		s.hints[addr] = hints
	}

	return nil
}

// hintFile returns the path to the hint file for a given target address.
func (s *Store) hintFile(targetAddr string) string {
	return filepath.Join(s.dir, sanitiseAddr(targetAddr)+".hints.json")
}

// sanitiseAddr replaces characters that are invalid in filenames.
func sanitiseAddr(addr string) string {
	r := strings.NewReplacer(":", "_", "/", "_", ".", "_")
	return r.Replace(addr)
}

// -------------------------------------------------------------------
// HandoffService watches for peer reconnections and delivers hints.
// -------------------------------------------------------------------

const maxDeliveryAttempts = 5

// DeliverFunc is called by HandoffService to deliver a hint to its target.
// It must send the encrypted data to targetAddr and return nil on success.
type DeliverFunc func(hint Hint) error

// HandoffService coordinates hint storage and delivery.
type HandoffService struct {
	store    *Store
	deliver  DeliverFunc
	stopCh   chan struct{}
	once     sync.Once
	stopOnce sync.Once
	// activeDeliveries prevents concurrent deliverPending calls for the same
	// target address. Without this, a rapid reconnect-disconnect cycle causes
	// two goroutines to read the same hint copies (pass-by-value), increment
	// Attempts independently, and overwrite each other's progress.
	activeDeliveries sync.Map // map[targetAddr]struct{}
}

// NewHandoffService creates a HandoffService backed by store.
func NewHandoffService(store *Store, deliver DeliverFunc) *HandoffService {
	return &HandoffService{
		store:   store,
		deliver: deliver,
		stopCh:  make(chan struct{}),
	}
}

// Start launches the background purge loop.
func (hs *HandoffService) Start() {
	hs.once.Do(func() {
		go hs.purgeLoop()
	})
}

// Stop shuts down the background goroutine.
func (hs *HandoffService) Stop() {
	hs.stopOnce.Do(func() {
		close(hs.stopCh)
	})
}

// OnPeerReconnect is called by server.OnPeer when a peer reconnects.
// It asynchronously delivers all pending hints for that address.
// Only one delivery loop runs per address at a time — rapid reconnect events
// are coalesced so concurrent goroutines don't overwrite each other's attempt counts.
func (hs *HandoffService) OnPeerReconnect(addr string) {
	if !hs.store.HasPending(addr) {
		return
	}
	if _, alreadyActive := hs.activeDeliveries.LoadOrStore(addr, struct{}{}); alreadyActive {
		log.Printf("[handoff] delivery already in progress for %s — skipping duplicate", addr)
		return
	}
	go func() {
		defer hs.activeDeliveries.Delete(addr)
		hs.deliverPending(addr)
	}()
}

// StoreHint records a hint for later delivery. Called from StoreData when a
// target node is unreachable.
func (hs *HandoffService) StoreHint(h Hint) {
	if err := hs.store.AddHint(h); err != nil {
		log.Printf("[handoff] failed to store hint for %s key=%s: %v", h.TargetAddr, h.Key, err)
		return
	}
	log.Printf("[handoff] stored hint for %s (key=%s)", h.TargetAddr, h.Key)
}

// deliverPending attempts to deliver all hints for addr in a background goroutine.
func (hs *HandoffService) deliverPending(addr string) {
	hints := hs.store.GetHints(addr)
	if len(hints) == 0 {
		return
	}

	log.Printf("[handoff] delivering %d pending hints to %s", len(hints), addr)

	for _, h := range hints {
		if err := hs.deliver(h); err != nil {
			log.Printf("[handoff] delivery failed for key=%s to %s (attempt %d): %v",
				h.Key, addr, h.Attempts+1, err)

			h.Attempts++
			if h.Attempts >= maxDeliveryAttempts {
				log.Printf("[handoff] discarding hint key=%s to %s after %d failed attempts",
					h.Key, addr, h.Attempts)
				_ = hs.store.DeleteHint(addr, h.Key)
			} else {
				_ = hs.store.UpdateHint(h)
			}
			continue
		}

		log.Printf("[handoff] delivered hint key=%s to %s successfully", h.Key, addr)
		_ = hs.store.DeleteHint(addr, h.Key)
	}
}

// purgeLoop runs every hour to evict expired hints.
func (hs *HandoffService) purgeLoop() {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			hs.store.PurgeExpired()
		case <-hs.stopCh:
			return
		}
	}
}
