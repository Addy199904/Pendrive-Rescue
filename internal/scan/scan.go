// Package scan reads the whole drive once, from start to end, and notes
// everything that later recovery steps need: which areas are empty, which
// are unreadable, where file systems and lost folders are, where known file
// types and text files begin. It can save a copy (image) of the drive at the
// same time, and continue an interrupted copy later.
package scan

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"sync"
	"time"

	"pendriverescue/internal/disk"
	"pendriverescue/internal/sig"
)

const (
	chunkSize = 1 << 20
	overlap   = 8 << 10
	NumRegion = 100
)

// ErrStopped is returned when the user stopped the scan.
var ErrStopped = errors.New("stopped by the user")

// Hit is a place where a known file type begins.
type Hit struct {
	Off  int64
	Kind sig.Kind
}

// BootType is a kind of volume boot sector.
type BootType uint8

const (
	BootFAT BootType = iota + 1
	BootExFAT
	BootNTFS
	BootBitLocker
)

// BootCand is a sector that looks like the first sector of a file system.
type BootCand struct {
	Off  int64
	Type BootType
	Data []byte // the 512-byte sector
}

// FatDir is a FAT directory cluster found by its "." and ".." entries.
type FatDir struct {
	Off          int64
	Self, Parent uint32
}

// ExfatSet is an exFAT file entry set (one file's directory entries).
type ExfatSet struct {
	Off     int64
	Deleted bool
}

// TextRun is a stretch of consecutive sectors that hold plain text.
type TextRun struct {
	Off, Len int64
	Wide     bool // UTF-16 text
}

// Region holds counts of 512-byte sectors by type for 1% of the drive.
type Region struct {
	Data, Zero, FF, Bad int64
}

// Range is a byte range.
type Range struct{ Off, Len int64 }

// Result is everything the scan learned.
type Result struct {
	Size        int64
	Zero        int64 // sectors that are all 0x00
	FF          int64 // sectors that are all 0xFF (erased flash)
	Data        int64 // sectors that hold something
	Bad         int64 // unreadable sectors
	Regions     [NumRegion]Region
	BadRanges   []Range
	Boot        []BootCand
	FatDirs     []FatDir
	ExfatSets   []ExfatSet
	MftRecs     []int64
	Hits        []Hit
	TextRuns    []TextRun
	ScannedTo   int64 // how far the scan got
	Careful     bool  // read twice, settling disagreements by majority
	Settled     int   // pages that read differently on different tries, settled by majority
	Unsure      int   // of those, pages whose vote stayed close
	UnsurePages []int64
	ImagePath   string
	Resumed     int64 // bytes taken from an earlier, interrupted copy
	Duration    time.Duration
	Interrupted error

	text textState
}

// Options control a scan.
type Options struct {
	ImagePath string // copy the drive into this file ("" = no copy)
	Resume    bool   // continue an earlier copy (uses ImagePath + ".map")
	Identity  string // identifies the drive in the map file
	Text      bool   // look for text files
	Careful   bool   // read everything twice; where reads disagree, read more and keep the majority
	Stop      <-chan struct{}
	// Progress is called from time to time with the partial result.
	Progress func(res *Result, done, total, badBytes int64)
}

type chunk struct {
	off       int64
	n         int   // valid bytes
	bad       int64 // unreadable bytes in this chunk
	skip      bool  // not read at all (inside a dead area)
	fromImage bool  // taken from the earlier copy
	buf       []byte
	err       error
	settled   []int64 // pages settled by voting (careful copy)
	unsure    []int64
}

var (
	zero512 = make([]byte, 512)
	ff512   = bytes.Repeat([]byte{0xFF}, 512)
	dot     = []byte(".          ")
	dotdot  = []byte("..         ")
)

// ---------------------------------------------------------------- map file

// Map remembers which parts of the drive are already in the image file, so
// an interrupted copy can continue later.
type Map struct {
	mu       sync.Mutex
	path     string
	Version  int        `json:"version"`
	Size     int64      `json:"size"`
	Identity string     `json:"identity"`
	Done     [][2]int64 `json:"done"` // [start, end) ranges copied without errors
	dirty    bool
	saved    time.Time
}

