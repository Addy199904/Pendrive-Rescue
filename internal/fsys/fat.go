package fsys

import (
	"fmt"
	"strings"
	"time"

	"pendriverescue/internal/disk"
	"pendriverescue/internal/sig"
)

// fatFS reads a FAT12/16/32 volume. With fat == nil it can still read
// folders and files from a lost (formatted-over) FAT file system as long as
// the cluster geometry is known.
type fatFS struct {
	r        *disk.Reader
	vol      *Volume
	g        geometry
	bits     int
	rootClus uint32
	rootOff  int64 // FAT12/16 fixed root folder
	rootLen  int64
	fat      []uint32
	seenDir  map[uint32]bool
	hiHint   int64
	maxFiles int
	lost     bool               // reading folders of a formatted-over FAT
	alloc    func(c int64) bool // optional "cluster in use now" test (lost mode)
}

type fatBoot struct {
	bps, spc, rsvd, nfats, rootEnt, totSec, fatSz int64
	rootClus                                      uint32
	label                                         string
	bits                                          int
	clusters                                      int64
	dataSec                                       int64
}

func parseFATBoot(b []byte) (fb fatBoot, ok bool) {
	fb.bps = int64(le16(b[11:13]))
	fb.spc = int64(b[13])
	fb.rsvd = int64(le16(b[14:16]))
	fb.nfats = int64(b[16])
	fb.rootEnt = int64(le16(b[17:19]))
	fb.totSec = int64(le16(b[19:21]))
	if fb.totSec == 0 {
		fb.totSec = int64(le32(b[32:36]))
	}
	fb.fatSz = int64(le16(b[22:24]))
	if fb.bps == 0 || fb.spc == 0 || fb.nfats == 0 || fb.rsvd == 0 {
		return fb, false
	}
	if fb.fatSz == 0 {
		fb.fatSz = int64(le32(b[36:40]))
		fb.rootClus = le32(b[44:48])
		fb.label = strings.TrimSpace(string(b[71:82]))
	} else {
		fb.label = strings.TrimSpace(string(b[43:54]))
	}
	if fb.fatSz == 0 {
		return fb, false
	}
	rootSecs := (fb.rootEnt*32 + fb.bps - 1) / fb.bps
	fb.dataSec = fb.rsvd + fb.nfats*fb.fatSz + rootSecs
	if fb.totSec <= fb.dataSec {
		return fb, false
	}
	fb.clusters = (fb.totSec - fb.dataSec) / fb.spc
	switch {
	case le16(b[22:24]) == 0:
		// FAT32 layout (FATSz16 = 0) - real drivers go by this, not by
		// the cluster count
		fb.bits = 32
	case fb.clusters < 4085:
		fb.bits = 12
	default:
		fb.bits = 16
	}
	if fb.bits == 32 && fb.rootClus < 2 {
		fb.rootClus = 2
	}
	if fb.label == "NO NAME" || !printableASCII(fb.label) {
		fb.label = ""
	}
	return fb, true
}

func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7E {
			return false
		}
	}
	return true
}

