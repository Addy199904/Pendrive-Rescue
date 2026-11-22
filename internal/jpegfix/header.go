package jpegfix

import (
	"bytes"
	"math/bits"
	"sync"
)

// The first few hundred bytes after a photo's EXIF block are tables:
// quantization tables, the frame header, Huffman tables and the scan header.
// A camera writes the same tables into every photo it takes at the same
// size and quality. On worn flash, a single wrong bit in them makes the
// whole photo unreadable. Those bytes can be taken from a good photo of the
// same camera: the rest of the photo is then readable again, up to the
// first wrong bit in the picture data itself.

// tablesOf returns where the table run of a well-formed photo starts and
// ends (the end of the scan header).
func tablesOf(b []byte) (start, end int, ok bool) {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return 0, 0, false
	}
	pos, start := 2, -1
	for pos+4 <= len(b) {
		if b[pos] != 0xFF {
			return 0, 0, false
		}
		m := b[pos+1]
		if m == 0xFF {
			pos++
			continue
		}
		if m == 0xD8 || m >= 0xD0 && m <= 0xD7 || m == 0x01 {
			pos += 2
			continue
		}
		n := int(b[pos+2])<<8 | int(b[pos+3])
		if n < 2 || pos+2+n > len(b) {
			return 0, 0, false
		}
		if isTable(m) {
			if start < 0 {
				start = pos
			}
		} else {
			start = -1
		}
		if m == 0xDA {
			return start, pos + 2 + n, start >= 2
		}
		pos += 2 + n
	}
	return 0, 0, false
}

func isTable(m byte) bool {
	return m == 0xDB || m == 0xC4 || m == 0xDD || m == 0xDA || m >= 0xC0 && m <= 0xCF && m != 0xC8 && m != 0xCC
}

func isApp(m byte) bool { return m >= 0xE0 && m <= 0xEF || m == 0xFE }

// SampleSet keeps the tables of photos known to be good, to repair others.
// Tables count as known once two photos share them, or one photo with them
// was checked from start to end.
type SampleSet struct {
	mu   sync.Mutex
	seen map[string]int
	good map[string]bool
	jfif []byte
}

// NewSampleSet makes an empty set.
func NewSampleSet() *SampleSet {
	return &SampleSet{seen: map[string]int{}, good: map[string]bool{}}
}

// Add records the tables of a photo from its first bytes (at least up to
// the end of the scan header; 128 KB is plenty). verified means the whole
// photo was checked and is fine.
func (s *SampleSet) Add(head []byte, verified bool) {
	a, z, ok := tablesOf(head)
	if !ok || z-a > 4096 {
		return
	}
	k := string(head[a:z])
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.seen) > 256 && s.seen[k] == 0 {
		return
	}
	s.seen[k]++
	if verified {
		s.good[k] = true
	}
	if s.jfif == nil && len(head) > 20 && head[2] == 0xFF && head[3] == 0xE0 && string(head[6:10]) == "JFIF" {
		n := int(head[4])<<8 | int(head[5])
		if 2+2+n <= len(head) && n < 64 {
			s.jfif = append([]byte(nil), head[2:4+n]...)
		}
	}
}

// Len returns how many different known tables there are.
func (s *SampleSet) Len() int {
	return len(s.known())
}

func (s *SampleSet) known() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out [][]byte
	for k, n := range s.seen {
		if n >= 2 || s.good[k] {
			out = append(out, []byte(k))
		}
	}
	return out
}

// HeaderFix is a repaired start for a photo: Head replaces the photo's bytes
// before From.
type HeaderFix struct {
	Head    []byte
	From    int64
	Dropped bool // the EXIF block was too damaged and was left out
	Bits    int  // wrong bits the tables had
}

// RepairHeader rebuilds the tables of a photo whose header is broken, using
// the known tables closest to its own. b holds the photo's first bytes (at
// least 256 KB, or all of it). It returns nil when the header is fine or
// nothing close enough is known.
func RepairHeader(b []byte, set *SampleSet) *HeaderFix {
	if len(b) < 64 || b[0] != 0xFF {
		return nil
	}
	if _, err := Parse(bytes.NewReader(b), int64(len(b))); err == nil || err == ErrUnsupported {
		return nil
	}
	known := set.known()
	if len(known) == 0 {
		return nil
	}
	bestD, bestP := -1, -1
	var bestT []byte
	searchTo := min(len(b), 256<<10)
	for _, t := range known {
		limit := len(t) * 8 * 4 / 100 // 4% of the bits may be wrong
		if bestD >= 0 {
			limit = min(limit, bestD)
		}
		for p := 2; p+len(t) <= searchTo; p++ {
			d := 0
			for i := 0; i < len(t); i++ {
				d += bits.OnesCount8(b[p+i] ^ t[i])
				if d > limit {
					break
				}
			}
			if d <= limit && (bestD < 0 || d < bestD) {
				bestD, bestP, bestT = d, p, t
				limit = d
			}
		}
	}
	if bestP < 0 {
		return nil
	}
	apps, fits := mendApps(b[2:bestP], bestP-2)
	dropped := false
	if !fits {
		set.mu.Lock()
		apps = append([]byte(nil), set.jfif...)
		set.mu.Unlock()
		if apps == nil {
			apps = []byte{0xFF, 0xE0, 0, 16, 'J', 'F', 'I', 'F', 0, 1, 1, 0, 0, 1, 0, 1, 0, 0}
		}
		dropped = true
	}
	head := make([]byte, 0, 2+len(apps)+len(bestT))
	head = append(head, 0xFF, 0xD8)
	head = append(head, apps...)
	head = append(head, bestT...)
	fix := &HeaderFix{Head: head, From: int64(bestP + len(bestT)), Dropped: dropped, Bits: bestD}
	// the result must at least parse
	test := append(append([]byte(nil), head...), b[fix.From:]...)
	if _, err := Parse(bytes.NewReader(test), int64(len(test))); err != nil {
		return nil
	}
	return fix
}

// mendApps checks the chain of APP segments between the start marker and the
// tables (n bytes); a length with a wrong bit is corrected when exactly one
// single-bit change makes the chain fit. ok is false if it cannot be made
// to fit.
func mendApps(a []byte, n int) (fixed []byte, ok bool) {
	a = append([]byte(nil), a...)
	pos := 0
	for pos < n {
		if pos+4 > n || a[pos] != 0xFF {
			return nil, false
		}
		if !isApp(a[pos+1]) {
			// a marker byte with one wrong bit?
			fixed := false
			for k := 0; k < 8; k++ {
				if m := a[pos+1] ^ 1<<k; isApp(m) {
					a[pos+1], fixed = m, true
					break
				}
			}
			if !fixed {
				return nil, false
			}
		}
		l := int(a[pos+2])<<8 | int(a[pos+3])
		fits := func(l int) bool {
			e := pos + 2 + l
			return l >= 2 && (e == n || e+2 <= n && a[e] == 0xFF && isApp(a[e+1]))
		}
		if !fits(l) {
			found := -1
			for k := 0; k < 16; k++ {
				if c := l ^ 1<<k; fits(c) {
					if found >= 0 {
						return nil, false // two ways to fix it: do not guess
					}
					found = c
				}
			}
			if found < 0 {
				return nil, false
			}
			l = found
			a[pos+2], a[pos+3] = byte(l>>8), byte(l)
		}
		pos += 2 + l
	}
	return a, true
}
