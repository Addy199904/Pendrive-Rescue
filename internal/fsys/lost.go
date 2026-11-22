package fsys

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"pendriverescue/internal/disk"
	"pendriverescue/internal/scan"
	"pendriverescue/internal/sig"
)

// HitIndex answers "which file type starts at this offset?".
type HitIndex struct {
	offs   []int64
	kinds  []sig.Kind
	byKind map[sig.Kind][]int64
}

// NewHitIndex builds an index over the scan's hits (sorted by offset).
func NewHitIndex(hits []scan.Hit) *HitIndex {
	h := &HitIndex{byKind: map[sig.Kind][]int64{}}
	for _, x := range hits {
		h.offs = append(h.offs, x.Off)
		h.kinds = append(h.kinds, x.Kind)
		h.byKind[x.Kind] = append(h.byKind[x.Kind], x.Off)
	}
	return h
}

// At returns the kind that starts exactly at off (or sig.None).
func (h *HitIndex) At(off int64) sig.Kind {
	i := sort.Search(len(h.offs), func(i int) bool { return h.offs[i] >= off })
	if i < len(h.offs) && h.offs[i] == off {
		return h.kinds[i]
	}
	return sig.None
}

// kindOfExt maps a file extension to the signature its data starts with.
func kindOfExt(ext string) sig.Kind { return sig.KindOfExt(ext) }

type obs struct {
	unit int64
	kind sig.Kind
}

var clusterSizes = func() []int64 {
	var out []int64
	for cs := int64(512); cs <= 32<<20; cs <<= 1 {
		out = append(out, cs)
	}
	return out
}()

// voteGeometry finds the cluster size and the offset of cluster `first`
// that make the most files start with the right kind of data.
func voteGeometry(ob []obs, hx *HitIndex, first, driveSize int64, maxCS int64) (cs, base int64, matches int) {
	var known []obs
	for _, o := range ob {
		if o.kind != sig.None && o.unit >= first {
			known = append(known, o)
		}
	}
	if len(known) == 0 {
		return 0, 0, 0
	}
	sample := known
	if len(sample) > 48 {
		step := len(sample) / 48
		var s2 []obs
		for i := 0; i < len(sample); i += step {
			s2 = append(s2, sample[i])
		}
		sample = s2
	}
	for _, c := range clusterSizes {
		if c > maxCS {
			break
		}
		votes := map[int64]int{}
		for _, o := range sample {
			for _, h := range hx.byKind[o.kind] {
				b := h - (o.unit-first)*c
				if b < 0 || b >= driveSize || b%512 != 0 {
					continue
				}
				votes[b]++
			}
		}
		bestB, bestV := int64(-1), 0
		for b, v := range votes {
			if v > bestV || (v == bestV && b < bestB) {
				bestB, bestV = b, v
			}
		}
		if bestV < 2 && len(sample) > 1 {
			continue
		}
		m := 0
		for _, o := range known {
			if hx.At(bestB+(o.unit-first)*c) == o.kind {
				m++
			}
		}
		if m > matches {
			cs, base, matches = c, bestB, m
		}
	}
	need := 2
	if len(known) >= 6 {
		need = 3
	}
	if len(known) == 1 {
		need = 1
	}
	if matches < need || matches*10 < len(known)*3 {
		return 0, 0, 0
	}
	return
}

// FindLost rebuilds folders and files of file systems that were formatted
// over (or whose boot sector is gone). It returns extra "volumes" that hold
// the lost files.
func FindLost(r *disk.Reader, sr *scan.Result, vols []*Volume, hx *HitIndex) []*Volume {
	var live rangeSet
	for _, v := range vols {
		for _, f := range v.Files {
			if f.Live {
				for _, e := range f.Extents {
					if e.Off >= 0 {
						live.add(e.Off, e.Len)
					}
				}
			}
		}
	}
	live.finish()
	var out []*Volume
	safe := func(fn func() []*Volume) {
		defer func() { recover() }()
		out = append(out, fn()...)
	}
	safe(func() []*Volume { return lostFAT(r, sr, vols, hx, &live) })
	safe(func() []*Volume { return lostExFAT(r, sr, vols, hx, &live) })
	safe(func() []*Volume { return lostNTFS(r, sr, vols, hx, &live) })
	return out
}