// openFAT opens a FAT volume starting at drive offset start.
func openFAT(r *disk.Reader, start int64, boot []byte) (*fatFS, error) {
	fb, ok := parseFATBoot(boot)
	if !ok {
		return nil, fmt.Errorf("invalid FAT boot sector")
	}
	if fb.clusters > 268435445 || fb.totSec*fb.bps > 16*r.Size()+(1<<30) {
		return nil, fmt.Errorf("implausible FAT boot sector")
	}
	f := &fatFS{
		r:        r,
		bits:     fb.bits,
		rootClus: fb.rootClus,
		seenDir:  map[uint32]bool{},
		hiHint:   -1,
		maxFiles: 5_000_000,
	}
	f.g = geometry{
		kind:      "fat",
		start:     start,
		dataStart: start + fb.dataSec*fb.bps,
		cs:        fb.spc * fb.bps,
		first:     2,
		clusters:  fb.clusters,
		fatBits:   fb.bits,
	}
	f.rootOff = start + (fb.rsvd+fb.nfats*fb.fatSz)*fb.bps
	f.rootLen = fb.rootEnt * 32
	typ := fmt.Sprintf("FAT%d", fb.bits)
	f.vol = &Volume{
		Type: typ, Start: start, Size: fb.totSec * fb.bps, Label: fb.label,
		ClusterSize: f.g.cs, geom: f.g, fatDirs: f.seenDir,
	}
	// read the allocation table (first copy; fall back to second if broken)
	for copyNo := int64(0); copyNo < fb.nfats && copyNo < 2; copyNo++ {
		fatOff := start + (fb.rsvd+copyNo*fb.fatSz)*fb.bps
		need := (fb.clusters + 2) * int64(fb.bits) / 8
		if need > fb.fatSz*fb.bps {
			need = fb.fatSz * fb.bps
		}
		if need > 1<<30 {
			return nil, fmt.Errorf("FAT too large")
		}
		raw, err := readAt(r, fatOff, int(need)+4)
		if err != nil {
			return nil, err
		}
		tbl := decodeFAT(raw, fb.bits, fb.clusters+2)
		if fatLooksValid(tbl, fb.bits) || copyNo == fb.nfats-1 || copyNo == 1 {
			f.fat = tbl
			break
		}
	}
	f.vol.allocated = func(off int64) bool {
		if off < f.g.dataStart {
			return true // reserved area / FAT / root
		}
		c := (off-f.g.dataStart)/f.g.cs + 2
		return c < int64(len(f.fat)) && f.fat[c] != 0
	}
	return f, nil
}

func decodeFAT(raw []byte, bits int, n int64) []uint32 {
	t := make([]uint32, n)
	for i := int64(0); i < n; i++ {
		switch bits {
		case 12:
			o := i * 3 / 2
			if o+2 > int64(len(raw)) {
				return t
			}
			v := uint32(le16(raw[o:]))
			if i&1 == 0 {
				v &= 0xFFF
			} else {
				v >>= 4
			}
			t[i] = v
		case 16:
			if i*2+2 > int64(len(raw)) {
				return t
			}
			t[i] = uint32(le16(raw[i*2:]))
		default:
			if i*4+4 > int64(len(raw)) {
				return t
			}
			t[i] = le32(raw[i*4:]) & 0x0FFFFFFF
		}
	}
	return t
}

func fatLooksValid(t []uint32, bits int) bool {
	if len(t) < 3 {
		return false
	}
	switch bits {
	case 12:
		return t[0]&0xF00 == 0xF00
	case 16:
		return t[0]&0xFF00 == 0xFF00
	}
	return t[0]&0x0FFFFF00 == 0x0FFFFF00
}

func (f *fatFS) eoc(v uint32) bool {
	switch f.bits {
	case 12:
		return v >= 0xFF8
	case 16:
		return v >= 0xFFF8
	}
	return v >= 0x0FFFFFF8
}

func (f *fatFS) isFree(c uint32) bool {
	if f.fat != nil {
		return int64(c) >= int64(len(f.fat)) || f.fat[c] == 0
	}
	if f.alloc != nil {
		return !f.alloc(int64(c))
	}
	return true
}

// startOf gives the first cluster of an entry (guessing for deleted ones).
func (f *fatFS) startOf(e fatEntry, isDir bool) uint32 {
	if e.deleted {
		return f.deletedStart(e, isDir)
	}
	if f.g.valid(int64(e.cluster)) {
		return e.cluster
	}
	return 0
}

// chain follows the cluster chain. ok=false if the chain is broken.
func (f *fatFS) chain(c uint32, max int64) ([]uint32, bool) {
	var out []uint32
	seen := map[uint32]bool{}
	for {
		if !f.g.valid(int64(c)) || seen[c] {
			return out, false
		}
		seen[c] = true
		out = append(out, c)
		if max > 0 && int64(len(out)) >= max {
			return out, true
		}
		if f.fat == nil || int64(c) >= int64(len(f.fat)) {
			return out, false
		}
		n := f.fat[c]
		if f.eoc(n) {
			return out, true
		}
		if n < 2 {
			return out, false
		}
		c = n
	}
}