// LoadMap reads a map file; ok=false when it does not exist or does not
// belong to this drive.
func LoadMap(path string, size int64, identity string) (*Map, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	m := &Map{}
	if json.Unmarshal(b, m) != nil || m.Size != size || m.Identity != identity {
		return nil, false
	}
	m.path = path
	return m, true
}

// NewMap starts an empty map.
func NewMap(path string, size int64, identity string) *Map {
	return &Map{path: path, Version: 1, Size: size, Identity: identity}
}

// Add marks [off, off+n) as copied.
func (m *Map) Add(off, n int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	start, end := off, off+n
	var out [][2]int64
	placed := false
	for _, r := range m.Done {
		switch {
		case r[1] < start:
			out = append(out, r)
		case r[0] > end:
			if !placed {
				out = append(out, [2]int64{start, end})
				placed = true
			}
			out = append(out, r)
		default:
			start = min(start, r[0])
			end = max(end, r[1])
		}
	}
	if !placed {
		out = append(out, [2]int64{start, end})
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	m.Done = out
	m.dirty = true
}

// Covers reports whether [off, off+n) is fully copied.
func (m *Map) Covers(off, n int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := sort.Search(len(m.Done), func(i int) bool { return m.Done[i][1] > off })
	return i < len(m.Done) && m.Done[i][0] <= off && m.Done[i][1] >= off+n
}

// CopiedBytes returns how much is already copied.
func (m *Map) CopiedBytes() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	var t int64
	for _, r := range m.Done {
		t += r[1] - r[0]
	}
	return t
}

// Save writes the map (atomically) if it changed; force writes anyway.
func (m *Map) Save(force bool) {
	m.mu.Lock()
	if !m.dirty && !force {
		m.mu.Unlock()
		return
	}
	b, _ := json.Marshal(m)
	m.dirty = false
	m.saved = time.Now()
	m.mu.Unlock()
	tmp := m.path + ".tmp"
	if os.WriteFile(tmp, b, 0644) == nil {
		os.Rename(tmp, m.path)
	}
}

// ---------------------------------------------------------------- the scan

