package carve

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// ---------------------------------------------------------------- PDF

var (
	reStartXref = regexp.MustCompile(`startxref\s+(\d+)\s*$`)
	reCreation  = regexp.MustCompile(`/CreationDate\s*\(D:(\d{8,14})`)
	reTitle     = regexp.MustCompile(`/Title\s*\(([^()\\]{2,80})\)`)
)

func (c *carver) pdfEOFs(from, to int64) []int64 {
	var out []int64
	const win = 1 << 20
	for p := from; p < to; p += win - 8 {
		n := int64(win)
		if p+n > to {
			n = to - p
		}
		b := c.readNew(p, int(n))
		i := 0
		for {
			j := bytes.Index(b[i:], []byte("%%EOF"))
			if j < 0 {
				break
			}
			at := p + int64(i+j)
			if len(out) == 0 || out[len(out)-1] != at {
				out = append(out, at)
			}
			i += j + 5
		}
		if c.err != nil {
			break
		}
	}
	return out
}

func (c *carver) pdfValidEOF(start, at int64) bool {
	back := int64(1024)
	if at-start < back {
		back = at - start
	}
	b := c.readNew(at-back, int(back))
	m := reStartXref.FindSubmatch(bytes.TrimRight(b, "\r\n \t"))
	if m == nil {
		return false
	}
	x, err := strconv.ParseInt(string(m[1]), 10, 64)
	return err == nil && x > 0 && x < at-start
}

func eolLen(b []byte) int64 {
	if len(b) >= 2 && b[0] == '\r' && b[1] == '\n' {
		return 2
	}
	if len(b) >= 1 && (b[0] == '\n' || b[0] == '\r') {
		return 1
	}
	return 0
}

func (c *carver) pdf(res *Result, limit int64) bool {
	end := int64(-1)
	from := res.Off + 8
	stop := c.nextHit(res.Off)
	for rounds := 0; rounds < 4096; rounds++ {
		to := stop
		if to > limit {
			to = limit
		}
		found := false
		for _, at := range c.pdfEOFs(from, to) {
			if c.pdfValidEOF(res.Off, at) {
				end = at + 5 + eolLen(c.read(at+5, 2))
				found = true
			}
		}
		if c.err != nil {
			return false
		}
		if end >= 0 {
			// look for an incremental update after this end, up to the next file
			if !found || stop >= limit {
				break
			}
			from = end
			stop = c.nextHit(end)
			if stop <= from {
				break
			}
			continue
		}
		if stop >= limit {
			break
		}
		// nothing yet: the next "hit" may be a picture inside the PDF
		from = to
		stop = c.nextHit(stop)
	}
	if end < 0 {
		// no end found: keep the part up to the next file as a damaged PDF
		e := c.nextHit(res.Off)
		if e > limit {
			e = limit
		}
		if e-res.Off < 1024 {
			return false
		}
		end = e
		res.Complete = false
	} else {
		res.Complete = true
	}
	res.Size = end - res.Off
	// date and title from the start or end of the file
	parts := [][]byte{c.readNew(res.Off, int(minI64(res.Size, 64<<10)))}
	if res.Size > 64<<10 {
		parts = append(parts, c.readNew(end-64<<10, 64<<10))
	}
	for _, b := range parts {
		if m := reCreation.FindSubmatch(b); m != nil && res.Time.IsZero() {
			s := string(m[1])
			for len(s) < 14 {
				s += "0"
			}
			if t, err := time.ParseInLocation("20060102150405", s, time.Local); err == nil && t.Year() > 1990 {
				res.Time = t
			}
		}
		if m := reTitle.FindSubmatch(b); m != nil && res.Title == "" {
			res.Title = cleanTitle(string(m[1]))
		}
	}
	return true
}

func cleanTitle(s string) string {
	s = strings.TrimSpace(s)
	for _, p := range []string{"Microsoft Word - ", "Microsoft PowerPoint - ", "Microsoft Excel - "} {
		s = strings.TrimPrefix(s, p)
	}
	for _, ext := range []string{".docx", ".doc", ".pptx", ".ppt", ".xlsx", ".xls"} {
		if strings.HasSuffix(strings.ToLower(s), ext) {
			s = s[:len(s)-len(ext)]
		}
	}
	low := strings.ToLower(s)
	if low == "untitled" || low == "title" || low == "document" || strings.HasPrefix(low, "untitled") || len(s) < 3 {
		return ""
	}
	for _, r := range s {
		if r < 0x20 {
			return ""
		}
	}
	return s
}

