package carve

import (
	"compress/flate"
	"encoding/binary"
	"hash/crc32"
	"io"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------- RAR

func rarVint(b []byte) (uint64, int) {
	var v uint64
	for i := 0; i < len(b) && i < 10; i++ {
		v |= uint64(b[i]&0x7F) << (7 * uint(i))
		if b[i]&0x80 == 0 {
			return v, i + 1
		}
	}
	return 0, 0
}

func (c *carver) rar(res *Result, limit int64) bool {
	h := c.read(res.Off, 8)
	if h[6] == 0x01 { // RAR 5
		pos := res.Off + 8
		blocks := 0
		for pos+7 <= limit {
			b := c.readNew(pos, 32)
			hs, n1 := rarVint(b[4:])
			if n1 == 0 || hs == 0 || hs > 2<<20 {
				break
			}
			hdr := c.readNew(pos+4+int64(n1), int(hs))
			if crc32.ChecksumIEEE(append(b[4:4+n1:4+n1], hdr...)) != binary.LittleEndian.Uint32(b[0:4]) {
				break
			}
			typ, n2 := rarVint(hdr)
			flags, n3 := rarVint(hdr[n2:])
			p := n2 + n3
			if flags&1 != 0 {
				_, n := rarVint(hdr[p:])
				p += n
			}
			var data uint64
			if flags&2 != 0 {
				data, _ = rarVint(hdr[p:])
			}
			pos += 4 + int64(n1) + int64(hs) + int64(data)
			blocks++
			if typ == 5 { // end of archive
				res.Size = pos - res.Off
				res.Complete = true
				return true
			}
		}
		if blocks > 1 {
			res.Size = minI64(pos, limit) - res.Off
			return true
		}
		return false
	}
	// RAR 4
	pos := res.Off + 7
	blocks := 0
	for pos+7 <= limit {
		b := c.read(pos, 11)
		typ := b[2]
		flags := binary.LittleEndian.Uint16(b[3:5])
		hs := int64(binary.LittleEndian.Uint16(b[5:7]))
		if typ < 0x72 || typ > 0x7B || hs < 7 {
			break
		}
		add := int64(0)
		if flags&0x8000 != 0 || typ == 0x74 || typ == 0x7A {
			add = int64(binary.LittleEndian.Uint32(b[7:11]))
			if typ == 0x74 && flags&0x100 != 0 {
				add += int64(c.le32(pos+32)) << 32
			}
		}
		pos += hs + add
		blocks++
		if typ == 0x7B {
			res.Size = pos - res.Off
			res.Complete = true
			return true
		}
	}
	if blocks > 1 {
		res.Size = minI64(pos, limit) - res.Off
		return true
	}
	return false
}

// ---------------------------------------------------------------- 7-Zip

func (c *carver) sevenz(res *Result, limit int64) bool {
	h := c.readNew(res.Off, 32)
	if crc32.ChecksumIEEE(h[12:32]) != binary.LittleEndian.Uint32(h[8:12]) {
		return false
	}
	size := 32 + int64(binary.LittleEndian.Uint64(h[12:20])) + int64(binary.LittleEndian.Uint64(h[20:28]))
	if size <= 32 || res.Off+size > limit {
		return false
	}
	res.Size = size
	res.Complete = true
	return true
}

// ---------------------------------------------------------------- GZIP

type countReader struct {
	c   *carver
	pos int64
	lim int64
	buf []byte
	bo  int64
	bl  int
}

func (r *countReader) ReadByte() (byte, error) {
	if r.pos >= r.lim {
		return 0, io.EOF
	}
	if r.pos < r.bo || r.pos >= r.bo+int64(r.bl) {
		n := int64(len(r.buf))
		if r.pos+n > r.lim {
			n = r.lim - r.pos
		}
		if _, err := r.c.r.ReadAt(r.buf[:n], r.pos); err != nil {
			return 0, err
		}
		r.bo, r.bl = r.pos, int(n)
	}
	b := r.buf[r.pos-r.bo]
	r.pos++
	return b, nil
}

func (r *countReader) Read(p []byte) (int, error) {
	for i := range p {
		b, err := r.ReadByte()
		if err != nil {
			return i, err
		}
		p[i] = b
	}
	return len(p), nil
}

func (c *carver) gzip(res *Result, limit int64) bool {
	pos := res.Off
	members := 0
	for pos+18 <= limit {
		h := c.readNew(pos, 10)
		if h[0] != 0x1F || h[1] != 0x8B || h[2] != 8 {
			break
		}
		flg := h[3]
		if members == 0 {
			if mt := binary.LittleEndian.Uint32(h[4:8]); mt > 0 {
				res.Time = time.Unix(int64(mt), 0)
			}
		}
		p := pos + 10
		if flg&4 != 0 {
			p += 2 + int64(c.le16(p))
		}
		if flg&8 != 0 {
			name := c.readNew(p, 256)
			n := 0
			for n < len(name) && name[n] != 0 {
				n++
			}
			if members == 0 && n > 0 && n < 200 {
				res.Title = string(name[:n])
			}
			p += int64(n) + 1
		}
		if flg&16 != 0 {
			cm := c.readNew(p, 4096)
			n := 0
			for n < len(cm) && cm[n] != 0 {
				n++
			}
			p += int64(n) + 1
		}
		if flg&2 != 0 {
			p += 2
		}
		cr := &countReader{c: c, pos: p, lim: limit, buf: make([]byte, 256<<10), bo: -1}
		fr := flate.NewReader(cr)
		n, err := io.Copy(io.Discard, fr)
		if err != nil || c.err != nil {
			break
		}
		tr := c.readNew(cr.pos, 8)
		if binary.LittleEndian.Uint32(tr[4:8]) != uint32(n) {
			break
		}
		pos = cr.pos + 8
		members++
	}
	if members == 0 {
		return false
	}
	res.Size = pos - res.Off
	res.Complete = true
	if res.Title != "" && !strings.HasSuffix(strings.ToLower(res.Title), ".gz") {
		res.Title += ".gz"
	}
	return true
}

// ---------------------------------------------------------------- TAR

func tarOctal(b []byte) int64 {
	s := strings.Trim(string(b), " \x00")
	if s == "" {
		return 0
	}
	v, err := strconv.ParseInt(s, 8, 64)
	if err != nil {
		return -1
	}
	return v
}

func (c *carver) tar(res *Result, limit int64) bool {
	pos := res.Off
	files := 0
	for pos+512 <= limit {
		h := c.readNew(pos, 512)
		zero := true
		for _, b := range h {
			if b != 0 {
				zero = false
				break
			}
		}
		if zero {
			pos += 1024
			// tar files are padded with zeros to a 10 KB record
			if rem := (pos - res.Off) % 10240; rem != 0 {
				pad := 10240 - rem
				if pos+pad <= limit && isZeroBytes(c.readNew(pos, int(pad))) {
					pos += pad
				}
			}
			res.Complete = true
			break
		}
		chk := tarOctal(h[148:156])
		var sum int64
		for i, b := range h {
			if i >= 148 && i < 156 {
				b = ' '
			}
			sum += int64(b)
		}
		if chk != sum {
			break
		}
		size := tarOctal(h[124:136])
		if size < 0 {
			break
		}
		pos += 512 + (size+511)/512*512
		files++
	}
	if files == 0 {
		return false
	}
	if pos > limit {
		pos = limit
	}
	res.Size = pos - res.Off
	return true
}

func isZeroBytes(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------- ISO 9660 (CD/DVD image)

func (c *carver) iso(res *Result, limit int64) bool {
	pvd := c.readNew(res.Off+0x8000, 2048)
	blocks := int64(binary.LittleEndian.Uint32(pvd[80:84]))
	bs := int64(binary.LittleEndian.Uint16(pvd[128:130]))
	if bs != 2048 || blocks < 18 {
		return false
	}
	size := blocks * bs
	if res.Off+size > limit {
		size = limit - res.Off
	} else {
		res.Complete = true
	}
	res.Size = size
	res.Title = strings.TrimSpace(string(pvd[40:72]))
	return true
}

// ---------------------------------------------------------------- SQLite

func (c *carver) sqlite(res *Result, limit int64) bool {
	h := c.readNew(res.Off, 100)
	ps := int64(binary.BigEndian.Uint16(h[16:18]))
	if ps == 1 {
		ps = 65536
	}
	if ps < 512 || ps&(ps-1) != 0 {
		return false
	}
	pages := int64(binary.BigEndian.Uint32(h[28:32]))
	if pages == 0 || binary.BigEndian.Uint32(h[24:28]) != binary.BigEndian.Uint32(h[92:96]) {
		// size not recorded: keep up to the next file
		e := c.nextHit(res.Off)
		if e > limit {
			e = limit
		}
		res.Size = (e - res.Off) / ps * ps
		return res.Size >= ps
	}
	if res.Off+ps*pages > limit {
		return false
	}
	res.Size = ps * pages
	res.Complete = true
	return true
}