// Run scans the whole drive behind r.
func Run(r *disk.Reader, opt Options) (*Result, error) {
	start := time.Now()
	res := &Result{Size: r.Size()}
	res.text.enabled = opt.Text
	size := r.Size()

	var img *os.File
	var mp *Map
	if opt.ImagePath != "" {
		res.ImagePath = opt.ImagePath
		mapPath := opt.ImagePath + ".map"
		if opt.Resume {
			if m, ok := LoadMap(mapPath, size, opt.Identity); ok {
				if st, err := os.Stat(opt.ImagePath); err == nil && st.Mode().IsRegular() {
					mp = m
				}
			}
		}
		var err error
		if mp != nil {
			img, err = os.OpenFile(opt.ImagePath, os.O_RDWR, 0644)
		} else {
			mp = NewMap(mapPath, size, opt.Identity)
			img, err = os.OpenFile(opt.ImagePath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0644)
			if err == nil {
				disk.SetSparse(img)
			}
		}
		if err != nil {
			return nil, err
		}
		if st, err := img.Stat(); err == nil && st.Size() < size {
			img.Truncate(size)
		}
		mp.Save(true)
	}

	free := make(chan []byte, 4)
	for i := 0; i < 4; i++ {
		free <- disk.AlignedBuf(chunkSize + overlap)
	}
	chunks := make(chan *chunk, 2)
	stop := make(chan struct{})

	var careful *carefulReader
	if opt.Careful {
		careful = newCareful(r, opt.Stop)
		res.Careful = true
	}
	go func() {
		defer close(chunks)
		skipLen := int64(0)
		dead := 0
		for off := int64(0); off < size; {
			var buf []byte
			select {
			case buf = <-free:
			case <-stop:
				return
			}
			n := int64(chunkSize)
			if off+n > size {
				n = size - off
			}
			c := &chunk{off: off, n: int(n), buf: buf}
			switch {
			case img != nil && mp.Covers(off, n):
				// already copied by an earlier run: read the copy instead
				k, _ := img.ReadAt(buf[:n], off)
				clear(buf[k:n])
				c.fromImage = true
			case careful != nil:
				c.bad, c.settled, c.unsure, c.err = careful.get(buf[:n], off)
			case skipLen > 0:
				clear(buf[:n])
				c.skip = true
				c.bad = n
				skipLen -= n
				r.MarkBadRange(off, n)
			default:
				bad, err := r.ReadTolerant(buf[:n], off)
				c.bad = bad
				c.err = err
				if err == nil && bad == n {
					dead++
					skipLen = int64(1<<20) << uint(min(dead-1, 6))
				} else {
					dead = 0
				}
			}
			select {
			case chunks <- c:
			case <-stop:
				return
			}
			if c.err != nil {
				return
			}
			off += n
		}
	}()

	var lastReport time.Time
	var badBytes int64
	var runErr error
	cur, ok := <-chunks
	for ok {
		if cur.err != nil {
			runErr = cur.err
			res.Interrupted = cur.err
			break
		}
		if opt.Stop != nil {
			select {
			case <-opt.Stop:
				runErr = ErrStopped
				res.Interrupted = ErrStopped
			default:
			}
			if runErr != nil {
				break
			}
		}
		next, more := <-chunks
		la := cur.buf[cur.n : cur.n+overlap]
		if more && next.err == nil {
			copy(la, next.buf[:overlap])
		} else {
			clear(la)
		}
		analyse(res, cur, size)
		res.Settled += len(cur.settled)
		res.Unsure += len(cur.unsure)
		if len(res.UnsurePages) < 100000 {
			res.UnsurePages = append(res.UnsurePages, cur.unsure...)
		}
		if cur.bad > 0 {
			badBytes += cur.bad
			appendBad(res, cur.off, cur.bad)
		}
		if cur.fromImage {
			res.Resumed += int64(cur.n)
		} else if img != nil && cur.bad < int64(cur.n) {
			if !allZero(cur.buf[:cur.n]) {
				if _, err := img.WriteAt(cur.buf[:cur.n], cur.off); err != nil {
					runErr = errors.New("could not write the safe copy: " + err.Error())
					res.Interrupted = runErr
					break
				}
			}
			if cur.bad == 0 {
				mp.Add(cur.off, int64(cur.n))
			}
		}
		res.ScannedTo = cur.off + int64(cur.n)
		free <- cur.buf
		if img != nil && time.Since(mp.saved) > 5*time.Second {
			mp.Save(false)
		}
		if opt.Progress != nil && time.Since(lastReport) > 250*time.Millisecond {
			opt.Progress(res, res.ScannedTo, size, badBytes)
			lastReport = time.Now()
		}
		cur, ok = next, more
	}
	close(stop)
	for range chunks {
	}
	res.text.flush(res)
	if img != nil {
		mp.Save(true)
		if err := img.Close(); err != nil && runErr == nil {
			runErr = err
		}
	}
	if opt.Progress != nil {
		opt.Progress(res, res.ScannedTo, size, badBytes)
	}
	res.Duration = time.Since(start)
	return res, runErr
}

func allZero(b []byte) bool {
	for len(b) >= 512 {
		if !bytes.Equal(b[:512], zero512) {
			return false
		}
		b = b[512:]
	}
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

func appendBad(res *Result, off, n int64) {
	if l := len(res.BadRanges); l > 0 {
		last := &res.BadRanges[l-1]
		if last.Off+last.Len >= off {
			if off+n > last.Off+last.Len {
				last.Len = off + n - last.Off
			}
			return
		}
	}
	if len(res.BadRanges) < 100000 {
		res.BadRanges = append(res.BadRanges, Range{off, n})
	}
}

func le16(b []byte) uint16 { return binary.LittleEndian.Uint16(b) }
func le32(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }
