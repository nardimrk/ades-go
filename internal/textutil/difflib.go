// Package textutil holds the text heuristics shared by the collector and the
// dashboard: similarity, campaign keys, option/order parsing, listing fields
// and WhatsApp .txt export parsing.
package textutil

import "sort"

// Ratio is a faithful port of Python's difflib.SequenceMatcher(None, a, b).ratio()
// over Unicode code points, including the "autojunk" heuristic (for b of 200+
// elements, elements occurring more than 1% of the time are treated as
// popular). The classification thresholds were tuned against Python's exact
// numbers, so this must not be replaced by a different similarity metric.
func Ratio(a, b string) float64 {
	ra, rb := []rune(a), []rune(b)
	la, lb := len(ra), len(rb)
	if la+lb == 0 {
		return 1.0
	}
	m := newMatcher(ra, rb)
	matches := 0
	for _, blk := range m.matchingBlocks() {
		matches += blk.size
	}
	return 2.0 * float64(matches) / float64(la+lb)
}

type match struct{ a, b, size int }

type matcher struct {
	a, b []rune
	b2j  map[rune][]int
}

func newMatcher(a, b []rune) *matcher {
	m := &matcher{a: a, b: b, b2j: map[rune][]int{}}
	for j, r := range b {
		m.b2j[r] = append(m.b2j[r], j)
	}
	if n := len(b); n >= 200 {
		ntest := n/100 + 1
		for r, idx := range m.b2j {
			if len(idx) > ntest {
				delete(m.b2j, r)
			}
		}
	}
	return m
}

func (m *matcher) findLongestMatch(alo, ahi, blo, bhi int) match {
	a, b := m.a, m.b
	besti, bestj, bestsize := alo, blo, 0
	j2len := map[int]int{}
	for i := alo; i < ahi; i++ {
		newj2len := map[int]int{}
		for _, j := range m.b2j[a[i]] {
			if j < blo {
				continue
			}
			if j >= bhi {
				break
			}
			k := j2len[j-1] + 1
			newj2len[j] = k
			if k > bestsize {
				besti, bestj, bestsize = i-k+1, j-k+1, k
			}
		}
		j2len = newj2len
	}
	// With isjunk=None there are no junk elements, so only these two
	// extensions apply (they absorb "popular" elements next to the match).
	for besti > alo && bestj > blo && a[besti-1] == b[bestj-1] {
		besti, bestj, bestsize = besti-1, bestj-1, bestsize+1
	}
	for besti+bestsize < ahi && bestj+bestsize < bhi && a[besti+bestsize] == b[bestj+bestsize] {
		bestsize++
	}
	return match{besti, bestj, bestsize}
}

func (m *matcher) matchingBlocks() []match {
	la, lb := len(m.a), len(m.b)
	type span struct{ alo, ahi, blo, bhi int }
	queue := []span{{0, la, 0, lb}}
	var blocks []match
	for len(queue) > 0 {
		q := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		x := m.findLongestMatch(q.alo, q.ahi, q.blo, q.bhi)
		if x.size == 0 {
			continue
		}
		blocks = append(blocks, x)
		if q.alo < x.a && q.blo < x.b {
			queue = append(queue, span{q.alo, x.a, q.blo, x.b})
		}
		if x.a+x.size < q.ahi && x.b+x.size < q.bhi {
			queue = append(queue, span{x.a + x.size, q.ahi, x.b + x.size, q.bhi})
		}
	}
	sort.Slice(blocks, func(i, j int) bool {
		if blocks[i].a != blocks[j].a {
			return blocks[i].a < blocks[j].a
		}
		return blocks[i].b < blocks[j].b
	})
	// Collapse adjacent blocks (only affects the block list, not the total).
	var out []match
	i1, j1, k1 := 0, 0, 0
	for _, blk := range blocks {
		if i1+k1 == blk.a && j1+k1 == blk.b {
			k1 += blk.size
		} else {
			if k1 > 0 {
				out = append(out, match{i1, j1, k1})
			}
			i1, j1, k1 = blk.a, blk.b, blk.size
		}
	}
	if k1 > 0 {
		out = append(out, match{i1, j1, k1})
	}
	return out
}
