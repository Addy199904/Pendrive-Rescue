package engine

import (
	"archive/zip"
	"bytes"
	"container/list"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"pendriverescue/internal/carve"
	"pendriverescue/internal/disk"
	"pendriverescue/internal/fsys"
	"pendriverescue/internal/jpegfix"
)

// itemReader reads one item's bytes from the drive (or the safe copy).
// Like disk.Reader it never fails for missing data: gaps read as zeros.
type itemReader struct {
	r     *disk.Reader
	parts []fsys.Extent
	data  []byte
	size  int64
	// a rebuilt start (photos with a broken header): head stands in for the
	// item's bytes before from
	head []byte
	from int64
}

func (ir *itemReader) Size() int64 { return ir.size }

func (ir *itemReader) ReadAt(p []byte, off int64) (int, error) {
	if ir.head == nil {
		return ir.readRaw(p, off)
	}
	n := len(p)
	hl := int64(len(ir.head))
	if off < hl {
		k := copy(p, ir.head[off:])
		if k == len(p) {
			return n, nil
		}
		if _, err := ir.readRaw(p[k:], ir.from); err != nil {
			return 0, err
		}
		return n, nil
	}
	return ir.readRaw(p, off-hl+ir.from)
}

// readRaw reads the item's own bytes; with a rebuilt start, past the end
// of the logical size reads as zeros like everywhere else.
func (ir *itemReader) readRaw(p []byte, off int64) (int, error) {
	n := len(p)
	size := ir.size
	if ir.head != nil {
		size = ir.size - int64(len(ir.head)) + ir.from
	}
	if off >= size || off < 0 {
		clear(p)
		return n, nil
	}
	if off+int64(len(p)) > size {
		clear(p[size-off:])
		p = p[:size-off]
	}
	if ir.data != nil {
		k := copy(p, ir.data[min(off, int64(len(ir.data))):])
		clear(p[k:])
		return n, nil
	}
	clear(p)
	pos := int64(0)
	for _, e := range ir.parts {
		if pos >= off+int64(len(p)) {
			break
		}
		if pos+e.Len > off && e.Off >= 0 {
			from := max(off, pos)
			to := min(off+int64(len(p)), pos+e.Len)
			if _, err := ir.r.ReadAt(p[from-off:to-off], e.Off+(from-pos)); err != nil {
				return 0, err
			}
		}
		pos += e.Len
	}
	return n, nil
}

// reader returns a reader over an item (alt = the second cluster layout).
func (s *Session) readerFor(it *Item, alt bool) (*itemReader, error) {
	s.mu.Lock()
	r := s.r
	parts := it.parts
	if alt {
		parts = it.alt
	}
	data, file, size, hdr := it.data, it.file, it.Size, it.hdr
	s.mu.Unlock()
	ir, err := s.rawReaderFor(r, parts, data, file, size)
	if err != nil || hdr == nil || alt {
		return ir, err
	}
	ir.head, ir.from = hdr.Head, hdr.From
	ir.size = size - hdr.From + int64(len(hdr.Head))
	return ir, nil
}

func (s *Session) rawReaderFor(r *disk.Reader, parts []fsys.Extent, data []byte, file *fsys.File, size int64) (*itemReader, error) {
	if data != nil {
		return &itemReader{data: data, size: size}, nil
	}
	if file != nil && file.Compressed {
		if size > 256<<20 {
			return nil, errors.New("this compressed file is too big to preview")
		}
		var buf bytes.Buffer
		if err := copyCompressed(r, file, func(b []byte) error { buf.Write(b); return nil }); err != nil {
			return nil, err
		}
		return &itemReader{data: buf.Bytes(), size: size}, nil
	}
	return &itemReader{r: r, parts: parts, size: size}, nil
}

// Open returns a reader over the item's content (for previews).
func (s *Session) Open(id int) (*io.SectionReader, Item, error) {
	it, ok := s.itemPtr(id)
	if !ok {
		return nil, Item{}, errors.New("no such file")
	}
	ir, err := s.readerFor(it, false)
	if err != nil {
		return nil, Item{}, err
	}
	c, _ := s.Item(id)
	return io.NewSectionReader(ir, 0, ir.size), c, nil
}

func (s *Session) itemPtr(id int) (*Item, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id < 1 || id > len(s.items) {
		return nil, false
	}
	return s.items[id-1], true
}

