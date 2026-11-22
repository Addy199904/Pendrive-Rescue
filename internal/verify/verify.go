// Package verify checks recovered files from end to end, so the user is
// told which ones are good and which are damaged before opening them.
package verify

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"regexp"
	"strconv"
	"strings"

	"pendriverescue/internal/carve"
	"pendriverescue/internal/sig"
)

// Cond is a file's condition.
type Cond string

const (
	Good    Cond = "good"
	Damaged Cond = "damaged"
	Unknown Cond = "unknown" // no check exists for this type
	Erased  Cond = "erased"  // only zeros left
)

// Result of a check.
type Result struct {
	Cond Cond
	Note string
}

// File is the content of one recovered file.
type File interface {
	ReadAt(p []byte, off int64) (int, error)
	Size() int64
}

type section struct{ f File }

func (s section) ReadAt(p []byte, off int64) (int, error) {
	if off >= s.f.Size() {
		return 0, io.EOF
	}
	if off+int64(len(p)) > s.f.Size() {
		n := int(s.f.Size() - off)
		k, err := s.f.ReadAt(p[:n], off)
		if err == nil {
			err = io.EOF
		}
		return k, err
	}
	return s.f.ReadAt(p, off)
}

func damaged(note string) Result { return Result{Damaged, note} }

// textExt are plain text types.
var textExt = map[string]bool{
	"txt": true, "csv": true, "tsv": true, "html": true, "htm": true, "xml": true, "json": true, "vcf": true,
	"ics": true, "eml": true, "svg": true, "md": true, "log": true, "ini": true, "c": true, "h": true, "cpp": true,
	"py": true, "java": true, "js": true, "css": true, "sh": true, "php": true, "srt": true, "bat": true,
}

var zipExt = map[string]bool{
	"zip": true, "docx": true, "xlsx": true, "pptx": true, "odt": true, "ods": true, "odp": true, "epub": true,
	"apk": true, "jar": true, "xps": true, "vsdx": true,
}

// Check verifies a file of the given extension. maxDecode limits how big a
// photo may be for a full decode (0 = no limit).
func Check(ext string, f File, maxDecode int64) Result {
	size := f.Size()
	if size == 0 {
		return Result{Good, ""}
	}
	if erasedSample(f) {
		return Result{Erased, "only zeros left"}
	}
	ext = strings.ToLower(ext)
	sr := io.NewSectionReader(section{f}, 0, size)
	head := make([]byte, 1024)
	n, _ := f.ReadAt(head[:min(int64(len(head)), size)], 0)
	head = head[:n]
	if m, known := sig.ExtMatches(ext, head); known && !m {
		return damaged("the start of the file is not a " + strings.ToUpper(ext) + " file (overwritten)")
	}
	switch {
	case ext == "jpg" || ext == "jpeg" || ext == "jfif" || ext == "jpe":
		res, ok := carve.Inspect(f, sig.JPEG)
		if !ok {
			return damaged("the picture data is broken")
		}
		if !res.Complete {
			return damaged("the end of the picture is missing")
		}
		if maxDecode == 0 || size <= maxDecode {
			if _, err := jpeg.Decode(sr); err != nil {
				var u jpeg.UnsupportedError
				if errors.As(err, &u) {
					return Result{Good, ""}
				}
				return damaged("the picture data is broken")
			}
		}
		return Result{Good, ""}
	case ext == "png":
		if maxDecode != 0 && size > maxDecode {
			res, ok := carve.Inspect(f, sig.PNG)
			if !ok || !res.Complete {
				return damaged("the picture data is broken")
			}
			return Result{Good, ""}
		}
		if _, err := png.Decode(sr); err != nil {
			var u png.UnsupportedError
			if errors.As(err, &u) {
				return Result{Unknown, ""}
			}
			return damaged("the picture data is broken")
		}
		return Result{Good, ""}
	case ext == "gif":
		if _, err := gif.DecodeAll(sr); err != nil {
			return damaged("the picture data is broken")
		}
		return Result{Good, ""}
	case zipExt[ext] || ext == "cdr" && len(head) > 2 && head[0] == 'P':
		return checkZip(sr, size)
	case ext == "pdf":
		return checkPDF(f)
	case ext == "gz" || ext == "tgz":
		zr, err := gzip.NewReader(sr)
		if err != nil {
			return damaged("the archive is broken")
		}
		if _, err := io.Copy(io.Discard, io.LimitReader(zr, 4<<30)); err != nil {
			return damaged("the archive contents are corrupted")
		}
		return Result{Good, ""}
	case textExt[ext]:
		return checkText(f)
	}
	kind := sig.KindOfExt(ext)
	if kind == sig.None {
		return Result{Unknown, ""}
	}
	res, ok := carve.Inspect(f, kind)
	if !ok {
		return damaged("the file structure is broken")
	}
	if res.Estimated {
		return Result{Unknown, ""}
	}
	if !res.Complete {
		return damaged("the end of the file is missing")
	}
	if kind == sig.ISOBMFF {
		if ext == "heic" || ext == "heif" || ext == "avif" || ext == "hif" {
			return checkHEIF(f)
		}
		if r := checkMP4Offsets(f); r.Cond == Damaged {
			return r
		}
	}
	return Result{Good, ""}
}

