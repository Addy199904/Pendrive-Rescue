// Package fsys understands the file systems found on pendrives (FAT12/16/32,
// exFAT and NTFS). It lists existing, hidden and deleted files with their
// names, and rebuilds folders that were lost when the drive was formatted.
package fsys

import (
	"encoding/binary"
	"sort"
	"strings"
	"time"
	"unicode/utf16"

	"pendriverescue/internal/disk"
	"pendriverescue/internal/sig"
)

// Extent is a piece of a file on the drive. Off < 0 means "zeros" (sparse).
type Extent struct {
	Off, Len int64
}

// Category says how a file was found.
type Category int

const (
	CatNamed Category = iota // in the file system tree (existing, hidden or deleted)
	CatLost                  // in a folder that was lost by formatting
)

// File is one recoverable file.
type File struct {
	Volume  string   // which volume / old file system it belongs to
	Dir     []string // folder path
	Name    string
	Size    int64
	Extents []Extent
	// AltExtents: a second guess for a deleted file whose clusters were
	// partly reused: the free clusters in order, skipping the ones in use.
	AltExtents []Extent
	Data       []byte // small files stored inside NTFS records
	MTime      time.Time
	Deleted    bool
	Hidden     bool
	Cat        Category
	Live       bool   // allocated in the current file system: extents are certain
	Damaged    string // why the content is probably damaged ("" = looks fine)
	Note       string

	// NTFS-compressed files: data runs in file order (Off < 0 = sparse) and
	// the cluster size; the writer decompresses 16-cluster units.
	Compressed  bool
	CompExtents []Extent
	CompCS      int64
}

// Start returns the drive offset where the file's data begins (or -1).
func (f *File) Start() int64 {
	for _, e := range f.Extents {
		if e.Len > 0 {
			return e.Off
		}
	}
	return -1
}

// Ext returns the lower-case file extension without the dot.
func (f *File) Ext() string {
	i := strings.LastIndexByte(f.Name, '.')
	if i < 0 || i == len(f.Name)-1 {
		return ""
	}
	return strings.ToLower(f.Name[i+1:])
}

// Volume is a file system found on the drive.
type Volume struct {
	Name        string // "Partition 1 (FAT32, MYDRIVE)"
	Type        string // FAT12, FAT16, FAT32, exFAT, NTFS, BitLocker
	Start, Size int64
	Label       string
	ClusterSize int64
	Files       []*File
	Live        int // existing files
	Hidden      int
	Deleted     int
	Dirs        int
	Notes       []string
	Encrypted   bool
	// allocation test for the volume's current state (nil = unknown)
	allocated func(off int64) bool
	// clusters / records belonging to the walked tree (excluded from lost search)
	fatDirs   map[uint32]bool
	exfatDirs map[uint32]bool
	mftRanges []Extent
	geom      geometry
}

// geometry maps cluster numbers to drive offsets.
type geometry struct {
	kind      string // "fat", "exfat", "ntfs"
	start     int64  // volume start
	dataStart int64  // offset of cluster `first`
	cs        int64  // cluster size
	first     int64  // number of the first cluster (2 for FAT/exFAT, 0 for NTFS)
	clusters  int64  // number of clusters
	fatBits   int    // 12/16/32 for FAT
}

func (g geometry) off(c int64) int64 { return g.dataStart + (c-g.first)*g.cs }
func (g geometry) valid(c int64) bool {
	return c >= g.first && (g.clusters == 0 || c < g.first+g.clusters)
}

// IsAllocated reports whether the current file system uses the byte at off.
func (v *Volume) IsAllocated(off int64) bool {
	if v.allocated == nil || off < v.Start || off >= v.Start+v.Size {
		return false
	}
	return v.allocated(off)
}

func le16(b []byte) uint16 { return binary.LittleEndian.Uint16(b) }
func le32(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }
func le64(b []byte) uint64 { return binary.LittleEndian.Uint64(b) }

// readAt reads n bytes at off (bad sectors read as zeros).
func readAt(r *disk.Reader, off int64, n int) ([]byte, error) {
	b := make([]byte, n)
	_, err := r.ReadAt(b, off)
	return b, err
}

// decodeUTF16 converts UTF-16LE bytes up to the first NUL (or 0xFFFF pad).
func decodeUTF16(b []byte) string {
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		c := le16(b[i:])
		if c == 0 || c == 0xFFFF {
			break
		}
		u = append(u, c)
	}
	return string(utf16.Decode(u))
}