// copyCompressed streams an NTFS-compressed file (16-cluster units).
func copyCompressed(r *disk.Reader, f *fsys.File, sink func([]byte) error) error {
	cs := f.CompCS
	unitSize := 16 * cs
	unit := make([]byte, unitSize)
	type piece struct{ off, n int64 }
	var vcn []piece
	for _, e := range f.CompExtents {
		vcn = append(vcn, piece{e.Off, e.Len / cs})
	}
	clusterAt := func(i int64) int64 {
		for _, p := range vcn {
			if i < p.n {
				if p.off < 0 {
					return -1
				}
				return p.off + i*cs
			}
			i -= p.n
		}
		return -1
	}
	left := f.Size
	for u := int64(0); left > 0; u++ {
		var stored []int64
		real := 0
		for k := int64(0); k < 16; k++ {
			o := clusterAt(u*16 + k)
			stored = append(stored, o)
			if o >= 0 {
				real++
			}
		}
		switch {
		case real == 0:
			clear(unit)
		case real == 16:
			for k, o := range stored {
				if _, err := r.ReadAt(unit[int64(k)*cs:int64(k+1)*cs], o); err != nil {
					return err
				}
			}
		default:
			src := make([]byte, int64(real)*cs)
			i := 0
			for _, o := range stored {
				if o >= 0 {
					if _, err := r.ReadAt(src[int64(i)*cs:int64(i+1)*cs], o); err != nil {
						return err
					}
					i++
				}
			}
			clear(unit)
			fsys.LZNT1(src, unit)
		}
		n := min(unitSize, left)
		if err := sink(unit[:n]); err != nil {
			return err
		}
		left -= n
	}
	return nil
}

// ---------------------------------------------------------------- thumbnails

type thumbCache struct {
	mu  sync.Mutex
	max int
	ll  *list.List
	m   map[int]*list.Element
	sem chan struct{}
}

type thumbEntry struct {
	id   int
	data []byte
}

func newThumbCache(max int) *thumbCache {
	n := runtime.NumCPU()
	if n > 4 {
		n = 4
	}
	if n < 2 {
		n = 2
	}
	return &thumbCache{max: max, ll: list.New(), m: map[int]*list.Element{}, sem: make(chan struct{}, n)}
}

func (c *thumbCache) get(id int) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[id]; ok {
		c.ll.MoveToFront(e)
		return e.Value.(*thumbEntry).data, true
	}
	return nil, false
}

func (c *thumbCache) put(id int, b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[id] = c.ll.PushFront(&thumbEntry{id, b})
	for c.ll.Len() > c.max {
		e := c.ll.Back()
		delete(c.m, e.Value.(*thumbEntry).id)
		c.ll.Remove(e)
	}
}

func (c *thumbCache) clear() {
	c.mu.Lock()
	c.ll.Init()
	c.m = map[int]*list.Element{}
	c.mu.Unlock()
}

// Thumb returns a small JPEG preview of a picture.
func (s *Session) Thumb(id int) ([]byte, error) {
	if b, ok := s.thumbs.get(id); ok {
		return b, nil
	}
	it, ok := s.itemPtr(id)
	if !ok {
		return nil, errors.New("no such file")
	}
	ext := it.Ext
	if ext != "jpg" && ext != "jpeg" && ext != "png" && ext != "gif" && ext != "jfif" {
		return nil, errors.New("no thumbnail for this type")
	}
	s.thumbs.sem <- struct{}{}
	defer func() { <-s.thumbs.sem }()
	ir, err := s.readerFor(it, false)
	if err != nil {
		return nil, err
	}
	if ext == "jpg" || ext == "jpeg" || ext == "jfif" {
		head := make([]byte, min(ir.size, 128<<10))
		ir.ReadAt(head, 0)
		if t := exifThumb(head); t != nil {
			s.thumbs.put(id, t)
			return t, nil
		}
	}
	if ir.size > 40<<20 {
		return nil, errors.New("too big for a thumbnail")
	}
	sr := io.NewSectionReader(ir, 0, ir.size)
	var img image.Image
	switch ext {
	case "png":
		img, err = png.Decode(sr)
	case "gif":
		img, err = gif.Decode(sr)
	default:
		img, err = jpeg.Decode(sr)
		if err != nil {
			// a damaged photo: show its intact part, as the label says
			if sv, e2 := jpegfix.SalvageFile(ir, ir.size); e2 == nil && sv != nil {
				switch {
				case sv.Intact != nil:
					img, err = jpeg.Decode(bytes.NewReader(sv.Intact))
				case sv.Preview != nil:
					img, err = jpeg.Decode(bytes.NewReader(sv.Preview))
				}
			}
		}
	}
	if err != nil && img == nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, scaleDown(img, 320), &jpeg.Options{Quality: 78}); err != nil {
		return nil, err
	}
	s.thumbs.put(id, out.Bytes())
	return out.Bytes(), nil
}

