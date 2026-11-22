//go:build demosim

package engine

// Simulated failing pendrives, for trying out the app. Only in builds made
// with -tags demosim: a source written as "sim:<kind>:<image file>" is the
// image behaving like a pendrive of that kind (see sim.Preset).

import (
	"strings"

	"pendriverescue/internal/disk"
	"pendriverescue/internal/sim"
)

func init() {
	open, dev := openSource, isDevice
	openSource = func(p string) (disk.Source, error) {
		kind, path, ok := splitSim(p)
		if !ok {
			return open(p)
		}
		img, err := disk.OpenImage(path)
		if err != nil {
			return nil, err
		}
		opt, _ := sim.Preset(kind, img.Size())
		if kind == "failing" {
			opt.Bad = append(opt.Bad, photoTrouble(img)...)
		}
		return sim.New(img, opt), nil
	}
	isDevice = func(p string) bool {
		if _, _, ok := splitSim(p); ok {
			return true
		}
		return dev(p)
	}
}

// photoTrouble puts small unreadable areas inside a few photos of the
// image, so the demo shows partly damaged photos.
func photoTrouble(img disk.Source) [][2]int64 {
	var starts []int64
	b := make([]byte, 4)
	for off := int64(0); off < min(img.Size(), 128<<20) && len(starts) < 40; off += 4096 {
		img.ReadAt(b, off)
		if b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF {
			starts = append(starts, off)
		}
	}
	var out [][2]int64
	for i, frac := range map[int]float64{3: 0.55, 8: 0.2, 14: 0.04} {
		if i+1 < len(starts) {
			l := starts[i+1] - starts[i]
			at := (starts[i] + int64(float64(l)*frac)) &^ 511
			out = append(out, [2]int64{at, at + 32<<10})
		}
	}
	return out
}

func splitSim(p string) (kind, path string, ok bool) {
	if !strings.HasPrefix(p, "sim:") {
		return "", "", false
	}
	rest := strings.TrimPrefix(p, "sim:")
	i := strings.IndexByte(rest, ':')
	if i < 0 {
		return "", "", false
	}
	return rest[:i], rest[i+1:], true
}