// erasedSample reports whether the file looks wiped (all zeros) by
// sampling its start, middle and end.
func erasedSample(f File) bool {
	size := f.Size()
	buf := make([]byte, 4096)
	for _, p := range []int64{0, size / 3, size / 2, 2 * size / 3, size - 4096} {
		if p < 0 {
			p = 0
		}
		n := min(int64(len(buf)), size-p)
		if n <= 0 {
			continue
		}
		f.ReadAt(buf[:n], p)
		for _, c := range buf[:n] {
			if c != 0 {
				return false
			}
		}
	}
	return true
}

func checkZip(sr *io.SectionReader, size int64) Result {
	zr, err := zip.NewReader(sr, size)
	if err != nil {
		return damaged("the file's index is missing or broken")
	}
	var total int64
	for _, zf := range zr.File {
		if total > 1<<30 {
			break
		}
		rc, err := zf.Open()
		if err != nil {
			if errors.Is(err, zip.ErrAlgorithm) {
				continue
			}
			return damaged("part of the contents is corrupted")
		}
		n, err := io.Copy(io.Discard, rc)
		rc.Close()
		total += n
		if err != nil {
			return damaged("part of the contents is corrupted")
		}
	}
	return Result{Good, ""}
}

var reXrefTarget = regexp.MustCompile(`^\s*(xref|\d+\s+\d+\s+obj)`)

func checkPDF(f File) Result {
	res, ok := carve.Inspect(f, sig.PDF)
	if !ok || !res.Complete {
		return damaged("the end of the PDF is missing")
	}
	size := f.Size()
	n := min(int64(2048), size)
	tail := make([]byte, n)
	f.ReadAt(tail, size-n)
	i := bytes.LastIndex(tail, []byte("startxref"))
	if i < 0 {
		return damaged("the PDF index is missing")
	}
	fields := strings.Fields(string(tail[i+9:]))
	if len(fields) == 0 {
		return damaged("the PDF index is missing")
	}
	x, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil || x <= 0 || x >= size {
		return damaged("the PDF index points outside the file")
	}
	at := make([]byte, 40)
	f.ReadAt(at[:min(int64(40), size-x)], x)
	if !reXrefTarget.Match(at) {
		return damaged("the PDF index points to the wrong place (data overwritten)")
	}
	return Result{Good, ""}
}

func checkText(f File) Result {
	size := f.Size()
	n := min(size, 256<<10)
	b := make([]byte, n)
	f.ReadAt(b, 0)
	binary := 0
	for _, c := range b {
		if c < 0x09 || (c > 0x0D && c < 0x20 && c != 0x1A) {
			binary++
		}
	}
	// UTF-16 text has many zero bytes in a regular pattern
	if binary*100 > len(b) && !looksUTF16(b) {
		return damaged("contains data that is not text (probably overwritten)")
	}
	return Result{Good, ""}
}

func looksUTF16(b []byte) bool {
	if len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE {
		return true
	}
	z := 0
	for i := 1; i < len(b); i += 2 {
		if b[i] == 0 {
			z++
		}
	}
	return z*3 > len(b)
}

// checkMP4Offsets makes sure the video's chunk table points inside the file.
func checkMP4Offsets(f File) Result {
	size := f.Size()
	var maxOff uint64
	var walk func(off, end int64, depth int)
	walk = func(off, end int64, depth int) {
		h := make([]byte, 16)
		for off+8 <= end && depth < 8 {
			f.ReadAt(h, off)
			sz := int64(binary.BigEndian.Uint32(h[0:4]))
			typ := string(h[4:8])
			hl := int64(8)
			if sz == 1 {
				sz = int64(binary.BigEndian.Uint64(h[8:16]))
				hl = 16
			}
			if sz == 0 {
				sz = end - off
			}
			if sz < hl || off+sz > end {
				return
			}
			switch typ {
			case "moov", "trak", "mdia", "minf", "stbl", "edts":
				walk(off+hl, off+sz, depth+1)
			case "stco", "co64":
				cnt := int64(0)
				b := make([]byte, 8)
				f.ReadAt(b, off+hl)
				cnt = int64(binary.BigEndian.Uint32(b[4:8]))
				w := int64(4)
				if typ == "co64" {
					w = 8
				}
				if cnt > 0 && cnt < 4<<20 && off+hl+8+cnt*w <= off+sz {
					tbl := make([]byte, cnt*w)
					f.ReadAt(tbl, off+hl+8)
					for i := int64(0); i < cnt; i++ {
						var v uint64
						if w == 4 {
							v = uint64(binary.BigEndian.Uint32(tbl[i*4:]))
						} else {
							v = binary.BigEndian.Uint64(tbl[i*8:])
						}
						if v > maxOff {
							maxOff = v
						}
					}
				}
			}
			off += sz
		}
	}
	walk(0, size, 0)
	if maxOff >= uint64(size) {
		return damaged("the video's data table points outside the file")
	}
	return Result{Good, ""}
}

