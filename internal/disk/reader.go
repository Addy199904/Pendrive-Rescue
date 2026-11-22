package disk

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	cacheBlock   = 64 << 10 // bytes per cache block
	cacheEntries = 512      // 32 MB of cache
	directBlock  = 1 << 20  // max bytes per raw read
)

// Reader wraps a Source and makes it safe and convenient to use:
//   - any offset / length is accepted (reads are aligned internally),
//   - unreadable (bad) sectors are returned as zeros instead of failing,
//   - small reads are cached,
//   - reading past the end returns zeros.
//
// The only error ever returned is ErrDeviceGone.
type Reader struct {
	src  Source
	size int64
	ss   int64

	mu       sync.Mutex
	cache    map[int64][]byte
	fifo     []int64
	bad      []badRange // known unreadable areas, sorted and merged
	badCnt   int64
	gone     bool
	lastGood int64
	scratch  []byte
	busy     atomic.Int64 // UnixNano when the current device read started (0 = idle)

	// patches: pages whose data was settled by reading them several times
	// (worn flash can return a few wrong bits without reporting an error).
	patches  map[int64][]byte
	pageKeys []int64 // sorted
}

// PageSize is the unit of Patch.
const PageSize = 4096

// Patch makes the Reader return b for the page at off (a multiple of
// PageSize, len(b) == PageSize) from now on.
func (r *Reader) Patch(off int64, b []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.patches == nil {
		r.patches = map[int64][]byte{}
	}
	if _, ok := r.patches[off]; !ok {
		i := sort.Search(len(r.pageKeys), func(i int) bool { return r.pageKeys[i] >= off })
		r.pageKeys = append(r.pageKeys, 0)
		copy(r.pageKeys[i+1:], r.pageKeys[i:])
		r.pageKeys[i] = off
	}
	r.patches[off] = append([]byte(nil), b...)
}

// Patched reports how many pages were patched.
func (r *Reader) Patched() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.patches)
}

// applyPatches copies patched pages over p (which holds [off, off+len(p))).
func (r *Reader) applyPatches(p []byte, off int64) {
	if len(r.pageKeys) == 0 || len(p) == 0 {
		return
	}
	end := off + int64(len(p))
	i := sort.Search(len(r.pageKeys), func(i int) bool { return r.pageKeys[i]+PageSize > off })
	for ; i < len(r.pageKeys) && r.pageKeys[i] < end; i++ {
		po := r.pageKeys[i]
		pg := r.patches[po]
		from, to := max(po, off), min(po+int64(len(pg)), end)
		if to > from {
			copy(p[from-off:to-off], pg[from-po:to-po])
		}
	}
}

// read calls the source, remembering when the read started so a stalled
// drive can be reported.
func (r *Reader) read(p []byte, off int64) (int, error) {
	r.busy.Store(time.Now().UnixNano())
	n, err := r.src.ReadAt(p, off)
	r.busy.Store(0)
	return n, err
}

// Stalled reports how long the current read has been waiting for the drive.
func (r *Reader) Stalled() time.Duration {
	b := r.busy.Load()
	if b == 0 {
		return 0
	}
	return time.Duration(time.Now().UnixNano() - b)
}

// ClearBad forgets unreadable areas inside [off, off+n) (after a retry
// managed to read them).
func (r *Reader) ClearBad(off, n int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	end := off + n
	var out []badRange
	for _, b := range r.bad {
		if b.end <= off || b.start >= end {
			out = append(out, b)
			continue
		}
		if b.start < off {
			out = append(out, badRange{b.start, off})
		}
		if b.end > end {
			out = append(out, badRange{end, b.end})
		}
		cleared := (min(b.end, end) - max(b.start, off)) / r.ss
		r.badCnt -= cleared
	}
	r.bad = out
}

// RetryRead reads an aligned range directly from the drive, without using
// the bad-area list, for retry passes. It reports whether it succeeded.
func (r *Reader) RetryRead(buf []byte, off int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gone {
		return false
	}
	n, err := r.read(buf, off)
	if err == nil && n == len(buf) {
		r.lastGood = off
		return true
	}
	if r.checkGone() {
		return false
	}
	return false
}

// NewReader creates a Reader over src.
func NewReader(src Source) *Reader {
	ss := int64(src.SectorSize())
	if ss <= 0 || ss&(ss-1) != 0 {
		ss = 512
	}
	return &Reader{
		src:      src,
		size:     src.Size(),
		ss:       ss,
		cache:    make(map[int64][]byte),
		lastGood: -1,
		scratch:  AlignedBuf(directBlock),
	}
}