func (f *fatFS) clusterExtents(cl []uint32) []Extent {
	ex := make([]Extent, 0, 4)
	for _, c := range cl {
		ex = append(ex, Extent{f.g.off(int64(c)), f.g.cs})
	}
	return mergeExtents(ex)
}

func (f *fatFS) readClusters(cl []uint32) []byte {
	buf := make([]byte, int64(len(cl))*f.g.cs)
	pos := int64(0)
	for _, e := range f.clusterExtents(cl) {
		f.r.ReadAt(buf[pos:pos+e.Len], e.Off)
		pos += e.Len
	}
	return buf
}

// fatEntry is one parsed directory entry.
type fatEntry struct {
	guess   bool // first letter of the short name is unknown
	name    string
	attr    byte
	cluster uint32
	hiZero  bool
	size    uint32
	mtime   time.Time
	deleted bool
	dir     bool
}

var cp437 = []rune("ÇüéâäàåçêëèïîìÄÅÉæÆôöòûùÿÖÜ¢£¥₧ƒáíóúñÑªº¿⌐¬½¼¡«»░▒▓│┤╡╢╖╕╣║╗╝╜╛┐└┴┬├─┼╞╟╚╔╩╦╠═╬╧╨╤╥╙╘╒╓╫╪┘┌█▄▌▐▀αßΓπΣσµτΦΘΩδ∞φε∩≡±≥≤⌠⌡÷≈°∙·√ⁿ²■ ")

func oemString(b []byte) string {
	var sb strings.Builder
	for _, c := range b {
		if c < 0x80 {
			sb.WriteByte(c)
		} else {
			sb.WriteRune(cp437[c-0x80])
		}
	}
	return sb.String()
}

func sfnChecksum(n []byte) byte {
	var s byte
	for i := 0; i < 11; i++ {
		s = (s>>1 | s<<7) + n[i]
	}
	return s
}

func sfnCharOK(c byte, first bool) bool {
	if c < 0x20 {
		return first && c == 0x05
	}
	switch c {
	case '"', '*', '+', ',', '.', '/', ':', ';', '<', '=', '>', '?', '[', '\\', ']', '|':
		return false
	}
	return true
}

// sfnValid checks a short-name entry for plausibility (filters garbage in
// deleted or lost folders).
func sfnValid(e []byte, deleted bool) bool {
	attr := e[11]
	if attr&0xC0 != 0 {
		return false
	}
	for i := 0; i < 11; i++ {
		c := e[i]
		if i == 0 && deleted {
			continue
		}
		if !sfnCharOK(c, i == 0) {
			return false
		}
	}
	if e[0] == ' ' {
		return false
	}
	// date must be sane when present
	if d := le16(e[24:26]); d != 0 {
		mo := (d >> 5) & 0xF
		day := d & 0x1F
		if mo == 0 || mo > 12 || day == 0 {
			return false
		}
	}
	if attr&0x10 != 0 && le32(e[28:32]) != 0 && !deleted {
		return false
	}
	return true
}

func shortName(e []byte, first byte) string {
	n := make([]byte, 11)
	copy(n, e[:11])
	n[0] = first
	if n[0] == 0x05 {
		n[0] = 0xE5
	}
	base := strings.TrimRight(oemString(n[:8]), " ")
	ext := strings.TrimRight(oemString(n[8:11]), " ")
	if e[12]&0x08 != 0 {
		base = strings.ToLower(base)
	}
	if e[12]&0x10 != 0 {
		ext = strings.ToLower(ext)
	}
	if ext != "" {
		return base + "." + ext
	}
	return base
}

