package engine

import (
	"crypto/md5"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pendriverescue/internal/disk"
	"pendriverescue/internal/sim"
)

// Test images made by the build scripts (skipped when missing).
const (
	fragImage = "/home/claude/frag/frag_fat32.img"
	photoDir  = "/home/claude/frag/photos"
	testImage = "/home/claude/imgs/fat32_qf.img"
)

// flaky wraps a drive like worn flash memory: in "weak" pages each read
// gets a few random bits wrong, differently every time, without an error.
type flaky struct {
	disk.Source
	weak  func(page int64) bool
	q     float64 // chance per byte of a wrong bit, in a weak page
	reads atomic.Int64
}

func (f *flaky) Name() string { return "/dev/flaky-test" }

func (f *flaky) ReadAt(p []byte, off int64) (int, error) {
	n, err := f.Source.ReadAt(p, off)
	rng := rand.New(rand.NewSource(f.reads.Add(1) * 7919))
	for pg := off &^ 4095; pg < off+int64(n); pg += 4096 {
		if !f.weak(pg) {
			continue
		}
		for i := max(pg, off); i < min(pg+4096, off+int64(n)); i++ {
			if rng.Float64() < f.q {
				p[i-off] ^= 1 << rng.Intn(8)
			}
		}
	}
	return n, err
}

// faulty fails reads of an area, or everything once a point is reached.
type faulty struct {
	disk.Source
	badFrom, badTo int64
	goneAfter      int64
	gone           atomic.Bool
}

func (f *faulty) ReadAt(p []byte, off int64) (int, error) {
	if f.goneAfter > 0 && off+int64(len(p)) > f.goneAfter {
		f.gone.Store(true)
	}
	if f.gone.Load() {
		return 0, errors.New("device not connected")
	}
	if off < f.badTo && off+int64(len(p)) > f.badFrom {
		return 0, errors.New("I/O error")
	}
	return f.Source.ReadAt(p, off)
}

func openImage(t *testing.T, path string) disk.Source {
	t.Helper()
	s, err := disk.OpenImage(path)
	if err != nil {
		t.Skip("test image not present: " + path)
	}
	return s
}

