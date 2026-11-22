package carve

import (
	"bytes"
	"encoding/binary"
	"time"
)

// ---------------------------------------------------------------- MP4 / MOV / 3GP / HEIC

var topBoxes = map[string]bool{
	"ftyp": true, "moov": true, "mdat": true, "free": true, "skip": true, "wide": true,
	"uuid": true, "meta": true, "pdin": true, "moof": true, "mfra": true, "styp": true,
	"sidx": true, "ssix": true, "prft": true, "emsg": true, "pnot": true, "PICT": true,
	"junk": true, "udta": true, "sefd": true, "auth": true, "Xtra": true, "beam": true,
	"mpvd": true, "jp2h": true, "idat": true, "iloc": true, "mvex": true, "camm": true,
	"gpmd": true, "SEFT": true, "fiel": true,
}

func (c *carver) isobmff(res *Result, limit int64) bool {
	hdr := c.readNew(res.Off, 64)
	ftypSize := int64(binary.BigEndian.Uint32(hdr[0:4]))
	major := string(hdr[8:12])
	brands := major
	if ftypSize >= 16 && ftypSize <= 64 {
		brands += string(hdr[16:ftypSize])
	}
	switch {
	case major == "qt  ":
		res.Ext = "mov"
	case len(major) == 4 && major[:3] == "3gp":
		res.Ext = "3gp"
	case len(major) == 4 && major[:3] == "3g2":
		res.Ext = "3g2"
	case major == "M4A " || major == "M4B ":
		res.Ext = "m4a"
	case major == "M4V " || major == "M4VH":
		res.Ext = "m4v"
	case major == "crx ":
		res.Ext = "cr3"
	case major == "avif" || major == "avis":
		res.Ext = "avif"
	case major == "heic" || major == "heix" || major == "heim" || major == "heis" || major == "hevc" || major == "hevx" ||
		((major == "mif1" || major == "msf1") && bytes.Contains([]byte(brands), []byte("heic"))):
		res.Ext = "heic"
	case major == "mif1" || major == "msf1":
		res.Ext = "heif"
	default:
		res.Ext = "mp4"
	}
	pos := res.Off
	sawMoov, sawMdat, sawMeta, sawMoof := false, false, false, false
	for pos+8 <= limit {
		h := c.read(pos, 16)
		size := int64(binary.BigEndian.Uint32(h[0:4]))
		typ := string(h[4:8])
		if !topBoxes[typ] {
			break
		}
		hdrLen := int64(8)
		if size == 1 {
			size = int64(binary.BigEndian.Uint64(h[8:16]))
			hdrLen = 16
		}
		if size == 0 {
			// box runs to the end of the file (unfinished recording)
			if typ == "mdat" {
				end := c.nextHit(pos)
				if end > limit {
					end = limit
				}
				pos = end
				sawMdat = true
			}
			break
		}
		if size < hdrLen || pos+size < pos {
			break
		}
		switch typ {
		case "moov":
			sawMoov = true
			if res.Time.IsZero() {
				res.Time = c.mvhdTime(pos+hdrLen, minI64(size-hdrLen, 1<<20))
			}
		case "mdat":
			sawMdat = true
		case "meta":
			sawMeta = true
		case "moof":
			sawMoof = true
		}
		if pos+size > limit {
			pos = limit
			break
		}
		pos += size
	}
	if pos <= res.Off+16 {
		return false
	}
	res.Size = pos - res.Off
	res.Complete = sawMoov || sawMoof || (sawMeta && !sawMdat) || (sawMeta && res.Ext != "mp4")
	if !res.Complete && !sawMdat {
		return false
	}
	return true
}

func (c *carver) mvhdTime(pos, n int64) time.Time {
	end := pos + n
	for p := pos; p+16 <= end; {
		h := c.read(p, 24)
		sz := int64(binary.BigEndian.Uint32(h[0:4]))
		if string(h[4:8]) == "mvhd" {
			var secs uint64
			if h[8] == 1 {
				secs = binary.BigEndian.Uint64(h[12:20])
			} else {
				secs = uint64(binary.BigEndian.Uint32(h[12:16]))
			}
			if secs > 2082844800 {
				t := time.Unix(int64(secs)-2082844800, 0)
				if t.Year() > 1990 && t.Year() < 2100 {
					return t.Local()
				}
			}
			return time.Time{}
		}
		if sz < 8 {
			break
		}
		p += sz
	}
	return time.Time{}
}

