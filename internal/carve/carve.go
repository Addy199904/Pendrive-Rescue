// Package carve recovers files by their content alone ("deep scan"): it
// takes the places where the scan saw a known file type begin, works out
// exactly how long each file is by walking its internal structure, and
// reports files that can be copied out.
package carve

import (
	"encoding/binary"
	"io"
	"sort"
	"time"

	"pendriverescue/internal/disk"
	"pendriverescue/internal/scan"
	"pendriverescue/internal/sig"
)

// Result is a file found by its content.
type Result struct {
	Kind     sig.Kind
	Off      int64
	Size     int64
	Ext      string
	Time     time.Time // date stored inside the file (photo taken, video recorded ...)
	Title    string    // a name stored inside the file, if any
	Complete bool      // the file's structure was followed to its end
	// Estimated: the format does not record its length, so the end is a
	// best guess (the file usually still opens).
	Estimated bool
	inner     []span // areas holding embedded content (e.g. JPEG thumbnails)
}

type span struct{ off, end int64 }

// Reader is what the carver reads from.
type Reader interface {
	ReadAt(p []byte, off int64) (int, error)
	Size() int64
}

type carver struct {
	r     Reader
	size  int64
	hits  []scan.Hit
	err   error
	scrap []byte
}

func (c *carver) read(off int64, n int) []byte {
	if n <= 0 {
		return nil
	}
	if cap(c.scrap) < n {
		c.scrap = make([]byte, n)
	}
	b := c.scrap[:n]
	if _, err := c.r.ReadAt(b, off); err != nil {
		c.err = err
		clear(b)
	}
	return b
}

func (c *carver) readNew(off int64, n int) []byte {
	b := make([]byte, n)
	if _, err := c.r.ReadAt(b, off); err != nil {
		c.err = err
	}
	return b
}

func (c *carver) u8(off int64) byte     { return c.read(off, 1)[0] }
func (c *carver) be16(off int64) uint16 { return binary.BigEndian.Uint16(c.read(off, 2)) }
func (c *carver) be32(off int64) uint32 { return binary.BigEndian.Uint32(c.read(off, 4)) }
func (c *carver) be64(off int64) uint64 { return binary.BigEndian.Uint64(c.read(off, 8)) }
func (c *carver) le16(off int64) uint16 { return binary.LittleEndian.Uint16(c.read(off, 2)) }
func (c *carver) le32(off int64) uint32 { return binary.LittleEndian.Uint32(c.read(off, 4)) }
func (c *carver) le64(off int64) uint64 { return binary.LittleEndian.Uint64(c.read(off, 8)) }

// nextHit returns the offset of the first hit after off (or the drive end).
func (c *carver) nextHit(off int64) int64 {
	i := sort.Search(len(c.hits), func(i int) bool { return c.hits[i].Off > off })
	if i < len(c.hits) {
		return c.hits[i].Off
	}
	return c.size
}

// seq is a buffered forward reader for byte-by-byte parsing.
type seq struct {
	c      *carver
	pos    int64 // absolute position of next byte
	limit  int64
	buf    []byte
	bufOff int64
	bufLen int
}

func (c *carver) seq(off, limit int64) *seq {
	if limit > c.size {
		limit = c.size
	}
	return &seq{c: c, pos: off, limit: limit, buf: make([]byte, 256<<10), bufOff: -1}
}

func (s *seq) fill() bool {
	if s.pos >= s.limit {
		return false
	}
	n := int64(len(s.buf))
	if s.pos+n > s.limit {
		n = s.limit - s.pos
	}
	if _, err := s.c.r.ReadAt(s.buf[:n], s.pos); err != nil {
		s.c.err = err
		return false
	}
	s.bufOff = s.pos
	s.bufLen = int(n)
	return true
}

func (s *seq) inBuf() bool {
	return s.bufOff >= 0 && s.pos >= s.bufOff && s.pos < s.bufOff+int64(s.bufLen)
}

// byte returns the next byte; ok=false at the limit.
func (s *seq) byte() (byte, bool) {
	if !s.inBuf() && !s.fill() {
		return 0, false
	}
	b := s.buf[s.pos-s.bufOff]
	s.pos++
	return b, true
}

// bytes returns the next n bytes (copied).
func (s *seq) bytes(n int) ([]byte, bool) {
	if s.pos+int64(n) > s.limit {
		return nil, false
	}
	out := make([]byte, n)
	if s.inBuf() && s.pos+int64(n) <= s.bufOff+int64(s.bufLen) {
		copy(out, s.buf[s.pos-s.bufOff:])
	} else if _, err := s.c.r.ReadAt(out, s.pos); err != nil {
		s.c.err = err
		return nil, false
	}
	s.pos += int64(n)
	return out, true
}

