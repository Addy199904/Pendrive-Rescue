// Package sim pretends to be a failing pendrive, for tests and for trying
// out the app without one. It wraps a disk image and can make it:
//
//   - wear out: some pages come back with a few wrong bits, different ones on
//     each read (and, optionally, a few weak bits that are wrong more often),
//   - fail: reads that touch bad areas return an error,
//   - be slow, or stall in some places,
//   - disappear (as if unplugged) after a number of bytes,
//   - be fake: claim more space than it has, repeating itself.
package sim

import (
	"errors"
	"hash/fnv"
	"math/rand"
	"sync"
	"time"

	"pendriverescue/internal/disk"
)

// Options describe how the drive misbehaves. The zero value is a healthy drive.
type Options struct {
	Seed int64

	// Worn flash. WeakPages is the share of 4 KB pages that are weak; a weak
	// page is read wrongly with chance FlipChance, and then FlipBits random
	// bits are wrong. StickyBits weak bits per weak page are wrong with chance
	// StickyChance on every read (StickyChance < 0.5, or no number of reads
	// can settle them).
	WeakPages    float64
	FlipChance   float64
	FlipBits     int
	StickyBits   int
	StickyChance float64

	// Bad areas: reads that touch them fail.
	Bad [][2]int64 // [start, end)

	// Speed in bytes per second (0 = as fast as the image). Reads that touch
	// a Stalls range take StallFor longer.
	Rate     int64
	Stalls   [][2]int64
	StallFor time.Duration

	// UnplugAfter makes the drive vanish after this many bytes were read.
	UnplugAfter int64

	// Fake capacity: the drive claims Claimed bytes but really holds only the
	// image's size; addresses past it wrap around.
	Claimed int64
}

// Drive is a simulated pendrive. It is safe for concurrent use.
type Drive struct {
	src  disk.Source
	real int64
	opt  Options

	mu    sync.Mutex
	rng   *rand.Rand
	read  int64
	gone  bool
	Reads int // number of ReadAt calls (for tests)
}

// New wraps src.
func New(src disk.Source, opt Options) *Drive {
	if opt.FlipBits <= 0 {
		opt.FlipBits = 3
	}
	return &Drive{src: src, real: src.Size(), opt: opt, rng: rand.New(rand.NewSource(opt.Seed + 1))}
}

// Open wraps an image file.
func Open(path string, opt Options) (*Drive, error) {
	src, err := disk.OpenImage(path)
	if err != nil {
		return nil, err
	}
	return New(src, opt), nil
}

// ErrIO is what a bad area answers.
var ErrIO = errors.New("I/O error")

// Size is what the drive claims.
func (d *Drive) Size() int64 {
	if d.opt.Claimed > d.real {
		return d.opt.Claimed
	}
	return d.real
}

// SectorSize is 512.
func (d *Drive) SectorSize() int { return 512 }

// Name of the drive.
func (d *Drive) Name() string { return "simulated pendrive" }

// Close closes the image.
func (d *Drive) Close() error { return d.src.Close() }

// Unplug makes the drive vanish now.
func (d *Drive) Unplug() {
	d.mu.Lock()
	d.gone = true
	d.mu.Unlock()
}

func touches(rs [][2]int64, off, n int64) bool {
	for _, r := range rs {
		if off < r[1] && off+n > r[0] {
			return true
		}
	}
	return false
}

// IsWeak reports whether the page at off is a weak page.
func (d *Drive) IsWeak(off int64) bool {
	if d.opt.WeakPages <= 0 {
		return false
	}
	h := fnv.New64a()
	var b [16]byte
	pg := (off % d.real) / disk.PageSize
	for i := 0; i < 8; i++ {
		b[i] = byte(pg >> (8 * i))
		b[8+i] = byte(d.opt.Seed >> (8 * i))
	}
	h.Write(b[:])
	return float64(h.Sum64()%1000000)/1000000 < d.opt.WeakPages
}

// sticky returns the weak bit positions of a page (stable for the page).
func (d *Drive) sticky(pg int64) []int {
	if d.opt.StickyBits <= 0 {
		return nil
	}
	r := rand.New(rand.NewSource(pg*7919 + d.opt.Seed))
	out := make([]int, d.opt.StickyBits)
	for i := range out {
		out[i] = r.Intn(disk.PageSize * 8)
	}
	return out
}

// ReadAt reads like a failing pendrive would.
func (d *Drive) ReadAt(p []byte, off int64) (int, error) {
	n := int64(len(p))
	d.mu.Lock()
	d.Reads++
	if d.gone {
		d.mu.Unlock()
		return 0, ErrIO
	}
	if d.opt.UnplugAfter > 0 && d.read+n > d.opt.UnplugAfter {
		d.gone = true
		d.mu.Unlock()
		return 0, ErrIO
	}
	d.read += n
	d.mu.Unlock()

	wait := time.Duration(0)
	if d.opt.Rate > 0 {
		wait += time.Duration(n * int64(time.Second) / d.opt.Rate)
	}
	if d.opt.StallFor > 0 && touches(d.opt.Stalls, off, n) {
		wait += d.opt.StallFor
	}
	if wait > 0 {
		time.Sleep(wait)
	}
	if touches(d.opt.Bad, off, n) {
		return 0, ErrIO
	}
	if off >= d.Size() {
		return 0, ErrIO
	}
	// fake drives wrap around their real capacity
	done := int64(0)
	for done < n {
		at := (off + done) % d.real
		k := min(n-done, d.real-at)
		if _, err := d.src.ReadAt(p[done:done+k], at); err != nil {
			return int(done), err
		}
		done += k
	}
	if d.opt.WeakPages > 0 {
		d.mu.Lock()
		for pg := off &^ (disk.PageSize - 1); pg < off+n; pg += disk.PageSize {
			if !d.IsWeak(pg) {
				continue
			}
			lo := max(pg, off)
			hi := min(pg+disk.PageSize, off+n)
			flip := func(bit int) {
				at := pg + int64(bit/8)
				if at >= lo && at < hi {
					p[at-off] ^= 1 << uint(bit%8)
				}
			}
			for _, b := range d.sticky((pg % d.real) / disk.PageSize) {
				if d.rng.Float64() < d.opt.StickyChance {
					flip(b)
				}
			}
			if d.rng.Float64() < d.opt.FlipChance {
				for i := 0; i < d.opt.FlipBits; i++ {
					flip(d.rng.Intn(disk.PageSize * 8))
				}
			}
		}
		d.mu.Unlock()
	}
	return int(n), nil
}

// Preset returns options for a named kind of trouble on an image of the
// given size (for the demo build and tests): healthy, worn, failing, slow,
// fake, unplug.
func Preset(name string, size int64) (Options, bool) {
	mb := int64(1 << 20)
	switch name {
	case "healthy":
		return Options{Rate: 60 * mb}, true
	case "worn":
		return Options{Seed: 7, WeakPages: 0.04, FlipChance: 0.6, FlipBits: 4, Rate: 30 * mb}, true
	case "failing":
		bad := size * 37 / 100
		return Options{Seed: 3, Bad: [][2]int64{{bad, bad + 256<<10}, {size * 61 / 100, size*61/100 + 64<<10}},
			Stalls: [][2]int64{{bad - 8*mb, bad}}, StallFor: 40 * time.Millisecond, Rate: 20 * mb}, true
	case "slow":
		return Options{Rate: 4 * mb}, true
	case "fake":
		return Options{Claimed: size * 8, Rate: 60 * mb}, true
	case "unplug":
		return Options{UnplugAfter: size / 2, Rate: 30 * mb}, true
	}
	return Options{}, false
}