// Size of the underlying drive/image in bytes.
func (r *Reader) Size() int64 { return r.size }

// SectorSize of the underlying drive.
func (r *Reader) SectorSize() int64 { return r.ss }

// Source returns the wrapped source.
func (r *Reader) Source() Source { return r.src }

// BadSectors returns how many unreadable sectors were met so far.
func (r *Reader) BadSectors() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.badCnt
}

// ReadAt fills p with the bytes at off. It never returns a partial read:
// unreadable areas and areas beyond the end come back as zeros.
func (r *Reader) ReadAt(p []byte, off int64) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gone {
		return 0, ErrDeviceGone
	}
	n, err := r.readAt(p, off)
	if err == nil {
		r.applyPatches(p, off)
	}
	return n, err
}

func (r *Reader) readAt(p []byte, off int64) (int, error) {
	n := len(p)
	if off < 0 {
		k := int(-off)
		if k > len(p) {
			k = len(p)
		}
		clear(p[:k])
		p = p[k:]
		off = 0
	}
	if off >= r.size {
		clear(p)
		return n, nil
	}
	if off+int64(len(p)) > r.size {
		k := int(r.size - off)
		clear(p[k:])
		p = p[:k]
	}
	if len(p) <= 2*cacheBlock {
		for len(p) > 0 {
			bi := off / cacheBlock
			blk, err := r.block(bi)
			if err != nil {
				return 0, err
			}
			o := int(off - bi*cacheBlock)
			c := copy(p, blk[o:])
			p = p[c:]
			off += int64(c)
		}
		return n, nil
	}
	for len(p) > 0 {
		start := off &^ (r.ss - 1)
		lead := int(off - start)
		want := lead + len(p)
		if want > directBlock {
			want = directBlock
		}
		want = int((int64(want) + r.ss - 1) &^ (r.ss - 1))
		if start+int64(want) > r.size {
			want = int(r.size - start)
		}
		buf := r.scratch[:want]
		if _, err := r.tolerantRead(buf, start); err != nil {
			return 0, err
		}
		c := copy(p, buf[lead:])
		p = p[c:]
		off += int64(c)
	}
	return n, nil
}

// ReadTolerant reads a sector-aligned range straight from the drive
// (bypassing the cache). It returns how many bytes were unreadable.
func (r *Reader) ReadTolerant(buf []byte, off int64) (badBytes int64, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gone {
		return 0, ErrDeviceGone
	}
	if off >= r.size {
		clear(buf)
		return 0, nil
	}
	if off+int64(len(buf)) > r.size {
		k := r.size - off
		clear(buf[k:])
		buf = buf[:k]
	}
	return r.tolerantRead(buf, off)
}

// block returns cache block bi (64 KB), reading it if needed.
func (r *Reader) block(bi int64) ([]byte, error) {
	if b, ok := r.cache[bi]; ok {
		return b, nil
	}
	off := bi * cacheBlock
	n := int64(cacheBlock)
	if off+n > r.size {
		n = r.size - off
	}
	buf := AlignedBuf(cacheBlock)
	if _, err := r.tolerantRead(buf[:n], off); err != nil {
		return nil, err
	}
	if len(r.fifo) >= cacheEntries {
		old := r.fifo[0]
		r.fifo = r.fifo[1:]
		delete(r.cache, old)
	}
	r.cache[bi] = buf
	r.fifo = append(r.fifo, bi)
	return buf, nil
}

