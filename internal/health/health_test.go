package health

import (
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"

	"pendriverescue/internal/disk"
	"pendriverescue/internal/sim"
)

// A 256 MB FAT32 image with photos, made by the build scripts (skipped when
// missing).
const image = "/home/claude/frag/frag_fat32.img"

func open(t *testing.T, opt sim.Options) *sim.Drive {
	t.Helper()
	if _, err := os.Stat(image); err != nil {
		t.Skip("test image missing")
	}
	d, err := sim.Open(image, opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func check(t *testing.T, d disk.Source) *Report {
	t.Helper()
	rep := Check(d, Options{Budget: 8 * time.Second})
	t.Logf("%s: %s %s | plan %s | spots %d, unstable %d in %d places, unreadable %d, slow %d, changed %d, speed %s/s, fs %+v, %.1fs",
		rep.Verdict, rep.Title, rep.Detail, rep.Plan, rep.Spots, rep.Unstable, rep.UnstableAt, rep.Unreadable, rep.Slow, rep.Changed,
		disk.HumanSize(int64(rep.Speed)), rep.FS, rep.Seconds)
	if rep.Title == "" || rep.Next == "" || rep.Shop == "" {
		t.Error("a verdict needs a title, a next step and shop advice")
	}
	return rep
}

func TestHealthy(t *testing.T) {
	rep := check(t, open(t, sim.Options{}))
	if rep.Verdict != Healthy || rep.Plan != PlanDirect {
		t.Errorf("verdict %s, plan %s; want healthy, direct", rep.Verdict, rep.Plan)
	}
	if len(rep.FS) == 0 || rep.FS[0].Files == 0 {
		t.Errorf("the FAT32 file system and its files were not found: %+v", rep.FS)
	}
}

func TestWorn(t *testing.T) {
	rep := check(t, open(t, sim.Options{Seed: 5, WeakPages: 0.02, FlipChance: 0.7, FlipBits: 3}))
	if rep.Verdict != Worn || rep.Plan != PlanCareful {
		t.Errorf("verdict %s, plan %s; want worn, careful", rep.Verdict, rep.Plan)
	}
}

func TestFailing(t *testing.T) {
	rep := check(t, open(t, sim.Options{Bad: [][2]int64{{100 << 20, 104 << 20}}}))
	if rep.Verdict != Failing || rep.Plan != PlanCopy || rep.Unreadable == 0 {
		t.Errorf("verdict %s, plan %s, unreadable %d; want failing, copy", rep.Verdict, rep.Plan, rep.Unreadable)
	}
}

func TestFake(t *testing.T) {
	rep := check(t, open(t, sim.Options{Claimed: 8 << 28}))
	if rep.Verdict != Fake || rep.RealSize != 1<<28 {
		t.Errorf("verdict %s, real size %d; want fake, 256 MB", rep.Verdict, rep.RealSize)
	}
	if !strings.Contains(rep.Next, "only the real") {
		t.Errorf("the plan should say only the real part is read: %q", rep.Next)
	}
}

func TestUnplugged(t *testing.T) {
	rep := check(t, open(t, sim.Options{UnplugAfter: 20 << 20}))
	if rep.Verdict != Gone {
		t.Errorf("verdict %s; want gone", rep.Verdict)
	}
}

// writer changes whole pages between reads, like Windows writing to a
// mounted pendrive: that is not worn flash.
type writer struct {
	disk.Source
	n int
}

func (w *writer) ReadAt(p []byte, off int64) (int, error) {
	n, err := w.Source.ReadAt(p, off)
	w.n++
	if off < 1<<20 && w.n%3 == 0 && len(p) >= 4096 {
		rand.New(rand.NewSource(int64(w.n))).Read(p[:4096])
	}
	return n, err
}

func TestWritesAreNotWear(t *testing.T) {
	rep := check(t, &writer{Source: open(t, sim.Options{})})
	if rep.Verdict == Worn || rep.Changed == 0 {
		t.Errorf("verdict %s, changed %d; rewritten pages must not count as wear", rep.Verdict, rep.Changed)
	}
}
