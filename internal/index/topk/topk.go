// Package topk provides a bounded top-k selection heap shared by the local
// vector backends (internal/index.HNSWIndex and internal/index/diskindex).
//
// Both backends previously scored every candidate and full-sorted the whole
// scored set before truncating to k — O(N log N) per query with a per-candidate
// allocation. This package replaces that with a bounded min-heap that retains
// only the running best k, ordered by the single ranking comparator Before, so
// each Search allocates O(k) not O(N) (issue #429 F1/F2). It lives in its own
// leaf package because internal/index imports internal/index/diskindex, so the
// shared code cannot live in either of those without an import cycle.
package topk

import (
	"math"

	"github.com/dirstral/dir2mcp/internal/model"
)

// ScoreEps is the score-equality tolerance for the ranking tiebreak: two scores
// within eps are treated as equal and ordered by ascending chunk_id, giving a
// deterministic total order over realistic (well-separated) embedding scores.
const ScoreEps = 1e-6

// Candidate pairs a chunk_id with its cosine score during search, carrying the
// payload so a retained hit can be materialised without a second lookup.
type Candidate struct {
	ChunkID uint64
	Score   float32
	Payload model.IndexPayload
}

// Before reports whether a candidate with (aScore, aID) ranks strictly before
// one with (bScore, bID): higher score first, with an eps-tolerant ascending
// chunk_id tiebreak. It is the sole ranking comparator, shared by the heap's
// eviction decision and the caller's final ordering of the retained hits, so the
// heap selects exactly the same k candidates the previous full O(N log N) sort
// would have kept (issue #429 F1).
func Before(aScore, bScore float32, aID, bID uint64) bool {
	if math.Abs(float64(aScore)-float64(bScore)) <= ScoreEps {
		return aID < bID
	}
	return aScore > bScore
}

// Heap is a bounded min-heap that retains the best k candidates seen so far,
// ordered by Before. The root is the *worst* retained candidate, so a newly
// scored candidate that ranks before the root evicts it in O(log k).
type Heap struct {
	items []Candidate
	k     int
}

// New returns an empty Heap that retains at most k candidates. A non-positive k
// yields a heap that accepts nothing.
func New(k int) *Heap {
	if k < 0 {
		k = 0
	}
	return &Heap{k: k, items: make([]Candidate, 0, k)}
}

// Items returns the retained candidates in heap (unordered) order. Callers sort
// them with Before to obtain the final best-first ranking.
func (h *Heap) Items() []Candidate { return h.items }

// Full reports whether the heap is at capacity.
func (h *Heap) Full() bool { return len(h.items) >= h.k }

// Better reports whether a candidate with (score, id) ranks before the current
// worst retained candidate (the root). An empty heap (including a k=0 heap, for
// which Full() is trivially true) has no worst element, so nothing is better than
// it — return false rather than indexing items[0].
func (h *Heap) Better(score float32, id uint64) bool {
	if len(h.items) == 0 {
		return false
	}
	root := h.items[0]
	return Before(score, root.Score, id, root.ChunkID)
}

// worse reports whether item a ranks after item b (a is the poorer match).
func (h *Heap) worse(a, b int) bool {
	x, y := h.items[a], h.items[b]
	return Before(y.Score, x.Score, y.ChunkID, x.ChunkID)
}

// Push adds c when the heap is under capacity, otherwise replaces the worst
// retained candidate. Callers must only push a candidate that is under capacity
// or better than the root (guard with Full/Better).
func (h *Heap) Push(c Candidate) {
	if h.k == 0 {
		return
	}
	if len(h.items) < h.k {
		h.items = append(h.items, c)
		h.siftUp(len(h.items) - 1)
		return
	}
	h.items[0] = c
	h.siftDown(0)
}

func (h *Heap) siftUp(n int) {
	for n > 0 {
		parent := (n - 1) / 2
		if !h.worse(n, parent) {
			break
		}
		h.items[n], h.items[parent] = h.items[parent], h.items[n]
		n = parent
	}
}

func (h *Heap) siftDown(n int) {
	size := len(h.items)
	for {
		left := 2*n + 1
		if left >= size {
			return
		}
		worst := left
		if right := left + 1; right < size && h.worse(right, left) {
			worst = right
		}
		if !h.worse(worst, n) {
			return
		}
		h.items[n], h.items[worst] = h.items[worst], h.items[n]
		n = worst
	}
}
