package scan

import (
	"bytes"
	"unicode/utf8"

	"pendriverescue/internal/sig"
)

// analyse looks at every 512-byte sector of a chunk.
func analyse(res *Result, c *chunk, size int64) {
	buf := c.buf
	n := c.n
	regionSize := (size + NumRegion - 1) / NumRegion
	if regionSize < 512 {
		regionSize = 512
	}
	if c.bad == int64(n) {
		res.Bad += int64(n) / 512
		for p := int64(0); p < int64(n); p += 512 {
			ri := (c.off + p) / regionSize
			if ri >= NumRegion {
				ri = NumRegion - 1
			}
			res.Regions[ri].Bad++
		}
		res.text.breakRun(res)
		return
	}
	badLeft := c.bad / 512
	for pos := 0; pos+512 <= n; pos += 512 {
		s := buf[pos : pos+512]
		abs := c.off + int64(pos)
		ri := abs / regionSize
		if ri >= NumRegion {
			ri = NumRegion - 1
		}
		reg := &res.Regions[ri]
		if s[0] == 0 && bytes.Equal(s, zero512) {
			if badLeft > 0 {
				badLeft--
				res.Bad++
				reg.Bad++
			} else {
				res.Zero++
				reg.Zero++
			}
			res.text.breakRun(res)
			continue
		}
		if s[0] == 0xFF && bytes.Equal(s, ff512) {
			res.FF++
			reg.FF++
			res.text.breakRun(res)
			continue
		}
		res.Data++
		reg.Data++

		look := buf[pos : pos+512+overlap/2]

		if k := sig.Detect(look); k != sig.None {
			off := abs
			if k == sig.ISO9660 {
				off -= sig.ISOOffset
			}
			if off >= 0 {
				res.Hits = append(res.Hits, Hit{off, k})
			}
		}
		if res.text.enabled {
			res.text.sector(res, s, abs)
		}

		if s[510] == 0x55 && s[511] == 0xAA {
			if t := bootType(s); t != 0 {
				cp := make([]byte, 512)
				copy(cp, s)
				res.Boot = append(res.Boot, BootCand{abs, t, cp})
			}
		}

		if s[0] == '.' && bytes.Equal(s[0:11], dot) && s[11]&0x10 != 0 && s[11]&0xC8 == 0 &&
			bytes.Equal(s[32:43], dotdot) && s[43]&0x10 != 0 && s[43]&0xC8 == 0 {
			self := uint32(le16(s[20:22]))<<16 | uint32(le16(s[26:28]))
			parent := uint32(le16(s[52:54]))<<16 | uint32(le16(s[58:60]))
			if self >= 2 && self != parent {
				res.FatDirs = append(res.FatDirs, FatDir{abs, self, parent})
			}
		}

		if s[0] == 'F' && s[1] == 'I' && s[2] == 'L' && s[3] == 'E' {
			usaOff := le16(s[4:6])
			usaCnt := le16(s[6:8])
			attrOff := le16(s[0x14:0x16])
			if (usaOff == 0x30 || usaOff == 0x2A || usaOff == 0x28) && usaCnt >= 2 && usaCnt <= 17 &&
				attrOff%8 == 0 && attrOff >= 0x30 && attrOff < 0x200 && le16(s[0x16:0x18]) <= 0x0F {
				res.MftRecs = append(res.MftRecs, abs)
			}
		}

		for e := 0; e < 512; e += 32 {
			t := s[e]
			if t != 0x85 && t != 0x05 {
				continue
			}
			if ok, del := exfatSetValid(buf[pos+e:pos+e+20*32], t); ok {
				res.ExfatSets = append(res.ExfatSets, ExfatSet{abs + int64(e), del})
			}
		}
	}
}

// ---------------------------------------------------------------- text files

// textTab: 1 = printable ASCII or text whitespace, 2 = byte >= 0x80 (part of
// a UTF-8 character), 0 = binary.
var textTab = func() (t [256]uint8) {
	for c := 0x20; c < 0x7F; c++ {
		t[c] = 1
	}
	t['\t'], t['\n'], t['\r'], t['\f'] = 1, 1, 1, 1
	for c := 0x80; c < 0x100; c++ {
		t[c] = 2
	}
	return
}()

type textState struct {
	enabled bool
	open    bool
	start   int64
	end     int64
	wide    bool
	chars   int64
}

func (t *textState) breakRun(res *Result) {
	if t.open {
		t.flush(res)
	}
}

func (t *textState) flush(res *Result) {
	if t.open && t.chars >= 32 {
		res.TextRuns = append(res.TextRuns, TextRun{t.start, t.end - t.start, t.wide})
	}
	t.open = false
	t.chars = 0
}

// sector classifies one sector and extends or ends the current text run.
func (t *textState) sector(res *Result, s []byte, abs int64) {
	kind, used, wide := classifyText(s)
	switch {
	case kind == 0:
		t.breakRun(res)
		return
	case t.open && (t.wide != wide || t.end != abs):
		t.flush(res)
	}
	if !t.open {
		t.open, t.start, t.wide = true, abs, wide
	}
	t.end = abs + int64(used)
	t.chars += int64(used)
	if kind == 2 { // text that ends inside this sector
		t.flush(res)
	}
}