func liveAllocFor(vols []*Volume, g geometry) func(off int64) bool {
	var vs []*Volume
	for _, v := range vols {
		if v.allocated != nil {
			vs = append(vs, v)
		}
	}
	if len(vs) == 0 {
		return nil
	}
	return func(off int64) bool {
		for _, v := range vs {
			if off >= v.Start && off < v.Start+v.Size && off >= v.geom.dataStart {
				if v.allocated(off) {
					return true
				}
			}
		}
		return false
	}
}

func sameGeom(a, b geometry) bool { return a.dataStart == b.dataStart && a.cs == b.cs }

// ---------------------------------------------------------------- FAT

func lostFAT(r *disk.Reader, sr *scan.Result, vols []*Volume, hx *HitIndex, live *rangeSet) []*Volume {
	// geometries we already know about
	var known []geometry
	var liveGeoms []geometry
	for _, v := range vols {
		if v.geom.kind == "fat" {
			known = append(known, v.geom)
			liveGeoms = append(liveGeoms, v.geom)
		}
	}
	for _, b := range sr.Boot {
		if b.Type != scan.BootFAT {
			continue
		}
		if fb, ok := parseFATBoot(b.Data); ok {
			for _, st := range []int64{b.Off, b.Off - 6*fb.bps} {
				if st < 0 {
					continue
				}
				known = append(known, geometry{kind: "fat", start: st, dataStart: st + fb.dataSec*fb.bps, cs: fb.spc * fb.bps, first: 2, clusters: fb.clusters, fatBits: fb.bits})
			}
		}
	}
	// lost folders = dot-entry folders not reached from a live tree
	var rem []scan.FatDir
	for _, d := range sr.FatDirs {
		if live.contains(d.Off) {
			continue
		}
		inLive := false
		for _, v := range vols {
			if v.geom.kind != "fat" {
				continue
			}
			g := v.geom
			rel := d.Off - g.dataStart
			if rel >= 0 && rel%g.cs == 0 && rel/g.cs+2 == int64(d.Self) && v.fatDirs[d.Self] {
				inLive = true
				break
			}
		}
		if !inLive {
			rem = append(rem, d)
		}
	}
	if len(rem) == 0 {
		return nil
	}
	fits := func(d scan.FatDir, g geometry) bool {
		rel := d.Off - g.dataStart
		return rel >= 0 && rel%g.cs == 0 && rel/g.cs+2 == int64(d.Self)
	}
	type group struct {
		g    geometry
		dirs []scan.FatDir
	}
	var groups []*group
	assign := func(g geometry) {
		var keep []scan.FatDir
		var grp *group
		for _, gg := range groups {
			if sameGeom(gg.g, g) {
				grp = gg
			}
		}
		for _, d := range rem {
			if fits(d, g) {
				if grp == nil {
					grp = &group{g: g}
					groups = append(groups, grp)
				}
				grp.dirs = append(grp.dirs, d)
			} else {
				keep = append(keep, d)
			}
		}
		rem = keep
	}
	for _, g := range known {
		assign(g)
	}
	// infer geometry from pairs of folders: offset = base + (self-2)*cs
	for len(rem) >= 2 {
		sort.Slice(rem, func(i, j int) bool { return rem[i].Off < rem[j].Off })
		type key struct{ cs, base int64 }
		votes := map[key]int{}
		for i := range rem {
			for j := i + 1; j < len(rem) && j < i+8; j++ {
				dS := int64(rem[j].Self) - int64(rem[i].Self)
				dO := rem[j].Off - rem[i].Off
				if dS <= 0 || dO <= 0 || dO%dS != 0 {
					continue
				}
				cs := dO / dS
				if cs < 512 || cs > 256<<10 || cs&(cs-1) != 0 {
					continue
				}
				base := rem[i].Off - (int64(rem[i].Self)-2)*cs
				if base < 0 {
					continue
				}
				votes[key{cs, base}]++
			}
		}
		var best key
		bv := 0
		for k, v := range votes {
			if v > bv || (v == bv && k.base < best.base) {
				best, bv = k, v
			}
		}
		if bv == 0 {
			break
		}
		g := geometry{kind: "fat", dataStart: best.base, cs: best.cs, first: 2, fatBits: 32}
		g.clusters = (r.Size()-g.dataStart)/g.cs + 1
		g.start = g.dataStart
		before := len(rem)
		assign(g)
		if len(rem) == before {
			break
		}
	}
	// single leftover folders: use their files' contents to find the geometry
	for len(rem) > 0 {
		d := rem[0]
		buf := make([]byte, 64<<10)
		r.ReadAt(buf, d.Off)
		ents, _, _ := parseFATDir(buf, 32)
		var bestG geometry
		bestScore := 0
		for _, cs := range clusterSizes {
			if cs > 256<<10 {
				break
			}
			base := d.Off - (int64(d.Self)-2)*cs
			if base < 0 {
				continue
			}
			score := 0
			for _, e := range ents {
				if e.dir || e.deleted {
					continue
				}
				k := kindOfExt(extOf(e.name))
				if k == sig.None {
					continue
				}
				if hx.At(base+(int64(e.cluster)-2)*cs) == k {
					score++
				}
			}
			if score > bestScore {
				bestScore = score
				bestG = geometry{kind: "fat", dataStart: base, start: base, cs: cs, first: 2, fatBits: 32}
				bestG.clusters = (r.Size()-base)/cs + 1
			}
		}
		if bestScore == 0 {
			rem = rem[1:]
			continue
		}
		assign(bestG)
	}

	var out []*Volume
	for gi, grp := range groups {
		g := grp.g
		if g.clusters == 0 {
			g.clusters = (r.Size()-g.dataStart)/g.cs + 1
		}
		if g.fatBits == 0 {
			g.fatBits = 32
		}
		isLiveGeom := false
		for _, lg := range liveGeoms {
			if sameGeom(lg, g) {
				isLiveGeom = true
			}
		}
		name := "Lost folders (old FAT file system)"
		if isLiveGeom {
			name = "Lost folders (from before the pendrive was formatted)"
		}
		if len(groups) > 1 {
			name = fmt.Sprintf("%s %d", name, gi+1)
		}
		v := &Volume{Name: name, Type: fmt.Sprintf("FAT%d (lost)", g.fatBits), Start: g.dataStart, ClusterSize: g.cs, geom: g}
		f := &fatFS{r: r, vol: v, g: g, bits: g.fatBits, seenDir: map[uint32]bool{}, hiHint: -1, maxFiles: 5_000_000, lost: true}
		if a := liveAllocFor(vols, g); a != nil {
			f.alloc = func(c int64) bool { return a(g.off(c)) }
		}
		bySelf := map[uint32]scan.FatDir{}
		for _, d := range grp.dirs {
			bySelf[d.Self] = d
		}
		// which lost folders are sub-folders of other lost folders?
		hasParent := map[uint32]bool{}
		for _, d := range grp.dirs {
			buf := f.readClusters(f.guessDirClusters(d.Self))
			ents, _, _ := parseFATDir(buf, f.bits)
			for _, e := range ents {
				if e.dir && !e.deleted && e.cluster != d.Self {
					if _, ok := bySelf[e.cluster]; ok {
						hasParent[e.cluster] = true
					}
				}
			}
		}
		// the old root folder may have survived (e.g. FAT32 -> exFAT format)
		if !isLiveGeom {
			rootData := f.readClusters([]uint32{2})
			ents, valid, invalid := parseFATDir(rootData, f.bits)
			if valid >= 2 && invalid*4 <= valid && string(rootData[0:2]) != ". " && len(ents) > 0 {
				f.seenDir[2] = true
				f.walkDir(rootData, nil, false, 0, CatLost)
			}
		}
		n := 0
		sort.Slice(grp.dirs, func(i, j int) bool { return grp.dirs[i].Off < grp.dirs[j].Off })
		for pass := 0; pass < 2; pass++ {
			for _, d := range grp.dirs {
				if f.seenDir[d.Self] || (pass == 0 && hasParent[d.Self]) {
					continue
				}
				cl := f.guessDirClusters(d.Self)
				if len(cl) == 0 {
					continue
				}
				for _, c := range cl {
					f.seenDir[c] = true
				}
				n++
				hdr := make([]byte, 32)
				r.ReadAt(hdr, d.Off)
				date := ""
				if t := dosTime(le16(hdr[16:18]), le16(hdr[14:16]), 0); !t.IsZero() {
					date = t.Format("2006-01-02")
				}
				v.Dirs++
				first := len(v.Files)
				f.walkDir(f.readClusters(cl), []string{"tmp"}, false, 1, CatLost)
				nameLostFolder(v.Files[first:], n, date)
			}
		}
		if len(v.Files) > 0 {
			for _, fl := range v.Files {
				fl.Volume = v.Name
				fl.Cat = CatLost
			}
			out = append(out, v)
		}
	}
	return out
}

