// Package jpegfix finds where a damaged JPEG photo stops being correct,
// saves the part that is still intact, and - with the drive at hand - looks
// for the missing piece of a photo that was stored in pieces and joins it
// back on.
//
// It follows the compressed data of baseline JPEGs (what cameras and phones
// write) without drawing the picture: Huffman codes, coefficient counts,
// restart markers and the average brightness of each 8x8 block. That is
// enough to tell the photo's own data from anything else, and fast enough to
// try thousands of places on a drive.
package jpegfix

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
)

// Info describes the parts of a JPEG needed to follow its compressed data.
type Info struct {
	Width, Height int
	Progressive   bool
	Restart       int   // MCUs per restart interval (0 = none)
	ScanStart     int64 // first byte of the compressed data
	SOFAt         int64 // position of the SOF marker
	MCUsX, MCUsY  int
	Orientation   int   // EXIF orientation (1 = upright, 0 = unknown)
	ThumbOff      int64 // EXIF preview picture inside the file (0 = none)
	ThumbLen      int64

	comps  []comp
	blocks []int // component index of each block of an MCU, in order
	cols   []int // per component: blocks per block row
	dc, ac [4]*huff
}

type comp struct {
	id, h, v int
	td, ta   int
}

type huff struct {
	maxcode [18]int32
	valptr  [17]int32
	mincode [17]int32
	vals    []byte
	look    [512]uint16 // fast path for codes of up to 9 bits: length<<8 | value
}

// Errors.
var (
	ErrNotJPEG     = errors.New("not a JPEG photo")
	ErrUnsupported = errors.New("this kind of JPEG (progressive or lossless) cannot be checked in detail")
)

func buildHuff(counts []byte, vals []byte) (*huff, error) {
	h := &huff{vals: vals}
	code, k := int32(0), int32(0)
	for l := 1; l <= 16; l++ {
		n := int32(counts[l-1])
		h.valptr[l] = k
		h.mincode[l] = code
		if n > 0 {
			h.maxcode[l] = code + n - 1
		} else {
			h.maxcode[l] = -1
		}
		code += n
		k += n
		if code > 1<<l {
			return nil, errors.New("bad Huffman table")
		}
		code <<= 1
	}
	h.maxcode[17] = 0x7fffffff
	if int(k) > len(vals) {
		return nil, errors.New("bad Huffman table")
	}
	// fast lookup
	code, k = 0, 0
	for l := 1; l <= 9; l++ {
		for i := int32(0); i < int32(counts[l-1]); i++ {
			first := code << (9 - l)
			for j := int32(0); j < 1<<(9-l); j++ {
				h.look[first+j] = uint16(l)<<8 | uint16(vals[k])
			}
			code++
			k++
		}
		code <<= 1
	}
	return h, nil
}

