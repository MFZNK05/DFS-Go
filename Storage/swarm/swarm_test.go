package swarm

import "testing"

func TestNewBitfield(t *testing.T) {
	bf := NewBitfield(0)
	if len(bf) != 0 {
		t.Fatalf("expected 0 bytes, got %d", len(bf))
	}
	bf = NewBitfield(1)
	if len(bf) != 1 {
		t.Fatalf("expected 1 byte, got %d", len(bf))
	}
	bf = NewBitfield(8)
	if len(bf) != 1 {
		t.Fatalf("expected 1 byte, got %d", len(bf))
	}
	bf = NewBitfield(9)
	if len(bf) != 2 {
		t.Fatalf("expected 2 bytes, got %d", len(bf))
	}
}

func TestNewFullBitfield(t *testing.T) {
	bf := NewFullBitfield(10)
	if len(bf) != 2 {
		t.Fatalf("expected 2 bytes, got %d", len(bf))
	}
	for i := 0; i < 10; i++ {
		if !HasBit(bf, i) {
			t.Fatalf("expected bit %d to be set", i)
		}
	}
	// Trailing bits beyond 10 should be clear.
	if HasBit(bf, 10) {
		t.Fatal("bit 10 should not be set")
	}
	if HasBit(bf, 15) {
		t.Fatal("bit 15 should not be set")
	}
	if CountBits(bf) != 10 {
		t.Fatalf("expected 10 bits set, got %d", CountBits(bf))
	}
}

func TestSetHasClearBit(t *testing.T) {
	bf := NewBitfield(16)

	SetBit(bf, 0)
	SetBit(bf, 7)
	SetBit(bf, 8)
	SetBit(bf, 15)

	if !HasBit(bf, 0) {
		t.Fatal("bit 0 should be set")
	}
	if !HasBit(bf, 7) {
		t.Fatal("bit 7 should be set")
	}
	if !HasBit(bf, 8) {
		t.Fatal("bit 8 should be set")
	}
	if !HasBit(bf, 15) {
		t.Fatal("bit 15 should be set")
	}
	if HasBit(bf, 1) {
		t.Fatal("bit 1 should not be set")
	}

	ClearBit(bf, 7)
	if HasBit(bf, 7) {
		t.Fatal("bit 7 should be cleared")
	}
	if CountBits(bf) != 3 {
		t.Fatalf("expected 3 bits set, got %d", CountBits(bf))
	}
}

func TestBoundsChecks(t *testing.T) {
	bf := NewBitfield(8)

	// Out-of-range should not panic.
	SetBit(bf, -1)
	SetBit(bf, 100)
	ClearBit(bf, -1)
	ClearBit(bf, 100)
	if HasBit(bf, -1) {
		t.Fatal("out-of-range should return false")
	}
	if HasBit(bf, 100) {
		t.Fatal("out-of-range should return false")
	}
}

func TestAllSet(t *testing.T) {
	bf := NewFullBitfield(5)
	if !AllSet(bf, 5) {
		t.Fatal("all 5 bits should be set")
	}

	ClearBit(bf, 3)
	if AllSet(bf, 5) {
		t.Fatal("should not be all set after clearing bit 3")
	}

	// Edge case: empty.
	if !AllSet(NewBitfield(0), 0) {
		t.Fatal("empty bitfield with 0 chunks should be AllSet")
	}
	if AllSet(nil, 1) {
		t.Fatal("nil bitfield with 1 chunk should not be AllSet")
	}
}

func TestFullBitfieldExact8(t *testing.T) {
	// Exactly 8 bits: all should be set, no trailing-bit issue.
	bf := NewFullBitfield(8)
	if CountBits(bf) != 8 {
		t.Fatalf("expected 8 bits, got %d", CountBits(bf))
	}
	if bf[0] != 0xFF {
		t.Fatalf("expected 0xFF, got 0x%02X", bf[0])
	}
}
