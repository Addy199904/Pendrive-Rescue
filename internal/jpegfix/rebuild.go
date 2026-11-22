package jpegfix

import (
	"errors"
	"io"
	"sort"
	"time"
)

// Drive is the whole drive or its safe copy.
type Drive interface {
	ReadAt(p []byte, off int64) (int, error)
	Size() int64
}

// Extent is a piece of a file on the drive.
type Extent struct{ Off, Len int64 }

// FileView reads a file laid out in extents; missing parts read as zeros.
type FileView struct {
	D     Drive
	Parts []Extent
	N     int64
}

func (f *FileView) ReadAt(p []byte, off int64) (int, error) {
	clear(p)
	end := off + int64(len(p))
	if end > f.N {
		end = f.N
	}
	pos := int64(0)
	for _, e := range f.Parts {
		if pos >= end {
			break
		}
		if pos+e.Len > off && e.Off >= 0 {
			from, to := max(off, pos), min(end, pos+e.Len)
			if to > from {
				f.D.ReadAt(p[from-off:to-off], e.Off+(from-pos))
			}
		}
		pos += e.Len
	}
	return len(p), nil
}

// driveOff returns where byte `at` of the file lies on the drive (-1 if in a gap).
func driveOff(parts []Extent, at int64) int64 {
	pos := int64(0)
	for _, e := range parts {
		if at < pos+e.Len {
			if e.Off < 0 {
				return -1
			}
			return e.Off + at - pos
		}
		pos += e.Len
	}
	return -1
}

// head returns the extents covering the first b bytes of the file.
func head(parts []Extent, b int64) []Extent {
	var out []Extent
	pos := int64(0)
	for _, e := range parts {
		if pos >= b {
			break
		}
		n := min(e.Len, b-pos)
		out = append(out, Extent{e.Off, n})
		pos += e.Len
	}
	return out
}

// joined reads the file up to b, then the drive from c on.
type joined struct {
	head io.ReaderAt
	b    int64
	d    io.ReaderAt
	c    int64
}

func (j *joined) ReadAt(p []byte, off int64) (int, error) {
	n := 0
	if off < j.b {
		n = int(min(int64(len(p)), j.b-off))
		j.head.ReadAt(p[:n], off)
	}
	if n < len(p) {
		j.d.ReadAt(p[n:], j.c+off+int64(n)-j.b)
	}
	return len(p), nil
}

// window keeps one large piece of the drive in memory, since the search
// reads small bits at increasing positions.
type window struct {
	d     Drive
	buf   []byte
	start int64
	n     int
}

func (w *window) ReadAt(p []byte, off int64) (int, error) {
	if off < w.start || off+int64(len(p)) > w.start+int64(w.n) {
		w.start = off
		w.n, _ = w.d.ReadAt(w.buf, off)
		if w.n < len(p) {
			clear(p)
			copy(p, w.buf[:w.n])
			return len(p), nil
		}
	}
	copy(p, w.buf[off-w.start:])
	return len(p), nil
}

// junctionAt decodes the start of the file up to byte b. It returns the
// decoder state just before byte b is needed, and how much the brightness
// normally changes from one block to the one below it in that area.
func junctionAt(info *Info, f io.ReaderAt, b int64) (dec, float64, bool) {
	s := &source{r: f, limit: b, buf: make([]byte, 0, 64<<10)}
	d := newDec(info)
	d.trackDC = true
	total := info.MCUsX * info.MCUsY
	type rowStat struct {
		sum float64
		n   int
	}
	rows := map[int]*rowStat{}
	baseline := func(cur int) float64 {
		var sum float64
		var n int
		for r := cur - 3; r <= cur; r++ {
			if st := rows[r]; st != nil {
				sum += st.sum
				n += st.n
			}
		}
		if n < 16 {
			return 12
		}
		return max(sum/float64(n), 2)
	}
	for {
		snap := d.clone()
		snap.unfeed()
		if err := d.boundary(info, s); err != nil {
			if err == errEnd {
				return snap, baseline(d.mcu / info.MCUsX), true
			}
			return dec{}, 0, false
		}
		if d.mcu >= total {
			return dec{}, 0, false // complete before b
		}
		snap = d.clone()
		snap.unfeed()
		sum, n, row := d.dcSum, d.dcN, d.mcu/info.MCUsX
		if err := d.mcuStep(info, s); err != nil {
			if err == errEnd {
				return snap, baseline(row), true
			}
			return dec{}, 0, false
		}
		st := rows[row]
		if st == nil {
			st = &rowStat{}
			rows[row] = st
			delete(rows, row-5)
		}
		st.sum += d.dcSum - sum
		st.n += d.dcN - n
	}
}

