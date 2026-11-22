package engine

import (
	"bytes"
	"os"
	"sort"
	"time"

	"pendriverescue/internal/disk"
	"pendriverescue/internal/fsys"
	"pendriverescue/internal/jpegfix"
	"pendriverescue/internal/verify"
)

// Worn flash memory sometimes hands back a few wrong bits without reporting
// an error; reading the same place again usually gives the right data. One
// wrong bit is enough to turn the rest of a photo grey. mend reads the parts
// of a damaged file that go wrong several more times and keeps, bit by bit,
// what most reads agree on. Photos say exactly where they go wrong, so only
// a few pages need reading again.

const page = disk.PageSize

// canReread reports whether the drive itself is still there to read again.
func (s *Session) canReread() bool {
	return isDevice(s.opt.Source) && !s.devR.Gone()
}

// filePages returns the drive pages holding bytes [from, to) of an item.
func filePages(parts []fsys.Extent, from, to int64) []int64 {
	seen := map[int64]bool{}
	var out []int64
	pos := int64(0)
	for _, e := range parts {
		if pos >= to {
			break
		}
		a, b := max(from, pos), min(to, pos+e.Len)
		if b > a && e.Off >= 0 {
			for p := (e.Off + a - pos) &^ (page - 1); p < e.Off+b-pos; p += page {
				if !seen[p] {
					seen[p] = true
					out = append(out, p)
				}
			}
		}
		pos += e.Len
	}
	sort.Slice(out, func(i, k int) bool { return out[i] < out[k] })
	return out
}