// parseFATDir decodes the entries of one directory.
func parseFATDir(data []byte, bits int) (entries []fatEntry, valid, invalid int) {
	var lfn [][]byte
	var lfnChk byte
	for p := 0; p+32 <= len(data); p += 32 {
		e := data[p : p+32]
		if e[0] == 0x00 {
			break
		}
		deleted := e[0] == 0xE5
		attr := e[11]
		if attr&0x3F == 0x0F && e[12] == 0 && le16(e[26:28]) == 0 {
			ord := e[0]
			if !deleted && ord&0x40 != 0 {
				lfn = lfn[:0]
			}
			if len(lfn) > 0 && e[13] != lfnChk {
				lfn = lfn[:0]
			}
			if !deleted && ord&0x1F == 0 {
				lfn = lfn[:0]
				invalid++
				continue
			}
			lfnChk = e[13]
			lfn = append(lfn, e)
			if len(lfn) > 20 {
				lfn = lfn[1:]
			}
			continue
		}
		parts := lfn
		lfn = nil
		if e[0] == '.' && (e[1] == ' ' || (e[1] == '.' && e[2] == ' ')) {
			continue
		}
		if attr&0x08 != 0 && attr&0x10 == 0 {
			continue // volume label
		}
		if !sfnValid(e, deleted) {
			invalid++
			continue
		}
		valid++
		long := ""
		first := e[0]
		if len(parts) > 0 {
			name11 := make([]byte, 11)
			copy(name11, e[:11])
			assembled := assembleLFN(parts)
			if deleted {
				// the first character was overwritten: guess it from the checksum
				cands := []byte{}
				if assembled != "" {
					c := assembled[0]
					if c >= 'a' && c <= 'z' {
						c -= 32
					}
					cands = append(cands, c)
				}
				for c := byte(0x21); c < 0x7F; c++ {
					cands = append(cands, c)
				}
				for _, c := range cands {
					name11[0] = c
					if sfnChecksum(name11) == lfnChk {
						first = c
						long = assembled
						break
					}
				}
			} else if sfnChecksum(name11) == lfnChk {
				long = assembled
			}
		} else if deleted {
			first = '_'
		}
		if deleted && long == "" && len(parts) == 0 {
			first = '_'
		}
		name := long
		if name == "" {
			if deleted && first == 0xE5 {
				first = '_'
			}
			name = shortName(e, first)
		}
		var cl uint32
		hi := le16(e[20:22])
		if bits == 32 {
			cl = uint32(hi)<<16 | uint32(le16(e[26:28]))
		} else {
			cl = uint32(le16(e[26:28]))
		}
		entries = append(entries, fatEntry{
			guess:   name == shortName(e, '_') && deleted,
			name:    name,
			attr:    attr,
			cluster: cl,
			hiZero:  bits == 32 && hi == 0,
			size:    le32(e[28:32]),
			mtime:   dosTime(le16(e[24:26]), le16(e[22:24]), 0),
			deleted: deleted,
			dir:     attr&0x10 != 0,
		})
	}
	guessFirstLetters(entries)
	return
}

// guessFirstLetters fills in the first letter of deleted short names from
// siblings that look alike (IMG_0001.JPG -> _MG_0002.JPG becomes IMG_0002.JPG).
func guessFirstLetters(entries []fatEntry) {
	for i := range entries {
		e := &entries[i]
		if !e.guess || len(e.name) < 4 {
			continue
		}
		votes := map[byte]int{}
		mine := strings.ToUpper(e.name[1:])
		for j, o := range entries {
			if j == i || o.guess || len(o.name) != len(e.name) {
				continue
			}
			theirs := strings.ToUpper(o.name[1:])
			k := 0
			for k < len(mine) && k < len(theirs) && mine[k] == theirs[k] {
				k++
			}
			if k >= 3 && o.name[0] < 0x80 {
				c := o.name[0]
				if c >= 'a' && c <= 'z' {
					c -= 32
				}
				votes[c]++
			}
		}
		best, bv := byte(0), 0
		for c, v := range votes {
			if v > bv || (v == bv && c < best) {
				best, bv = c, v
			}
		}
		if bv > 0 {
			if l := e.name[1]; l >= 'a' && l <= 'z' && best >= 'A' && best <= 'Z' {
				best += 32
			}
			e.name = string(best) + e.name[1:]
		}
	}
}

func assembleLFN(parts [][]byte) string {
	var raw []byte
	for i := len(parts) - 1; i >= 0; i-- {
		e := parts[i]
		raw = append(raw, e[1:11]...)
		raw = append(raw, e[14:26]...)
		raw = append(raw, e[28:32]...)
	}
	return decodeUTF16(raw)
}