// trial decodes from the junction state with the drive's bytes from c on.
type trial struct {
	info  *Info
	j     dec
	file  io.ReaderAt
	b     int64
	drive io.ReaderAt
	src   source
	rd    joined
}

// outcome of a trial: whether the data fits, how much the brightness and
// colour jump between neighbouring blocks, how many MCUs were decoded, and
// how many of the candidate's bytes were used before any error.
type outcome struct {
	ok    bool
	score float64
	mcus  int
	used  int64
}

func (t *trial) try(c int64, mcus int, maxBytes int64) outcome {
	t.rd = joined{head: t.file, b: t.b, d: t.drive, c: c}
	t.src.r = &t.rd
	t.src.limit = t.b + maxBytes
	t.src.buf = t.src.buf[:0]
	t.src.start = -1 << 62
	x := t.j
	x.dcRow = append(x.dcRow[:0:0], t.j.dcRow...)
	x.trackDC, x.dcSum, x.dcN = true, 0, 0
	from := x.mcu
	err := x.run(t.info, &t.src, from+mcus)
	o := outcome{mcus: x.mcu - from, used: x.pos - t.b}
	if x.dcN > 0 {
		o.score = x.dcSum / float64(x.dcN)
	}
	o.ok = err == nil || (err == errEnd && x.mcu > from && x.err == nil)
	return o
}

// looksLikeData is a quick test that a cluster may hold JPEG compressed data.
func looksLikeData(b []byte) bool {
	if len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF {
		return false // the start of another picture
	}
	zeros := 0
	for i := 0; i < len(b); i++ {
		switch b[i] {
		case 0:
			zeros++
		case 0xFF:
			if i+1 < len(b) {
				nb := b[i+1]
				if nb != 0 && nb != 0xFF && nb != 0xD9 && (nb < 0xD0 || nb > 0xD7) {
					return false
				}
			}
		}
	}
	return zeros < len(b)/4
}

// Options for Rebuild.
type Options struct {
	Cluster  int64 // cluster size
	Phase    int64 // clusters start at offsets Phase + k*Cluster
	Deadline time.Time
	// From, To limit the area searched (0, 0 = the whole drive).
	From, To int64
	// Taken reports areas known to belong to other files (optional).
	Taken func(off int64) bool
	// Progress reports the searched bytes.
	Progress func(done, total int64)
	// SizeKnown: the file system says how long the file is. Otherwise
	// (files found by their content) the length is worked out.
	SizeKnown bool
}

// Repair is the outcome of Rebuild.
type Repair struct {
	Parts  []Extent // the file's pieces, joined (when OK)
	Size   int64    // its size (may change for files found by content)
	OK     bool     // the whole picture now decodes
	Pieces int      // pieces joined
	Before Result   // how the file was before
}

// ErrNoRepair is returned when nothing could be joined.
var ErrNoRepair = errors.New("the missing part was not found on the drive")

type rebuilder struct {
	d        Drive
	info     *Info
	opt      Options
	cs       int64
	win      *window
	searches int // searches of the drive so far (each reads all of it)
}

// at most this many searches per photo, and this many choices per piece
const (
	maxSearches = 12
	maxChoices  = 3
)

func (rb *rebuilder) late() bool {
	return !rb.opt.Deadline.IsZero() && time.Now().After(rb.opt.Deadline)
}