// reread reads the pages k times and keeps the majority of every bit. It
// returns how many pages now read differently than before. Neighbouring
// pages are read together, and between rounds something far away is read so
// the drive does not hand back what it read a moment ago.
func (s *Session) reread(pages []int64, k int) int {
	if len(pages) == 0 {
		return 0
	}
	sort.Slice(pages, func(i, j int) bool { return pages[i] < pages[j] })
	size := s.devR.Size()
	type run struct{ from, to int64 }
	var runs []run
	for _, pg := range pages {
		if pg+page > size {
			continue
		}
		if n := len(runs); n > 0 && runs[n-1].to == pg {
			runs[n-1].to += page
		} else if n == 0 || runs[n-1].to < pg {
			runs = append(runs, run{pg, pg + page})
		}
	}
	reads := map[int64][][]byte{}
	flush := disk.AlignedBuf(1 << 20)
	for pass := 0; pass < k; pass++ {
		if s.stopped() || s.devR.Gone() {
			break
		}
		far := (pages[0] + int64(pass+1)*(97<<20)) % max(size-int64(len(flush)), 1)
		s.devR.RetryRead(flush, far&^(page-1))
		for _, r := range runs {
			for off := r.from; off < r.to; {
				n := min(int64(1<<20), r.to-off)
				buf := disk.AlignedBuf(int(n))
				if s.devR.RetryRead(buf, off) {
					for p := int64(0); p < n; p += page {
						reads[off+p] = append(reads[off+p], buf[p:p+page])
					}
				}
				off += n
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	changed := 0
	cur := make([]byte, page)
	for _, r := range runs {
		for pg := r.from; pg < r.to; pg += page {
			rs := reads[pg]
			if len(rs) < 2 {
				continue
			}
			voted := majority(rs)
			s.devR.ReadAt(cur, pg)
			if bytes.Equal(voted, cur) {
				continue
			}
			s.devR.Patch(pg, voted)
			if s.r != s.devR {
				// the safe copy holds the old bits: correct it too
				s.r.Patch(pg, voted)
				if f, err := os.OpenFile(s.st.ImagePath, os.O_WRONLY, 0); err == nil {
					f.WriteAt(voted, pg)
					f.Close()
				}
			}
			changed++
			s.mu.Lock()
			s.st.Unstable++
			s.mu.Unlock()
		}
	}
	return changed
}

// majority returns, for every bit, the value most of the reads have.
func majority(reads [][]byte) []byte {
	out := make([]byte, len(reads[0]))
	need := len(reads)/2 + 1
	for i := range out {
		same := true
		for _, r := range reads[1:] {
			if r[i] != reads[0][i] {
				same = false
				break
			}
		}
		if same {
			out[i] = reads[0][i]
			continue
		}
		var v byte
		for b := 0; b < 8; b++ {
			n := 0
			for _, r := range reads {
				n += int(r[i] >> b & 1)
			}
			if n >= need {
				v |= 1 << b
			}
		}
		out[i] = v
	}
	return out
}

// mend tries to make a damaged file whole. It returns a note describing
// what helped, or "" if nothing changed.
func (s *Session) mend(it *Item) string {
	s.mendMu.Lock() // one at a time: the drive is read again, and items change
	defer s.mendMu.Unlock()
	s.mu.Lock()
	parts, size, ext, live, cat := it.parts, it.Size, it.Ext, it.Live, it.Cat
	s.mu.Unlock()
	if len(parts) == 0 || size <= 0 {
		return ""
	}
	jpg := ext == "jpg" || ext == "jpeg" || ext == "jfif"
	note := ""
	if s.canReread() {
		if jpg {
			note = s.mendJPEG(it)
		} else if size <= 64<<20 {
			if s.reread(filePages(parts, 0, size), 5) > 0 {
				note = "parts the pendrive had read wrongly were read again"
			}
		}
	}
	if !jpg || cat == "named" && live {
		return note
	}
	// a photo in pieces whose places are not known: look for the missing piece
	ir, err := s.readerFor(it, false)
	if err != nil {
		return note
	}
	if info, err := jpegfix.Parse(ir, size); err != nil || jpegfix.Check(info, ir, size).OK {
		return note
	}
	cs, phase := s.clusterGrid(it)
	ex := make([]jpegfix.Extent, len(parts))
	for i, p := range parts {
		ex[i] = jpegfix.Extent{Off: p.Off, Len: p.Len}
	}
	rep, err := jpegfix.Rebuild(readerDrive{s.r}, ex, size, jpegfix.Options{
		Cluster: cs, Phase: phase, SizeKnown: cat != "found",
		Deadline: time.Now().Add(45 * time.Second),
	})
	if err != nil || !rep.OK || rep.Pieces == 0 {
		return note
	}
	np := make([]fsys.Extent, len(rep.Parts))
	for i, p := range rep.Parts {
		np[i] = fsys.Extent{Off: p.Off, Len: p.Len}
	}
	s.mu.Lock()
	it.parts, it.Size = np, rep.Size
	s.st.Joined++
	s.mu.Unlock()
	return "rebuilt: its missing pieces were found elsewhere on the drive"
}

// mendJPEG follows the photo to where it goes wrong and reads that part
// again. If that is not enough, the whole photo is read several times.
func (s *Session) mendJPEG(it *Item) string {
	check := func() (jpegfix.Result, []fsys.Extent, int64, bool) {
		s.mu.Lock()
		parts, size := it.parts, it.Size
		s.mu.Unlock()
		ir, err := s.readerFor(it, false)
		if err != nil {
			return jpegfix.Result{}, nil, 0, false
		}
		info, err := jpegfix.Parse(ir, ir.size)
		if err != nil {
			// even the header is wrong: read all of it again
			return jpegfix.Result{ErrAt: min(size, 64<<10)}, parts, size, true
		}
		return jpegfix.Check(info, ir, ir.size), parts, size, true
	}
	fixed := 0
	res, parts, size, ok := check()
	if !ok || res.OK {
		return ""
	}
	// 1. read the whole photo several times: a wrong bit does not always
	// stop the decoding, so every part must be settled, not only the part
	// where it fails
	if size <= 64<<20 {
		fixed += s.reread(filePages(parts, 0, size), 7)
	} else {
		fixed += s.reread(filePages(parts, max(0, res.ErrAt-8192), res.ErrAt+1), 7)
	}
	res, parts, _, _ = check()
	// 2. what is left, one place at a time, with more reads
	lastErr, tries := int64(-1), 0
	for round := 0; !res.OK && round < 80 && !s.stopped(); round++ {
		if res.ErrAt == lastErr {
			tries++
		} else {
			lastErr, tries = res.ErrAt, 0
		}
		if tries > 2 {
			break // reading again does not change this part
		}
		back := int64(4096 << tries)
		fixed += s.reread(filePages(parts, max(0, res.ErrAt-back), res.ErrAt+1), []int{9, 15, 25}[tries])
		res, parts, _, _ = check()
	}
	if fixed == 0 {
		return ""
	}
	return "parts the pendrive had read wrongly were read again"
}

// clusterGrid returns the cluster size of the file system the item comes
// from and where its clusters start.
func (s *Session) clusterGrid(it *Item) (int64, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	start := int64(-1)
	for _, p := range it.parts {
		if p.Off >= 0 {
			start = p.Off
			break
		}
	}
	for _, vs := range [][]*fsys.Volume{s.vols, s.lost} {
		for _, v := range vs {
			if v.Name == it.Vol || (start >= v.Start && start < v.Start+v.Size) {
				if v.ClusterSize > 0 {
					cs := v.ClusterSize
					return cs, start % cs
				}
			}
		}
	}
	return 4096, start % 4096
}

// readerDrive lets the photo repair read the drive (or safe copy).
type readerDrive struct{ r *disk.Reader }

func (d readerDrive) ReadAt(p []byte, off int64) (int, error) { return d.r.ReadAt(p, off) }
func (d readerDrive) Size() int64                             { return d.r.Size() }

// salvage writes what can still be used of a damaged photo next to it:
// the intact part, and the small preview picture stored inside it.
func salvage(path string) (parts int, share float64, preview bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	st, _ := f.Stat()
	sv, err := jpegfix.SalvageFile(f, st.Size())
	if err != nil {
		return 0, 0, false
	}
	base := path[:len(path)-len(extOfPath(path))]
	if sv.Intact != nil && sv.Share > 0.01 {
		if os.WriteFile(base+" (intact part).jpg", sv.Intact, 0644) == nil {
			parts++
		}
	}
	if sv.Preview != nil {
		if os.WriteFile(base+" (small preview).jpg", sv.Preview, 0644) == nil {
			parts++
		}
	}
	return parts, sv.Share, sv.Preview != nil
}

func isJPEG(ext string) bool { return ext == "jpg" || ext == "jpeg" || ext == "jfif" }

// headOf reads the first n bytes of an item as it is on the drive.
func (s *Session) headOf(it *Item, n int64) []byte {
	s.mu.Lock()
	r, parts, data, file, size := s.r, it.parts, it.data, it.file, it.Size
	s.mu.Unlock()
	ir, err := s.rawReaderFor(r, parts, data, file, size)
	if err != nil {
		return nil
	}
	b := make([]byte, min(n, size))
	if _, err := ir.ReadAt(b, 0); err != nil {
		return nil
	}
	return b
}

// learnPhoto remembers the header tables of a good photo, to repair other
// photos from the same camera.
func (s *Session) learnPhoto(head []byte, verified bool) {
	if len(head) > 0 {
		s.samples.Add(head, verified)
	}
}

// fixHeader rebuilds the start of a photo whose header has wrong bits, from
// a good photo taken with the same camera. It reports whether it did.
func (s *Session) fixHeader(it *Item) bool {
	s.mu.Lock()
	has := it.hdr != nil
	s.mu.Unlock()
	if has || s.samples.Len() == 0 {
		return false
	}
	fix := jpegfix.RepairHeader(s.headOf(it, 256<<10), s.samples)
	if fix == nil {
		return false
	}
	s.mu.Lock()
	it.hdr = fix
	s.st.Headers++
	s.mu.Unlock()
	return true
}

// headerNote says what fixHeader did.
func headerNote(it *Item) string {
	if it.hdr != nil && it.hdr.Dropped {
		return "its broken header was rebuilt from a good photo taken with the same camera (its date and rotation were lost)"
	}
	return "its broken header was rebuilt from a good photo taken with the same camera"
}

// damageFacts works out, for a damaged photo, how much of the picture is
// intact and whether its small preview is.
func (s *Session) damageFacts(it *Item) {
	ir, err := s.readerFor(it, false)
	if err != nil {
		return
	}
	sv, err := jpegfix.SalvageFile(ir, ir.size)
	if err != nil || sv == nil {
		return
	}
	s.mu.Lock()
	it.Share = sv.Share
	it.Preview = sv.Preview != nil
	s.mu.Unlock()
}

func extOfPath(p string) string {
	for i := len(p) - 1; i >= 0 && p[i] != '/' && p[i] != '\\'; i-- {
		if p[i] == '.' {
			return p[i:]
		}
	}
	return ""
}

// mendSaved is used after saving: if the saved file is damaged and mending
// changes the data, the file is written again and checked again.
func (s *Session) mendSaved(it *Item, path, ext string, res verify.Result) (verify.Result, string, string, bool) {
	note := s.mend(it)
	if note == "" {
		// the background check may have mended it meanwhile
		note = "parts the pendrive had read wrongly were read again"
	}
	tmp := path + ".mend"
	hash, _, err := s.writeTo(tmp, it, false, nil)
	if err != nil {
		os.Remove(tmp)
		return res, "", "", false
	}
	r2 := checkSaved(tmp, ext)
	if r2.Cond == verify.Damaged {
		os.Remove(tmp)
		return res, "", "", false
	}
	os.Remove(path)
	os.Rename(tmp, path)
	return r2, note, hash, true
}