// checkHEIF checks an iPhone-style photo (HEIC, HEIF, AVIF): the picture is
// stored as items whose places are listed in the "iloc" table. Every item
// must lie inside the file, and the picture data must not be wiped.
func checkHEIF(f File) Result {
	size := f.Size()
	box := func(off, end int64) (typ string, hl, sz int64, ok bool) {
		h := make([]byte, 16)
		if off+8 > end {
			return "", 0, 0, false
		}
		f.ReadAt(h, off)
		sz = int64(binary.BigEndian.Uint32(h[0:4]))
		typ = string(h[4:8])
		hl = 8
		if sz == 1 {
			sz = int64(binary.BigEndian.Uint64(h[8:16]))
			hl = 16
		}
		if sz == 0 {
			sz = end - off
		}
		return typ, hl, sz, sz >= hl && off+sz <= end
	}
	var meta, iloc [2]int64 // start, end
	for off := int64(0); off < size; {
		typ, hl, sz, ok := box(off, size)
		if !ok {
			return damaged("the photo's structure is broken")
		}
		if typ == "meta" {
			meta = [2]int64{off + hl + 4, off + sz} // full box: 4 bytes of version and flags
		}
		off += sz
	}
	if meta[1] == 0 {
		return damaged("the photo's description (meta) is missing")
	}
	for off := meta[0]; off < meta[1]; {
		typ, hl, sz, ok := box(off, meta[1])
		if !ok {
			return damaged("the photo's description is broken")
		}
		if typ == "iloc" {
			iloc = [2]int64{off + hl, off + sz}
		}
		off += sz
	}
	if iloc[1] == 0 {
		return damaged("the table of the photo's parts (iloc) is missing")
	}
	t := make([]byte, min(iloc[1]-iloc[0], 1<<20))
	f.ReadAt(t, iloc[0])
	p := 0
	need := func(n int) bool { return p+n <= len(t) }
	read := func(n int) uint64 {
		var v uint64
		for i := 0; i < n; i++ {
			v = v<<8 | uint64(t[p+i])
		}
		p += n
		return v
	}
	if !need(6) {
		return damaged("the table of the photo's parts is broken")
	}
	version := t[0]
	p = 4
	sizes := read(2)
	offSize, lenSize, baseSize := int(sizes>>12&15), int(sizes>>8&15), int(sizes>>4&15)
	idxSize := 0
	if version == 1 || version == 2 {
		idxSize = int(sizes & 15)
	}
	countSize, idSize := 2, 2
	if version == 2 {
		countSize, idSize = 4, 4
	}
	if !need(countSize) {
		return damaged("the table of the photo's parts is broken")
	}
	items := read(countSize)
	var dataFrom, dataTo uint64
	for i := uint64(0); i < items; i++ {
		if !need(idSize + 2) {
			return damaged("the table of the photo's parts is broken")
		}
		read(idSize)
		method := uint64(0)
		if version == 1 || version == 2 {
			method = read(2) & 15
		}
		if !need(2 + baseSize + 2) {
			return damaged("the table of the photo's parts is broken")
		}
		read(2) // data reference
		base := read(baseSize)
		ext := read(2)
		for k := uint64(0); k < ext; k++ {
			if !need(idxSize + offSize + lenSize) {
				return damaged("the table of the photo's parts is broken")
			}
			read(idxSize)
			eo, el := read(offSize), read(lenSize)
			if method != 0 {
				continue // stored inside the description, not in the file's data
			}
			a := base + eo
			if a+el > uint64(size) {
				return damaged("the end of the photo is missing")
			}
			if el > 0 && (dataTo == 0 || a < dataFrom) {
				dataFrom = a
			}
			dataTo = max(dataTo, a+el)
		}
	}
	if dataTo > dataFrom {
		// the picture data should not be all zeros or erased flash
		b := make([]byte, 4096)
		blank := true
		for _, at := range []uint64{dataFrom, (dataFrom + dataTo) / 2, dataTo - min(dataTo-dataFrom, 4096)} {
			n := min(uint64(len(b)), dataTo-at)
			f.ReadAt(b[:n], int64(at))
			for _, c := range b[:n] {
				if c != 0 && c != 0xFF {
					blank = false
					break
				}
			}
		}
		if blank {
			return damaged("the picture data was wiped")
		}
	}
	return Result{Good, ""}
}
