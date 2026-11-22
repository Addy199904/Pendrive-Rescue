package fsys

import (
	"fmt"
	"time"

	"pendriverescue/internal/disk"
	"pendriverescue/internal/scan"
)

// exfatFS reads an exFAT volume (or, with fat == nil, folders of a lost one).
type exfatFS struct {
	r        *disk.Reader
	vol      *Volume
	g        geometry
	fat      []uint32
	bitmap   []byte
	rootClus uint32
	seenDir  map[uint32]bool
	maxFiles int
	alloc    func(off int64) bool // optional "in use now" test (lost mode)
}

type exEntry struct {
	name       string
	attr       uint16
	first      uint32
	size       int64
	validSize  int64
	noFatChain bool
	mtime      time.Time
	deleted    bool
	dir        bool
}

func openExFAT(r *disk.Reader, start int64, boot []byte) (*exfatFS, error) {
	bpsShift := int64(boot[108])
	spcShift := int64(boot[109])
	if bpsShift < 9 || bpsShift > 12 || bpsShift+spcShift > 25 {
		return nil, fmt.Errorf("invalid exFAT boot sector")
	}
	bps := int64(1) << bpsShift
	fatOff := int64(le32(boot[80:84]))
	fatLen := int64(le32(boot[84:88]))
	heapOff := int64(le32(boot[88:92]))
	count := int64(le32(boot[92:96]))
	root := le32(boot[96:100])
	volLen := int64(le64(boot[72:80]))
	if heapOff == 0 || count == 0 || fatOff == 0 {
		return nil, fmt.Errorf("invalid exFAT boot sector")
	}
	if count > 1<<32-11 || count*(bps<<spcShift) > 16*r.Size()+(1<<30) {
		return nil, fmt.Errorf("implausible exFAT boot sector")
	}
	x := &exfatFS{r: r, rootClus: root, seenDir: map[uint32]bool{}, maxFiles: 5_000_000}
	x.g = geometry{kind: "exfat", start: start, dataStart: start + heapOff*bps, cs: bps << spcShift, first: 2, clusters: count}
	x.vol = &Volume{Type: "exFAT", Start: start, Size: volLen * bps, ClusterSize: x.g.cs, geom: x.g, exfatDirs: x.seenDir}
	need := (count + 2) * 4
	if need > fatLen*bps {
		need = fatLen * bps
	}
	if need > 1<<30 {
		return nil, fmt.Errorf("exFAT table too large")
	}
	raw, err := readAt(r, start+fatOff*bps, int(need))
	if err != nil {
		return nil, err
	}
	x.fat = make([]uint32, len(raw)/4)
	for i := range x.fat {
		x.fat[i] = le32(raw[i*4:])
	}
	x.vol.allocated = func(off int64) bool {
		if off < x.g.dataStart {
			return true
		}
		c := (off-x.g.dataStart)/x.g.cs + 2
		return x.allocatedCluster(c)
	}
	return x, nil
}

func (x *exfatFS) allocatedCluster(c int64) bool {
	if x.bitmap == nil {
		return false
	}
	i := c - 2
	if i < 0 || i/8 >= int64(len(x.bitmap)) {
		return false
	}
	return x.bitmap[i/8]&(1<<uint(i%8)) != 0
}

func (x *exfatFS) chain(c uint32, max int64) ([]uint32, bool) {
	var out []uint32
	seen := map[uint32]bool{}
	for {
		if !x.g.valid(int64(c)) || seen[c] {
			return out, false
		}
		seen[c] = true
		out = append(out, c)
		if max > 0 && int64(len(out)) >= max {
			return out, true
		}
		if x.fat == nil || int(c) >= len(x.fat) {
			return out, false
		}
		n := x.fat[c]
		if n == 0xFFFFFFFF {
			return out, true
		}
		if n < 2 || n == 0xFFFFFFF7 {
			return out, false
		}
		c = n
	}
}

// extentsFor returns where an entry's data lives.
func (x *exfatFS) extentsFor(e exEntry, deleted bool) (ex []Extent, guessed bool) {
	if e.size <= 0 || e.first < 2 {
		return nil, false
	}
	need := (e.size + x.g.cs - 1) / x.g.cs
	if e.noFatChain || x.fat == nil {
		return x.contig(e.first, e.size), deleted
	}
	cl, ok := x.chain(e.first, need)
	if ok && int64(len(cl)) == need {
		good := true
		if deleted {
			for _, c := range cl {
				if x.allocatedCluster(int64(c)) {
					good = false
					break
				}
			}
		}
		if good {
			for _, c := range cl {
				ex = append(ex, Extent{x.g.off(int64(c)), x.g.cs})
			}
			return trimExtents(mergeExtents(ex), e.size), deleted
		}
	}
	return x.contig(e.first, e.size), true
}

