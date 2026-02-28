// Package conflict provides strategies for resolving conflicting versions of
// the same key when vector clock comparison returns Concurrent.
//
// The default strategy is Last-Write-Wins (LWW): among concurrent versions the
// one with the highest wall-clock Timestamp wins. When clocks are causally
// ordered the causally later version always wins regardless of wall time.
package conflict

import (
	"github.com/Faizan2005/DFS-Go/Cluster/vclock"
)

// Version is a single candidate version of a key, carrying the metadata needed
// to compare it against other candidates.
type Version struct {
	// NodeAddr identifies which node produced this version (for logging).
	NodeAddr string
	// Clock is the vector clock at the time of the write.
	Clock vclock.VectorClock
	// Timestamp is the wall-clock time (UnixNano) of the write. Used as a
	// tiebreaker when two versions have Concurrent vector clocks.
	Timestamp int64
	// EncryptedKey is the per-file AES key, hex-encoded.
	EncryptedKey string
}

// Resolver picks the authoritative version from a set of candidates.
type Resolver interface {
	Resolve(versions []Version) Version
}

// LWWResolver implements Last-Write-Wins conflict resolution.
//
// Decision rules in priority order:
//  1. Causally later version (vector clock After) always wins.
//  2. If two versions are Concurrent, higher Timestamp wins.
//  3. On an exact Timestamp tie, the first candidate in the input slice wins
//     (deterministic, caller controls ordering).
type LWWResolver struct{}

// NewLWWResolver returns a ready-to-use LWWResolver.
func NewLWWResolver() *LWWResolver { return &LWWResolver{} }

// Resolve returns the single winning Version from versions.
// Panics if versions is empty (caller's responsibility to check).
func (r *LWWResolver) Resolve(versions []Version) Version {
	if len(versions) == 0 {
		panic("conflict.LWWResolver.Resolve: called with empty versions slice")
	}
	best := versions[0]
	for i := 1; i < len(versions); i++ {
		candidate := versions[i]
		rel := best.Clock.Compare(candidate.Clock)
		switch rel {
		case vclock.Before:
			// candidate is causally after best — candidate wins.
			best = candidate
		case vclock.After:
			// best is causally after candidate — best stays.
		case vclock.Equal:
			// Identical clocks — keep first (no tiebreak needed).
		case vclock.Concurrent:
			// No causal order — use wall-clock tiebreak.
			if candidate.Timestamp > best.Timestamp {
				best = candidate
			}
			// On exact tie, keep best (first-writer wins).
		}
	}
	return best
}
