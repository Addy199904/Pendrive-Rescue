package fsys

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"pendriverescue/internal/disk"
)

// ntfsFS reads an NTFS volume through its Master File Table, or a set of
// loose MFT records found by scanning after a format.
type ntfsFS struct {
	r        *disk.Reader
	vol      *Volume
	start    int64
	cs       int64
	recSize  int64
	mftEx    []Extent
	bitmap   []byte
	recs     map[uint32]*mftRec
	maxFiles int
	lost     bool
}

type run struct {
	lcn int64 // -1 = sparse
	n   int64 // clusters
}

type dataAttr struct {
	resident   bool
	content    []byte
	runs       []run
	startVCN   int64
	size       int64
	initSize   int64
	compressed bool
	encrypted  bool
}

type fnAttr struct {
	parent uint64
	name   string
	ns     byte
	mtime  time.Time
	size   int64
}

type mftRec struct {
	num     uint32
	seq     uint16
	inUse   bool
	isDir   bool
	base    uint32
	names   []fnAttr
	mtime   time.Time
	fattr   uint32
	data    []*dataAttr
	hasList bool
}

func (m *mftRec) bestName() *fnAttr {
	var best *fnAttr
	for i := range m.names {
		n := &m.names[i]
		if best == nil || (best.ns == 2 && n.ns != 2) {
			best = n
		}
	}
	return best
}

func openNTFS(r *disk.Reader, start int64, boot []byte) (*ntfsFS, error) {
	bps := int64(le16(boot[11:13]))
	spcRaw := boot[13]
	var spc int64
	if spcRaw > 0x80 {
		spc = int64(1) << uint(256-int(spcRaw))
	} else {
		spc = int64(spcRaw)
	}
	if bps == 0 || spc == 0 {
		return nil, fmt.Errorf("invalid NTFS boot sector")
	}
	cs := bps * spc
	total := int64(le64(boot[40:48]))
	mftLCN := int64(le64(boot[48:56]))
	cpr := int8(boot[64])
	var recSize int64
	if cpr < 0 {
		recSize = int64(1) << uint(-cpr)
	} else {
		recSize = int64(cpr) * cs
	}
	if recSize < 256 || recSize > 65536 {
		return nil, fmt.Errorf("invalid NTFS record size")
	}
	n := &ntfsFS{r: r, start: start, cs: cs, recSize: recSize, recs: map[uint32]*mftRec{}, maxFiles: 5_000_000}
	n.vol = &Volume{Type: "NTFS", Start: start, Size: total * bps, ClusterSize: cs}
	n.vol.geom = geometry{kind: "ntfs", start: start, dataStart: start, cs: cs, first: 0, clusters: total * bps / cs}
	// read $MFT's own record to learn where the MFT is
	rec0, err := readAt(r, start+mftLCN*cs, int(recSize))
	if err != nil {
		return nil, err
	}
	m0, ok := parseMFTRecord(rec0)
	if !ok || len(m0.data) == 0 || m0.data[0].resident {
		return nil, fmt.Errorf("the NTFS master file table is damaged")
	}
	d := m0.data[0]
	var ex []Extent
	for _, rn := range d.runs {
		if rn.lcn < 0 {
			continue
		}
		ex = append(ex, Extent{start + rn.lcn*cs, rn.n * cs})
	}
	mftSize := d.size
	if mftSize > 8<<30 || mftSize > n.vol.Size {
		mftSize = min(8<<30, n.vol.Size)
	}
	n.mftEx = trimExtents(mergeExtents(ex), mftSize)
	for _, e := range n.mftEx {
		n.vol.mftRanges = append(n.vol.mftRanges, e)
	}
	n.vol.allocated = func(off int64) bool {
		if n.bitmap == nil {
			return false
		}
		c := (off - start) / cs
		if c < 0 || c/8 >= int64(len(n.bitmap)) {
			return false
		}
		return n.bitmap[c/8]&(1<<uint(c%8)) != 0
	}
	return n, nil
}