// Rebuild looks for the missing pieces of a JPEG on the drive. parts is
// where the file is now thought to be; size its length.
func Rebuild(d Drive, parts []Extent, size int64, opt Options) (Repair, error) {
	rep := Repair{Parts: parts, Size: size}
	fv := &FileView{D: d, Parts: parts, N: size}
	info, err := Parse(fv, size)
	if err != nil {
		return rep, err
	}
	res := Check(info, fv, size)
	rep.Before = res
	if res.OK {
		rep.OK = true
		return rep, nil
	}
	if opt.Deadline.IsZero() {
		opt.Deadline = time.Now().Add(3 * time.Minute)
	}
	rb := &rebuilder{d: d, info: info, opt: opt, cs: opt.Cluster, win: &window{d: d, buf: make([]byte, 8<<20)}}
	if rb.cs <= 0 {
		rb.cs = 4096
	}
	if rb.opt.To <= 0 || rb.opt.To > d.Size() {
		rb.opt.To = d.Size()
	}
	out, n, pieces, ok := rb.extend(parts, size, res, 0)
	if !ok {
		return rep, ErrNoRepair
	}
	rep.Parts, rep.Size, rep.Pieces, rep.OK = out, n, pieces, true
	return rep, nil
}

// extend finds the next piece after the point where the file goes wrong,
// trying the most likely places first and going back if a choice leads
// nowhere.
func (rb *rebuilder) extend(cur []Extent, size int64, res Result, depth int) ([]Extent, int64, int, bool) {
	if res.OK {
		if !rb.opt.SizeKnown && res.End > 0 && res.End < size {
			return head(cur, res.End), res.End, depth, true
		}
		return cur, size, depth, true
	}
	if depth >= 8 || rb.late() {
		return nil, 0, 0, false
	}
	info, cs := rb.info, rb.cs
	fv := &FileView{D: rb.d, Parts: cur, N: size}
	// Where the pieces meet: a cluster boundary. A decoding error shows up a
	// little after it; a picture that stops continuing is found right at it.
	var tries []int64
	if res.jumped {
		b0 := (res.ErrAt + cs/2) / cs * cs
		tries = []int64{b0, b0 - cs, b0 + cs}
	} else {
		// The data may also have gone on decoding past the point where
		// another file's data begins: try where the picture changes most.
		// A strong change well before the error comes first.
		b0 := res.ErrAt / cs * cs
		gs := res.guesses(cs, 3)
		if len(gs) > 0 && gs[0].ratio >= 1.5 && gs[0].b <= b0-2*cs {
			tries = append(tries, gs[0].b)
		}
		tries = append(tries, b0, b0-cs)
		for _, g := range gs {
			if g.b != b0 && g.b != b0-cs && (len(tries) == 0 || g.b != tries[0]) {
				tries = append(tries, g.b)
			}
		}
		tries = append(tries, b0-2*cs)
	}
	type next struct {
		parts []Extent
		n     int64
		res   Result
	}
	var nexts []next
	tried := map[int64]bool{}
	for _, b := range tries {
		if b <= info.ScanStart || tried[b] || rb.late() || rb.searches >= maxSearches {
			continue
		}
		tried[b] = true
		j, base, ok := junctionAt(info, fv, b)
		if !ok {
			continue
		}
		rb.searches++
		for _, c := range rb.search(j, base, fv, b, cur, size) {
			rest := size - b
			if !rb.opt.SizeKnown {
				done := max(j.mcu, 1)
				per := float64(b-info.ScanStart) / float64(done)
				rest = int64(per*float64(info.MCUsX*info.MCUsY-done)*1.5) + 256<<10
			}
			rest = min(rest, rb.d.Size()-c)
			parts := append(head(cur, b), Extent{c, rest})
			n := b + rest
			r2 := Check(info, &FileView{D: rb.d, Parts: parts, N: n}, n)
			if r2.OK {
				return rb.extend(parts, n, r2, depth+1)
			}
			if r2.ErrAt < b+cs/2 {
				continue // goes wrong inside the piece itself
			}
			nexts = append(nexts, next{parts, n, r2})
		}
	}
	// follow the choices that get furthest
	sort.Slice(nexts, func(i, k int) bool { return nexts[i].res.MCUs > nexts[k].res.MCUs })
	for i, nx := range nexts {
		if i >= 2 {
			break
		}
		if out, sz, pieces, ok := rb.extend(nx.parts, nx.n, nx.res, depth+1); ok {
			return out, sz, pieces, true
		}
	}
	return nil, 0, 0, false
}

type cand struct {
	c     int64
	score float64
	ends  bool // the picture's end marker is exactly where the file ends
}