// run recovers everything from src into a new folder and returns it.
func run(t *testing.T, src disk.Source, source string, image bool) (*Session, string) {
	t.Helper()
	openSource = func(string) (disk.Source, error) { return src, nil }
	isDevice = func(p string) bool { return strings.HasPrefix(p, "/dev/") }
	t.Cleanup(func() { openSource, isDevice = disk.Open, disk.IsDevicePath })
	out := t.TempDir()
	s, err := Start(Options{Source: source, SourceDesc: "test", OutDir: out, MakeImage: image, DeepScan: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	deadline := time.Now().Add(5 * time.Minute)
	for st := s.Status(); st.Phase != PhaseReady && st.Phase != PhaseFailed; st = s.Status() {
		if time.Now().After(deadline) {
			t.Fatal("recovery did not finish")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if st := s.Status(); st.Phase == PhaseFailed {
		return s, out
	}
	if err := s.Recover(Selection{All: true}); err != nil {
		t.Fatal(err)
	}
	for st := s.Status(); st.Save.Running || st.Save.Report == ""; st = s.Status() {
		if time.Now().After(deadline) {
			t.Fatal("saving did not finish")
		}
		time.Sleep(50 * time.Millisecond)
	}
	return s, out
}

// exact counts how many of the original photos were saved without a single
// wrong byte.
func exact(t *testing.T, out string) (int, int) {
	t.Helper()
	want := map[[16]byte]bool{}
	ents, err := os.ReadDir(photoDir)
	if err != nil {
		t.Skip("photos not present")
	}
	for _, e := range ents {
		b, _ := os.ReadFile(filepath.Join(photoDir, e.Name()))
		want[md5.Sum(b)] = true
	}
	got := map[[16]byte]bool{}
	filepath.Walk(out, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() && strings.HasPrefix(fi.Name(), "IMG_") && strings.HasSuffix(fi.Name(), ".jpg") &&
			!strings.Contains(fi.Name(), "(") {
			b, _ := os.ReadFile(p)
			if h := md5.Sum(b); want[h] {
				got[h] = true
			}
		}
		return nil
	})
	return len(got), len(want)
}

func TestWornFlashIsReadAgain(t *testing.T) {
	for _, image := range []bool{false, true} {
		base := openImage(t, fragImage)
		f := &flaky{Source: base, q: 0.03, weak: func(pg int64) bool {
			// about a quarter of the data area, like the samples from a real worn pendrive
			return pg > 8<<20 && (pg/4096*2654435761)%100 < 28
		}}
		s, out := run(t, f, "/dev/flaky-test", image)
		got, want := exact(t, out)
		st := s.Status()
		t.Logf("image=%v: %d of %d photos exact; %d pages read again; save: good %d damaged %d", image, got, want, st.Unstable, st.Save.Good, st.Save.Damaged)
		if got != want {
			t.Errorf("image=%v: only %d of %d photos came out right", image, got, want)
		}
		if st.Save.Damaged > 0 {
			csv, _ := os.ReadFile(filepath.Join(out, "File list.csv"))
			for _, l := range strings.Split(string(csv), "\n") {
				if strings.Contains(l, "damaged") {
					t.Logf("  %s", l)
				}
			}
		}
		if st.Unstable == 0 {
			t.Errorf("image=%v: no unstable places were reported", image)
		}
		found := false
		for _, d := range st.Diagnosis {
			found = found || strings.Contains(d.Title, "wearing out")
		}
		if !found {
			t.Errorf("image=%v: the diagnosis does not mention the worn memory", image)
		}
		base.Close()
	}
}

func TestBadSectors(t *testing.T) {
	for _, image := range []bool{false, true} {
		base := openImage(t, testImage)
		f := &faulty{Source: base, badFrom: 5214208 + 65536, badTo: 5214208 + 65536 + 4096}
		s, out := run(t, f, "test", image)
		st := s.Status()
		if st.BadBytes == 0 && s.r.BadSectors() == 0 && s.devR.BadSectors() == 0 {
			t.Errorf("image=%v: the unreadable area was not noticed", image)
		}
		rep, _ := os.ReadFile(filepath.Join(out, "REPORT.txt"))
		if !strings.Contains(string(rep), "could not be read") {
			t.Errorf("image=%v: the report does not mention the unreadable area", image)
		}
		if st.Save.Good < 30 {
			t.Errorf("image=%v: only %d good files", image, st.Save.Good)
		}
		base.Close()
	}
}

func TestDeviceGone(t *testing.T) {
	// with a safe copy, what was read before the pendrive vanished is used
	base := openImage(t, testImage)
	f := &faulty{Source: base, goneAfter: 9 << 20}
	s, _ := run(t, f, "/dev/gone-test", true)
	st := s.Status()
	if st.Phase != PhaseReady || st.Files < 10 {
		t.Errorf("image mode: phase %s, %d files", st.Phase, st.Files)
	}
	base.Close()
	// reading directly: stops with an explanation
	base = openImage(t, testImage)
	f = &faulty{Source: base, goneAfter: 9 << 20}
	s, _ = run(t, f, "/dev/gone-test", false)
	if st := s.Status(); st.Phase != PhaseFailed || !strings.Contains(st.Error, "stopped responding") {
		t.Errorf("direct mode: phase %s error %q", st.Phase, st.Error)
	}
	base.Close()
}

// md5 of the whole image, and of what the careful copy made of it
func samePages(t *testing.T, a, b string) (diff, total int) {
	t.Helper()
	x, err1 := os.ReadFile(a)
	y, err2 := os.ReadFile(b)
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	for p := 0; p+4096 <= len(x) && p+4096 <= len(y); p += 4096 {
		total++
		if string(x[p:p+4096]) != string(y[p:p+4096]) {
			diff++
		}
	}
	return
}

func TestCarefulCopy(t *testing.T) {
	base := openImage(t, fragImage)
	defer base.Close()
	f := &flaky{Source: base, q: 0.002, weak: func(pg int64) bool {
		return (pg/4096*2654435761)%100 < 5
	}}
	openSource = func(string) (disk.Source, error) { return f, nil }
	isDevice = func(p string) bool { return strings.HasPrefix(p, "/dev/") }
	t.Cleanup(func() { openSource, isDevice = disk.Open, disk.IsDevicePath })
	out := t.TempDir()
	s, err := Start(Options{Source: "/dev/careful-test", SourceDesc: "test", OutDir: out, MakeImage: true, Careful: true, DeepScan: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for st := s.Status(); st.Phase != PhaseReady && st.Phase != PhaseFailed; st = s.Status() {
		time.Sleep(50 * time.Millisecond)
	}
	st := s.Status()
	diff, total := samePages(t, fragImage, filepath.Join(out, ImageName))
	t.Logf("careful copy: %d pages settled, %d unsure; %d of %d pages differ from the original", st.Settled, st.Unsure, diff, total)
	if st.Settled == 0 {
		t.Error("no pages were settled")
	}
	if diff > st.Unsure {
		t.Errorf("%d pages of the copy are wrong, but only %d were reported unsure", diff, st.Unsure)
	}
	if !st.Careful {
		t.Error("the status does not say the copy was careful")
	}
}

func TestFakeLimit(t *testing.T) {
	base := openImage(t, fragImage)
	defer base.Close()
	real := base.Size()
	f := &fake{Source: base, claimed: real * 8}
	openSource = func(string) (disk.Source, error) { return f, nil }
	isDevice = func(p string) bool { return strings.HasPrefix(p, "/dev/") }
	t.Cleanup(func() { openSource, isDevice = disk.Open, disk.IsDevicePath })
	out := t.TempDir()
	s, err := Start(Options{Source: "/dev/fake-test", SourceDesc: "test", OutDir: out, MakeImage: true, Limit: real, DeepScan: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for st := s.Status(); st.Phase != PhaseReady && st.Phase != PhaseFailed; st = s.Status() {
		time.Sleep(50 * time.Millisecond)
	}
	st := s.Status()
	fi, err := os.Stat(filepath.Join(out, ImageName))
	if err != nil || fi.Size() != real {
		t.Errorf("the copy should hold only the real %d bytes: %v %v", real, fi.Size(), err)
	}
	if st.Files < 40 || f.maxOff > real {
		t.Errorf("%d files found; reads went up to %d of %d real bytes", st.Files, f.maxOff, real)
	}
	fakeNoted := false
	for _, d := range st.Diagnosis {
		fakeNoted = fakeNoted || strings.Contains(d.Title, "fake")
	}
	if !fakeNoted {
		t.Error("the diagnosis does not say the drive is fake")
	}
}

type fake struct {
	disk.Source
	claimed, maxOff int64
}

func (f *fake) Size() int64 { return f.claimed }
func (f *fake) ReadAt(p []byte, off int64) (int, error) {
	f.maxOff = max(f.maxOff, off+int64(len(p)))
	real := f.Source.Size()
	for i := 0; i < len(p); {
		at := (off + int64(i)) % real
		n := min(int64(len(p)-i), real-at)
		f.Source.ReadAt(p[i:i+int(n)], at)
		i += int(n)
	}
	return len(p), nil
}

// sticky flips bits at fixed places on every read (not worn flash: reading
// again does not help).
type sticky struct {
	disk.Source
	at []int64
}

func (s *sticky) ReadAt(p []byte, off int64) (int, error) {
	n, err := s.Source.ReadAt(p, off)
	for _, a := range s.at {
		if a >= off && a < off+int64(n) {
			p[a-off] ^= 0x20
		}
	}
	return n, err
}

func TestHeaderRebuilt(t *testing.T) {
	base := openImage(t, fragImage)
	defer base.Close()
	// find a photo and damage its quantization table length
	img, _ := os.ReadFile(fragImage)
	var bad []int64
	for p := 8 << 20; p+4 < len(img) && len(bad) < 2; p++ {
		if img[p] == 0xFF && img[p+1] == 0xD8 && img[p+2] == 0xFF && p%512 == 0 {
			for q := p; q < p+4096; q++ {
				if img[q] == 0xFF && img[q+1] == 0xDB {
					bad = append(bad, int64(q+3))
					break
				}
			}
			p += 1 << 20
		}
	}
	if len(bad) == 0 {
		t.Skip("no photo found")
	}
	s, out := run(t, &sticky{Source: base, at: bad}, "/dev/sticky-test", true)
	st := s.Status()
	t.Logf("headers rebuilt: %d; saved good %d, damaged %d", st.Headers, st.Save.Good, st.Save.Damaged)
	if st.Headers == 0 {
		t.Error("no photo header was rebuilt")
	}
	got, want := exact(t, out)
	if got != want {
		t.Errorf("only %d of %d photos came out right", got, want)
	}
}

func TestCarefulCopyWeakBits(t *testing.T) {
	// weak bits that are wrong in 30% of reads need many reads to settle
	base := openImage(t, fragImage)
	defer base.Close()
	d := sim.New(base, sim.Options{Seed: 9, WeakPages: 0.01, FlipChance: 0.5, FlipBits: 2, StickyBits: 3, StickyChance: 0.3})
	openSource = func(string) (disk.Source, error) { return d, nil }
	isDevice = func(p string) bool { return strings.HasPrefix(p, "/dev/") }
	t.Cleanup(func() { openSource, isDevice = disk.Open, disk.IsDevicePath })
	out := t.TempDir()
	s, err := Start(Options{Source: "/dev/weak-test", SourceDesc: "test", OutDir: out, MakeImage: true, Careful: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for st := s.Status(); st.Phase != PhaseReady && st.Phase != PhaseFailed; st = s.Status() {
		time.Sleep(50 * time.Millisecond)
	}
	st := s.Status()
	diff, total := samePages(t, fragImage, filepath.Join(out, ImageName))
	t.Logf("weak bits: %d pages settled, %d unsure; %d of %d pages differ; %d reads", st.Settled, st.Unsure, diff, total, d.Reads)
	if diff*20 > st.Settled {
		t.Errorf("too many pages still wrong: %d of %d settled", diff, st.Settled)
	}
}
