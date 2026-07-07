package topk

import (
	"sort"
	"testing"
)

// TestHeap_EmptyAndZeroKNoPanic pins the #429-review fix: Better must not index
// items[0] on an empty heap. A k=0 heap is trivially Full() (0 >= 0), so a
// Full()-guarded caller would otherwise call Better on empty items and panic.
func TestHeap_EmptyAndZeroKNoPanic(t *testing.T) {
	// k=0: Full() true, but the heap retains nothing and Better must be false.
	z := New(0)
	if !z.Full() {
		t.Fatalf("k=0 heap should report Full()")
	}
	if z.Better(9.9, 1) {
		t.Fatalf("k=0 heap: Better must be false, not panic")
	}
	z.Push(Candidate{ChunkID: 1, Score: 9.9})
	if len(z.Items()) != 0 {
		t.Fatalf("k=0 heap must retain nothing, got %d", len(z.Items()))
	}

	// Non-zero k but no pushes yet: Better on the empty heap must be false.
	h := New(3)
	if h.Full() {
		t.Fatalf("empty k=3 heap should not be Full()")
	}
	if h.Better(1.0, 1) {
		t.Fatalf("empty heap: Better must be false, not panic")
	}
}

// TestHeap_RetainsBestK pins that the bounded heap keeps exactly the top-k by the
// Before comparator, matching a reference full sort.
func TestHeap_RetainsBestK(t *testing.T) {
	in := []Candidate{
		{ChunkID: 1, Score: 0.10}, {ChunkID: 2, Score: 0.90},
		{ChunkID: 3, Score: 0.50}, {ChunkID: 4, Score: 0.70},
		{ChunkID: 5, Score: 0.30},
	}
	h := New(3)
	for _, c := range in {
		if !h.Full() || h.Better(c.Score, c.ChunkID) {
			h.Push(c)
		}
	}
	got := append([]Candidate(nil), h.Items()...)
	sort.Slice(got, func(i, j int) bool {
		return Before(got[i].Score, got[j].Score, got[i].ChunkID, got[j].ChunkID)
	})
	want := []uint64{2, 4, 3} // top-3 by score
	if len(got) != len(want) {
		t.Fatalf("kept %d, want %d: %+v", len(got), len(want), got)
	}
	for i, id := range want {
		if got[i].ChunkID != id {
			t.Fatalf("rank %d = chunk %d, want %d (%+v)", i, got[i].ChunkID, id, got)
		}
	}
}
