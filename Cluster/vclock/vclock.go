// Package vclock implements vector clocks for tracking causal relationships
// between writes across distributed nodes.
//
// A VectorClock is a map[nodeAddr]uint64. Every time a node writes a key it
// increments its own counter. When a node receives a write from a peer it
// merges that write's clock into its own (taking the per-node maximum). This
// gives us enough information to determine whether two versions are causally
// ordered or genuinely concurrent.
package vclock

import (
	"bytes"
	"encoding/gob"
)

// VectorClock maps node addresses to logical timestamps.
// The zero value (nil map) is treated as an empty clock in all operations.
type VectorClock map[string]uint64

// Relation describes how two vector clocks relate causally.
type Relation int

const (
	Before     Relation = iota // this clock happened before other
	After                      // this clock happened after other
	Concurrent                 // neither dominates — genuine conflict
	Equal                      // identical clocks
)

func (r Relation) String() string {
	switch r {
	case Before:
		return "Before"
	case After:
		return "After"
	case Concurrent:
		return "Concurrent"
	case Equal:
		return "Equal"
	default:
		return "Unknown"
	}
}

// New returns an empty VectorClock.
func New() VectorClock {
	return VectorClock{}
}

// Increment returns a new clock with nodeAddr's counter incremented by 1.
// The original clock is never mutated.
func (vc VectorClock) Increment(nodeAddr string) VectorClock {
	c := vc.Copy()
	c[nodeAddr]++
	return c
}

// Merge returns a new clock containing the per-node maximum of vc and other.
// Neither input clock is mutated.
func (vc VectorClock) Merge(other VectorClock) VectorClock {
	result := vc.Copy()
	for k, v := range other {
		if v > result[k] {
			result[k] = v
		}
	}
	return result
}

// Compare determines the causal relationship between vc and other.
//
//   - Before:     vc is causally before other (every entry in vc ≤ other, at least one strictly less)
//   - After:      vc is causally after  other (every entry in vc ≥ other, at least one strictly greater)
//   - Concurrent: neither dominates — a genuine conflict
//   - Equal:      all entries are identical
func (vc VectorClock) Compare(other VectorClock) Relation {
	// Collect all keys from both clocks.
	keys := make(map[string]struct{})
	for k := range vc {
		keys[k] = struct{}{}
	}
	for k := range other {
		keys[k] = struct{}{}
	}

	thisGreater := false
	otherGreater := false

	for k := range keys {
		a := vc[k]
		b := other[k]
		if a > b {
			thisGreater = true
		} else if b > a {
			otherGreater = true
		}
		// If both flags are set we can short-circuit.
		if thisGreater && otherGreater {
			return Concurrent
		}
	}

	switch {
	case !thisGreater && !otherGreater:
		return Equal
	case thisGreater:
		return After
	default:
		return Before
	}
}

// Copy returns a deep copy of the clock.
func (vc VectorClock) Copy() VectorClock {
	c := make(VectorClock, len(vc))
	for k, v := range vc {
		c[k] = v
	}
	return c
}

// Encode serialises the clock to a byte slice using gob encoding.
func (vc VectorClock) Encode() ([]byte, error) {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(map[string]uint64(vc)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Decode deserialises a clock that was produced by Encode.
func Decode(data []byte) (VectorClock, error) {
	var m map[string]uint64
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&m); err != nil {
		return nil, err
	}
	return VectorClock(m), nil
}