// Parse reads the header of a JPEG.
func Parse(r io.ReaderAt, size int64) (*Info, error) {
	b2 := make([]byte, 4)
	if _, err := r.ReadAt(b2[:2], 0); err != nil || b2[0] != 0xFF || b2[1] != 0xD8 {
		return nil, ErrNotJPEG
	}
	info := &Info{}
	pos := int64(2)
	sofSeen := false
	for pos+4 <= size {
		if _, err := r.ReadAt(b2, pos); err != nil {
			return nil, ErrNotJPEG
		}
		if b2[0] != 0xFF {
			return nil, ErrNotJPEG
		}
		m := b2[1]
		if m == 0xFF { // fill byte
			pos++
			continue
		}
		if m == 0xD8 || (m >= 0xD0 && m <= 0xD7) || m == 0x01 {
			pos += 2
			continue
		}
		if m == 0xD9 {
			return nil, ErrNotJPEG
		}
		n := int64(binary.BigEndian.Uint16(b2[2:]))
		if n < 2 || pos+2+n > size {
			return nil, ErrNotJPEG
		}
		seg := make([]byte, n-2)
		if _, err := r.ReadAt(seg, pos+4); err != nil {
			return nil, ErrNotJPEG
		}
		switch {
		case m == 0xE1 && len(seg) > 14 && string(seg[:6]) == "Exif\x00\x00":
			info.parseExif(seg[6:], pos+4+6)
		case m == 0xC0 || m == 0xC1:
			if len(seg) < 6 || seg[0] != 8 {
				return nil, ErrUnsupported
			}
			info.SOFAt = pos
			info.Height = int(binary.BigEndian.Uint16(seg[1:]))
			info.Width = int(binary.BigEndian.Uint16(seg[3:]))
			nc := int(seg[5])
			if nc < 1 || nc > 4 || len(seg) < 6+3*nc {
				return nil, ErrNotJPEG
			}
			for i := 0; i < nc; i++ {
				c := seg[6+3*i:]
				h, v := int(c[1]>>4), int(c[1]&15)
				if h < 1 || h > 4 || v < 1 || v > 4 {
					return nil, ErrNotJPEG
				}
				info.comps = append(info.comps, comp{id: int(c[0]), h: h, v: v})
			}
			sofSeen = true
		case m == 0xC2 || m == 0xC3 || (m >= 0xC5 && m <= 0xC7) || (m >= 0xC9 && m <= 0xCB) || (m >= 0xCD && m <= 0xCF):
			info.Progressive = m == 0xC2
			return info, ErrUnsupported
		case m == 0xC4:
			for p := 0; p < len(seg); {
				if p+17 > len(seg) {
					return nil, ErrNotJPEG
				}
				class, id := seg[p]>>4, int(seg[p]&15)
				counts := seg[p+1 : p+17]
				total := 0
				for _, c := range counts {
					total += int(c)
				}
				if class > 1 || id > 3 || p+17+total > len(seg) {
					return nil, ErrNotJPEG
				}
				h, err := buildHuff(counts, seg[p+17:p+17+total])
				if err != nil {
					return nil, ErrNotJPEG
				}
				if class == 0 {
					info.dc[id] = h
				} else {
					info.ac[id] = h
				}
				p += 17 + total
			}
		case m == 0xDD:
			if len(seg) >= 2 {
				info.Restart = int(binary.BigEndian.Uint16(seg))
			}
		case m == 0xDA:
			if !sofSeen || len(seg) < 1 {
				return nil, ErrNotJPEG
			}
			ns := int(seg[0])
			if len(seg) < 1+2*ns+3 || (ns != len(info.comps)) {
				return info, ErrUnsupported // separate scans per colour
			}
			for i := 0; i < ns; i++ {
				id := int(seg[1+2*i])
				t := seg[2+2*i]
				for j := range info.comps {
					if info.comps[j].id == id {
						info.comps[j].td, info.comps[j].ta = int(t>>4), int(t&15)
					}
				}
			}
			info.ScanStart = pos + 2 + n
			return info, info.setup()
		}
		pos += 2 + n
	}
	return nil, ErrNotJPEG
}

func (info *Info) setup() error {
	if info.Width == 0 || info.Height == 0 {
		return ErrUnsupported
	}
	maxH, maxV := 1, 1
	for _, c := range info.comps {
		maxH, maxV = max(maxH, c.h), max(maxV, c.v)
		if c.td > 3 || c.ta > 3 || info.dc[c.td] == nil || info.ac[c.ta] == nil {
			return ErrNotJPEG
		}
	}
	if len(info.comps) == 1 {
		info.comps[0].h, info.comps[0].v = 1, 1
		maxH, maxV = 1, 1
	}
	info.MCUsX = (info.Width + 8*maxH - 1) / (8 * maxH)
	info.MCUsY = (info.Height + 8*maxV - 1) / (8 * maxV)
	info.blocks, info.cols = nil, nil
	for i, c := range info.comps {
		for j := 0; j < c.h*c.v; j++ {
			info.blocks = append(info.blocks, i)
		}
		info.cols = append(info.cols, info.MCUsX*c.h)
	}
	return nil
}