func extOf(name string) string {
	i := strings.LastIndexByte(name, '.')
	if i < 0 {
		return ""
	}
	return strings.ToLower(name[i+1:])
}

// ---------------------------------------------------------------- exFAT

func lostExFAT(r *disk.Reader, sr *scan.Result, vols []*Volume, hx *HitIndex, live *rangeSet) []*Volume {
	var liveGeoms []geometry
	for _, v := range vols {
		if v.geom.kind == "exfat" {
			liveGeoms = append(liveGeoms, v.geom)
		}
	}
	type setInfo struct {
		off int64
		e   exEntry
	}
	var rem []setInfo
	buf := make([]byte, 19*32)
	for _, s := range sr.ExfatSets {
		if live.contains(s.Off) {
			continue
		}
		inLive := false
		for _, v := range vols {
			if v.geom.kind != "exfat" {
				continue
			}
			rel := s.Off - v.geom.dataStart
			if rel >= 0 && v.exfatDirs[uint32(rel/v.geom.cs+2)] {
				inLive = true
				break
			}
		}
		if inLive {
			continue
		}
		r.ReadAt(buf, s.Off)
		if e, _, ok := parseExfatSet(buf); ok {
			rem = append(rem, setInfo{s.Off, e})
		}
	}
	if len(rem) == 0 {
		return nil
	}
	// candidate geometries: live exFAT volumes and any exFAT boot sector seen
	var cands []geometry
	cands = append(cands, liveGeoms...)
	for _, b := range sr.Boot {
		if b.Type != scan.BootExFAT {
			continue
		}
		bpsShift := int64(b.Data[108])
		spcShift := int64(b.Data[109])
		if bpsShift < 9 || bpsShift > 12 || bpsShift+spcShift > 25 {
			continue
		}
		bps := int64(1) << bpsShift
		heap := int64(le32(b.Data[88:92]))
		count := int64(le32(b.Data[92:96]))
		for _, st := range []int64{b.Off, b.Off - 12*bps} {
			if st >= 0 {
				cands = append(cands, geometry{kind: "exfat", start: st, dataStart: st + heap*bps, cs: bps << spcShift, first: 2, clusters: count})
			}
		}
	}
	var ob []obs
	for _, s := range rem {
		if !s.e.dir && s.e.size > 0 {
			ob = append(ob, obs{int64(s.e.first), kindOfExt(extOf(s.e.name))})
		}
	}
	score := func(g geometry) int {
		m := 0
		for _, o := range ob {
			if o.kind != sig.None && hx.At(g.off(o.unit)) == o.kind {
				m++
			}
		}
		return m
	}
	var best geometry
	bestScore := -1
	for _, g := range cands {
		// the folder clusters must also be inside the heap
		if s := score(g); s > bestScore {
			best, bestScore = g, s
		}
	}
	if bestScore <= 0 {
		if cs, base, m := voteGeometry(ob, hx, 2, r.Size(), 32<<20); m > 0 {
			best = geometry{kind: "exfat", start: base, dataStart: base, cs: cs, first: 2}
			bestScore = m
		}
	}
	if bestScore < 0 || best.cs == 0 {
		return nil
	}
	g := best
	if g.clusters == 0 {
		g.clusters = (r.Size()-g.dataStart)/g.cs + 1
	}
	isLiveGeom := false
	for _, lg := range liveGeoms {
		if sameGeom(lg, g) {
			isLiveGeom = true
		}
	}
	name := "Lost folders (old exFAT file system)"
	if isLiveGeom {
		name = "Lost folders (from before the pendrive was formatted)"
	}
	v := &Volume{Name: name, Type: "exFAT (lost)", Start: g.dataStart, ClusterSize: g.cs, geom: g}
	x := &exfatFS{r: r, vol: v, g: g, seenDir: map[uint32]bool{}, maxFiles: 5_000_000, alloc: liveAllocFor(vols, g)}

	clusterOf := func(off int64) int64 { return (off-g.dataStart)/g.cs + 2 }
	// clusters that hold sets, and which of them are known sub-folders
	setClusters := map[int64]bool{}
	for _, s := range rem {
		if s.off >= g.dataStart {
			setClusters[clusterOf(s.off)] = true
		}
	}
	covered := map[int64]bool{}
	for _, s := range rem {
		if s.e.dir && s.e.first >= 2 && s.e.size > 0 && s.e.size < 256<<20 {
			n := (s.e.size + g.cs - 1) / g.cs
			if !s.e.noFatChain {
				n = 1
			}
			for c := int64(s.e.first); c < int64(s.e.first)+n; c++ {
				covered[c] = true
			}
		}
	}
	var roots []int64
	for c := range setClusters {
		if !covered[c] {
			roots = append(roots, c)
		}
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i] < roots[j] })
	n := 0
	for i := 0; i < len(roots); {
		// join neighbouring clusters into one folder
		j := i + 1
		for j < len(roots) && roots[j] == roots[j-1]+1 {
			j++
		}
		first, cnt := roots[i], int64(j-i)
		i = j
		if x.seenDir[uint32(first)] {
			continue
		}
		for c := first; c < first+cnt; c++ {
			x.seenDir[uint32(c)] = true
		}
		if cnt*g.cs > 32<<20 {
			cnt = (32 << 20) / g.cs
			if cnt < 1 {
				cnt = 1
			}
		}
		data := make([]byte, cnt*g.cs)
		r.ReadAt(data, g.off(first))
		v.Dirs++
		if isExfatRoot(data) && !isLiveGeom {
			// the old root folder survived: its folders keep their names
			x.walkDir(data, nil, false, 0, CatLost)
			continue
		}
		n++
		startIdx := len(v.Files)
		x.walkDir(data, []string{"tmp"}, false, 1, CatLost)
		nameLostFolder(v.Files[startIdx:], n, "")
	}
	for _, f := range v.Files {
		f.Volume = v.Name
		f.Cat = CatLost
		f.Live = false
	}
	if len(v.Files) == 0 {
		return nil
	}
	return []*Volume{v}
}

