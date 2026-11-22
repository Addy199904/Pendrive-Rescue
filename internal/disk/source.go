// Package disk gives read-only access to a pendrive (or a saved image of
// one). Nothing in this package ever opens a drive for writing.
package disk

import (
	"errors"
	"fmt"
	"io"
	"os"
	"unsafe"
)

// Source is a read-only, random-access view of a whole drive or image file.
// ReadAt on a device may require offsets and lengths that are multiples of
// SectorSize(); use Reader for arbitrary reads.
type Source interface {
	ReadAt(p []byte, off int64) (int, error)
	Size() int64
	SectorSize() int
	Name() string
	Close() error
}

// ErrDeviceGone is returned when the drive stops answering altogether
// (for example when it was unplugged).
var ErrDeviceGone = errors.New("the drive stopped responding (was it unplugged?)")

// DriveInfo describes a drive that can be scanned.
type DriveInfo struct {
	Path       string   // \\.\PhysicalDrive2 or /dev/sdb
	Number     int      // Windows disk number (-1 on Linux)
	Model      string   // vendor + model text
	Size       int64    // bytes
	SectorSize int      // logical sector size
	USB        bool     // connected by USB (or SD/MMC card reader)
	Removable  bool     // reported as removable media
	System     bool     // holds the running operating system
	Letters    []string // Windows drive letters / Linux mount points
	FSInfo     []string // file systems the OS sees on it
	Note       string
}

// imageSource reads a disk image file (.img / .dd / .bin).
type imageSource struct {
	f    *os.File
	size int64
	name string
}

// OpenImage opens an image file read-only.
func OpenImage(path string) (Source, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	return &imageSource{f: f, size: st.Size(), name: path}, nil
}

func (s *imageSource) ReadAt(p []byte, off int64) (int, error) {
	n, err := s.f.ReadAt(p, off)
	if err == io.EOF && n == len(p) {
		err = nil
	}
	return n, err
}
func (s *imageSource) Size() int64     { return s.size }
func (s *imageSource) SectorSize() int { return 512 }
func (s *imageSource) Name() string    { return s.name }
func (s *imageSource) Close() error    { return s.f.Close() }

// Open opens either a device path or an image file.
func Open(path string) (Source, error) {
	if IsDevicePath(path) {
		return OpenDevice(path)
	}
	return OpenImage(path)
}

// AlignedBuf returns a byte slice of length n whose start address is
// 4096-byte aligned (needed for raw disk reads on Windows).
func AlignedBuf(n int) []byte {
	b := make([]byte, n+4096)
	a := int(uintptr(unsafe.Pointer(&b[0])) & 4095)
	off := 0
	if a != 0 {
		off = 4096 - a
	}
	return b[off : off+n : off+n]
}

// HumanSize formats a byte count for people ("14.9 GB").
func HumanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d bytes", n)
	}
	f := float64(n)
	suffixes := []string{"KB", "MB", "GB", "TB"}
	i := -1
	for f >= unit && i < len(suffixes)-1 {
		f /= unit
		i++
	}
	if f >= 100 {
		return fmt.Sprintf("%.0f %s", f, suffixes[i])
	}
	return fmt.Sprintf("%.1f %s", f, suffixes[i])
}