// parseExif finds the orientation and the preview picture in an EXIF block.
func (info *Info) parseExif(t []byte, base int64) {
	if len(t) < 8 {
		return
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return
	}
	ifd := func(off uint32, fn func(tag uint16, val uint32)) uint32 {
		if int(off)+2 > len(t) {
			return 0
		}
		n := int(bo.Uint16(t[off:]))
		for i := 0; i < n; i++ {
			e := int(off) + 2 + 12*i
			if e+12 > len(t) {
				return 0
			}
			typ := bo.Uint16(t[e+2:])
			val := bo.Uint32(t[e+8:])
			if typ == 3 { // SHORT: value sits in the first two bytes
				val = uint32(bo.Uint16(t[e+8:]))
			}
			fn(bo.Uint16(t[e:]), val)
		}
		e := int(off) + 2 + 12*n
		if e+4 > len(t) {
			return 0
		}
		return bo.Uint32(t[e:])
	}
	next := ifd(bo.Uint32(t[4:]), func(tag uint16, val uint32) {
		if tag == 0x0112 {
			info.Orientation = int(val)
		}
	})
	if next == 0 {
		return
	}
	var off, n uint32
	ifd(next, func(tag uint16, val uint32) {
		switch tag {
		case 0x0201:
			off = val
		case 0x0202:
			n = val
		}
	})
	if off > 0 && n > 0 && int64(off)+int64(n) <= int64(len(t)) {
		info.ThumbOff, info.ThumbLen = base+int64(off), int64(n)
	}
}

// ---------------------------------------------------------------- following the compressed data

// source gives the decoder the bytes of the file.
type source struct {
	r     io.ReaderAt
	limit int64 // end of the bytes that may be read
	buf   []byte
	start int64
	data  []byte // when set, the bytes themselves (no reader)
}

func (s *source) byteAt(p int64) (byte, bool) {
	if p >= s.limit || p < 0 {
		return 0, false
	}
	if s.data != nil {
		return s.data[p], true
	}
	if p < s.start || p >= s.start+int64(len(s.buf)) {
		n := int64(cap(s.buf))
		if p+n > s.limit {
			n = s.limit - p
		}
		s.buf = s.buf[:n]
		k, _ := s.r.ReadAt(s.buf, p)
		s.buf = s.buf[:k]
		s.start = p
		if k == 0 {
			return 0, false
		}
	}
	return s.buf[p-s.start], true
}

// errors found while following the data
var (
	errCode     = errors.New("a code that does not exist")
	errValue    = errors.New("an impossible value")
	errOverflow = errors.New("too many values in a block")
	errMarker   = errors.New("a marker in the wrong place")
	errZeros    = errors.New("the data is all zeros (wiped)")
	errErased   = errors.New("the data is erased (all FF)")
	errEnd      = errors.New("the data ends too early")
)

const endOfData = 0x100 // "marker" meaning: no more bytes

// dec is the state of the decoder. Copying it (with clone) saves the state.
type dec struct {
	pos    int64 // next byte to load
	acc    uint64
	n      uint // bits in acc
	fed    int  // zero bytes fed after a marker or the end
	marker int  // marker ahead (-1 = none, endOfData = no more bytes)
	mpos   int64
	pred   [4]int32
	mcu    int // MCUs done
	inIntv int // MCUs done in the current restart interval
	rst    int // number of the next restart marker
	zeros  int // 0x00 data bytes in a row
	err    error

	// dcRow holds, per component, the DC value (average brightness or
	// colour) of the last block decoded in each block column.
	dcRow   []int32
	trackDC bool
	dcSum   float64 // |DC - DC of the block above|, summed
	dcN     int
}

func newDec(info *Info) dec {
	n := 0
	for _, c := range info.cols {
		n += c
	}
	return dec{pos: info.ScanStart, marker: -1, dcRow: make([]int32, n)}
}

func (d *dec) clone() dec {
	c := *d
	c.dcRow = append([]int32(nil), d.dcRow...)
	return c
}

// unfeed takes back the zero bytes fed at the end of the available data, so
// that decoding can go on with more bytes.
func (d *dec) unfeed() {
	if d.marker != endOfData || d.err != nil {
		return
	}
	d.acc >>= 8 * uint(d.fed)
	d.n -= 8 * uint(d.fed)
	d.fed, d.marker = 0, -1
}