// tolerantRead reads an aligned range; unreadable sectors become zeros.
// It returns the number of unreadable bytes.
func (r *Reader) tolerantRead(buf []byte, off int64) (int64, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	// possibly unaligned tail at the very end of the drive
	if int64(len(buf))%r.ss != 0 {
		full := int64(len(buf)) &^ (r.ss - 1)
		var bad int64
		if full > 0 {
			b, err := r.tolerantRead(buf[:full], off)
			if err != nil {
				return 0, err
			}
			bad = b
		}
		tmp := AlignedBuf(int(r.ss))
		n, _ := r.read(tmp, off+full)
		clear(buf[full:])
		copy(buf[full:], tmp[:n])
		return bad, nil
	}
	if r.hasKnownBad(off, int64(len(buf))) && int64(len(buf)) == r.ss {
		clear(buf)
		return r.ss, nil
	}
	if !r.hasKnownBad(off, int64(len(buf))) {
		n, err := r.read(buf, off)
		if err == nil && n == len(buf) {
			r.lastGood = off
			return 0, nil
		}
		if r.checkGone() {
			return 0, ErrDeviceGone
		}
	}
	if int64(len(buf)) == r.ss {
		clear(buf)
		r.markBad(off / r.ss)
		return r.ss, nil
	}
	// split the read to isolate the bad sectors; in a dense bad area give
	// up early so a dying drive does not take days.
	piece := int64(len(buf))
	switch {
	case piece > 64<<10:
		piece = 64 << 10
	case piece > 4<<10:
		piece = 4 << 10
	default:
		piece = r.ss
	}
	var bad int64
	fails := 0
	for i := int64(0); i < int64(len(buf)); i += piece {
		end := i + piece
		if end > int64(len(buf)) {
			end = int64(len(buf))
		}
		part := buf[i:end]
		if fails >= 2 {
			clear(part)
			r.addBad(off+i, off+end)
			bad += end - i
			continue
		}
		b, err := r.tolerantRead(part, off+i)
		if err != nil {
			return 0, err
		}
		bad += b
		if b == end-i {
			fails++
		} else {
			fails = 0
		}
	}
	return bad, nil
}

type badRange struct{ start, end int64 }

// findBad returns the index of the first bad range ending after off.
func (r *Reader) findBad(off int64) int {
	lo, hi := 0, len(r.bad)
	for lo < hi {
		m := (lo + hi) / 2
		if r.bad[m].end <= off {
			lo = m + 1
		} else {
			hi = m
		}
	}
	return lo
}

func (r *Reader) hasKnownBad(off, n int64) bool {
	i := r.findBad(off)
	return i < len(r.bad) && r.bad[i].start < off+n
}

func (r *Reader) markBad(sector int64) {
	r.addBad(sector*r.ss, (sector+1)*r.ss)
}

func (r *Reader) addBad(start, end int64) {
	i := r.findBad(start)
	if i < len(r.bad) && r.bad[i].start <= start && r.bad[i].end >= end {
		return // already known
	}
	// count only sectors not yet known
	newBytes := end - start
	for k := i; k < len(r.bad) && r.bad[k].start < end; k++ {
		s, e := max(r.bad[k].start, start), min(r.bad[k].end, end)
		if e > s {
			newBytes -= e - s
		}
	}
	r.badCnt += newBytes / r.ss
	// merge with neighbours
	j := i
	if j > 0 && r.bad[j-1].end >= start {
		j--
	}
	k := j
	for k < len(r.bad) && r.bad[k].start <= end {
		start = min(start, r.bad[k].start)
		end = max(end, r.bad[k].end)
		k++
	}
	r.bad = append(r.bad[:j], append([]badRange{{start, end}}, r.bad[k:]...)...)
}

// MarkBadRange records an area that could not be read (or was skipped).
func (r *Reader) MarkBadRange(off, n int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.addBad(off&^(r.ss-1), (off+n+r.ss-1)&^(r.ss-1))
}

// BadAreas returns the known unreadable areas as (offset, length) pairs.
func (r *Reader) BadAreas() [][2]int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][2]int64, len(r.bad))
	for i, b := range r.bad {
		out[i] = [2]int64{b.start, b.end - b.start}
	}
	return out
}

// RangeHasBad reports whether [off, off+n) touches an unreadable area.
func (r *Reader) RangeHasBad(off, n int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hasKnownBad(off, n)
}

// Truncate makes the reader treat the drive as only n bytes long (used for
// fake pendrives whose real capacity is smaller than they claim).
func (r *Reader) Truncate(n int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n > 0 && n < r.size {
		r.size = n
	}
}

// checkGone tests whether the drive still answers at all.
func (r *Reader) checkGone() bool {
	tmp := AlignedBuf(int(r.ss))
	probes := []int64{0}
	if r.lastGood > 0 {
		probes = append(probes, r.lastGood&^(r.ss-1))
	}
	probes = append(probes, (r.size/2)&^(r.ss-1), 1<<20, 8*r.ss)
	for _, p := range probes {
		if p < 0 || p+r.ss > r.size {
			continue
		}
		if n, err := r.read(tmp, p); err == nil && n == len(tmp) {
			return false
		}
	}
	r.gone = true
	return true
}

// DropCache forgets cached data.
func (r *Reader) DropCache() {
	r.mu.Lock()
	r.cache = make(map[int64][]byte)
	r.fifo = nil
	r.mu.Unlock()
}

// Gone reports whether the drive stopped responding.
func (r *Reader) Gone() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gone
}