func (s *seq) skip(n int64) bool {
	s.pos += n
	return s.pos <= s.limit
}

// findByte advances to the next occurrence of b; ok=false if not found.
func (s *seq) findByte(b byte) bool {
	for {
		if !s.inBuf() && !s.fill() {
			return false
		}
		chunk := s.buf[s.pos-s.bufOff : s.bufLen]
		for i, x := range chunk {
			if x == b {
				s.pos += int64(i)
				return true
			}
		}
		s.pos += int64(len(chunk))
	}
}

// Max sizes per type (also never past the end of the drive).
var maxSize = map[sig.Kind]int64{
	sig.JPEG: 256 << 20, sig.PNG: 512 << 20, sig.GIF: 128 << 20, sig.BMP: 1 << 30,
	sig.TIFF: 2 << 30, sig.ORF: 2 << 30, sig.RW2: 2 << 30, sig.ISOBMFF: 64 << 30,
	sig.RIFF: 64 << 30, sig.EBML: 64 << 30, sig.ASF: 64 << 30, sig.FLV: 16 << 30,
	sig.MP3: 1 << 30, sig.OGG: 4 << 30, sig.FLAC: 4 << 30, sig.AMR: 512 << 20,
	sig.PDF: 2 << 30, sig.ZIP: 16 << 30, sig.OLE: 2 << 30, sig.RAR: 64 << 30,
	sig.SEVENZ: 64 << 30, sig.SQLITE: 8 << 30, sig.PSD: 8 << 30, sig.MPEGPS: 32 << 30,
	sig.MPEGTS: 64 << 30, sig.M2TS: 64 << 30, sig.GZIP: 8 << 30, sig.RTF: 512 << 20,
	sig.TAR: 64 << 30, sig.ISO9660: 64 << 30, sig.MIDI: 64 << 20, sig.AIFF: 4 << 30,
	sig.DJVU: 2 << 30, sig.PST: 64 << 30, sig.DWG: 512 << 20, sig.AAC: 1 << 30,
	sig.EPS: 1 << 30, sig.EPSBIN: 1 << 30, sig.KDBX: 256 << 20, sig.RM: 16 << 30, sig.CAB: 4 << 30,
}

// one carves the file of the given kind that starts at off.
func (c *carver) one(off int64, kind sig.Kind) (res Result, ok bool) {
	defer func() {
		if p := recover(); p != nil {
			ok = false
		}
	}()
	limit := off + maxSize[kind]
	if limit > c.size || limit < off {
		limit = c.size
	}
	res = Result{Kind: kind, Off: off, Ext: kind.Ext()}
	switch kind {
	case sig.JPEG:
		ok = c.jpeg(&res, limit)
	case sig.PNG:
		ok = c.png(&res, limit)
	case sig.GIF:
		ok = c.gif(&res, limit)
	case sig.BMP:
		ok = c.bmp(&res, limit)
	case sig.TIFF, sig.ORF, sig.RW2:
		ok = c.tiff(&res, limit)
	case sig.PSD:
		ok = c.psd(&res, limit)
	case sig.ISOBMFF:
		ok = c.isobmff(&res, limit)
	case sig.RIFF:
		ok = c.riff(&res, limit)
	case sig.EBML:
		ok = c.ebml(&res, limit)
	case sig.ASF:
		ok = c.asf(&res, limit)
	case sig.FLV:
		ok = c.flv(&res, limit)
	case sig.MP3:
		ok = c.mp3(&res, limit)
	case sig.OGG:
		ok = c.ogg(&res, limit)
	case sig.FLAC:
		ok = c.flac(&res, limit)
	case sig.AMR:
		ok = c.amr(&res, limit)
	case sig.MPEGPS:
		ok = c.mpegps(&res, limit)
	case sig.MPEGTS:
		ok = c.mpegts(&res, limit, 188, 0)
	case sig.M2TS:
		ok = c.mpegts(&res, limit, 192, 4)
	case sig.PDF:
		ok = c.pdf(&res, limit)
	case sig.ZIP:
		ok = c.zip(&res, limit)
	case sig.OLE:
		ok = c.ole(&res, limit)
	case sig.RTF:
		ok = c.rtf(&res, limit)
	case sig.RAR:
		ok = c.rar(&res, limit)
	case sig.SEVENZ:
		ok = c.sevenz(&res, limit)
	case sig.GZIP:
		ok = c.gzip(&res, limit)
	case sig.TAR:
		ok = c.tar(&res, limit)
	case sig.ISO9660:
		ok = c.iso(&res, limit)
	case sig.SQLITE:
		ok = c.sqlite(&res, limit)
	case sig.MIDI:
		ok = c.midi(&res, limit)
	case sig.AIFF:
		ok = c.aiff(&res, limit)
	case sig.DJVU:
		ok = c.djvu(&res, limit)
	case sig.PST:
		ok = c.pst(&res, limit)
	case sig.DWG, sig.KDBX:
		ok = c.estimated(&res, limit)
	case sig.AAC:
		ok = c.aac(&res, limit)
	case sig.EPS:
		ok = c.eps(&res, limit)
	case sig.EPSBIN:
		ok = c.epsbin(&res, limit)
	case sig.RM:
		ok = c.realmedia(&res, limit)
	case sig.CAB:
		ok = c.cab(&res, limit)
	}
	if c.err != nil {
		return res, false
	}
	if ok && (res.Size <= 0 || res.Off+res.Size > c.size) {
		if res.Size > 0 && res.Off < c.size {
			res.Size = c.size - res.Off
			res.Complete = false
		} else {
			ok = false
		}
	}
	return res, ok
}

