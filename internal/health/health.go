// Package health gives a pendrive a one-minute check before anything else is
// done with it, and says in plain words what is wrong and what to do.
//
// It reads a few hundred places spread over the drive three times each and
// compares the answers (worn flash hands back different bits on different
// reads), times the reads (slow or stalling areas), notes places that cannot
// be read, checks whether the drive repeats itself (fake capacity), and
// looks at the file systems. Everything is read-only.
package health

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"pendriverescue/internal/disk"
	"pendriverescue/internal/fsys"
)

// What kind of trouble the drive has, most serious first.
const (
	Gone    = "gone"    // stopped answering during the check
	Dead    = "dead"    // nothing can be read
	Fake    = "fake"    // repeats itself: less real space than it claims
	Failing = "failing" // places that cannot be read, or very slow ones
	Worn    = "worn"    // the same place reads differently on different tries
	Slow    = "slow"    // reads correctly, but slowly
	Healthy = "healthy" // reads normally
)

// How the recovery should read the drive.
const (
	PlanDirect  = "direct"  // read it once, straight from the drive
	PlanCopy    = "copy"    // copy it first, retry the bad parts, then work from the copy
	PlanCareful = "careful" // copy it reading everything twice and voting where reads disagree
)

// FS is a file system found on the drive.
type FS struct {
	Type      string `json:"type"`
	Label     string `json:"label"`
	Files     int    `json:"files"`
	Deleted   int    `json:"deleted"`
	Encrypted bool   `json:"encrypted"`
}

// Report is the outcome of a check.
type Report struct {
	Size       int64   `json:"size"`
	Speed      float64 `json:"speed"`      // bytes per second when reading straight through
	Spots      int     `json:"spots"`      // places read three times
	Pages      int     `json:"pages"`      // 4 KB pages compared
	Unstable   int     `json:"unstable"`   // pages that read differently on different tries
	UnstableAt int     `json:"unstableAt"` // places with at least one such page
	Unreadable int     `json:"unreadable"` // places that could not be read
	Changed    int     `json:"changed"`    // pages rewritten while reading (something else wrote to the drive)
	Slow       int     `json:"slow"`       // places that took more than a second
	SlowestMs  int     `json:"slowestMs"`
	RealSize   int64   `json:"realSize"` // fake drives: what they really hold
	FS         []FS    `json:"fs"`
	FSChecked  bool    `json:"fsChecked"`
	Seconds    float64 `json:"seconds"`

	Verdict string `json:"verdict"` // Healthy, Worn, ...
	Title   string `json:"title"`   // one sentence: what is wrong
	Detail  string `json:"detail"`  // what was seen, in plain words
	Plan    string `json:"plan"`    // PlanDirect, PlanCopy, PlanCareful
	Next    string `json:"next"`    // one sentence: what happens next
	Shop    string `json:"shop"`    // advice for taking it to a repair shop
	Lab     bool   `json:"lab"`     // only a lab can help further
}

// Options control a check.
type Options struct {
	Budget   time.Duration // time for the three rounds (default 40 s)
	Stop     <-chan struct{}
	Progress func(step string, done, total int)
}

const (
	spotSize = 32 << 10
	page     = disk.PageSize
)

type spot struct {
	off      int64
	first    []byte // what the first round read
	reads    int
	unstable []bool // per page: read differently later
	bad      bool
}

// flipLike tells worn flash from a real change: worn flash gets a few
// single bits wrong, while data that was rewritten differs all over.
func flipLike(a, b []byte) bool {
	diff, single := 0, 0
	for i := range a {
		if x := a[i] ^ b[i]; x != 0 {
			diff++
			if x&(x-1) == 0 {
				single++
			}
		}
	}
	return diff > 0 && diff*20 <= len(a) && single*10 >= diff*6
}

func stopped(c <-chan struct{}) bool {
	if c == nil {
		return false
	}
	select {
	case <-c:
		return true
	default:
		return false
	}
}