// ---------------------------------------------------------------- ZIP (DOCX / XLSX / PPTX / ODT / APK ...)

func (c *carver) zip(res *Result, limit int64) bool {
	pos := res.Off
	var names []string
	var coreXML int64 = -1
	var coreLen, coreComp int64
	var coreMethod uint16
	var mimetype string
	entries := 0
	complete := false
	for steps := 0; steps < 1<<20 && pos+4 <= limit; steps++ {
		h := c.readNew(pos, 46)
		switch binary.LittleEndian.Uint32(h[0:4]) {
		case 0x04034b50: // local file header
			flags := binary.LittleEndian.Uint16(h[6:8])
			method := binary.LittleEndian.Uint16(h[8:10])
			comp := int64(binary.LittleEndian.Uint32(h[18:22]))
			nl := int64(binary.LittleEndian.Uint16(h[26:28]))
			xl := int64(binary.LittleEndian.Uint16(h[28:30]))
			if nl == 0 || nl > 4096 {
				goto broken
			}
			name := string(c.readNew(pos+30, int(nl)))
			if len(names) < 4096 {
				names = append(names, name)
			}
			data := pos + 30 + nl + xl
			if comp == 0xFFFFFFFF && xl >= 20 {
				ex := c.readNew(pos+30+nl, int(xl))
				for i := 0; i+4 <= len(ex); {
					id := binary.LittleEndian.Uint16(ex[i:])
					sz := int(binary.LittleEndian.Uint16(ex[i+2:]))
					if id == 1 && sz >= 16 && i+4+16 <= len(ex) {
						comp = int64(binary.LittleEndian.Uint64(ex[i+12:]))
					}
					i += 4 + sz
				}
			}
			if name == "docProps/core.xml" || name == "meta.xml" {
				coreXML, coreComp, coreMethod = data, comp, method
				coreLen = int64(binary.LittleEndian.Uint32(h[22:26]))
			}
			if name == "mimetype" && method == 0 && comp < 100 {
				mimetype = string(c.readNew(data, int(comp)))
			}
			entries++
			if flags&8 == 0 && comp != 0xFFFFFFFF {
				pos = data + comp
				continue
			}
			// sizes come after the data: search for the next header
			next := c.zipNextSig(data, limit)
			if next < 0 {
				goto broken
			}
			pos = next
		case 0x08074b50: // data descriptor
			pos += 16
		case 0x02014b50: // central directory entry
			nl := int64(binary.LittleEndian.Uint16(h[28:30]))
			xl := int64(binary.LittleEndian.Uint16(h[30:32]))
			cl := int64(binary.LittleEndian.Uint16(h[32:34]))
			if len(names) < 4096 && nl < 4096 {
				names = append(names, string(c.readNew(pos+46, int(nl))))
			}
			pos += 46 + nl + xl + cl
		case 0x06054b50: // end of central directory
			cl := int64(binary.LittleEndian.Uint16(h[20:22]))
			pos += 22 + cl
			complete = true
			goto finish
		case 0x06064b50: // zip64 end of central directory
			pos += 12 + int64(binary.LittleEndian.Uint64(h[4:12]))
		case 0x07064b50: // zip64 locator
			pos += 20
		case 0x05054b50: // digital signature
			pos += 6 + int64(binary.LittleEndian.Uint16(h[4:6]))
		default:
			goto broken
		}
	}
broken:
	// structure broken: look for the end record further on
	if e := c.zipFindEOCD(res.Off, pos, limit); e > 0 {
		pos = e
		complete = true
	}
finish:
	if entries == 0 && !complete {
		return false
	}
	if pos > limit {
		pos = limit
	}
	res.Size = pos - res.Off
	res.Complete = complete
	res.Ext = zipKind(names, mimetype)
	if coreXML > 0 && coreComp > 0 && coreComp < 1<<20 {
		raw := c.readNew(coreXML, int(coreComp))
		var x []byte
		if coreMethod == 0 {
			x = raw
		} else if coreMethod == 8 {
			x, _ = io.ReadAll(io.LimitReader(flate.NewReader(bytes.NewReader(raw)), coreLen+1))
		}
		res.Title, res.Time = officeMeta(x)
	}
	return true
}

