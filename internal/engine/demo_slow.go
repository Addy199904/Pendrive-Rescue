//go:build demoslow

package engine

// A slow drive with an unreadable patch, for trying out the window. Only in
// builds made with -tags demoslow: a source written as "slow:<image file>"
// is read at about 40 MB/s and fails in a small area.

import (
	"errors"
	"strings"
	"time"

	"pendriverescue/internal/disk"
)

func init() {
	openSource = func(p string) (disk.Source, error) {
		if !strings.HasPrefix(p, "slow:") {
			return disk.Open(p)
		}
		s, err := disk.OpenImage(strings.TrimPrefix(p, "slow:"))
		if err != nil {
			return nil, err
		}
		bad := s.Size() * 37 / 100
		return &slowSource{Source: s, rate: 40 << 20, badFrom: bad, badTo: bad + 512<<10}, nil
	}
}

type slowSource struct {
	disk.Source
	rate           int64
	badFrom, badTo int64
}

func (s *slowSource) ReadAt(p []byte, off int64) (int, error) {
	time.Sleep(time.Duration(int64(len(p)) * int64(time.Second) / s.rate))
	if off < s.badTo && off+int64(len(p)) > s.badFrom {
		return 0, errors.New("I/O error")
	}
	return s.Source.ReadAt(p, off)
}
