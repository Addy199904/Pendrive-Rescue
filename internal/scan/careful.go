package scan

import (
	"bytes"
	"sort"

	"pendriverescue/internal/disk"
)

// The careful copy is for worn flash memory, which hands back a few wrong
// bits without reporting an error, and different ones each time. Every part
// of the drive is read twice; pages whose two reads differ are read again
// several times, and every bit keeps the value most reads agree on.
//
// The drive is read in windows of 16 MB: first all of the window, then all
// of it again. By the time a page is read the second time, 16 MB of other
// reads have gone by, so the pendrive cannot answer from its own small cache.

const (
	carefulWindow = 16 << 20
	carefulBatch  = 256 // pages settled together
	page          = disk.PageSize
)

type carefulReader struct {
	r    *disk.Reader
	size int64

	data     []byte // the window, settled
	from, to int64
	bad      map[int64]int64 // unreadable bytes per chunk in the window
	tmp      []byte
	flush    []byte

	skipLen int64
	dead    int
	stop    <-chan struct{}
}

func newCareful(r *disk.Reader, stop <-chan struct{}) *carefulReader {
	return &carefulReader{
		r: r, size: r.Size(), stop: stop,
		data:  disk.AlignedBuf(carefulWindow),
		tmp:   disk.AlignedBuf(chunkSize),
		flush: disk.AlignedBuf(chunkSize),
		from:  -1, to: -1,
	}
}

func (c *carefulReader) stopped() bool {
	if c.stop == nil {
		return false
	}
	select {
	case <-c.stop:
		return true
	default:
		return false
	}
}

// get fills buf with the settled data at off (a chunk start) and reports
// unreadable bytes, plus pages settled by voting and pages left unsure.
func (c *carefulReader) get(buf []byte, off int64) (bad int64, settled, unsure []int64, err error) {
	if off < c.from || off+int64(len(buf)) > c.to {
		settled, unsure, err = c.load(off)
		if err != nil {
			return 0, nil, nil, err
		}
	}
	copy(buf, c.data[off-c.from:])
	return c.bad[off], settled, unsure, nil
}

// load reads, re-reads and settles the window starting at off.
func (c *carefulReader) load(off int64) (settled, unsure []int64, err error) {
	end := min(off+carefulWindow, c.size)
	c.from, c.to = off, end
	c.bad = map[int64]int64{}
	// first pass
	type part struct {
		off, n int64
		skip   bool
	}
	var parts []part
	for o := off; o < end; o += chunkSize {
		n := min(int64(chunkSize), end-o)
		b := c.data[o-off : o-off+n]
		if c.skipLen > 0 {
			clear(b)
			c.r.MarkBadRange(o, n)
			c.bad[o] = n
			c.skipLen -= n
			parts = append(parts, part{o, n, true})
			continue
		}
		bad, e := c.r.ReadTolerant(b, o)
		if e != nil {
			return nil, nil, e
		}
		c.bad[o] = bad
		if bad == n {
			c.dead++
			c.skipLen = int64(1<<20) << uint(min(c.dead-1, 6))
		} else {
			c.dead = 0
		}
		parts = append(parts, part{o, n, bad == n})
	}
	// second pass: read again and compare page by page
	var odd []int64
	second := map[int64][]byte{}
	for _, p := range parts {
		if p.skip || c.stopped() {
			continue
		}
		t := c.tmp[:p.n]
		whole := c.bad[p.off] == 0 && c.r.RetryRead(t, p.off)
		for po := p.off; po < p.off+p.n; po += page {
			pn := min(int64(page), p.off+p.n-po)
			if !whole {
				if c.r.RangeHasBad(po, pn) || !c.r.RetryRead(t[po-p.off:po-p.off+pn], po) {
					continue
				}
			}
			if !bytes.Equal(t[po-p.off:po-p.off+pn], c.data[po-off:po-off+pn]) {
				odd = append(odd, po)
				second[po] = append([]byte(nil), t[po-p.off:po-p.off+pn]...)
			}
		}
		if c.r.Gone() {
			return nil, nil, disk.ErrDeviceGone
		}
	}
	// settle the pages that disagreed, a batch at a time
	for len(odd) > 0 && !c.stopped() {
		k := min(len(odd), carefulBatch)
		s, u := c.settle(odd[:k], second)
		settled = append(settled, s...)
		unsure = append(unsure, u...)
		odd = odd[k:]
	}
	return settled, unsure, nil
}

