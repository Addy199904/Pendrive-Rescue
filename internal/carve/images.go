package carve

import (
	"encoding/binary"
	"strings"
	"time"
)

// ---------------------------------------------------------------- JPEG

func (c *carver) jpeg(res *Result, limit int64) bool {
	s := c.seq(res.Off+2, limit)
	sawSOF, sawSOS := false, false
	for {
		b, ok := s.byte()
		if !ok {
			break
		}
		if b != 0xFF {
			break // structure broken
		}
		m, ok := s.byte()
		for ok && m == 0xFF {
			m, ok = s.byte()
		}
		if !ok {
			break
		}
		switch {
		case m == 0xD9: // end of image
			res.Size = s.pos - res.Off
			res.Complete = sawSOF && sawSOS
			return sawSOF && sawSOS
		case m == 0xD8 || m == 0x00:
			s.pos -= 2
			goto done
		case (m >= 0xD0 && m <= 0xD7) || m == 0x01:
			continue
		}
		lb, ok := s.bytes(2)
		if !ok {
			goto done
		}
		l := int64(binary.BigEndian.Uint16(lb))
		if l < 2 {
			goto done
		}
		segStart := s.pos
		if m >= 0xE0 && m <= 0xEF {
			res.inner = append(res.inner, span{segStart, segStart + l - 2})
			if m == 0xE1 && res.Time.IsZero() && l > 16 {
				n := l - 2
				if n > 64<<10 {
					n = 64 << 10
				}
				seg := c.readNew(segStart, int(n))
				if string(seg[:6]) == "Exif\x00\x00" {
					res.Time = exifTime(seg[6:])
				}
			}
		}
		if (m >= 0xC0 && m <= 0xCF) && m != 0xC4 && m != 0xC8 && m != 0xCC {
			sawSOF = true
		}
		if !s.skip(l - 2) {
			goto done
		}
		if m == 0xDA {
			sawSOS = true
			// entropy-coded data: runs until a marker that is not RSTn / stuffing
			for {
				if !s.findByte(0xFF) {
					goto done
				}
				s.pos++
				n, ok := s.byte()
				if !ok {
					goto done
				}
				if n == 0x00 || (n >= 0xD0 && n <= 0xD7) {
					continue
				}
				if n == 0xFF {
					s.pos--
					continue
				}
				s.pos -= 2
				break
			}
		}
	}
done:
	// no end marker: keep what we have if it is a real picture
	if sawSOF && sawSOS && s.pos-res.Off > 1024 {
		res.Size = s.pos - res.Off
		res.Complete = false
		return true
	}
	return false
}

// exifTime reads DateTimeOriginal (or DateTime) from a TIFF/EXIF block.
func exifTime(t []byte) time.Time {
	if len(t) < 8 {
		return time.Time{}
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return time.Time{}
	}
	var dateTime, original string
	readIFD := func(off uint32) (exifPtr uint32) {
		if int(off)+2 > len(t) {
			return 0
		}
		n := int(bo.Uint16(t[off:]))
		for i := 0; i < n && i < 512; i++ {
			e := int(off) + 2 + i*12
			if e+12 > len(t) {
				break
			}
			tag := bo.Uint16(t[e:])
			typ := bo.Uint16(t[e+2:])
			cnt := bo.Uint32(t[e+4:])
			val := bo.Uint32(t[e+8:])
			switch tag {
			case 0x8769:
				exifPtr = val
			case 0x0132, 0x9003, 0x9004:
				if typ == 2 && cnt >= 19 && int(val)+19 <= len(t) {
					s := string(t[val : val+19])
					if tag == 0x0132 {
						dateTime = s
					} else if original == "" || tag == 0x9003 {
						original = s
					}
				}
			}
		}
		return
	}
	if p := readIFD(bo.Uint32(t[4:8])); p != 0 {
		readIFD(p)
	}
	for _, s := range []string{original, dateTime} {
		if tm, err := time.ParseInLocation("2006:01:02 15:04:05", s, time.Local); err == nil && tm.Year() > 1980 {
			return tm
		}
	}
	return time.Time{}
}

// ---------------------------------------------------------------- PNG