// search tries every cluster of the drive as the continuation of the file
// after byte b. It returns the places that fit, best first.
func (rb *rebuilder) search(j dec, base float64, fv *FileView, b int64, cur []Extent, size int64) []int64 {
	info, cs, opt, win := rb.info, rb.cs, rb.opt, rb.win
	t := &trial{info: info, j: j, file: fv, b: b, drive: win, src: source{buf: make([]byte, 0, 32<<10)}}
	here := driveOff(cur, b)
	own := head(cur, b)
	inFile := func(c int64) bool {
		for _, e := range own {
			if c >= e.Off && c < e.Off+e.Len {
				return true
			}
		}
		return false
	}
	phase := ((opt.Phase % cs) + cs) % cs
	align := func(x int64) int64 { return (x-phase+cs-1)/cs*cs + phase }

	// first test: a few MCUs, or up to the next restart marker
	n1, max1 := 24, min(2*cs, 64<<10)
	if info.Restart > 0 {
		n1 = info.Restart - j.inIntv + 1
		max1 = 1 << 20
	}
	// near the break first (file systems put the next piece close by),
	// then the rest of the drive
	var steps [][2]int64
	if here >= 0 {
		nearTo := min(opt.To, here+512<<20)
		nearFrom := max(opt.From, here-64<<20)
		steps = append(steps,
			[2]int64{align(max(here+1, opt.From)), nearTo},
			[2]int64{align(nearFrom), here},
			[2]int64{align(nearTo), opt.To},
			[2]int64{align(opt.From), nearFrom})
	} else {
		steps = append(steps, [2]int64{align(opt.From), opt.To})
	}
	want := info.MCUsX
	if info.Restart > 0 {
		want = max(want, 2*info.Restart+1)
	}
	tail := make([]byte, 2)
	endsRight := func(c int64) bool {
		if !opt.SizeKnown {
			return false
		}
		rb.d.ReadAt(tail, c+size-b-2)
		return tail[0] == 0xFF && tail[1] == 0xD9
	}
	quick := make([]byte, 512)
	var survivors []cand
	total := opt.To - opt.From
	var done int64
	checked := 0
	for _, st := range steps {
		for c := st[0]; c+512 <= st[1]; c += cs {
			done += cs
			if checked++; checked&4095 == 0 {
				if opt.Progress != nil {
					opt.Progress(done, total)
				}
				if rb.late() {
					break
				}
			}
			if c == here || inFile(c) || (opt.Taken != nil && opt.Taken(c)) {
				continue
			}
			win.ReadAt(quick, c)
			if !looksLikeData(quick) {
				continue
			}
			o := t.try(c, n1, max1)
			if !(o.ok || o.used >= cs) || o.mcus == 0 || o.score > 6*base+40 {
				continue
			}
			if endsRight(c) {
				// a photo in two pieces ends exactly where the file ends
				if o2 := t.try(c, want, 4<<20); (o2.ok || o2.used >= cs) && o2.mcus >= min(want, 8) && o2.score <= max(1.8*base, base+8) {
					return []int64{c}
				}
			}
			survivors = append(survivors, cand{c: c, score: o.score})
			if len(survivors) > 8000 {
				sort.Slice(survivors, func(i, k int) bool { return survivors[i].score < survivors[k].score })
				survivors = survivors[:2000]
			}
		}
	}
	// second test: a whole row of MCUs (and two restart intervals)
	sort.Slice(survivors, func(i, k int) bool { return survivors[i].score < survivors[k].score })
	if len(survivors) > 256 {
		survivors = survivors[:256]
	}
	var good []cand
	for _, s := range survivors {
		o := t.try(s.c, want, 4<<20)
		if !(o.ok || o.used >= cs) || o.mcus < min(want, 8) {
			continue
		}
		if o.score <= max(1.8*base, base+8) {
			good = append(good, cand{c: s.c, score: o.score, ends: endsRight(s.c)})
		}
	}
	sort.Slice(good, func(i, k int) bool {
		if good[i].ends != good[k].ends {
			return good[i].ends
		}
		return good[i].score < good[k].score
	})
	var out []int64
	for i := 0; i < len(good) && i < maxChoices; i++ {
		out = append(out, good[i].c)
	}
	return out
}