// walk lists the whole tree of a FAT volume.
func (f *fatFS) walk() {
	var data []byte
	if f.bits == 32 {
		cl, _ := f.chain(f.rootClus, f.maxDirClusters())
		for _, c := range cl {
			f.seenDir[c] = true
		}
		data = f.readClusters(cl)
	} else {
		data, _ = readAt(f.r, f.rootOff, int(f.rootLen))
	}
	if f.vol.Label == "" {
		// a label entry in the root folder wins over the boot sector
		for p := 0; p+32 <= len(data); p += 32 {
			e := data[p : p+32]
			if e[0] == 0 {
				break
			}
			if e[0] != 0xE5 && e[11]&0x3F == 0x08 {
				f.vol.Label = strings.TrimSpace(oemString(e[:11]))
				break
			}
		}
	}
	f.walkDir(data, nil, false, 0, CatNamed)
}

func (f *fatFS) walkDir(data []byte, path []string, parentDeleted bool, depth int, cat Category) {
	entries, _, _ := parseFATDir(data, f.bits)
	for _, e := range entries {
		if len(f.vol.Files) >= f.maxFiles {
			return
		}
		deleted := e.deleted || parentDeleted
		if e.dir {
			if depth > 40 {
				continue
			}
			var cl []uint32
			if !deleted && f.fat != nil && !f.lost {
				cl, _ = f.chain(e.cluster, f.maxDirClusters())
			} else {
				start := f.startOf(e, true)
				if start == 0 {
					continue
				}
				cl = f.guessDirClusters(start)
			}
			if len(cl) == 0 || f.seenDir[cl[0]] {
				continue
			}
			for _, c := range cl {
				f.seenDir[c] = true
			}
			f.vol.Dirs++
			sub := f.readClusters(cl)
			np := append(append([]string{}, path...), e.name)
			f.walkDir(sub, np, deleted, depth+1, cat)
			continue
		}
		file := &File{
			Volume: f.vol.Name, Dir: path, Name: e.name, Size: int64(e.size),
			MTime: e.mtime, Deleted: deleted, Hidden: e.attr&0x06 != 0, Cat: cat,
		}
		if e.size > 0 {
			need := (int64(e.size) + f.g.cs - 1) / f.g.cs
			if !deleted && f.fat != nil && !f.lost {
				cl, ok := f.chain(e.cluster, need)
				file.Extents = trimExtents(f.clusterExtents(cl), file.Size)
				file.Live = true
				if !ok || int64(len(cl)) < need {
					// broken chain: continue with the next clusters in a row
					var last int64 = int64(e.cluster)
					if len(cl) > 0 {
						last = int64(cl[len(cl)-1]) + 1
					}
					rest := need - int64(len(cl))
					if len(cl) == 0 {
						rest = need
					}
					if f.g.valid(last) {
						file.Extents = append(file.Extents, Extent{f.g.off(last), rest * f.g.cs})
						file.Extents = trimExtents(mergeExtents(file.Extents), file.Size)
					}
					file.Damaged = "file system damaged - file may be incomplete"
					file.Live = false
				}
			} else {
				start := f.startOf(e, false)
				if start == 0 || !f.g.valid(int64(start)) {
					file.Damaged = "location of the data is unknown"
				} else {
					file.Extents = f.contiguous(start, file.Size)
					if f.fat != nil || f.alloc != nil {
						for c := int64(start); c < int64(start)+need && f.g.valid(c); c++ {
							if !f.isFree(uint32(c)) {
								file.Damaged = "partly overwritten by newer files"
								file.AltExtents = f.freeRun(start, need, file.Size)
								break
							}
						}
					}
				}
			}
		}
		f.vol.Files = append(f.vol.Files, file)
		switch {
		case file.Deleted:
			f.vol.Deleted++
		case file.Hidden:
			f.vol.Hidden++
			f.vol.Live++
		default:
			f.vol.Live++
		}
	}
}