func (x *exfatFS) contig(c uint32, size int64) []Extent {
	off := x.g.off(int64(c))
	end := x.g.off(x.g.first + x.g.clusters)
	if x.g.clusters == 0 {
		end = x.r.Size()
	}
	if off+size > end {
		size = end - off
	}
	if size <= 0 {
		return nil
	}
	return []Extent{{off, size}}
}

func (x *exfatFS) read(ex []Extent, max int64) []byte {
	var total int64
	for _, e := range ex {
		total += e.Len
	}
	if total > max {
		total = max
	}
	buf := make([]byte, total)
	pos := int64(0)
	for _, e := range ex {
		if pos >= total {
			break
		}
		n := e.Len
		if pos+n > total {
			n = total - pos
		}
		x.r.ReadAt(buf[pos:pos+n], e.Off)
		pos += n
	}
	return buf
}

func exfatTime(ts uint32, ms10, utc byte) time.Time {
	if ts == 0 {
		return time.Time{}
	}
	sec := int(ts&0x1F) * 2
	min := int((ts >> 5) & 0x3F)
	hour := int((ts >> 11) & 0x1F)
	day := int((ts >> 16) & 0x1F)
	mon := time.Month((ts >> 21) & 0xF)
	year := int(ts>>25) + 1980
	if mon < 1 || mon > 12 || day < 1 {
		return time.Time{}
	}
	loc := time.Local
	if utc&0x80 != 0 {
		q := int(int8(utc<<1) >> 1)
		loc = time.FixedZone("", q*15*60)
	}
	t := time.Date(year, mon, day, hour, min, sec, 0, loc)
	if ms10 < 200 {
		t = t.Add(time.Duration(ms10) * 10 * time.Millisecond)
	}
	return t
}

// parseExfatSet decodes one file entry set starting at b[0].
func parseExfatSet(b []byte) (exEntry, int, bool) {
	sec := int(b[1])
	if sec < 2 || sec > 18 || len(b) < (sec+1)*32 {
		return exEntry{}, 1, false
	}
	set := b[:(sec+1)*32]
	if scan.ExfatChecksum(set) != le16(b[2:4]) {
		return exEntry{}, 1, false
	}
	deleted := b[0] == 0x05
	st := set[32:64]
	if st[0]&0x7F != 0x40 {
		return exEntry{}, 1, false
	}
	e := exEntry{
		attr:       le16(b[4:6]),
		deleted:    deleted,
		noFatChain: st[1]&0x02 != 0,
		first:      le32(st[20:24]),
		size:       int64(le64(st[24:32])),
		validSize:  int64(le64(st[8:16])),
		mtime:      exfatTime(le32(b[12:16]), b[21], b[23]),
	}
	e.dir = e.attr&0x10 != 0
	nameLen := int(st[3])
	var raw []byte
	for i := 2; i <= sec && len(raw) < nameLen*2; i++ {
		ent := set[i*32 : i*32+32]
		if ent[0]&0x7F != 0x41 {
			break
		}
		raw = append(raw, ent[2:32]...)
	}
	if len(raw) > nameLen*2 {
		raw = raw[:nameLen*2]
	}
	e.name = decodeUTF16(raw)
	if e.name == "" {
		return exEntry{}, 1, false
	}
	return e, sec + 1, true
}

// parseExfatDir decodes all entry sets of a directory.
func parseExfatDir(data []byte) (ents []exEntry, label string, bitmap Extent) {
	for p := 0; p+32 <= len(data); {
		t := data[p]
		if t == 0x00 {
			break
		}
		switch t {
		case 0x83:
			n := int(data[p+1])
			if n > 11 {
				n = 11
			}
			label = decodeUTF16(data[p+2 : p+2+n*2])
		case 0x81:
			if bitmap.Len == 0 {
				bitmap = Extent{int64(le32(data[p+20 : p+24])), int64(le64(data[p+24 : p+32]))}
			}
		case 0x85, 0x05:
			if e, n, ok := parseExfatSet(data[p:]); ok {
				ents = append(ents, e)
				p += n * 32
				continue
			}
		}
		p += 32
	}
	return
}

func (x *exfatFS) walk() {
	cl, _ := x.chain(x.rootClus, (32<<20)/x.g.cs+1)
	for _, c := range cl {
		x.seenDir[c] = true
	}
	var ex []Extent
	for _, c := range cl {
		ex = append(ex, Extent{x.g.off(int64(c)), x.g.cs})
	}
	data := x.read(mergeExtents(ex), 64<<20)
	_, label, bm := parseExfatDir(data)
	x.vol.Label = label
	if bm.Len > 0 && bm.Len <= x.g.clusters/8+4096 && bm.Len < 512<<20 && x.g.valid(bm.Off) {
		bcl, _ := x.chain(uint32(bm.Off), (bm.Len+x.g.cs-1)/x.g.cs)
		var bex []Extent
		for _, c := range bcl {
			bex = append(bex, Extent{x.g.off(int64(c)), x.g.cs})
		}
		x.bitmap = x.read(mergeExtents(bex), bm.Len)
	}
	x.walkDir(data, nil, false, 0, CatNamed)
}

func (x *exfatFS) walkDir(data []byte, path []string, parentDeleted bool, depth int, cat Category) {
	ents, _, _ := parseExfatDir(data)
	for _, e := range ents {
		if len(x.vol.Files) >= x.maxFiles {
			return
		}
		deleted := e.deleted || parentDeleted
		if e.dir {
			if depth > 40 || e.size <= 0 || e.size > 32<<20 {
				continue
			}
			ex, _ := x.extentsFor(e, deleted)
			if len(ex) == 0 {
				continue
			}
			c := uint32((ex[0].Off-x.g.dataStart)/x.g.cs + 2)
			if x.seenDir[c] {
				continue
			}
			for _, e2 := range ex {
				for o := e2.Off; o < e2.Off+e2.Len; o += x.g.cs {
					x.seenDir[uint32((o-x.g.dataStart)/x.g.cs+2)] = true
				}
			}
			x.vol.Dirs++
			sub := x.read(ex, e.size)
			np := append(append([]string{}, path...), e.name)
			x.walkDir(sub, np, deleted, depth+1, cat)
			continue
		}
		f := &File{
			Volume: x.vol.Name, Dir: path, Name: e.name, Size: e.size, MTime: e.mtime,
			Deleted: deleted, Hidden: e.attr&0x06 != 0, Cat: cat,
		}
		if e.size > 0 {
			ex, guessed := x.extentsFor(e, deleted)
			if len(ex) == 0 {
				f.Damaged = "location of the data is unknown"
			} else {
				if e.validSize >= 0 && e.validSize < e.size {
					// bytes after ValidDataLength are defined as zeros
					ex = trimExtents(ex, e.validSize)
					ex = append(ex, Extent{-1, e.size - e.validSize})
				}
				f.Extents = ex
				f.Live = !deleted && x.fat != nil
				if (deleted || x.fat == nil) && (x.bitmap != nil || x.alloc != nil) && x.overlapsAllocated(ex) {
					f.Damaged = "partly overwritten by newer files"
					f.AltExtents = x.freeRun(e.first, (e.size+x.g.cs-1)/x.g.cs, e.size)
				}
				_ = guessed
			}
		}
		x.vol.Files = append(x.vol.Files, f)
		switch {
		case f.Deleted:
			x.vol.Deleted++
		case f.Hidden:
			x.vol.Hidden++
			x.vol.Live++
		default:
			x.vol.Live++
		}
	}
}

// freeRun returns the first `need` clusters from start that are not in use.
func (x *exfatFS) freeRun(start uint32, need, size int64) []Extent {
	var ex []Extent
	got := int64(0)
	limit := int64(start) + need*8 + 4096
	for c := int64(start); got < need && c < limit && x.g.valid(c); c++ {
		off := x.g.off(c)
		used := false
		if x.bitmap != nil {
			used = x.allocatedCluster(c)
		} else if x.alloc != nil {
			used = x.alloc(off)
		}
		if !used {
			ex = append(ex, Extent{off, x.g.cs})
			got++
		}
	}
	if got < need {
		return nil
	}
	return trimExtents(mergeExtents(ex), size)
}

// overlapsAllocated reports whether any cluster of ex is in use now.
func (x *exfatFS) overlapsAllocated(ex []Extent) bool {
	checks := 0
	for _, e := range ex {
		if e.Off < 0 {
			continue
		}
		for o := e.Off; o < e.Off+e.Len; o += x.g.cs {
			if x.bitmap != nil && x.allocatedCluster((o-x.g.dataStart)/x.g.cs+2) {
				return true
			}
			if x.bitmap == nil && x.alloc != nil && x.alloc(o) {
				return true
			}
			checks++
			if checks > 1<<20 {
				return false
			}
		}
	}
	return false
}