// classifyText returns 0 (not text), 1 (a full sector of text) or 2 (text
// followed by zero padding: the end of a text file), the number of text
// bytes, and whether it is UTF-16.
func classifyText(s []byte) (kind, used int, wide bool) {
	z := len(s)
	for z > 0 && s[z-1] == 0 {
		z--
	}
	if z == 0 {
		return 0, 0, false
	}
	if k, u := classifyText16(s); k != 0 {
		return k, u, true
	}
	body := s[:z]
	hi := 0
	for _, c := range body {
		switch textTab[c] {
		case 0:
			return 0, 0, false
		case 2:
			hi++
		}
	}
	if hi > 0 {
		// tolerate a character cut at either sector edge
		b := body
		for i := 0; i < 3 && len(b) > 0 && b[0]&0xC0 == 0x80; i++ {
			b = b[1:]
		}
		for i := 0; i < 3 && len(b) > 0; i++ {
			if r, _ := utf8.DecodeLastRune(b); r != utf8.RuneError {
				break
			}
			b = b[:len(b)-1]
		}
		if !utf8.Valid(b) || hi*3 > len(body) {
			return 0, 0, false
		}
	}
	if z < len(s) {
		return 2, z, false
	}
	return 1, z, false
}

// classifyText16 recognises UTF-16LE text (English, Latin and Indian scripts).
func classifyText16(s []byte) (kind, used int) {
	if len(s) < 64 || s[1] != 0 && !(s[0] == 0xFF && s[1] == 0xFE) && !(s[1] >= 0x09 && s[1] <= 0x0D) {
		return 0, 0
	}
	n := len(s) / 2
	end := n
	for end > 0 && s[2*end-1] == 0 && s[2*end-2] == 0 {
		end--
	}
	if end < 16 {
		return 0, 0
	}
	ascii := 0
	for i := 0; i < end; i++ {
		lo, hi := s[2*i], s[2*i+1]
		switch {
		case hi == 0 && textTab[lo] == 1:
			ascii++
		case hi >= 0x09 && hi <= 0x0D: // Indian scripts
		case hi == 0x20 && lo <= 0x6F: // punctuation such as quotes and dashes
		case hi == 0xFE && lo == 0xFF && i == 0: // byte order mark
		case hi == 0x00 && lo >= 0xA0: // Latin-1 letters
		default:
			return 0, 0
		}
	}
	if ascii*4 < end {
		return 0, 0
	}
	if end < n {
		return 2, end * 2
	}
	return 1, n * 2
}

// ---------------------------------------------------------------- boot sectors / exFAT

func bootType(s []byte) BootType {
	switch string(s[3:11]) {
	case "EXFAT   ":
		return BootExFAT
	case "NTFS    ":
		if le16(s[11:13])&0x1FF == 0 && le16(s[11:13]) != 0 {
			return BootNTFS
		}
		return 0
	case "-FVE-FS-":
		return BootBitLocker
	}
	if s[0] != 0xEB && s[0] != 0xE9 {
		return 0
	}
	bps := le16(s[11:13])
	if bps != 512 && bps != 1024 && bps != 2048 && bps != 4096 {
		return 0
	}
	spc := s[13]
	if spc == 0 || spc&(spc-1) != 0 {
		return 0
	}
	if le16(s[14:16]) == 0 || s[16] == 0 || s[16] > 2 {
		return 0
	}
	tot := uint32(le16(s[19:21]))
	if tot == 0 {
		tot = le32(s[32:36])
	}
	if tot == 0 {
		return 0
	}
	fat16 := le16(s[22:24])
	rootEnt := le16(s[17:19])
	if fat16 == 0 {
		if le32(s[36:40]) == 0 || rootEnt != 0 {
			return 0
		}
	} else if rootEnt == 0 {
		return 0
	}
	return BootFAT
}

// ExfatChecksum computes the entry set checksum with the "in use" bits set,
// so deleted sets validate too.
func ExfatChecksum(set []byte) uint16 {
	var sum uint16
	for i, b := range set {
		if i == 2 || i == 3 {
			continue
		}
		if i%32 == 0 {
			b |= 0x80
		}
		sum = (sum>>1 | sum<<15) + uint16(b)
	}
	return sum
}

func exfatSetValid(b []byte, t byte) (ok bool, deleted bool) {
	sec := int(b[1])
	if sec < 2 || sec > 18 || len(b) < (sec+1)*32 {
		return false, false
	}
	deleted = t == 0x05
	st := b[32]
	if (!deleted && st != 0xC0) || (deleted && st != 0x40) {
		return false, false
	}
	nameLen := int(b[32+3])
	if nameLen == 0 || (nameLen+14)/15 > sec-1 {
		return false, false
	}
	for i := 2; i <= 1+(nameLen+14)/15; i++ {
		et := b[i*32]
		if (!deleted && et != 0xC1) || (deleted && et != 0x41) {
			return false, false
		}
	}
	set := b[:(sec+1)*32]
	return ExfatChecksum(set) == le16(b[2:4]), deleted
}