// ---------------------------------------------------------------- RIFF (AVI / WAV / WEBP)

func (c *carver) riff(res *Result, limit int64) bool {
	h := c.read(res.Off, 12)
	size := int64(binary.LittleEndian.Uint32(h[4:8])) + 8
	switch string(h[8:12]) {
	case "AVI ":
		res.Ext = "avi"
	case "WAVE":
		res.Ext = "wav"
	case "WEBP":
		res.Ext = "webp"
	case "RMID":
		res.Ext = "mid"
	case "CDXA":
		res.Ext = "dat"
	case "ACON":
		res.Ext = "ani"
	case "QLCM":
		res.Ext = "qcp"
	}
	if t := string(h[8:11]); t == "CDR" || t == "cdr" {
		res.Ext = "cdr"
	}
	if size < 12 {
		return false
	}
	pos := res.Off + size
	if pos+1 <= limit && size&1 == 1 {
		pos++ // pad byte
	}
	// large AVI files continue with "RIFF....AVIX" chunks
	for res.Ext == "avi" && pos+12 <= limit {
		n := c.read(pos, 12)
		if string(n[0:4]) != "RIFF" || string(n[8:12]) != "AVIX" {
			break
		}
		s := int64(binary.LittleEndian.Uint32(n[4:8])) + 8
		pos += s + s&1
	}
	res.Complete = pos <= limit
	if pos > limit {
		pos = limit
	}
	res.Size = pos - res.Off
	return true
}

// ---------------------------------------------------------------- Matroska / WebM

func ebmlVint(b []byte, keepMarker bool) (val uint64, n int, unknown bool) {
	if len(b) == 0 || b[0] == 0 {
		return 0, 0, false
	}
	l := 1
	for m := byte(0x80); b[0]&m == 0; m >>= 1 {
		l++
	}
	if l > 8 || l > len(b) {
		return 0, 0, false
	}
	v := uint64(b[0])
	if !keepMarker {
		v &= uint64(0xFF >> uint(l))
	}
	allOnes := v == uint64(0xFF>>uint(l))
	for i := 1; i < l; i++ {
		v = v<<8 | uint64(b[i])
		if b[i] != 0xFF {
			allOnes = false
		}
	}
	return v, l, allOnes && !keepMarker
}

var mkvTop = map[uint64]bool{
	0x114D9B74: true, 0x1549A966: true, 0x1654AE6B: true, 0x1F43B675: true, 0x1C53BB6B: true,
	0x1941A469: true, 0x1043A770: true, 0x1254C367: true, 0xEC: true, 0xBF: true,
}