// ---------------------------------------------------------------- NTFS

func lostNTFS(r *disk.Reader, sr *scan.Result, vols []*Volume, hx *HitIndex, live *rangeSet) []*Volume {
	var mft rangeSet
	for _, v := range vols {
		for _, e := range v.mftRanges {
			mft.add(e.Off, e.Len)
		}
	}
	mft.finish()
	type recAt struct {
		off int64
		m   *mftRec
	}
	var recs []recAt
	buf := make([]byte, 4096)
	for _, off := range sr.MftRecs {
		if mft.contains(off) || live.contains(off) {
			continue
		}
		r.ReadAt(buf[:1024], off)
		size := 1024
		if c := int(le16(buf[6:8])); c == 9 {
			size = 4096
			r.ReadAt(buf, off)
		}
		if m, ok := parseMFTRecord(buf[:size]); ok && len(m.names) > 0 {
			recs = append(recs, recAt{off, m})
		}
	}
	if len(recs) == 0 {
		return nil
	}
	// geometry: a live NTFS volume, an NTFS boot sector, or inferred
	var cands []geometry
	var liveVols []*Volume
	for _, v := range vols {
		if v.geom.kind == "ntfs" {
			cands = append(cands, v.geom)
			liveVols = append(liveVols, v)
		}
	}
	for _, b := range sr.Boot {
		if b.Type != scan.BootNTFS {
			continue
		}
		bps := int64(le16(b.Data[11:13]))
		spcRaw := b.Data[13]
		spc := int64(spcRaw)
		if spcRaw > 0x80 {
			spc = int64(1) << uint(256-int(spcRaw))
		}
		tot := int64(le64(b.Data[40:48]))
		for _, st := range []int64{b.Off, b.Off - tot*bps} {
			if st >= 0 && bps > 0 && spc > 0 {
				cands = append(cands, geometry{kind: "ntfs", start: st, dataStart: st, cs: bps * spc, first: 0, clusters: tot / spc})
			}
		}
	}
	var ob []obs
	for _, ra := range recs {
		m := ra.m
		if m.isDir || len(m.data) == 0 || m.data[0].resident || len(m.data[0].runs) == 0 {
			continue
		}
		bn := m.bestName()
		if bn == nil || m.data[0].runs[0].lcn < 0 {
			continue
		}
		ob = append(ob, obs{m.data[0].runs[0].lcn, kindOfExt(extOf(bn.name))})
	}
	score := func(g geometry) int {
		s := 0
		for _, o := range ob {
			if o.kind != sig.None && hx.At(g.start+o.unit*g.cs) == o.kind {
				s++
			}
		}
		return s
	}
	var best geometry
	bestScore := -1
	for _, g := range cands {
		if s := score(g); s > bestScore {
			best, bestScore = g, s
		}
	}
	if bestScore <= 0 {
		if cs, base, m := voteGeometry(ob, hx, 0, r.Size(), 2<<20); m > 0 {
			best = geometry{kind: "ntfs", start: base, dataStart: base, cs: cs}
			bestScore = m
		}
	}
	if best.cs == 0 {
		if len(cands) == 0 {
			return nil
		}
		best = cands[0]
	}
	isLive := false
	for _, v := range liveVols {
		if sameGeom(v.geom, best) {
			isLive = true
		}
	}
	name := "Lost files (old NTFS file system)"
	if isLive {
		name = "Lost files (from before the pendrive was formatted)"
	}
	v := &Volume{Name: name, Type: "NTFS (lost)", Start: best.start, ClusterSize: best.cs, geom: best}
	n := &ntfsFS{r: r, vol: v, start: best.start, cs: best.cs, recSize: 1024, recs: map[uint32]*mftRec{}, maxFiles: 5_000_000}
	if a := liveAllocFor(vols, best); a != nil {
		v.allocated = a
	}
	for _, ra := range recs {
		m := ra.m
		old := n.recs[m.num]
		if old == nil || (!old.inUse && m.inUse) || (len(old.data) == 0 && len(m.data) > 0) {
			n.recs[m.num] = m
		}
	}
	n.walkRecords(true)
	for _, f := range v.Files {
		f.Volume = v.Name
		f.Cat = CatLost
		f.Live = false
	}
	if len(v.Files) == 0 {
		return nil
	}
	return []*Volume{v}
}