func (d *dec) fill(s *source) {
	for d.n <= 56 {
		if d.marker >= 0 {
			d.acc <<= 8
			d.n += 8
			d.fed++
			continue
		}
		b, ok := s.byteAt(d.pos)
		if !ok {
			d.marker, d.mpos = endOfData, d.pos
			continue
		}
		if b == 0xFF {
			nb, ok := s.byteAt(d.pos + 1)
			if !ok {
				d.marker, d.mpos = endOfData, d.pos
				continue
			}
			if nb == 0x00 {
				d.pos += 2
				d.acc = d.acc<<8 | 0xFF
				d.n += 8
				d.zeros = 0
				continue
			}
			if nb == 0xFF { // fill bytes before a marker
				q, run := d.pos+1, 1
				for {
					x, ok := s.byteAt(q + 1)
					if !ok || x != 0xFF {
						break
					}
					q++
					if run++; run > 64 {
						d.err = errErased
						break
					}
				}
				if d.err != nil {
					d.marker, d.mpos = endOfData, d.pos
					continue
				}
				d.pos = q
				continue
			}
			d.marker, d.mpos = int(nb), d.pos
			continue
		}
		if b == 0 {
			if d.zeros++; d.zeros >= 48 {
				d.err = errZeros
				d.marker, d.mpos = endOfData, d.pos
				continue
			}
		} else {
			d.zeros = 0
		}
		d.pos++
		d.acc = d.acc<<8 | uint64(b)
		d.n += 8
	}
}

func (d *dec) bits(s *source, k uint) int32 {
	if d.n < k {
		d.fill(s)
	}
	v := int32(d.acc>>(d.n-k)) & (1<<k - 1)
	d.n -= k
	return v
}

func (d *dec) decode(s *source, h *huff) (byte, error) {
	if d.n < 16 {
		d.fill(s)
	}
	if e := h.look[(d.acc>>(d.n-9))&0x1FF]; e != 0 {
		d.n -= uint(e >> 8)
		return byte(e), nil
	}
	code := int32(0)
	for l := 1; l <= 16; l++ {
		code = code<<1 | int32((d.acc>>(d.n-uint(l)))&1)
		if code <= h.maxcode[l] {
			d.n -= uint(l)
			return h.vals[h.valptr[l]+code-h.mincode[l]], nil
		}
	}
	return 0, errCode
}

func extend(v int32, s uint) int32 {
	if v < 1<<(s-1) {
		return v - (1 << s) + 1
	}
	return v
}

// mcuStep decodes one MCU. The brightness of each luma block goes into
// dcRow; with trackDC the change from the block above is added up.
func (d *dec) mcuStep(info *Info, s *source) error {
	mx := d.mcu % info.MCUsX
	var seen [4]int
	base := 0
	var bases [4]int
	for i, n := range info.cols {
		bases[i] = base
		base += n
	}
	for _, ci := range info.blocks {
		c := &info.comps[ci]
		t, err := d.decode(s, info.dc[c.td])
		if err != nil {
			return err
		}
		if t > 11 {
			return errValue
		}
		if t > 0 {
			d.pred[ci] += extend(d.bits(s, uint(t)), uint(t))
			if d.pred[ci] < -2048 || d.pred[ci] > 2047 {
				return errValue
			}
		}
		k := seen[ci]
		seen[ci]++
		col := bases[ci] + mx*c.h + k%c.h
		if d.trackDC && (d.mcu >= info.MCUsX || k >= c.h) {
			diff := d.pred[ci] - d.dcRow[col]
			if diff < 0 {
				diff = -diff
			}
			d.dcSum += float64(diff)
			d.dcN++
		}
		d.dcRow[col] = d.pred[ci]
		for k := 1; k < 64; {
			rs, err := d.decode(s, info.ac[c.ta])
			if err != nil {
				return err
			}
			r, sz := int(rs>>4), uint(rs&15)
			if sz == 0 {
				if r == 15 {
					if k += 16; k > 64 {
						return errOverflow
					}
					continue
				}
				if r == 0 {
					break
				}
				return errValue
			}
			k += r
			if k > 63 || sz > 10 {
				return errOverflow
			}
			d.bits(s, sz)
			k++
		}
	}
	if d.err != nil {
		return d.err
	}
	if d.fed > 0 && int(d.n) < 8*d.fed { // used bits that are not in the file
		if d.marker == endOfData {
			return errEnd
		}
		return errMarker
	}
	d.mcu++
	d.inIntv++
	return nil
}

