package jpegfix

import (
	"bytes"
	"errors"
	"sort"
	"time"
)

// Copies of a photo read from worn flash memory each have a few wrong bits,
// in different places. Combine makes one photo out of them: with three or
// more copies, every bit takes the value most copies have; with two, the
// photo's own structure decides, at every place where it goes wrong, which
// copy's byte (or which single bit) lets it continue.

// Combine returns the best photo it can make from the copies (all the same
// length), and how good it is.
func Combine(copies [][]byte, deadline time.Time) ([]byte, Result, error) {
	if len(copies) == 0 {
		return nil, Result{}, errors.New("no copies")
	}
	n := len(copies[0])
	var same [][]byte
	for _, c := range copies {
		if len(c) == n {
			same = append(same, c)
		}
	}
	cands := append([][]byte(nil), same...)
	if len(same) >= 3 {
		cands = append([][]byte{vote(same)}, cands...)
	}
	// start from the version that gets furthest
	var best []byte
	var bestRes Result
	var info *Info
	for _, c := range cands {
		in, err := Parse(bytes.NewReader(c), int64(n))
		if err != nil {
			continue
		}
		r := Check(in, bytes.NewReader(c), int64(n))
		if best == nil || r.OK || r.ErrAt > bestRes.ErrAt {
			best, bestRes, info = append([]byte(nil), c...), r, in
		}
		if r.OK {
			return best, r, nil
		}
	}
	if best == nil {
		return nil, Result{}, ErrNotJPEG
	}
	// the header: where copies differ before the compressed data, take the
	// bytes of a copy whose header reads correctly (that is `best`)
	alts := map[int][]byte{}
	for i := int(info.ScanStart); i < n; i++ {
		var vals []byte
		for _, c := range same {
			if c[i] != best[i] && bytes.IndexByte(vals, c[i]) < 0 {
				vals = append(vals, c[i])
			}
		}
		if vals != nil {
			alts[i] = vals
		}
	}
	diff := make([]int, 0, len(alts))
	for i := range alts {
		diff = append(diff, i)
	}
	sort.Ints(diff)
	fx := &fixer{info: info, buf: best, alts: alts, diff: diff, deadline: deadline}
	res := fx.run()
	return fx.buf, res, nil
}

// vote takes, for every bit, the value most copies have.
func vote(copies [][]byte) []byte {
	out := make([]byte, len(copies[0]))
	need := len(copies)/2 + 1
	for i := range out {
		b0 := copies[0][i]
		same := true
		for _, c := range copies[1:] {
			if c[i] != b0 {
				same = false
				break
			}
		}
		if same {
			out[i] = b0
			continue
		}
		var v byte
		for bit := 0; bit < 8; bit++ {
			k := 0
			for _, c := range copies {
				k += int(c[i] >> bit & 1)
			}
			if k >= need {
				v |= 1 << bit
			}
		}
		out[i] = v
	}
	return out
}

type fixer struct {
	info     *Info
	buf      []byte
	alts     map[int][]byte // other copies' values where they differ
	diff     []int          // sorted positions of alts
	deadline time.Time
	scratch  []int32
	fixes    int
}

type snap struct {
	d   dec
	pos int64
}

// reach decodes from state d until an error and returns where it stopped
// (len(buf) when the picture is complete).
func (f *fixer) reach(d dec, limit int64) int64 {
	s := &source{data: f.buf, limit: int64(len(f.buf))}
	total := f.info.MCUsX * f.info.MCUsY
	for d.pos < limit {
		if err := d.boundary(f.info, s); err != nil {
			return d.pos
		}
		if d.mcu >= total {
			return int64(len(f.buf))
		}
		if err := d.mcuStep(f.info, s); err != nil {
			return d.pos
		}
	}
	return d.pos
}

func (f *fixer) run() Result {
	info := f.info
	s := &source{data: f.buf, limit: int64(len(f.buf))}
	f.scratch = make([]int32, len(newDec(info).dcRow))
	d := newDec(info)
	d.dcRow = f.scratch
	total := info.MCUsX * info.MCUsY
	var snaps []snap
	for {
		if !f.deadline.IsZero() && time.Now().After(f.deadline) {
			break
		}
		if d.mcu%4 == 0 && d.inIntv >= 0 && d.marker < 0 {
			if len(snaps) == 0 || snaps[len(snaps)-1].d.mcu != d.mcu {
				snaps = append(snaps, snap{d, d.pos})
				if len(snaps) > 4096 {
					snaps = snaps[2048:]
				}
			}
		}
		err := d.boundary(info, s)
		if err == nil && d.mcu >= total {
			break
		}
		if err == nil {
			err = d.mcuStep(info, s)
		}
		if err == nil {
			continue
		}
		// it goes wrong around d.pos: try the other copies' bytes and
		// single bits just before
		e := d.pos
		var from snap
		for k := len(snaps) - 1; k >= 0; k-- {
			if snaps[k].pos <= e-600 || k == 0 {
				from = snaps[k]
				snaps = snaps[:k+1]
				break
			}
		}
		if !f.fixNear(from.d, e) {
			break
		}
		d = from.d
		d.dcRow = f.scratch
	}
	return Check(info, bytes.NewReader(f.buf), int64(len(f.buf)))
}

// fixNear changes one byte before position e so that decoding from d gets
// as far as possible. It reports whether anything helped.
func (f *fixer) fixNear(d dec, e int64) bool {
	limit := e + 64<<10
	base := f.reach(d, limit)
	type change struct {
		at int
		v  byte
	}
	var tries []change
	lo := sort.SearchInts(f.diff, int(e)-520)
	for k := lo; k < len(f.diff) && f.diff[k] <= int(e)+8; k++ {
		i := f.diff[k]
		cur := f.buf[i]
		for _, v := range f.alts[i] {
			tries = append(tries, change{i, v})
			x := cur ^ v
			if x&(x-1) != 0 { // several bits differ: also each mix of them
				for m := x; m > 0; m = (m - 1) & x {
					if mv := cur ^ m; mv != v {
						tries = append(tries, change{i, mv})
					}
				}
			}
		}
	}
	bestAt, bestReach := -1, base
	var bestV byte
	eval := func(c change) {
		old := f.buf[c.at]
		f.buf[c.at] = c.v
		x := d
		x.dcRow = f.scratch
		if r := f.reach(x, limit); r > bestReach {
			bestAt, bestReach, bestV = c.at, r, c.v
		}
		f.buf[c.at] = old
	}
	for _, c := range tries {
		eval(c)
	}
	if bestAt < 0 || bestReach < e+4 {
		// both copies wrong here: try every single bit just before
		for i := max(int(e)-96, int(f.info.ScanStart)); i <= int(e) && i < len(f.buf); i++ {
			for bit := 0; bit < 8; bit++ {
				eval(change{i, f.buf[i] ^ 1<<bit})
			}
		}
	}
	if bestAt < 0 || bestReach <= base {
		return false
	}
	f.buf[bestAt] = bestV
	f.fixes++
	return true
}