// isExfatRoot reports whether folder data looks like an exFAT root folder
// (it holds the allocation bitmap / up-case table entries).
func isExfatRoot(data []byte) bool {
	for p := 0; p+32 <= len(data) && p < 32*8; p += 32 {
		switch data[p] {
		case 0x81, 0x82:
			return true
		case 0x00:
			return false
		}
	}
	return false
}

// nameLostFolder gives a lost top-level folder a name that says what is in
// it, e.g. "Lost folder 1 - 14 photos (2023-05-01)".
func nameLostFolder(files []*File, n int, date string) {
	counts := map[string]int{}
	for _, f := range files {
		counts[fileCategory(f.Ext())]++
	}
	type kv struct {
		k string
		v int
	}
	var list []kv
	for k, v := range counts {
		if k != "" {
			list = append(list, kv{k, v})
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].v > list[j].v || (list[i].v == list[j].v && list[i].k < list[j].k) })
	var parts []string
	for i, e := range list {
		if i == 2 {
			break
		}
		k := e.k
		if e.v == 1 {
			k = strings.TrimSuffix(k, "s")
		}
		parts = append(parts, fmt.Sprintf("%d %s", e.v, k))
	}
	label := fmt.Sprintf("Lost folder %d", n)
	if len(parts) > 0 {
		label += " - " + strings.Join(parts, ", ")
	}
	// the dates of the files inside say more than the folder's own date
	var lo, hi time.Time
	for _, f := range files {
		if t := f.MTime; !t.IsZero() && t.Year() > 1980 {
			if lo.IsZero() || t.Before(lo) {
				lo = t
			}
			if t.After(hi) {
				hi = t
			}
		}
	}
	switch {
	case !lo.IsZero() && lo.Format("2006-01") == hi.Format("2006-01"):
		label += " (" + lo.Format("Jan 2006") + ")"
	case !lo.IsZero():
		label += " (" + lo.Format("Jan 2006") + " to " + hi.Format("Jan 2006") + ")"
	case date != "":
		label += " (" + date + ")"
	}
	for _, f := range files {
		if len(f.Dir) > 0 {
			f.Dir[0] = label
		}
	}
}

func fileCategory(ext string) string {
	switch ext {
	case "jpg", "jpeg", "png", "gif", "bmp", "tif", "tiff", "heic", "heif", "webp", "cr2", "cr3", "nef", "arw", "dng", "orf", "rw2", "psd":
		return "photos"
	case "mp4", "mov", "3gp", "avi", "mkv", "wmv", "flv", "mpg", "mpeg", "vob", "ts", "mts", "m2ts", "m4v", "webm":
		return "videos"
	case "mp3", "wav", "ogg", "flac", "m4a", "aac", "wma", "amr", "opus":
		return "songs"
	case "pdf", "doc", "docx", "xls", "xlsx", "ppt", "pptx", "txt", "rtf", "odt", "ods", "odp", "csv":
		return "documents"
	case "zip", "rar", "7z", "gz", "tar", "iso":
		return "archives"
	case "apk", "exe", "msi":
		return "programs"
	}
	return "files"
}