// boundary checks the restart marker after a restart interval, or the end
// of the image after the last MCU. Elsewhere it does nothing.
func (d *dec) boundary(info *Info, s *source) error {
	last := d.mcu >= info.MCUsX*info.MCUsY
	if !last && (info.Restart == 0 || d.inIntv < info.Restart) {
		return nil
	}
	if d.marker < 0 {
		d.fill(s)
	}
	if d.err != nil {
		return d.err
	}
	real := int(d.n) - 8*d.fed
	if d.marker < 0 || real > 7 {
		return errMarker
	}
	if d.marker == endOfData {
		if last && d.err == nil {
			return nil // the picture is complete; the end marker is missing
		}
		return errEnd
	}
	if last {
		if d.marker != 0xD9 {
			return errMarker
		}
		return nil
	}
	if d.marker != 0xD0+d.rst {
		return errMarker
	}
	d.pos = d.mpos + 2
	d.acc, d.n, d.fed, d.marker = 0, 0, 0, -1
	d.pred = [4]int32{}
	d.inIntv = 0
	d.rst = (d.rst + 1) & 7
	return nil
}

// run decodes until the picture is complete, an error, or until `upTo`
// MCUs are done (0 = no limit).
func (d *dec) run(info *Info, s *source, upTo int) error {
	total := info.MCUsX * info.MCUsY
	for {
		if err := d.boundary(info, s); err != nil {
			return err
		}
		if d.mcu >= total || (upTo > 0 && d.mcu >= upTo) {
			return nil
		}
		if err := d.mcuStep(info, s); err != nil {
			return err
		}
	}
}

// Result says how much of a JPEG is intact.
type Result struct {
	OK       bool
	MCUs     int   // MCUs that decode correctly
	Total    int   // MCUs in the whole picture
	ErrAt    int64 // where the data stops making sense (file offset)
	End      int64 // where the compressed data ends (after the end marker)
	Wiped    bool  // the bad part is zeros or erased flash
	Why      string
	GoodRows int // complete MCU rows that are intact
	rowCut   cut // where the last intact MCU row ends
	jumped   bool
	recs     []mcuRec // per MCU (no restart markers): where it starts, how well it fits
	w        int
}

// cut is a position in the compressed data: the byte that holds the next
// unread bit and how many of its bits are already used.
type cut struct {
	off  int64
	used uint
	ok   bool
}

// Check follows the whole compressed data of a JPEG.
func Check(info *Info, r io.ReaderAt, size int64) Result {
	s := &source{r: r, limit: size, buf: make([]byte, 0, 64<<10)}
	d := newDec(info)
	d.trackDC = true
	total := info.MCUsX * info.MCUsY
	res := Result{Total: total}
	// without restart markers, keep track of each MCU to find later where
	// the picture stopped continuing
	var recs []mcuRec
	if info.Restart == 0 {
		recs = make([]mcuRec, 0, total)
	}
	lastGood := 0 // MCUs up to the last confirmed restart marker
	rows := 0
	rowCuts := []cut{{}}
	var err error
	for {
		if err = d.boundary(info, s); err != nil {
			break
		}
		if info.Restart > 0 && d.inIntv == 0 {
			lastGood = d.mcu
		}
		if d.mcu > 0 && d.mcu%info.MCUsX == 0 && d.mcu/info.MCUsX > rows {
			rows = d.mcu / info.MCUsX
			rowCuts = append(rowCuts, d.position(s))
		}
		if d.mcu >= total {
			break
		}
		var at int64
		sum, n := d.dcSum, d.dcN
		if recs != nil {
			at = d.position(s).off
		}
		if err = d.mcuStep(info, s); err != nil {
			break
		}
		if recs != nil {
			recs = append(recs, mcuRec{at, float32(d.dcSum - sum), uint16(d.dcN - n)})
		}
	}
	if d.mcu >= total && err == nil {
		res.OK = true
		res.MCUs, res.GoodRows = total, info.MCUsY
		res.End = size
		if d.marker == 0xD9 {
			res.End = d.mpos + 2
		}
		return res
	}
	if err == nil {
		err = errEnd
	}
	res.Why = err.Error()
	res.Wiped = err == errZeros || err == errErased
	res.ErrAt = d.pos
	res.recs, res.w = recs, info.MCUsX
	if info.Restart > 0 {
		res.MCUs = lastGood
	} else if m := jump(recs, info.MCUsX); m >= 0 {
		// the data went on decoding, but the picture stops continuing here
		res.MCUs, res.ErrAt, res.jumped = m, recs[m].at, true
		res.Why = "the picture does not continue: from here on the data belongs to something else"
	} else {
		// damage is noticed a little after it starts
		res.MCUs = max(0, d.mcu-info.MCUsX/4)
	}
	rows = min(rows, res.MCUs/info.MCUsX)
	res.GoodRows = rows
	if rows < len(rowCuts) {
		res.rowCut = rowCuts[rows]
	}
	return res
}