// fixup applies the NTFS update sequence to a record in place.
func fixup(rec []byte) bool {
	usaOff := int(le16(rec[4:6]))
	usaCnt := int(le16(rec[6:8]))
	if usaCnt < 2 || usaOff+usaCnt*2 > len(rec) {
		return false
	}
	ok := true
	for i := 1; i < usaCnt; i++ {
		pos := i*512 - 2
		if pos+2 > len(rec) {
			break
		}
		if rec[pos] != rec[usaOff] || rec[pos+1] != rec[usaOff+1] {
			ok = false
		}
		rec[pos] = rec[usaOff+2*i]
		rec[pos+1] = rec[usaOff+2*i+1]
	}
	return ok
}

func parseRuns(b []byte) []run {
	var runs []run
	lcn := int64(0)
	for i := 0; i < len(b); {
		h := b[i]
		if h == 0 {
			break
		}
		ls := int(h & 0xF)
		os := int(h >> 4)
		if ls == 0 || ls > 8 || os > 8 || i+1+ls+os > len(b) {
			break
		}
		var length int64
		for k := ls - 1; k >= 0; k-- {
			length = length<<8 | int64(b[i+1+k])
		}
		if os == 0 {
			runs = append(runs, run{-1, length})
		} else {
			var off int64
			for k := os - 1; k >= 0; k-- {
				off = off<<8 | int64(b[i+1+ls+k])
			}
			// sign-extend
			shift := uint(64 - 8*os)
			off = off << shift >> shift
			lcn += off
			runs = append(runs, run{lcn, length})
		}
		i += 1 + ls + os
	}
	return runs
}

// parseMFTRecord decodes one FILE record (a copy is fixed up).
func parseMFTRecord(raw []byte) (*mftRec, bool) {
	if len(raw) < 64 || string(raw[0:4]) != "FILE" {
		return nil, false
	}
	rec := make([]byte, len(raw))
	copy(rec, raw)
	fixup(rec)
	m := &mftRec{
		seq:   le16(rec[0x10:]),
		inUse: le16(rec[0x16:])&1 != 0,
		isDir: le16(rec[0x16:])&2 != 0,
		base:  uint32(le64(rec[0x20:]) & 0xFFFFFFFFFFFF),
		num:   le32(rec[0x2C:]),
	}
	p := int(le16(rec[0x14:]))
	used := int(le32(rec[0x18:]))
	if used > len(rec) || used < p {
		used = len(rec)
	}
	for p+16 <= used {
		typ := le32(rec[p:])
		if typ == 0xFFFFFFFF {
			break
		}
		alen := int(le32(rec[p+4:]))
		if alen < 16 || p+alen > len(rec) {
			break
		}
		a := rec[p : p+alen]
		nonRes := a[8] != 0
		nameLen := int(a[9])
		aflags := le16(a[12:14])
		var content []byte
		if !nonRes && alen >= 24 {
			vl := int(le32(a[16:20]))
			vo := int(le16(a[20:22]))
			if vo+vl <= alen {
				content = a[vo : vo+vl]
			}
		}
		switch typ {
		case 0x10:
			if len(content) >= 36 {
				m.mtime = filetime(le64(content[8:16]))
				m.fattr = le32(content[32:36])
			}
		case 0x20:
			m.hasList = true
		case 0x30:
			if len(content) >= 66 {
				nl := int(content[64])
				if 66+nl*2 <= len(content) {
					m.names = append(m.names, fnAttr{
						parent: le64(content[0:8]),
						mtime:  filetime(le64(content[16:24])),
						size:   int64(le64(content[48:56])),
						ns:     content[65],
						name:   decodeUTF16(content[66 : 66+nl*2]),
					})
				}
			}
		case 0x80:
			if nameLen != 0 {
				break // alternate data stream: ignore
			}
			d := &dataAttr{compressed: aflags&0x0001 != 0, encrypted: aflags&0x4000 != 0}
			if !nonRes {
				d.resident = true
				d.content = append([]byte(nil), content...)
				d.size = int64(len(content))
				d.initSize = d.size
			} else if alen >= 64 {
				d.startVCN = int64(le64(a[16:24]))
				ro := int(le16(a[32:34]))
				d.size = int64(le64(a[48:56]))
				d.initSize = int64(le64(a[56:64]))
				if ro < alen {
					d.runs = parseRuns(a[ro:])
				}
			}
			m.data = append(m.data, d)
		}
		p += alen
	}
	return m, true
}

// readMFT parses every record of the master file table.
func (n *ntfsFS) readMFT() {
	buf := make([]byte, 1<<20)
	num := uint32(0)
	for _, e := range n.mftEx {
		for pos := int64(0); pos < e.Len; {
			chunk := int64(len(buf))
			if pos+chunk > e.Len {
				chunk = e.Len - pos
			}
			chunk -= chunk % n.recSize
			if chunk <= 0 {
				break
			}
			n.r.ReadAt(buf[:chunk], e.Off+pos)
			for o := int64(0); o < chunk; o += n.recSize {
				if m, ok := parseMFTRecord(buf[o : o+n.recSize]); ok {
					m.num = num
					n.recs[num] = m
				}
				num++
			}
			pos += chunk
		}
	}
	// $Bitmap (record 6) tells which clusters are in use
	if b := n.recs[6]; b != nil && len(b.data) > 0 {
		d := b.data[0]
		ex := n.runsToExtents(d.runs)
		ex = trimExtents(ex, d.size)
		var total int64
		for _, e := range ex {
			total += e.Len
		}
		if total > 0 && total < 512<<20 {
			n.bitmap = make([]byte, total)
			p := int64(0)
			for _, e := range ex {
				if e.Off >= 0 {
					n.r.ReadAt(n.bitmap[p:p+e.Len], e.Off)
				}
				p += e.Len
			}
		}
	}
}

func (n *ntfsFS) runsToExtents(runs []run) []Extent {
	var ex []Extent
	for _, rn := range runs {
		if rn.lcn < 0 {
			ex = append(ex, Extent{-1, rn.n * n.cs})
		} else {
			ex = append(ex, Extent{n.start + rn.lcn*n.cs, rn.n * n.cs})
		}
	}
	return mergeExtents(ex)
}

// mergeExtensionData joins $DATA pieces stored in extension records.
func (n *ntfsFS) mergeExtensionData() {
	ext := map[uint32][]*dataAttr{}
	for _, m := range n.recs {
		if m.base != 0 && m.base != m.num {
			for _, d := range m.data {
				if !d.resident {
					ext[m.base] = append(ext[m.base], d)
				}
			}
		}
	}
	for b, parts := range ext {
		m := n.recs[b]
		if m == nil {
			continue
		}
		all := append([]*dataAttr{}, m.data...)
		all = append(all, parts...)
		sort.Slice(all, func(i, j int) bool { return all[i].startVCN < all[j].startVCN })
		var first *dataAttr
		for _, d := range all {
			if d.resident {
				continue
			}
			if first == nil {
				cp := *d
				cp.runs = append([]run{}, d.runs...)
				first = &cp
				continue
			}
			first.runs = append(first.runs, d.runs...)
			if d.size > first.size {
				first.size = d.size
				first.initSize = d.initSize
			}
		}
		if first != nil {
			m.data = []*dataAttr{first}
		}
	}
}

// pathOf builds the folder path of a record, following parent references.
func (n *ntfsFS) pathOf(fn *fnAttr) ([]string, bool) {
	var parts []string
	ref := fn.parent
	for depth := 0; depth < 64; depth++ {
		num := uint32(ref & 0xFFFFFFFFFFFF)
		seq := uint16(ref >> 48)
		if num == 5 {
			// reverse
			for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
				parts[i], parts[j] = parts[j], parts[i]
			}
			return parts, true
		}
		p := n.recs[num]
		if p == nil || !p.isDir {
			break
		}
		if seq != 0 && p.seq != seq && p.seq != seq+1 {
			break
		}
		pn := p.bestName()
		if pn == nil {
			break
		}
		parts = append(parts, pn.name)
		ref = pn.parent
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return append([]string{"Folder unknown"}, parts...), false
}

// walkRecords turns parsed MFT records into files. lost=true is used for
// records of an old, formatted-over NTFS found by scanning.
func (n *ntfsFS) walkRecords(lost bool) {
	n.lost = lost
	n.mergeExtensionData()
	nums := make([]uint32, 0, len(n.recs))
	for k := range n.recs {
		nums = append(nums, k)
	}
	sort.Slice(nums, func(i, j int) bool { return nums[i] < nums[j] })
	for _, num := range nums {
		m := n.recs[num]
		if m.base != 0 && m.base != m.num {
			continue
		}
		if num < 24 && !n.lost {
			continue // NTFS system files
		}
		fn := m.bestName()
		if fn == nil {
			continue
		}
		if m.isDir {
			n.vol.Dirs++
			continue
		}
		if strings.HasPrefix(fn.name, "$") && uint32(fn.parent&0xFFFFFFFFFFFF) <= 11 {
			continue
		}
		dir, _ := n.pathOf(fn)
		if len(dir) > 0 && dir[0] == "$Extend" {
			continue
		}
		f := &File{
			Volume: n.vol.Name, Dir: dir, Name: fn.name, MTime: m.mtime,
			Deleted: !m.inUse, Hidden: m.fattr&0x06 != 0,
		}
		if n.lost {
			f.Cat = CatLost
		}
		if f.MTime.IsZero() {
			f.MTime = fn.mtime
		}
		if len(m.data) == 0 {
			f.Size = 0
			if fn.size > 0 {
				f.Damaged = "file details are incomplete (data location lost)"
				f.Size = 0
			}
		} else {
			d := m.data[0]
			f.Size = d.size
			switch {
			case d.encrypted:
				f.Note = "encrypted with Windows EFS - needs the original Windows account"
			case d.compressed:
				f.Note = "NTFS-compressed"
			}
			if d.resident {
				f.Data = d.content
			} else {
				ex := n.runsToExtents(d.runs)
				if d.startVCN > 0 {
					ex = append([]Extent{{-1, d.startVCN * n.cs}}, ex...)
				}
				if d.initSize >= 0 && d.initSize < d.size {
					ex = trimExtents(ex, d.initSize)
					ex = append(ex, Extent{-1, d.size - d.initSize})
				}
				f.Extents = trimExtents(mergeExtents(ex), d.size)
				f.Live = m.inUse && !n.lost
				if (f.Deleted || n.lost) && n.vol.allocated != nil && n.overlapsAllocated(f.Extents) {
					f.Damaged = "partly overwritten by newer files"
				}
				if d.compressed {
					f.Compressed = true
					f.CompExtents = n.runsToExtents(d.runs)
					f.CompCS = n.cs
					f.Note = ""
				}
			}
		}
		n.vol.Files = append(n.vol.Files, f)
		switch {
		case f.Deleted:
			n.vol.Deleted++
		case f.Hidden:
			n.vol.Hidden++
			n.vol.Live++
		default:
			n.vol.Live++
		}
		if len(n.vol.Files) >= n.maxFiles {
			break
		}
	}
}

func (n *ntfsFS) overlapsAllocated(ex []Extent) bool {
	checks := 0
	for _, e := range ex {
		if e.Off < 0 {
			continue
		}
		for o := e.Off; o < e.Off+e.Len; o += n.cs {
			if n.vol.allocated(o) {
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