func (c *carver) zipNextSig(from, limit int64) int64 {
	const win = 1 << 20
	for p := from; p < limit && p < from+(4<<30); p += win - 4 {
		n := int64(win)
		if p+n > limit {
			n = limit - p
		}
		b := c.readNew(p, int(n))
		for i := 0; i+4 <= len(b); i++ {
			j := bytes.Index(b[i:], []byte("PK"))
			if j < 0 {
				break
			}
			i += j
			if i+4 > len(b) {
				break
			}
			switch string(b[i+2 : i+4]) {
			case "\x03\x04", "\x01\x02", "\x07\x08", "\x05\x06":
				return p + int64(i)
			}
		}
		if c.err != nil {
			return -1
		}
	}
	return -1
}

func (c *carver) zipFindEOCD(start, from, limit int64) int64 {
	const win = 1 << 20
	max := from + (256 << 20)
	if max > limit {
		max = limit
	}
	for p := from; p < max; p += win - 22 {
		n := int64(win)
		if p+n > max {
			n = max - p
		}
		b := c.readNew(p, int(n))
		i := 0
		for {
			j := bytes.Index(b[i:], []byte("PK\x05\x06"))
			if j < 0 || i+j+22 > len(b) {
				break
			}
			e := b[i+j:]
			cdSize := int64(binary.LittleEndian.Uint32(e[12:16]))
			cdOff := int64(binary.LittleEndian.Uint32(e[16:20]))
			at := p + int64(i+j)
			if cdOff+cdSize == at-start {
				return at + 22 + int64(binary.LittleEndian.Uint16(e[20:22]))
			}
			i += j + 4
		}
	}
	return -1
}

func zipKind(names []string, mime string) string {
	switch {
	case strings.Contains(mime, "opendocument.text"):
		return "odt"
	case strings.Contains(mime, "opendocument.spreadsheet"):
		return "ods"
	case strings.Contains(mime, "opendocument.presentation"):
		return "odp"
	case strings.Contains(mime, "epub"):
		return "epub"
	}
	has := func(prefix string) bool {
		for _, n := range names {
			if strings.HasPrefix(n, prefix) {
				return true
			}
		}
		return false
	}
	switch {
	case has("word/"):
		return "docx"
	case has("xl/"):
		return "xlsx"
	case has("ppt/"):
		return "pptx"
	case has("visio/"):
		return "vsdx"
	case has("AndroidManifest.xml") || has("classes.dex"):
		return "apk"
	case has("META-INF/MANIFEST.MF"):
		return "jar"
	case has("FixedDocSeq.fdseq") || has("Documents/1/FixedDocument.fdoc"):
		return "xps"
	case has("content/riffData.cdr") || has("content/root.dat"):
		return "cdr"
	}
	return "zip"
}

var (
	reDCTitle  = regexp.MustCompile(`<dc:title>([^<]{2,120})</dc:title>`)
	reModified = regexp.MustCompile(`<dcterms:(?:modified|created)[^>]*>(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)`)
	reODFDate  = regexp.MustCompile(`<(?:dc:date|meta:creation-date)>(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)`)
)

func officeMeta(x []byte) (string, time.Time) {
	var title string
	var t time.Time
	if m := reDCTitle.FindSubmatch(x); m != nil {
		title = cleanTitle(string(m[1]))
	}
	for _, re := range []*regexp.Regexp{reModified, reODFDate} {
		if m := re.FindSubmatch(x); m != nil {
			if tm, err := time.Parse("2006-01-02T15:04:05", string(m[1])); err == nil && tm.Year() > 1990 {
				t = tm.Local()
				break
			}
		}
	}
	return title, t
}

// ---------------------------------------------------------------- OLE2 (DOC / XLS / PPT / MSG)