// exifThumb pulls the small preview picture that cameras store inside JPEGs.
func exifThumb(b []byte) []byte {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return nil
	}
	p := 2
	for p+4 <= len(b) && b[p] == 0xFF {
		m := b[p+1]
		l := int(binary.BigEndian.Uint16(b[p+2:]))
		if m == 0xDA || l < 2 || p+2+l > len(b) {
			return nil
		}
		seg := b[p+4 : p+2+l]
		if m == 0xE1 && len(seg) > 14 && string(seg[:6]) == "Exif\x00\x00" {
			t := seg[6:]
			var bo binary.ByteOrder
			switch string(t[:2]) {
			case "II":
				bo = binary.LittleEndian
			case "MM":
				bo = binary.BigEndian
			default:
				return nil
			}
			ifd0 := int(bo.Uint32(t[4:8]))
			if ifd0+2 > len(t) {
				return nil
			}
			n := int(bo.Uint16(t[ifd0:]))
			next := ifd0 + 2 + n*12
			if next+4 > len(t) {
				return nil
			}
			ifd1 := int(bo.Uint32(t[next:]))
			if ifd1 == 0 || ifd1+2 > len(t) {
				return nil
			}
			n = int(bo.Uint16(t[ifd1:]))
			var off, ln int
			for i := 0; i < n; i++ {
				e := ifd1 + 2 + i*12
				if e+12 > len(t) {
					break
				}
				switch bo.Uint16(t[e:]) {
				case 0x0201:
					off = int(bo.Uint32(t[e+8:]))
				case 0x0202:
					ln = int(bo.Uint32(t[e+8:]))
				}
			}
			if off > 0 && ln > 100 && off+ln <= len(t) && t[off] == 0xFF && t[off+1] == 0xD8 {
				return append([]byte(nil), t[off:off+ln]...)
			}
			return nil
		}
		p += 2 + l
	}
	return nil
}

// scaleDown shrinks a picture so its longer side is at most max pixels.
func scaleDown(img image.Image, max int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w == 0 || h == 0 {
		return img
	}
	big := w
	if h > big {
		big = h
	}
	if big <= max {
		return img
	}
	nw, nh := w*max/big, h*max/big
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	yc, isYC := img.(*image.YCbCr)
	const k = 3 // samples per axis per output pixel
	for y := 0; y < nh; y++ {
		for x := 0; x < nw; x++ {
			var rs, gs, bs int
			for sy := 0; sy < k; sy++ {
				py := b.Min.Y + (y*k+sy)*h/(nh*k)
				for sx := 0; sx < k; sx++ {
					px := b.Min.X + (x*k+sx)*w/(nw*k)
					if isYC {
						yi, ci := yc.YOffset(px, py), yc.COffset(px, py)
						r, g, bb := color.YCbCrToRGB(yc.Y[yi], yc.Cb[ci], yc.Cr[ci])
						rs, gs, bs = rs+int(r), gs+int(g), bs+int(bb)
					} else {
						r, g, bb, _ := img.At(px, py).RGBA()
						rs, gs, bs = rs+int(r>>8), gs+int(g>>8), bs+int(bb>>8)
					}
				}
			}
			i := dst.PixOffset(x, y)
			dst.Pix[i] = uint8(rs / (k * k))
			dst.Pix[i+1] = uint8(gs / (k * k))
			dst.Pix[i+2] = uint8(bs / (k * k))
			dst.Pix[i+3] = 255
		}
	}
	return dst
}

// ---------------------------------------------------------------- text and hex previews

var reTags = regexp.MustCompile(`<[^>]+>`)

// Text returns readable text from a text file or an Office document.
func (s *Session) Text(id int) (string, error) {
	it, ok := s.itemPtr(id)
	if !ok {
		return "", errors.New("no such file")
	}
	ir, err := s.readerFor(it, false)
	if err != nil {
		return "", err
	}
	switch it.Ext {
	case "docx", "pptx", "xlsx", "odt", "ods", "odp":
		zr, err := zip.NewReader(io.NewSectionReader(ir, 0, ir.size), ir.size)
		if err != nil {
			return "", errors.New("the document's contents cannot be read (it may be damaged)")
		}
		var sb strings.Builder
		for _, f := range zr.File {
			n := f.Name
			want := n == "word/document.xml" || n == "xl/sharedStrings.xml" || n == "content.xml" ||
				strings.HasPrefix(n, "ppt/slides/slide") && strings.HasSuffix(n, ".xml")
			if !want {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				continue
			}
			x, _ := io.ReadAll(io.LimitReader(rc, 4<<20))
			rc.Close()
			t := strings.NewReplacer("</w:p>", "\n", "</a:p>", "\n", "</si>", "\n", "</text:p>", "\n").Replace(string(x))
			t = reTags.ReplaceAllString(t, "")
			t = strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", "\"", "&apos;", "'").Replace(t)
			sb.WriteString(t)
			sb.WriteString("\n")
			if sb.Len() > 64<<10 {
				break
			}
		}
		return truncateText(sb.String(), 64<<10), nil
	}
	if it.Group != "text" && !textLike[it.Ext] {
		return "", errors.New("not a text file")
	}
	b := make([]byte, min(ir.size, 64<<10))
	ir.ReadAt(b, 0)
	wide := it.wide || (len(b) > 1 && b[0] == 0xFF && b[1] == 0xFE)
	return carve.DecodeText(b, wide), nil
}

var textLike = map[string]bool{"csv": true, "rtf": true, "eps": true, "svg": true, "vcf": true, "ps": true}

func truncateText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// Hex returns a hex dump of the start of a file.
func (s *Session) Hex(id int) (string, error) {
	it, ok := s.itemPtr(id)
	if !ok {
		return "", errors.New("no such file")
	}
	ir, err := s.readerFor(it, false)
	if err != nil {
		return "", err
	}
	b := make([]byte, min(ir.size, 1024))
	ir.ReadAt(b, 0)
	return hex.Dump(b), nil
}