// Check examines the drive behind src. It never writes to it.
func Check(src disk.Source, opt Options) *Report {
	if opt.Budget <= 0 {
		opt.Budget = 40 * time.Second
	}
	prog := func(step string, d, t int) {
		if opt.Progress != nil {
			opt.Progress(step, d, t)
		}
	}
	start := time.Now()
	r := disk.NewReader(src)
	size := r.Size()
	rep := &Report{Size: size}
	defer func() { rep.Seconds = time.Since(start).Seconds() }()
	if size <= 0 {
		rep.judge()
		return rep
	}

	// 1. how fast does it read straight through? (16 MB from a third in)
	prog("Measuring how fast it reads", 0, 1)
	buf := disk.AlignedBuf(1 << 20)
	seqFrom := (size / 3) &^ (1<<20 - 1)
	var seqBytes int64
	t0 := time.Now()
	failures := 0
	for i := int64(0); i < 16 && seqFrom+(i+1)<<20 <= size; i++ {
		if r.RetryRead(buf, seqFrom+i<<20) {
			seqBytes += 1 << 20
		} else {
			failures++
			if r.Gone() {
				rep.Verdict = Gone
				rep.judge()
				return rep
			}
		}
		if time.Since(t0) > 10*time.Second {
			break
		}
	}
	if el := time.Since(t0).Seconds(); seqBytes > 0 && el > 0 {
		rep.Speed = float64(seqBytes) / el
	}

	// 2. places spread over the drive, read three times each
	perRead := 0.01 // seconds, a guess for a USB pendrive
	if rep.Speed > 0 {
		perRead = 0.004 + float64(spotSize)/rep.Speed
	}
	n := int(opt.Budget.Seconds() / (3 * perRead))
	n = max(64, min(n, 768))
	var spots []*spot
	seen := map[int64]bool{}
	addSpot := func(off int64) {
		off = off &^ (spotSize - 1)
		if off < 0 || off+spotSize > size || seen[off] {
			return
		}
		seen[off] = true
		spots = append(spots, &spot{off: off})
	}
	// the start holds the partition table and file system records
	for off := int64(0); off < 1<<20; off += spotSize {
		addSpot(off)
	}
	for i := 0; i < n; i++ {
		addSpot(size / int64(n) * int64(i))
	}
	sb := disk.AlignedBuf(spotSize)
	for round := 0; round < 3; round++ {
		for i, sp := range spots {
			if stopped(opt.Stop) {
				break
			}
			if i%16 == 0 {
				prog("Reading "+fmt.Sprint(len(spots))+" places three times", round*len(spots)+i, 3*len(spots))
			}
			if sp.bad {
				continue
			}
			t := time.Now()
			ok := r.RetryRead(sb, sp.off)
			ms := int(time.Since(t).Milliseconds())
			if round == 0 {
				if ms > 1000 {
					rep.Slow++
				}
				rep.SlowestMs = max(rep.SlowestMs, ms)
			}
			if !ok {
				if r.Gone() {
					rep.Verdict = Gone
					rep.judge()
					return rep
				}
				sp.bad = true
				rep.Unreadable++
				continue
			}
			sp.reads++
			if sp.first == nil {
				sp.first = append([]byte(nil), sb...)
				sp.unstable = make([]bool, spotSize/page)
				continue
			}
			for p := 0; p < spotSize/page; p++ {
				a, b := sp.first[p*page:(p+1)*page], sb[p*page:(p+1)*page]
				if !bytes.Equal(a, b) {
					if flipLike(a, b) {
						sp.unstable[p] = true
					} else {
						rep.Changed++
					}
				}
			}
		}
	}
	for _, sp := range spots {
		if sp.bad || sp.reads < 2 {
			continue
		}
		rep.Spots++
		unstable := 0
		for _, u := range sp.unstable {
			rep.Pages++
			if u {
				unstable++
			}
		}
		if unstable > 0 {
			rep.Unstable += unstable
			rep.UnstableAt++
		}
		sp.first = nil
	}
	if rep.Spots == 0 && rep.Unreadable > 0 {
		rep.Verdict = Dead
	}

	// 3. does it repeat itself? (fake capacity)
	prog("Checking its real size", 0, 1)
	if real, fake := fakeSize(r, spots); fake {
		rep.RealSize = real
	}

	// 4. the file systems on it
	prog("Looking at the file systems", 0, 1)
	type fsDone struct{ vols []*fsys.Volume }
	ch := make(chan fsDone, 1)
	fr := r
	if rep.RealSize > 0 {
		fr = disk.NewReader(src)
		fr.Truncate(rep.RealSize)
	}
	go func() {
		defer func() { recover() }()
		vols, _ := fsys.FindVolumes(fr, nil)
		ch <- fsDone{vols}
	}()
	select {
	case d := <-ch:
		rep.FSChecked = true
		for _, v := range d.vols {
			rep.FS = append(rep.FS, FS{Type: v.Type, Label: strings.TrimSpace(v.Label), Files: v.Live, Deleted: v.Deleted, Encrypted: v.Encrypted})
		}
	case <-time.After(25 * time.Second):
	}
	if r.Gone() {
		rep.Verdict = Gone
	}
	rep.judge()
	return rep
}