func (c *carver) ole(res *Result, limit int64) bool {
	h := c.readNew(res.Off, 512)
	shift := binary.LittleEndian.Uint16(h[30:32])
	if shift != 9 && shift != 12 {
		return false
	}
	ss := int64(1) << shift
	nFat := int64(binary.LittleEndian.Uint32(h[44:48]))
	dirStart := binary.LittleEndian.Uint32(h[48:52])
	difat := binary.LittleEndian.Uint32(h[68:72])
	nDifat := int64(binary.LittleEndian.Uint32(h[72:76]))
	if nFat == 0 || nFat > 1<<20 {
		return false
	}
	secOff := func(s uint32) int64 { return res.Off + (int64(s)+1)*ss }
	var fatSecs []uint32
	for i := 0; i < 109 && int64(len(fatSecs)) < nFat; i++ {
		fatSecs = append(fatSecs, binary.LittleEndian.Uint32(h[76+i*4:]))
	}
	maxUsed := int64(0)
	for d, n := difat, int64(0); int64(len(fatSecs)) < nFat && n < nDifat && d < 0xFFFFFFFA; n++ {
		if int64(d) > maxUsed {
			maxUsed = int64(d)
		}
		b := c.readNew(secOff(d), int(ss))
		per := int(ss/4) - 1
		for i := 0; i < per && int64(len(fatSecs)) < nFat; i++ {
			fatSecs = append(fatSecs, binary.LittleEndian.Uint32(b[i*4:]))
		}
		d = binary.LittleEndian.Uint32(b[per*4:])
	}
	per := ss / 4
	fat := make([]uint32, 0, int64(len(fatSecs))*per)
	for _, fs := range fatSecs {
		if fs >= 0xFFFFFFFA {
			break
		}
		if int64(fs) > maxUsed {
			maxUsed = int64(fs)
		}
		if secOff(fs) >= limit {
			return false
		}
		b := c.readNew(secOff(fs), int(ss))
		for i := int64(0); i < per; i++ {
			fat = append(fat, binary.LittleEndian.Uint32(b[i*4:]))
		}
	}
	for i := len(fat) - 1; i >= 0; i-- {
		if fat[i] != 0xFFFFFFFF {
			if int64(i) > maxUsed {
				maxUsed = int64(i)
			}
			break
		}
	}
	size := (maxUsed + 2) * ss
	if res.Off+size > limit || size < 1024 {
		return false
	}
	res.Size = size
	res.Complete = true
	// directory names tell which program made the file
	var names []string
	seen := map[uint32]bool{}
	for d := dirStart; d < 0xFFFFFFFA && int(d) < len(fat) && !seen[d] && len(seen) < 256; d = fat[d] {
		seen[d] = true
		b := c.readNew(secOff(d), int(ss))
		for i := int64(0); i+128 <= ss; i += 128 {
			nl := int(binary.LittleEndian.Uint16(b[i+64:]))
			if nl < 2 || nl > 64 {
				continue
			}
			u := make([]uint16, nl/2-1)
			for k := range u {
				u[k] = binary.LittleEndian.Uint16(b[int(i)+k*2:])
			}
			names = append(names, string(utf16.Decode(u)))
		}
	}
	res.Ext = "ole"
	for _, n := range names {
		switch {
		case n == "WordDocument":
			res.Ext = "doc"
		case n == "Workbook" || n == "Book":
			res.Ext = "xls"
		case n == "PowerPoint Document":
			res.Ext = "ppt"
		case strings.HasPrefix(n, "__substg1.0_"):
			res.Ext = "msg"
		case n == "VisioDocument":
			res.Ext = "vsd"
		case n == "EncryptedPackage":
			res.Ext = "docx" // password protected Office file
		}
		if res.Ext != "ole" {
			break
		}
	}
	if res.Ext == "ole" {
		for _, n := range names {
			if n == "Catalog" {
				res.Ext = "db" // Thumbs.db
			}
		}
	}
	return true
}

// ---------------------------------------------------------------- RTF

func (c *carver) rtf(res *Result, limit int64) bool {
	s := c.seq(res.Off, limit)
	depth := 0
	for {
		b, ok := s.byte()
		if !ok {
			return false
		}
		switch b {
		case '\\':
			n, ok := s.byte()
			if !ok {
				return false
			}
			if n == 'b' {
				w, _ := s.bytes(3)
				if string(w) == "in" || (len(w) > 1 && string(w[:2]) == "in") {
					// \binN: skip N raw bytes
					s.pos -= 1
					num := 0
					for {
						d, ok := s.byte()
						if !ok {
							return false
						}
						if d < '0' || d > '9' {
							break
						}
						num = num*10 + int(d-'0')
					}
					s.skip(int64(num))
				} else {
					s.pos -= int64(len(w))
				}
			}
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				res.Size = s.pos - res.Off
				res.Complete = true
				return true
			}
		case 0:
			return false
		}
	}
}