func (c *carver) png(res *Result, limit int64) bool {
	pos := res.Off + 8
	first := true
	for pos+12 <= limit {
		h := c.read(pos, 8)
		l := int64(binary.BigEndian.Uint32(h[0:4]))
		typ := h[4:8]
		for _, ch := range typ {
			if !((ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z')) {
				goto done
			}
		}
		if first && (string(typ) != "IHDR" || l != 13) {
			return false
		}
		first = false
		if l > 1<<31 {
			goto done
		}
		pos += 12 + l
		if string(typ) == "IEND" {
			res.Size = pos - res.Off
			res.Complete = true
			return true
		}
	}
done:
	if !first && pos-res.Off > 64 {
		res.Size = pos - res.Off
		if res.Off+res.Size > limit {
			res.Size = limit - res.Off
		}
		return true
	}
	return false
}

// ---------------------------------------------------------------- GIF

func (c *carver) gif(res *Result, limit int64) bool {
	s := c.seq(res.Off+6, limit)
	lsd, ok := s.bytes(7)
	if !ok {
		return false
	}
	if lsd[4]&0x80 != 0 {
		s.skip(3 << (uint(lsd[4]&7) + 1))
	}
	subBlocks := func() bool {
		for {
			n, ok := s.byte()
			if !ok {
				return false
			}
			if n == 0 {
				return true
			}
			if !s.skip(int64(n)) {
				return false
			}
		}
	}
	images := 0
	for {
		b, ok := s.byte()
		if !ok {
			break
		}
		switch b {
		case 0x3B:
			res.Size = s.pos - res.Off
			res.Complete = images > 0
			return images > 0
		case 0x21:
			if _, ok := s.byte(); !ok || !subBlocks() {
				goto done
			}
		case 0x2C:
			d, ok := s.bytes(9)
			if !ok {
				goto done
			}
			if d[8]&0x80 != 0 {
				s.skip(3 << (uint(d[8]&7) + 1))
			}
			if _, ok := s.byte(); !ok || !subBlocks() {
				goto done
			}
			images++
		default:
			s.pos--
			goto done
		}
	}
done:
	if images > 0 {
		res.Size = s.pos - res.Off
		return true
	}
	return false
}

// ---------------------------------------------------------------- BMP

func (c *carver) bmp(res *Result, limit int64) bool {
	h := c.read(res.Off, 54)
	size := int64(binary.LittleEndian.Uint32(h[2:6]))
	dataOff := int64(binary.LittleEndian.Uint32(h[10:14]))
	if size == 0 {
		w := int64(int32(binary.LittleEndian.Uint32(h[18:22])))
		ht := int64(int32(binary.LittleEndian.Uint32(h[22:26])))
		if ht < 0 {
			ht = -ht
		}
		bpp := int64(binary.LittleEndian.Uint16(h[28:30]))
		size = dataOff + ((bpp*w+31)/32)*4*ht
	}
	if size < 26 || res.Off+size > limit {
		return false
	}
	res.Size = size
	res.Complete = true
	return true
}

// ---------------------------------------------------------------- TIFF / RAW

func (c *carver) tiff(res *Result, limit int64) bool {
	head := c.read(res.Off, 16)
	var bo binary.ByteOrder = binary.LittleEndian
	if head[0] == 'M' {
		bo = binary.BigEndian
	}
	if string(head[8:10]) == "CR" {
		res.Ext = "cr2"
	}
	switch res.Kind {
	case 0:
	}
	if string(head[0:4]) == "IIRO" || string(head[0:4]) == "IIRS" {
		res.Ext = "orf"
	} else if string(head[0:4]) == "IIU\x00" {
		res.Ext = "rw2"
	}
	end := int64(8)
	seen := map[uint32]bool{}
	var make_ string
	dng := false
	var strips, counts []uint32
	readArr := func(typ uint16, cnt, val uint32) []uint32 {
		if cnt > 1<<16 {
			return nil
		}
		sz := uint32(4)
		if typ == 3 {
			sz = 2
		}
		var raw []byte
		if sz*cnt <= 4 {
			raw = make([]byte, 4)
			bo.PutUint32(raw, val)
		} else {
			raw = c.readNew(res.Off+int64(val), int(sz*cnt))
		}
		out := make([]uint32, cnt)
		for i := range out {
			if sz == 2 {
				out[i] = uint32(bo.Uint16(raw[i*2:]))
			} else {
				out[i] = bo.Uint32(raw[i*4:])
			}
		}
		return out
	}
	typeSize := []uint32{0, 1, 1, 2, 4, 8, 1, 1, 2, 4, 8, 4, 8, 4}
	var walk func(off uint32, depth int)
	walk = func(off uint32, depth int) {
		for off != 0 && depth < 8 && !seen[off] && len(seen) < 64 {
			seen[off] = true
			if res.Off+int64(off)+2 > limit {
				return
			}
			n := uint32(bo.Uint16(c.read(res.Off+int64(off), 2)))
			if n == 0 || n > 1000 {
				return
			}
			ents := c.readNew(res.Off+int64(off)+2, int(n*12+4))
			if e := int64(off) + 2 + int64(n)*12 + 4; e > end {
				end = e
			}
			var so, sc []uint32
			var jo, jl uint32
			for i := uint32(0); i < n; i++ {
				e := ents[i*12:]
				tag := bo.Uint16(e)
				typ := bo.Uint16(e[2:])
				cnt := bo.Uint32(e[4:])
				val := bo.Uint32(e[8:])
				if typ == 0 || int(typ) >= len(typeSize) {
					continue
				}
				if sz := int64(typeSize[typ]) * int64(cnt); sz > 4 && sz < 1<<30 {
					if v := int64(val) + sz; v > end {
						end = v
					}
				}
				switch tag {
				case 0x010F:
					if cnt < 64 {
						if cnt <= 4 {
							b := make([]byte, 4)
							bo.PutUint32(b, val)
							make_ = string(b[:cnt])
						} else {
							make_ = string(c.readNew(res.Off+int64(val), int(cnt)))
						}
					}
				case 0x0111, 0x0144:
					so = readArr(typ, cnt, val)
				case 0x0117, 0x0145:
					sc = readArr(typ, cnt, val)
				case 0x0201:
					jo = val
				case 0x0202:
					jl = val
				case 0x014A:
					for _, p := range readArr(typ, cnt, val) {
						walk(p, depth+1)
					}
				case 0x8769, 0x8825, 0xA005:
					walk(val, depth+1)
				case 0xC612:
					dng = true
				}
			}
			if len(so) == len(sc) {
				strips = append(strips, so...)
				counts = append(counts, sc...)
			}
			if jo != 0 && jl != 0 {
				strips = append(strips, jo)
				counts = append(counts, jl)
			}
			next := bo.Uint32(ents[n*12:])
			off = next
		}
	}
	walk(bo.Uint32(head[4:8]), 0)
	for i := range strips {
		if v := int64(strips[i]) + int64(counts[i]); v > end && counts[i] < 1<<30 {
			end = v
		}
	}
	if len(strips) == 0 || end <= 8 || res.Off+end > limit {
		return false
	}
	m := strings.ToUpper(make_)
	switch {
	case dng:
		res.Ext = "dng"
	case res.Ext == "tif" && strings.HasPrefix(m, "NIKON"):
		res.Ext = "nef"
	case res.Ext == "tif" && strings.HasPrefix(m, "SONY"):
		res.Ext = "arw"
	case res.Ext == "tif" && strings.HasPrefix(m, "PENTAX"):
		res.Ext = "pef"
	case res.Ext == "tif" && strings.HasPrefix(m, "SAMSUNG"):
		res.Ext = "srw"
	}
	res.Size = end
	res.Complete = true
	// photo date
	res.Time = exifTime(c.readNew(res.Off, int(minI64(end, 256<<10))))
	return true
}

func minI64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// ---------------------------------------------------------------- PSD

func (c *carver) psd(res *Result, limit int64) bool {
	h := c.read(res.Off, 26)
	ver := binary.BigEndian.Uint16(h[4:6])
	ch := int64(binary.BigEndian.Uint16(h[12:14]))
	height := int64(binary.BigEndian.Uint32(h[14:18]))
	width := int64(binary.BigEndian.Uint32(h[18:22]))
	depth := int64(binary.BigEndian.Uint16(h[22:24]))
	if ch == 0 || ch > 56 || height == 0 || width == 0 {
		return false
	}
	pos := res.Off + 26
	pos += 4 + int64(c.be32(pos)) // color mode data
	pos += 4 + int64(c.be32(pos)) // image resources
	if ver == 2 {
		pos += 8 + int64(c.be64(pos))
	} else {
		pos += 4 + int64(c.be32(pos))
	}
	if pos+2 > limit {
		return false
	}
	comp := c.be16(pos)
	pos += 2
	switch comp {
	case 0:
		pos += ch * height * ((width*depth + 7) / 8)
	case 1:
		rows := ch * height
		cntSize := int64(2)
		if ver == 2 {
			cntSize = 4
		}
		if rows > 1<<24 {
			return false
		}
		tbl := c.readNew(pos, int(rows*cntSize))
		pos += rows * cntSize
		for i := int64(0); i < rows; i++ {
			if cntSize == 2 {
				pos += int64(binary.BigEndian.Uint16(tbl[i*2:]))
			} else {
				pos += int64(binary.BigEndian.Uint32(tbl[i*4:]))
			}
		}
	default:
		return false
	}
	if pos > limit {
		return false
	}
	res.Size = pos - res.Off
	res.Complete = true
	return true
}
