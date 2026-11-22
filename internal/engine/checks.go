package engine

import (
	"bytes"
	"os"
	"sort"
	"sync"
	"time"

	"pendriverescue/internal/disk"
	"pendriverescue/internal/scan"
	"pendriverescue/internal/verify"
)

// checkAll verifies files in the background, most doubtful first, so the
// user can see which files are good before saving them.
func (s *Session) checkAll() {
	s.mu.Lock()
	imageMode := s.st.ImagePath != ""
	var list []*Item
	for _, it := range s.items {
		if it.Cond != "" || it.Size == 0 {
			continue
		}
		if it.Live && !imageMode {
			continue // files that still exist are fine; spare the pendrive
		}
		list = append(list, it)
	}
	rank := func(it *Item) int {
		switch {
		case it.Cat == "found":
			return 0
		case it.Cat == "lost":
			return 1
		case it.Deleted:
			return 2
		}
		return 3
	}
	sort.SliceStable(list, func(i, j int) bool { return rank(list[i]) < rank(list[j]) })
	s.st.ToCheck = len(list)
	s.st.Checked = 0
	var photos []*Item
	for _, it := range s.items {
		if isJPEG(it.Ext) && it.Size > 0 && len(photos) < 400 {
			photos = append(photos, it)
		}
	}
	s.mu.Unlock()

	// learn the header tables of the cameras first, from many photos, so
	// photos with a broken header can be repaired when their turn comes
	for _, it := range photos {
		if s.stopped() || s.r.Gone() {
			break
		}
		s.learnPhoto(s.headOf(it, 128<<10), false)
	}

	workers := 1
	if imageMode {
		workers = 2
	}
	jobs := make(chan *Item)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range jobs {
				s.checkOne(it)
				s.mu.Lock()
				s.st.Checked++
				s.mu.Unlock()
			}
		}()
	}
	for _, it := range list {
		// give way while files are being saved (the saver checks them itself)
		for {
			s.mu.Lock()
			busy := s.st.Save.Running
			s.mu.Unlock()
			if !busy {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if s.r.Gone() {
			break
		}
		jobs <- it
	}
	close(jobs)
	wg.Wait()
}

func (s *Session) checkOne(it *Item) {
	s.mu.Lock()
	if it.Cond != "" {
		s.mu.Unlock()
		return
	}
	hasAlt := len(it.alt) > 0
	s.mu.Unlock()
	ir, err := s.readerFor(it, false)
	if err != nil {
		return
	}
	res := verify.Check(it.Ext, ir, 40<<20)
	cond, note := string(res.Cond), res.Note
	if res.Cond == verify.Damaged && hasAlt {
		if ar, err := s.readerFor(it, true); err == nil {
			if r2 := verify.Check(it.Ext, ar, 40<<20); r2.Cond == verify.Good {
				s.mu.Lock()
				it.parts, it.alt = it.alt, nil
				s.mu.Unlock()
				cond, note = "good", "rebuilt from scattered pieces"
			}
		}
	}
	if cond == "damaged" {
		// read the wrong parts again, or look for missing pieces
		if how := s.mend(it); how != "" {
			if ir2, err := s.readerFor(it, false); err == nil {
				if r3 := verify.Check(it.Ext, ir2, 40<<20); r3.Cond != verify.Damaged {
					cond, note = string(r3.Cond), how
				}
			}
		}
	}
	if cond == "damaged" && isJPEG(it.Ext) && s.fixHeader(it) {
		note = headerNote(it)
		if ir2, err := s.readerFor(it, false); err == nil {
			if r3 := verify.Check(it.Ext, ir2, 40<<20); r3.Cond != verify.Damaged {
				cond = string(r3.Cond)
			} else {
				note += "; the picture itself is damaged further on"
			}
		}
	}
	if isJPEG(it.Ext) {
		if cond == "good" {
			s.learnPhoto(s.headOf(it, 128<<10), true)
		} else if cond == "damaged" {
			s.damageFacts(it)
		}
	}
	if cond == "damaged" && note == "" {
		note = it.sum
	}
	s.mu.Lock()
	if it.Cond == "" {
		it.Cond = cond
		if note != "" {
			it.Note = note
		}
	}
	s.mu.Unlock()
}

// retry reads the unreadable areas again: first in 64 KB blocks from the
// end of each area backwards (failing drives often read better that way),
// then sector by sector, within a time budget. What it gets goes into the
// safe copy. It returns how many bytes it recovered.
func (s *Session) retry(imgPath string) int64 {
	bad := s.devR.BadAreas()
	var total int64
	for _, b := range bad {
		total += b[1]
	}
	if total == 0 {
		return 0
	}
	s.setPhase(PhaseRetrying, "Trying the unreadable areas again", total)
	img, err := os.OpenFile(imgPath, os.O_RDWR, 0644)
	if err != nil {
		return 0
	}
	defer img.Close()
	mp, _ := scan.LoadMap(imgPath+".map", s.devR.Size(), s.identity)
	deadline := time.Now().Add(20 * time.Minute)
	ss := s.devR.SectorSize()
	const block = 64 << 10
	buf := disk.AlignedBuf(block)
	var got, tried int64
	give := func(off, n int64) {
		img.WriteAt(buf[:n], off)
		s.devR.ClearBad(off, n)
		if mp != nil {
			mp.Add(off, n)
		}
		got += n
	}
	over := func() bool { return s.stopped() || time.Now().After(deadline) || s.devR.Gone() }
	// pass 2: 64 KB blocks, backwards
	for i := len(bad) - 1; i >= 0 && !over(); i-- {
		start, end := bad[i][0], bad[i][0]+bad[i][1]
		for b := (end - 1) / block * block; b+block > start && !over(); b -= block {
			lo, hi := max(b, start), min(b+block, end)
			n := hi - lo
			if n <= 0 || n%ss != 0 {
				continue
			}
			if s.devR.RetryRead(buf[:n], lo) {
				give(lo, n)
			}
			tried += n
			s.progress(tried, total)
			if b == 0 {
				break
			}
		}
	}
	// pass 3: sector by sector over what is still unreadable
	for _, b := range s.devR.BadAreas() {
		for off := b[0]; off < b[0]+b[1] && !over(); off += ss {
			if s.devR.RetryRead(buf[:ss], off) {
				give(off, ss)
			}
		}
	}
	if mp != nil {
		mp.Save(true)
	}
	return got
}

// rescanImage scans the safe copy again (fast, from the computer's disk)
// after a retry filled in areas that were unreadable at first.
func (s *Session) rescanImage(imgPath string) *scan.Result {
	img, err := disk.OpenImage(imgPath)
	if err != nil {
		return nil
	}
	defer img.Close()
	s.setPhase(PhaseAnalysing, "Re-reading the safe copy", img.Size())
	r := disk.NewReader(img)
	for _, b := range s.devR.BadAreas() {
		r.MarkBadRange(b[0], b[1])
	}
	res, err := scan.Run(r, scan.Options{Text: s.opt.TextSearch, Progress: func(_ *scan.Result, d, t, _ int64) { s.progress(d, t) }})
	if err != nil {
		return nil
	}
	return res
}

// fakeCapacity checks (read-only) whether the drive repeats itself, which is
// what fake "64 GB" pendrives that really hold 8 GB do.
func fakeCapacity(r *disk.Reader, sr *scan.Result) (int64, bool) {
	size := r.Size()
	var samples []int64
	add := func(o int64) { samples = append(samples, o&^4095) }
	for _, b := range sr.Boot {
		add(b.Off)
	}
	step := len(sr.Hits)/256 + 1
	for i := 0; i < len(sr.Hits); i += step {
		add(sr.Hits[i].Off)
	}
	step = len(sr.FatDirs)/64 + 1
	for i := 0; i < len(sr.FatDirs); i += step {
		add(sr.FatDirs[i].Off)
	}
	step = len(sr.MftRecs)/64 + 1
	for i := 0; i < len(sr.MftRecs); i += step {
		add(sr.MftRecs[i])
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	a := make([]byte, 4096)
	b := make([]byte, 4096)
	for c := int64(32 << 20); c <= size/2; c *= 2 {
		tested, same := 0, 0
		for _, o := range samples {
			if o >= c {
				break
			}
			r.ReadAt(a, o)
			if isBlank(a) {
				continue
			}
			for k := int64(1); k <= 3 && o+k*c+4096 <= size; k++ {
				r.ReadAt(b, o+k*c)
				tested++
				if bytes.Equal(a, b) {
					same++
				}
			}
			if tested >= 24 {
				break
			}
		}
		if tested >= 3 && same == tested {
			return c, true
		}
	}
	return 0, false
}

func isBlank(b []byte) bool {
	z, f := true, true
	for _, x := range b {
		if x != 0 {
			z = false
		}
		if x != 0xFF {
			f = false
		}
		if !z && !f {
			return false
		}
	}
	return true
}

// limitScan drops everything the scan found beyond the real capacity.
func limitScan(sr *scan.Result, n int64) {
	var hits []scan.Hit
	for _, h := range sr.Hits {
		if h.Off < n {
			hits = append(hits, h)
		}
	}
	sr.Hits = hits
	var boot []scan.BootCand
	for _, b := range sr.Boot {
		if b.Off < n {
			boot = append(boot, b)
		}
	}
	sr.Boot = boot
	var fd []scan.FatDir
	for _, d := range sr.FatDirs {
		if d.Off < n {
			fd = append(fd, d)
		}
	}
	sr.FatDirs = fd
	var ex []scan.ExfatSet
	for _, e := range sr.ExfatSets {
		if e.Off < n {
			ex = append(ex, e)
		}
	}
	sr.ExfatSets = ex
	var mr []int64
	for _, m := range sr.MftRecs {
		if m < n {
			mr = append(mr, m)
		}
	}
	sr.MftRecs = mr
	var tr []scan.TextRun
	for _, t := range sr.TextRuns {
		if t.Off < n {
			tr = append(tr, t)
		}
	}
	sr.TextRuns = tr
}
