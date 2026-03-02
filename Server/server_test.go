package Server

import "testing"

// TestWriteQuorum verifies the quorum threshold function used by replicateChunk.
// For n<=2 we must reach all replicas; for n>=3 we need a strict majority.
func TestWriteQuorum(t *testing.T) {
	s := &Server{} // zero value is sufficient — writeQuorum has no dependencies

	cases := []struct {
		n    int
		want int
	}{
		{0, 0},
		{1, 1},
		{2, 2},
		{3, 2},
		{4, 3},
		{5, 3},
		{6, 4},
		{7, 4},
	}
	for _, tc := range cases {
		got := s.writeQuorum(tc.n)
		if got != tc.want {
			t.Errorf("writeQuorum(%d) = %d, want %d", tc.n, got, tc.want)
		}
	}
}