// mergeExtents joins touching extents.
func mergeExtents(ex []Extent) []Extent {
	var out []Extent
	for _, e := range ex {
		if e.Len <= 0 {
			continue
		}
		if l := len(out); l > 0 {
			last := &out[l-1]
			if last.Off >= 0 && e.Off >= 0 && last.Off+last.Len == e.Off {
				last.Len += e.Len
				continue
			}
			if last.Off < 0 && e.Off < 0 {
				last.Len += e.Len
				continue
			}
		}
		out = append(out, e)
	}
	return out
}

// trimExtents cuts the list of extents to exactly size bytes.
func trimExtents(ex []Extent, size int64) []Extent {
	var out []Extent
	left := size
	for _, e := range ex {
		if left <= 0 {
			break
		}
		if e.Len > left {
			e.Len = left
		}
		out = append(out, e)
		left -= e.Len
	}
	return out
}

// checkContent reads the start of a file and compares it with what the
// extension promises. Returns a damage note or "".
func checkContent(r *disk.Reader, f *File) string {
	if f.Size == 0 {
		return ""
	}
	var head []byte
	if f.Data != nil {
		head = f.Data
	} else {
		st := f.Start()
		if st < 0 {
			return ""
		}
		n := int64(1024)
		if f.Extents[0].Len < n {
			n = f.Extents[0].Len
		}
		b, err := readAt(r, st, int(n))
		if err != nil {
			return ""
		}
		head = b
	}
	if isAllZero(head) && f.Size >= 512 {
		// check a bit more of the file before calling it erased
		if allZeroSample(r, f) {
			return "erased (only zeros)"
		}
	}
	if m, known := sig.ExtMatches(f.Ext(), head); known && !m {
		return "content does not match file type (probably overwritten)"
	}
	return ""
}

func isAllZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

func allZeroSample(r *disk.Reader, f *File) bool {
	buf := make([]byte, 4096)
	checked := 0
	for _, e := range f.Extents {
		if e.Off < 0 {
			continue
		}
		for _, p := range []int64{0, e.Len / 2, e.Len - 4096} {
			if p < 0 || p >= e.Len {
				continue
			}
			n := int64(len(buf))
			if p+n > e.Len {
				n = e.Len - p
			}
			if _, err := r.ReadAt(buf[:n], e.Off+p); err != nil {
				return false
			}
			if !isAllZero(buf[:n]) {
				return false
			}
			checked++
			if checked > 12 {
				return true
			}
		}
	}
	return true
}

// dosTime converts FAT date and time fields.
func dosTime(date, tm uint16, tenth byte) time.Time {
	if date == 0 {
		return time.Time{}
	}
	y := int(date>>9) + 1980
	mo := time.Month((date >> 5) & 0xF)
	d := int(date & 0x1F)
	if mo < 1 || mo > 12 || d < 1 || d > 31 {
		return time.Time{}
	}
	h := int(tm >> 11)
	mi := int((tm >> 5) & 0x3F)
	s := int(tm&0x1F) * 2
	if h > 23 || mi > 59 || s > 59 {
		h, mi, s = 0, 0, 0
	}
	t := time.Date(y, mo, d, h, mi, s, 0, time.Local)
	if tenth > 0 && tenth < 200 {
		t = t.Add(time.Duration(tenth) * 10 * time.Millisecond)
	}
	return t
}

// filetime converts a Windows FILETIME.
func filetime(v uint64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	const epochDiff = 116444736000000000
	if v < epochDiff {
		return time.Time{}
	}
	ns := int64(v-epochDiff) * 100
	t := time.Unix(0, ns)
	if t.Year() > 2100 {
		return time.Time{}
	}
	return t
}

// interval set used to test "is this offset inside one of these ranges"
type rangeSet struct {
	r []Extent
}

func (s *rangeSet) add(off, n int64) { s.r = append(s.r, Extent{off, n}) }
func (s *rangeSet) finish() {
	sort.Slice(s.r, func(i, j int) bool { return s.r[i].Off < s.r[j].Off })
	var out []Extent
	for _, e := range s.r {
		if l := len(out); l > 0 && out[l-1].Off+out[l-1].Len >= e.Off {
			if e.Off+e.Len > out[l-1].Off+out[l-1].Len {
				out[l-1].Len = e.Off + e.Len - out[l-1].Off
			}
			continue
		}
		out = append(out, e)
	}
	s.r = out
}
func (s *rangeSet) contains(off int64) bool {
	i := sort.Search(len(s.r), func(i int) bool { return s.r[i].Off+s.r[i].Len > off })
	return i < len(s.r) && s.r[i].Off <= off
}