func (c *carver) ebml(res *Result, limit int64) bool {
	b := c.readNew(res.Off, 64)
	_, idLen, _ := ebmlVint(b, true)
	sz, szLen, _ := ebmlVint(b[idLen:], false)
	if idLen != 4 || szLen == 0 || sz > 4096 {
		return false
	}
	hdrEnd := res.Off + int64(idLen+szLen) + int64(sz)
	hb := c.readNew(res.Off, int(int64(idLen+szLen)+int64(sz)))
	if bytes.Contains(hb, []byte("webm")) {
		res.Ext = "webm"
	} else {
		res.Ext = "mkv"
	}
	s := c.readNew(hdrEnd, 16)
	id, il, _ := ebmlVint(s, true)
	if id != 0x18538067 {
		return false
	}
	segSize, sl, unknown := ebmlVint(s[il:], false)
	dataStart := hdrEnd + int64(il+sl)
	if !unknown {
		end := dataStart + int64(segSize)
		res.Complete = end <= limit
		if end > limit {
			end = limit
		}
		res.Size = end - res.Off
		return true
	}
	// size unknown (live recording): walk the top-level elements
	pos := dataStart
	for pos+2 <= limit {
		e := c.read(pos, 16)
		id, il, _ := ebmlVint(e, true)
		if il == 0 || !mkvTop[id] {
			break
		}
		sz, sl, unk := ebmlVint(e[il:], false)
		if sl == 0 {
			break
		}
		if unk {
			// a cluster of unknown size: walk its children
			p := pos + int64(il+sl)
			for p+2 <= limit {
				ce := c.read(p, 16)
				cid, cil, _ := ebmlVint(ce, true)
				if cil == 0 || mkvTop[cid] {
					break
				}
				csz, csl, cunk := ebmlVint(ce[cil:], false)
				if csl == 0 || cunk {
					break
				}
				p += int64(cil+csl) + int64(csz)
			}
			pos = p
			continue
		}
		pos += int64(il+sl) + int64(sz)
	}
	if pos > limit {
		pos = limit
	}
	res.Size = pos - res.Off
	return res.Size > 64
}

// ---------------------------------------------------------------- WMV / WMA (ASF)

var asfTop = [][]byte{
	{0x30, 0x26, 0xB2, 0x75, 0x8E, 0x66, 0xCF, 0x11, 0xA6, 0xD9, 0x00, 0xAA, 0x00, 0x62, 0xCE, 0x6C},
	{0x36, 0x26, 0xB2, 0x75, 0x8E, 0x66, 0xCF, 0x11, 0xA6, 0xD9, 0x00, 0xAA, 0x00, 0x62, 0xCE, 0x6C},
	{0x90, 0x08, 0x00, 0x33, 0xB1, 0xE5, 0xCF, 0x11, 0x89, 0xF4, 0x00, 0xA0, 0xC9, 0x03, 0x49, 0xCB},
	{0xD3, 0x29, 0xE2, 0xD6, 0xDA, 0x35, 0xD1, 0x11, 0x90, 0x34, 0x00, 0xA0, 0xC9, 0x03, 0x49, 0xBE},
	{0xF8, 0x03, 0xB1, 0xFE, 0xAD, 0x12, 0x64, 0x4C, 0x84, 0x0F, 0x2A, 0x1D, 0x2F, 0x7A, 0xD4, 0x8C},
	{0xD0, 0x3F, 0xB7, 0x3C, 0x4A, 0x0C, 0x03, 0x48, 0x95, 0x3D, 0xED, 0xF7, 0xB6, 0x22, 0x8F, 0x0C},
}
var asfVideo = []byte{0xC0, 0xEF, 0x19, 0xBC, 0x4D, 0x5B, 0xCF, 0x11, 0xA8, 0xFD, 0x00, 0x80, 0x5F, 0x5C, 0x44, 0x2B}

func (c *carver) asf(res *Result, limit int64) bool {
	pos := res.Off
	objects := 0
	for pos+24 <= limit {
		h := c.read(pos, 24)
		known := false
		for _, g := range asfTop {
			if bytes.Equal(h[:16], g) {
				known = true
			}
		}
		if !known {
			break
		}
		sz := int64(binary.LittleEndian.Uint64(h[16:24]))
		if sz < 24 {
			break
		}
		if objects == 0 {
			hdr := c.readNew(pos, int(minI64(sz, 1<<20)))
			if bytes.Contains(hdr, asfVideo) {
				res.Ext = "wmv"
			} else {
				res.Ext = "wma"
			}
		}
		objects++
		pos += sz
	}
	if objects < 2 {
		return false
	}
	if pos > limit {
		pos = limit
	}
	res.Size = pos - res.Off
	res.Complete = true
	return true
}

// ---------------------------------------------------------------- FLV