// Kinds whose inner sector-aligned hits are just parts of the same stream.
var streamKind = map[sig.Kind]bool{
	sig.MPEGTS: true, sig.M2TS: true, sig.MPEGPS: true, sig.OGG: true, sig.MP3: true, sig.AMR: true, sig.AAC: true,
}

// Inspect runs a format's structure check on a single file (offset 0 of r),
// for verifying recovered files. ok=false means the structure is broken.
func Inspect(r Reader, kind sig.Kind) (Result, bool) {
	c := &carver{r: r, size: r.Size()}
	res, ok := c.one(0, kind)
	if c.err != nil {
		return res, false
	}
	return res, ok
}

// Kinds whose contents are other files stored in aligned blocks.
var containerKind = map[sig.Kind]bool{sig.TAR: true, sig.ISO9660: true}

// Options control which hits are carved.
type Options struct {
	// Skip reports whether the hit at off belongs to a file that was already
	// recovered with its name (so it would only be a duplicate).
	Skip func(off int64, kind sig.Kind) bool
	// Progress is called with (done, total) hits.
	Progress func(done, total int)
}

// Run carves every hit and returns the files found, in drive order.
func Run(r *disk.Reader, hits []scan.Hit, opt Options) ([]Result, error) {
	c := &carver{r: r, size: r.Size(), hits: hits}
	var out []Result
	outerEnd := int64(-1)
	var outerKind sig.Kind
	var outerInner []span
	last := time.Now()
	for i, h := range hits {
		if opt.Progress != nil && (i%256 == 0 && time.Since(last) > 300*time.Millisecond) {
			opt.Progress(i, len(hits))
			last = time.Now()
		}
		if opt.Skip != nil && opt.Skip(h.Off, h.Kind) {
			continue
		}
		inside := h.Off < outerEnd
		if inside {
			if containerKind[outerKind] {
				continue
			}
			if streamKind[h.Kind] && h.Kind == outerKind {
				continue
			}
			emb := false
			for _, s := range outerInner {
				if h.Off >= s.off && h.Off < s.end {
					emb = true
					break
				}
			}
			if emb {
				continue
			}
		}
		res, ok := c.one(h.Off, h.Kind)
		if c.err != nil {
			if c.err == disk.ErrDeviceGone {
				return out, c.err
			}
			c.err = nil
			continue
		}
		if !ok {
			continue
		}
		end := res.Off + res.Size
		if inside && h.Kind == outerKind && end <= outerEnd {
			continue // an embedded copy (e.g. a thumbnail) inside the previous file
		}
		out = append(out, res)
		if end > outerEnd {
			outerEnd, outerKind, outerInner = end, h.Kind, res.inner
		}
	}
	if opt.Progress != nil {
		opt.Progress(len(hits), len(hits))
	}
	return out, nil
}

// CopyTo writes a carved file to w.
func CopyTo(r *disk.Reader, res Result, w io.Writer) error {
	buf := make([]byte, 1<<20)
	for p := int64(0); p < res.Size; {
		n := int64(len(buf))
		if p+n > res.Size {
			n = res.Size - p
		}
		if _, err := r.ReadAt(buf[:n], res.Off+p); err != nil {
			return err
		}
		if _, err := w.Write(buf[:n]); err != nil {
			return err
		}
		p += n
	}
	return nil
}