// fakeSize looks for the drive repeating itself: fake "64 GB" pendrives that
// really hold 8 GB answer for address 8 GB + x with what is at x.
func fakeSize(r *disk.Reader, spots []*spot) (int64, bool) {
	size := r.Size()
	a := disk.AlignedBuf(page)
	b := disk.AlignedBuf(page)
	var low []int64
	for _, sp := range spots {
		if sp.off < 32<<20 && !sp.bad {
			for p := int64(0); p < spotSize; p += page {
				low = append(low, sp.off+p)
			}
		}
	}
	for c := int64(32 << 20); c <= size/2; c *= 2 {
		tested, same := 0, 0
		for _, o := range low {
			if o >= c {
				break
			}
			if !r.RetryRead(a, o) || blank(a) {
				continue
			}
			for k := int64(1); k <= 2 && o+k*c+page <= size; k++ {
				if !r.RetryRead(b, o+k*c) {
					continue
				}
				tested++
				if bytes.Equal(a, b) {
					same++
				}
			}
			if tested >= 16 {
				break
			}
		}
		if tested >= 3 && same == tested {
			return c, true
		}
	}
	return 0, false
}

func blank(b []byte) bool {
	z, f := true, true
	for _, x := range b {
		z = z && x == 0
		f = f && x == 0xFF
		if !z && !f {
			return false
		}
	}
	return true
}