func (c *carver) flv(res *Result, limit int64) bool {
	pos := res.Off + int64(c.be32(res.Off+5)) + 4
	tags := 0
	for pos+15 <= limit {
		h := c.read(pos, 11)
		t := h[0] & 0x1F
		if t != 8 && t != 9 && t != 18 {
			break
		}
		ds := int64(h[1])<<16 | int64(h[2])<<8 | int64(h[3])
		if h[8] != 0 || h[9] != 0 || h[10] != 0 {
			break // stream id must be 0
		}
		next := pos + 11 + ds + 4
		if next > limit {
			break
		}
		if int64(c.be32(next-4)) != 11+ds {
			break
		}
		pos = next
		tags++
	}
	if tags == 0 {
		return false
	}
	res.Size = pos - res.Off
	res.Complete = true
	return true
}

// ---------------------------------------------------------------- MP3

var mp3Bitrates = [2][3][16]int{
	{ // MPEG-1: layer I, II, III
		{0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448, 0},
		{0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384, 0},
		{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0},
	},
	{ // MPEG-2 / 2.5
		{0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256, 0},
		{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},
		{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},
	},
}
var mp3Rates = [4][3]int{{11025, 12000, 8000}, {0, 0, 0}, {22050, 24000, 16000}, {44100, 48000, 32000}}

// mp3Frame returns the length of the MPEG audio frame whose header is h.
func mp3Frame(h []byte) int {
	if h[0] != 0xFF || h[1]&0xE0 != 0xE0 {
		return 0
	}
	ver := (h[1] >> 3) & 3
	layer := (h[1] >> 1) & 3
	bri := h[2] >> 4
	sri := (h[2] >> 2) & 3
	pad := int((h[2] >> 1) & 1)
	if ver == 1 || layer == 0 || bri == 0 || bri == 15 || sri == 3 {
		return 0
	}
	v := 1
	if ver == 3 {
		v = 0
	}
	l := 3 - int(layer) // 0 = layer I
	br := mp3Bitrates[v][l][bri] * 1000
	sr := mp3Rates[ver][sri]
	if sr == 0 {
		return 0
	}
	switch l {
	case 0:
		return (12*br/sr + pad) * 4
	case 1:
		return 144*br/sr + pad
	default:
		if v == 0 {
			return 144*br/sr + pad
		}
		return 72*br/sr + pad
	}
}

func (c *carver) mp3(res *Result, limit int64) bool {
	h := c.read(res.Off, 10)
	tag := int64(h[6])<<21 | int64(h[7])<<14 | int64(h[8])<<7 | int64(h[9])
	pos := res.Off + 10 + tag
	if h[5]&0x10 != 0 {
		pos += 10
	}
	res.inner = append(res.inner, span{res.Off, pos})
	// skip padding before the first frame
	for i := 0; i < 64<<10 && pos+4 <= limit; i++ {
		b := c.read(pos, 4)
		if b[0] == 0xFF && mp3Frame(b) > 0 {
			break
		}
		if string(b[:3]) == "ID3" { // a second tag
			return false
		}
		pos++
	}
	frames := 0
	for pos+4 <= limit {
		fl := mp3Frame(c.read(pos, 4))
		if fl < 21 {
			break
		}
		pos += int64(fl)
		frames++
	}
	if frames < 2 {
		return false
	}
	if pos+128 <= limit && string(c.read(pos, 3)) == "TAG" {
		pos += 128
	}
	if pos > limit {
		pos = limit
	}
	res.Size = pos - res.Off
	res.Complete = true
	return true
}

// ---------------------------------------------------------------- OGG

func (c *carver) ogg(res *Result, limit int64) bool {
	pos := res.Off
	pages := 0
	for pos+27 <= limit {
		h := c.readNew(pos, 27+255)
		if string(h[0:4]) != "OggS" || h[4] != 0 {
			break
		}
		n := int(h[26])
		size := int64(27 + n)
		for i := 0; i < n; i++ {
			size += int64(h[27+i])
		}
		if pages == 0 {
			p := c.readNew(pos+27+int64(n), 8)
			switch {
			case string(p[1:7]) == "vorbis":
				res.Ext = "ogg"
			case string(p[0:8]) == "OpusHead":
				res.Ext = "opus"
			case string(p[1:7]) == "theora":
				res.Ext = "ogv"
			case string(p[1:5]) == "FLAC":
				res.Ext = "oga"
			}
		}
		pos += size
		pages++
	}
	if pages < 2 {
		return false
	}
	if pos > limit {
		pos = limit
	}
	res.Size = pos - res.Off
	res.Complete = true
	return true
}