// freeRun returns the first `need` free clusters from start on, as extents
// (how FAT places a file around clusters that were in use at the time).
func (f *fatFS) freeRun(start uint32, need, size int64) []Extent {
	var ex []Extent
	got := int64(0)
	limit := int64(start) + need*8 + 4096
	for c := int64(start); got < need && c < limit && f.g.valid(c); c++ {
		if f.isFree(uint32(c)) {
			ex = append(ex, Extent{f.g.off(c), f.g.cs})
			got++
		}
	}
	if got < need {
		return nil
	}
	return trimExtents(mergeExtents(ex), size)
}

// maxDirClusters limits how big a folder may be (corrupt chains).
func (f *fatFS) maxDirClusters() int64 {
	n := int64(32<<20) / f.g.cs
	if n < 4 {
		n = 4
	}
	return n
}

// contiguous returns extents for size bytes starting at cluster c, cut at
// the end of the volume.
func (f *fatFS) contiguous(c uint32, size int64) []Extent {
	off := f.g.off(int64(c))
	end := f.g.off(f.g.first + f.g.clusters)
	if f.g.clusters == 0 {
		end = f.r.Size()
	}
	n := size
	if off+n > end {
		n = end - off
	}
	if n <= 0 {
		return nil
	}
	return []Extent{{off, n}}
}

// deletedStart works out the first cluster of a deleted entry. On FAT32,
// Windows clears the upper 16 bits of the cluster number when deleting, so
// several candidates are checked by their content.
func (f *fatFS) deletedStart(e fatEntry, isDir bool) uint32 {
	c := e.cluster
	if f.bits != 32 || !e.hiZero || f.g.clusters+2 <= 0xFFFF {
		if f.g.valid(int64(c)) {
			return c
		}
		return 0
	}
	lo := c & 0xFFFF
	ext := ""
	if i := strings.LastIndexByte(e.name, '.'); i >= 0 {
		ext = strings.ToLower(e.name[i+1:])
	}
	best, bestScore, bestHi := uint32(0), -1000, int64(0)
	head := make([]byte, 1024)
	for hi := int64(0); ; hi++ {
		cand := uint32(hi)<<16 | lo
		if int64(cand) >= f.g.first+f.g.clusters {
			break
		}
		if cand < 2 {
			continue
		}
		score := 0
		if f.isFree(cand) {
			score += 10
		} else {
			score -= 10
		}
		f.r.ReadAt(head, f.g.off(int64(cand)))
		if isDir {
			if string(head[0:11]) == ".          " && head[11]&0x10 != 0 {
				self := uint32(le16(head[20:22]))<<16 | uint32(le16(head[26:28]))
				if self == cand {
					score += 100
				}
			}
		} else if m, known := sig.ExtMatches(ext, head); known {
			if m {
				score += 50
			} else {
				score -= 5
			}
		}
		if hi == f.hiHint {
			score += 5
		}
		if hi == 0 {
			score++
		}
		if score > bestScore {
			best, bestScore, bestHi = cand, score, hi
		}
	}
	if bestScore >= 50 {
		f.hiHint = bestHi
	}
	return best
}

// guessDirClusters returns the clusters of a deleted folder: its first
// cluster plus following clusters that still look like folder entries.
func (f *fatFS) guessDirClusters(start uint32) []uint32 {
	if !f.g.valid(int64(start)) {
		return nil
	}
	buf := make([]byte, f.g.cs)
	f.r.ReadAt(buf, f.g.off(int64(start)))
	ents, valid, invalid := parseFATDir(buf, f.bits)
	_ = ents
	isDot := string(buf[0:11]) == ".          "
	if !isDot && (valid == 0 || invalid > valid) {
		return nil
	}
	out := []uint32{start}
	for c := start + 1; len(out) < 32 && f.g.valid(int64(c)); c++ {
		if !f.isFree(c) {
			break
		}
		f.r.ReadAt(buf, f.g.off(int64(c)))
		if string(buf[0:11]) == ".          " {
			break
		}
		_, v, inv := parseFATDir(buf, f.bits)
		if v == 0 || inv > v/4 {
			break
		}
		out = append(out, c)
	}
	return out
}