// HumanSize is disk.HumanSize, rounded the way pendrives are sold.
func sold(n int64) string {
	g := float64(n) / 1e9
	for _, s := range []float64{1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1024, 2048} {
		if g <= s*1.02 && g >= s*0.85 {
			return fmt.Sprintf("%g GB", s)
		}
	}
	return disk.HumanSize(n)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// judge turns the numbers into a verdict, a plan and plain words.
func (rep *Report) judge() {
	switch {
	case rep.Verdict == Gone:
	case rep.Size <= 0 || rep.Verdict == Dead:
		rep.Verdict = Dead
	case rep.RealSize > 0:
		rep.Verdict = Fake
	case rep.Unreadable > 0 || rep.Slow >= 3:
		rep.Verdict = Failing
	case rep.Unstable > 0:
		rep.Verdict = Worn
	case rep.Speed > 0 && rep.Speed < 3<<20:
		rep.Verdict = Slow
	default:
		rep.Verdict = Healthy
	}
	total := rep.Spots + rep.Unreadable
	places := plural(total, "place", "places")
	fsLine := rep.fsSentence()
	lab := "If the pendrive is not seen at all, or shows 0 bytes on other ports and other computers, its controller chip has probably failed. Then only a data recovery lab can help, by reading the memory chip directly (chip-off). Ask for a price first; many labs charge only if they get files back."
	switch rep.Verdict {
	case Gone:
		rep.Title = "Your pendrive stopped answering while I was checking it."
		rep.Detail = "It disappeared from the computer in the middle of reading. A loose plug, a weak USB port or a failing controller chip can do that."
		rep.Plan = PlanCopy
		rep.Next = "Unplug it, plug it straight into another USB port (not a hub), and check it again. If it keeps disappearing, I'll copy it in one go and continue where it stopped each time."
		rep.Shop = lab
		rep.Lab = true
	case Dead:
		rep.Title = "Nothing on your pendrive can be read."
		rep.Detail = "Every place I tried failed to read. The memory or its controller chip has probably failed."
		rep.Plan = PlanCopy
		rep.Next = "Try another USB port and another computer once. If it still cannot be read, no program can help."
		rep.Shop = lab
		rep.Lab = true
	case Fake:
		rep.Title = "This pendrive is fake."
		rep.Detail = fmt.Sprintf("It says it holds %s, but it repeats itself after about %s. Anything saved beyond that was never stored anywhere. %s", sold(rep.Size), sold(rep.RealSize), fsLine)
		rep.Plan = PlanDirect
		if rep.Unstable > 0 || rep.Unreadable > 0 {
			rep.Plan = PlanCareful
		}
		rep.Next = fmt.Sprintf("I'll read only the real %s and look for your files there.", sold(rep.RealSize))
		rep.Shop = "Files saved after the real space ran out were never stored, so no lab can bring them back. Be wary of anyone who promises to. Files saved before that point can be recovered like on any pendrive."
	case Failing:
		bits := []string{}
		if rep.Unreadable > 0 {
			bits = append(bits, fmt.Sprintf("%d of the %d places I read could not be read", rep.Unreadable, total))
		}
		if rep.Slow > 0 {
			bits = append(bits, fmt.Sprintf("%s took more than a second to answer", plural(rep.Slow, "place", "places")))
		}
		if rep.Unstable > 0 {
			bits = append(bits, fmt.Sprintf("%s gave a different answer when read again", plural(rep.Unstable, "small part", "small parts")))
		}
		rep.Title = "Your pendrive has parts that cannot be read."
		rep.Detail = capital(strings.Join(bits, ", ")) + ". " + fsLine
		rep.Plan = PlanCopy
		rep.Next = "I'll copy it in one go first, skipping the bad parts and trying them again at the end, so the pendrive only has to get through one reading. Then I'll look for your files in the copy."
		if rep.Unstable > 0 {
			rep.Plan = PlanCareful
			rep.Next = "I'll copy it in one go first, reading every part twice and again where the reads disagree, and trying the bad parts again at the end. Then I'll look for your files in the copy."
		}
		rep.Shop = "Parts of the memory cannot be read any more. Pendrive Rescue has already copied everything that can be read and tried the rest again. What is still unreadable can only be attempted by a lab that reads the memory chip directly (chip-off), at lab prices."
		rep.Lab = true
	case Worn:
		rep.Title = "Your pendrive is wearing out."
		rep.Detail = fmt.Sprintf("Reading the same place again gave different data in %d of the %d places I read three times. Worn memory hands back a few wrong bits without saying so. That is what turns photos grey partway down. %s",
			rep.UnstableAt, total, fsLine)
		rep.Plan = PlanCareful
		rep.Next = "I'll copy it carefully first: every part is read twice, and wherever the two reads disagree I read it again and keep what most reads agree on. Then I'll look for your files in the copy."
		rep.Shop = "The memory is wearing out and returns slightly different data each time it is read. Pendrive Rescue already reads every part several times and keeps the majority, as professional imaging tools do. If some photos are still damaged after that, a lab can read the memory chip directly (chip-off), which sometimes gets the right data back. That costs lab prices."
	case Slow:
		rep.Title = "Your pendrive is slow, but it reads correctly."
		rep.Detail = fmt.Sprintf("It reads about %s per second, and every place I read three times gave the same answer. %s", disk.HumanSize(int64(rep.Speed)), fsLine)
		rep.Plan = PlanDirect
		rep.Next = "I'll read it once and look for your files. It will take a while, but nothing on it is getting worse."
		rep.Shop = "The pendrive works, just slowly. Lost files are most likely deleted or the pendrive was formatted. That is the easy case: any recovery program can do it, so it should not cost lab prices."
	default:
		rep.Title = "Your pendrive reads normally."
		rep.Detail = fmt.Sprintf("I read %s on it three times each and got the same answer every time. %s", places, fsLine)
		rep.Plan = PlanDirect
		rep.Next = "I'll read it once and look for your files, with their names where they are still known."
		rep.Shop = "The pendrive works. Lost files are most likely deleted, or the pendrive was formatted. That is the easy case: any recovery program can do it, so it should not cost lab prices."
	}
	rep.Detail = strings.TrimSpace(rep.Detail)
}

func (rep *Report) fsSentence() string {
	if !rep.FSChecked {
		return ""
	}
	if len(rep.FS) == 0 {
		return "It has no working file system, so it was probably formatted or its records were damaged. Lost files can still be found by what is inside them."
	}
	var enc, files, deleted int
	types := []string{}
	for _, f := range rep.FS {
		if f.Encrypted {
			enc++
			continue
		}
		files += f.Files
		deleted += f.Deleted
		types = append(types, f.Type)
	}
	if enc > 0 && len(types) == 0 {
		return "It is encrypted with BitLocker. Unlock it in Windows with its password or recovery key first; without the key nobody can read it."
	}
	s := fmt.Sprintf("Its %s file system lists %s", strings.Join(types, " and "), plural(files, "file", "files"))
	if deleted > 0 {
		s += fmt.Sprintf(" and %s", plural(deleted, "deleted one", "deleted ones"))
	}
	return s + "."
}

func capital(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