// ---------------------------------------------------------------- FLAC

var crc8Tab, crc16Tab = func() ([256]byte, [256]uint16) {
	var t8 [256]byte
	var t16 [256]uint16
	for i := 0; i < 256; i++ {
		c := byte(i)
		for k := 0; k < 8; k++ {
			if c&0x80 != 0 {
				c = c<<1 ^ 0x07
			} else {
				c <<= 1
			}
		}
		t8[i] = c
		w := uint16(i) << 8
		for k := 0; k < 8; k++ {
			if w&0x8000 != 0 {
				w = w<<1 ^ 0x8005
			} else {
				w <<= 1
			}
		}
		t16[i] = w
	}
	return t8, t16
}()

// flacHeaderOK validates a FLAC frame header at b (with its CRC-8).
func flacHeaderOK(b []byte) bool {
	if len(b) < 16 || b[0] != 0xFF || b[1]&0xFE != 0xF8 {
		return false
	}
	bs := b[2] >> 4
	sr := b[2] & 0xF
	ch := b[3] >> 4
	ss := (b[3] >> 1) & 7
	if bs == 0 || sr == 15 || ch > 10 || ss == 3 || ss == 7 || b[3]&1 != 0 {
		return false
	}
	p := 4
	// UTF-8 coded frame/sample number
	x := b[p]
	n := 0
	switch {
	case x&0x80 == 0:
		n = 0
	case x&0xE0 == 0xC0:
		n = 1
	case x&0xF0 == 0xE0:
		n = 2
	case x&0xF8 == 0xF0:
		n = 3
	case x&0xFC == 0xF8:
		n = 4
	case x&0xFE == 0xFC:
		n = 5
	case x == 0xFE:
		n = 6
	default:
		return false
	}
	p += 1 + n
	if bs == 6 {
		p++
	} else if bs == 7 {
		p += 2
	}
	if sr == 12 {
		p++
	} else if sr == 13 || sr == 14 {
		p += 2
	}
	if p >= len(b) {
		return false
	}
	var crc byte
	for i := 0; i < p; i++ {
		crc = crc8Tab[crc^b[i]]
	}
	return crc == b[p]
}

func (c *carver) flac(res *Result, limit int64) bool {
	pos := res.Off + 4
	var minFrame, maxFrame int64
	for i := 0; i < 128; i++ {
		h := c.read(pos, 4+18)
		last := h[0]&0x80 != 0
		l := int64(h[1])<<16 | int64(h[2])<<8 | int64(h[3])
		if h[0]&0x7F == 0 {
			minFrame = int64(h[8])<<16 | int64(h[9])<<8 | int64(h[10])
			maxFrame = int64(h[11])<<16 | int64(h[12])<<8 | int64(h[13])
		}
		pos += 4 + l
		if last {
			break
		}
		if pos > limit {
			return false
		}
	}
	if maxFrame == 0 {
		maxFrame = 1 << 20
	}
	if minFrame < 10 {
		minFrame = 10
	}
	// walk frame headers; the last frame ends where its CRC-16 checks out
	s := c.seq(pos, limit)
	frame := pos
	if !flacHeaderOK(c.read(frame, 16)) {
		return false
	}
	frames := 0
	for {
		s.pos = frame + minFrame
		found := int64(-1)
		for s.pos < frame+maxFrame+16 {
			if !s.findByte(0xFF) {
				break
			}
			if s.pos > frame+maxFrame+16 {
				break
			}
			if flacHeaderOK(c.read(s.pos, 16)) && c.frameCRC(frame, s.pos) {
				found = s.pos
				break
			}
			s.pos++
		}
		frames++
		if found < 0 {
			break
		}
		frame = found
	}
	// end of the last frame: first position where CRC-16 of the frame is valid
	var crc uint16
	buf := c.readNew(frame, int(minI64(maxFrame+16, limit-frame)))
	end := int64(-1)
	for i := 0; i < len(buf); i++ {
		crc = crc<<8 ^ crc16Tab[byte(crc>>8)^buf[i]]
		if int64(i+1) >= minFrame && crc == 0 {
			end = frame + int64(i) + 1
			break
		}
	}
	if end < 0 {
		end = minI64(frame+maxFrame, limit)
	}
	res.Size = end - res.Off
	res.Complete = frames > 0
	return frames > 0
}