// settle reads the pages several more times and keeps, for every bit, the
// value most reads agree on. Pages whose vote is close are read more.
func (c *carefulReader) settle(pages []int64, second map[int64][]byte) (settled, unsure []int64) {
	sort.Slice(pages, func(i, j int) bool { return pages[i] < pages[j] })
	reads := make(map[int64][][]byte, len(pages))
	for _, pg := range pages {
		pn := min(int64(page), c.size-pg)
		reads[pg] = [][]byte{append([]byte(nil), c.data[pg-c.from:pg-c.from+pn]...), second[pg]}
	}
	pass := 0
	// readMore reads the given pages times more (neighbours together)
	readMore := func(pgs []int64, times int) {
		type run struct{ from, to int64 }
		var runs []run
		for _, pg := range pgs {
			pn := min(int64(page), c.size-pg)
			if n := len(runs); n > 0 && runs[n-1].to == pg {
				runs[n-1].to += pn
			} else {
				runs = append(runs, run{pg, pg + pn})
			}
		}
		for i := 0; i < times && !c.stopped(); i++ {
			pass++
			// something far away in between, so the pendrive's own cache
			// cannot answer
			far := (c.from + int64(pass)*(97<<20) + c.size/3) % max(c.size-int64(len(c.flush)), 1)
			c.r.RetryRead(c.flush, far&^(page-1))
			for _, rn := range runs {
				for o := rn.from; o < rn.to; {
					n := min(int64(chunkSize), rn.to-o)
					b := c.tmp[:n]
					if c.r.RetryRead(b, o) {
						for po := o; po < o+n; po += page {
							pn := min(int64(page), o+n-po)
							if _, ok := reads[po]; ok {
								reads[po] = append(reads[po], append([]byte(nil), b[po-o:po-o+pn]...))
							}
						}
					}
					o += n
				}
			}
		}
	}
	// close: the vote on some bit is not yet clear (a quarter or more of
	// the reads disagree with the majority)
	isClose := func(pg int64) bool {
		rs := reads[pg]
		if len(rs) < 3 {
			return true
		}
		_, worst := vote(rs)
		return worst*4 >= len(rs)
	}
	// every page has 2 reads that disagree. All get at least 11 reads: with
	// fewer, a weak bit that is wrong more often than not can look settled.
	// Then the pages whose vote is still close are read more, up to 61.
	todo := pages
	for stage, more := range []int{9, 10, 20, 20} {
		if len(todo) == 0 || c.stopped() {
			break
		}
		readMore(todo, more)
		var next []int64
		for _, pg := range todo {
			if isClose(pg) {
				next = append(next, pg)
			}
		}
		todo = next
		_ = stage
	}
	for _, pg := range pages {
		rs := reads[pg]
		if len(rs) < 2 || rs[1] == nil {
			unsure = append(unsure, pg)
			continue
		}
		v, _ := vote(rs)
		copy(c.data[pg-c.from:], v)
		settled = append(settled, pg)
		if isClose(pg) {
			unsure = append(unsure, pg)
		}
	}
	return settled, unsure
}

// vote returns, for every bit, the value most reads have, and the largest
// number of reads that disagreed with the majority on any one bit.
func vote(reads [][]byte) ([]byte, int) {
	out := make([]byte, len(reads[0]))
	worst := 0
	for i := range out {
		same := true
		for _, r := range reads[1:] {
			if r[i] != reads[0][i] {
				same = false
				break
			}
		}
		if same {
			out[i] = reads[0][i]
			continue
		}
		var v byte
		for b := 0; b < 8; b++ {
			n := 0
			for _, r := range reads {
				n += int(r[i] >> b & 1)
			}
			if n*2 > len(reads) {
				v |= 1 << b
				worst = max(worst, len(reads)-n)
			} else {
				worst = max(worst, n)
			}
		}
		out[i] = v
	}
	return out, worst
}