type mcuRec struct {
	at  int64   // where the MCU's data starts
	sum float32 // brightness and colour change from the blocks above
	n   uint16
}

// jump finds the first MCU after which, for a whole row, the blocks no
// longer match the ones above them: where a photo's data was replaced by
// another's. It returns -1 if there is no such place.
func jump(recs []mcuRec, w int) int {
	if len(recs) < 3*w {
		return -1
	}
	cs := make([]float64, len(recs)+1)
	cn := make([]float64, len(recs)+1)
	for i, r := range recs {
		cs[i+1] = cs[i] + float64(r.sum)
		cn[i+1] = cn[i] + float64(r.n)
	}
	avg := func(a, b int) float64 { return (cs[b] - cs[a]) / max(cn[b]-cn[a], 1) }
	for m := 2 * w; m+8 <= len(recs); m++ {
		base := avg(m-2*w, m)
		limit := max(3.5*base, base+24)
		if avg(m, min(len(recs), m+w)) > limit && avg(m, m+8) > limit {
			return m
		}
	}
	return -1
}

// guesses ranks the cluster boundaries before the point of failure by how
// suddenly the picture changes there: where a piece of another file may
// begin even though its data decodes without errors.
type guess struct {
	b     int64
	ratio float64
}

func (r Result) guesses(cs int64, n int) []guess {
	recs, w := r.recs, r.w
	if len(recs) < 3*w || w == 0 {
		return nil
	}
	cs1 := make([]float64, len(recs)+1)
	cn := make([]float64, len(recs)+1)
	for i, x := range recs {
		cs1[i+1] = cs1[i] + float64(x.sum)
		cn[i+1] = cn[i] + float64(x.n)
	}
	avg := func(a, b int) float64 { return (cs1[b] - cs1[a]) / max(cn[b]-cn[a], 1) }
	var all []guess
	m := 2 * w
	for b := (recs[m].at/cs + 1) * cs; ; b += cs {
		for m < len(recs) && recs[m].at < b {
			m++
		}
		if m+w/2 > len(recs) {
			break
		}
		before := avg(m-2*w, m-1)
		after := avg(m, min(len(recs), m+w))
		if ratio := after / max(before, 2); ratio > 1.3 {
			all = append(all, guess{b, ratio})
		}
	}
	sort.Slice(all, func(i, k int) bool { return all[i].ratio > all[k].ratio })
	return all[:min(n, len(all))]
}

// position returns where the next unread bit is in the file.
func (d *dec) position(s *source) cut {
	realBits := int(d.n) - 8*d.fed
	p := d.pos
	if d.marker >= 0 {
		p = d.mpos
	}
	if realBits <= 0 {
		return cut{off: p, ok: true}
	}
	for i := 0; i < realBits/8; i++ {
		p = prevDataByte(s, p)
	}
	if realBits%8 == 0 {
		return cut{off: p, ok: true}
	}
	p = prevDataByte(s, p)
	return cut{off: p, used: uint(8 - realBits%8), ok: true}
}

// prevDataByte returns the start of the data byte that ends just before p.
func prevDataByte(s *source, p int64) int64 {
	if p >= 2 {
		a, _ := s.byteAt(p - 2)
		b, _ := s.byteAt(p - 1)
		if a == 0xFF && b == 0x00 {
			return p - 2
		}
	}
	return p - 1
}

func (r Result) String() string {
	if r.OK {
		return "complete"
	}
	return fmt.Sprintf("intact up to %d of %d blocks (%s at %d)", r.MCUs, r.Total, r.Why, r.ErrAt)
}