// frameCRC checks the CRC-16 of the frame [start, end).
func (c *carver) frameCRC(start, end int64) bool {
	if end-start < 4 || end-start > 16<<20 {
		return false
	}
	b := c.readNew(start, int(end-start))
	var crc uint16
	for _, x := range b {
		crc = crc<<8 ^ crc16Tab[byte(crc>>8)^x]
	}
	return crc == 0
}

// ---------------------------------------------------------------- AMR (phone voice recordings)

var amrNB = [16]int{13, 14, 16, 18, 20, 21, 27, 32, 6, 0, 0, 0, 0, 0, 0, 1}
var amrWB = [16]int{18, 24, 33, 37, 41, 47, 51, 59, 61, 6, 0, 0, 0, 0, 0, 1}

func (c *carver) amr(res *Result, limit int64) bool {
	h := c.read(res.Off, 9)
	tbl := amrNB
	pos := res.Off + 6
	if string(h[:9]) == "#!AMR-WB\n" {
		tbl = amrWB
		pos = res.Off + 9
		res.Ext = "awb"
	}
	s := c.seq(pos, limit)
	frames := 0
	for {
		b, ok := s.byte()
		if !ok || b&0x83 != 0 {
			if ok {
				s.pos--
			}
			break
		}
		l := tbl[(b>>3)&0xF]
		if l == 0 {
			s.pos--
			break
		}
		s.skip(int64(l - 1))
		frames++
	}
	if frames < 5 {
		return false
	}
	if s.pos > limit {
		s.pos = limit
	}
	res.Size = s.pos - res.Off
	res.Complete = true
	return true
}

// ---------------------------------------------------------------- MPEG program stream (VOB / MPG)

func (c *carver) mpegps(res *Result, limit int64) bool {
	pos := res.Off
	packs := 0
	for pos+14 <= limit {
		b := c.read(pos, 14)
		if b[0] != 0 || b[1] != 0 || b[2] != 1 {
			break
		}
		code := b[3]
		switch {
		case code == 0xBA:
			if b[4]&0xC0 == 0x40 {
				pos += 14 + int64(b[13]&7)
			} else if b[4]&0xF0 == 0x20 {
				pos += 12
			} else {
				goto done
			}
			packs++
		case code == 0xB9:
			pos += 4
			goto done
		case code >= 0xBB:
			pos += 6 + int64(binary.BigEndian.Uint16(b[4:6]))
		default:
			goto done
		}
	}
done:
	if packs < 2 {
		return false
	}
	if pos > limit {
		pos = limit
	}
	res.Size = pos - res.Off
	res.Complete = true
	return true
}

// ---------------------------------------------------------------- MPEG transport stream (TS / MTS)

func (c *carver) mpegts(res *Result, limit int64, pkt, syncOff int64) bool {
	buf := make([]byte, pkt*1024)
	pos := res.Off
	packets := int64(0)
	for pos+pkt <= limit {
		n := int64(len(buf))
		if pos+n > limit {
			n = (limit - pos) / pkt * pkt
		}
		if _, err := c.r.ReadAt(buf[:n], pos); err != nil {
			c.err = err
			return false
		}
		i := int64(0)
		for ; i+pkt <= n; i += pkt {
			if buf[i+syncOff] != 0x47 {
				break
			}
		}
		packets += i / pkt
		pos += i
		if i < n {
			break
		}
	}
	if packets < 10 {
		return false
	}
	res.Size = pos - res.Off
	res.Complete = true
	return true
}
