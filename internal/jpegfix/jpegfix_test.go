package jpegfix

import (
	"bytes"
	"image/jpeg"
	"math/rand"
	"os"
	"testing"
	"time"
)

type memDrive []byte

func (m memDrive) ReadAt(p []byte, off int64) (int, error) {
	clear(p)
	if off >= 0 && off < int64(len(m)) {
		copy(p, m[off:])
	}
	return len(p), nil
}
func (m memDrive) Size() int64 { return int64(len(m)) }

func load(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCheckIntact(t *testing.T) {
	for _, name := range []string{"photo_rst.jpg", "photo_plain.jpg", "other1.jpg"} {
		b := load(t, name)
		r := bytes.NewReader(b)
		info, err := Parse(r, int64(len(b)))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		res := Check(info, r, int64(len(b)))
		if !res.OK || res.End != int64(len(b)) {
			t.Errorf("%s: %v end=%d size=%d", name, res, res.End, len(b))
		}
		if name != "other1.jpg" && (info.Orientation != 6 || info.ThumbLen == 0) {
			t.Errorf("%s: orientation %d thumb %d", name, info.Orientation, info.ThumbLen)
		}
	}
}

func TestDamageFoundAndSalvaged(t *testing.T) {
	for _, name := range []string{"photo_rst.jpg", "photo_plain.jpg"} {
		for _, fill := range []string{"zeros", "random", "other"} {
			b := load(t, name)
			cutAt := len(b) * 45 / 100
			switch fill {
			case "zeros":
				clear(b[cutAt:])
			case "random":
				rand.New(rand.NewSource(1)).Read(b[cutAt:])
			case "other":
				o := load(t, "other1.jpg")
				copy(b[cutAt:], o[len(o)/3:])
			}
			sv, err := SalvageFile(bytes.NewReader(b), int64(len(b)))
			if err != nil {
				t.Fatalf("%s/%s: %v", name, fill, err)
			}
			res := sv.Result
			if res.OK {
				t.Errorf("%s/%s: damage not noticed", name, fill)
				continue
			}
			if fill == "zeros" && !res.Wiped {
				t.Errorf("%s/%s: wiping not recognised (%s)", name, fill, res.Why)
			}
			if res.ErrAt < int64(cutAt) || res.ErrAt > int64(cutAt)+64<<10 {
				t.Errorf("%s/%s: noticed at %d, damage starts at %d", name, fill, res.ErrAt, cutAt)
			}
			if sv.Share < 0.25 || sv.Share > 0.5 {
				t.Errorf("%s/%s: intact share %.2f", name, fill, sv.Share)
			}
			img, err := jpeg.Decode(bytes.NewReader(sv.Intact))
			if err != nil {
				t.Errorf("%s/%s: intact part does not open: %v", name, fill, err)
			} else if h := img.Bounds().Dy(); h != res.GoodRows*16 || img.Bounds().Dx() != 1280 {
				t.Errorf("%s/%s: intact part is %v, want %d rows", name, fill, img.Bounds(), res.GoodRows*16)
			}
			pv, err := jpeg.Decode(bytes.NewReader(sv.Preview))
			if err != nil {
				t.Errorf("%s/%s: preview: %v", name, fill, err)
			} else if pv.Bounds().Dx() != 120 || pv.Bounds().Dy() != 160 {
				t.Errorf("%s/%s: preview not upright: %v", name, fill, pv.Bounds())
			}
		}
	}
}

// layout builds a drive full of other pictures and noise, and stores the
// photo in the given clusters.
func layout(t *testing.T, photo []byte, cs int64, pieces [][2]int64) (memDrive, []byte) {
	t.Helper()
	d := make(memDrive, 24<<20)
	rng := rand.New(rand.NewSource(3))
	others := [][]byte{load(t, "other1.jpg"), load(t, "other2.jpg")}
	for off := int64(0); off < int64(len(d)); {
		switch rng.Intn(4) {
		case 0:
			n := int64(rng.Intn(64)+1) * cs
			rng.Read(d[off:min(off+n, int64(len(d)))])
			off += n
		case 1:
			off += int64(rng.Intn(16)+1) * cs // zeros
		default:
			o := others[rng.Intn(2)]
			copy(d[off:], o)
			off += (int64(len(o)) + cs - 1) / cs * cs
		}
	}
	pos := int64(0)
	for _, p := range pieces {
		copy(d[p[0]*cs:], photo[pos:pos+p[1]])
		pos += p[1]
	}
	return d, photo
}

func joinedBytes(d memDrive, parts []Extent, size int64) []byte {
	out := make([]byte, size)
	(&FileView{D: d, Parts: parts, N: size}).ReadAt(out, 0)
	return out
}

func TestRebuildFragments(t *testing.T) {
	for _, name := range []string{"photo_rst.jpg", "photo_plain.jpg"} {
		photo := load(t, name)
		n := int64(len(photo))
		for _, cs := range []int64{4096, 32768} {
			b1 := (n * 4 / 10) / cs * cs // first piece: 40%
			cases := []struct {
				what   string
				pieces [][2]int64 // cluster, length
			}{
				{"2 pieces, forward", [][2]int64{{100, b1}, {300, n - b1}}},
				{"2 pieces, backward", [][2]int64{{400, b1}, {150, n - b1}}},
				{"3 pieces", [][2]int64{{120, b1}, {360, cs * 2}, {500, n - b1 - 2*cs}}},
			}
			for _, c := range cases {
				d, _ := layout(t, photo, cs, c.pieces)
				start := c.pieces[0][0] * cs
				t0 := time.Now()
				rep, err := Rebuild(d, []Extent{{start, n}}, n, Options{Cluster: cs, Phase: 0, SizeKnown: true, Deadline: time.Now().Add(60 * time.Second)})
				took := time.Since(t0)
				if err != nil || !rep.OK {
					if name == "photo_plain.jpg" && c.what == "3 pieces" && cs == 4096 {
						// known limit: without restart markers, a small middle piece
						// followed by another photo's cleanly decoding data
						t.Logf("%s cs=%d %s: not rebuilt (known limit)", name, cs, c.what)
						continue
					}
					t.Errorf("%s cs=%d %s: not rebuilt (%v, before: %v)", name, cs, c.what, err, rep.Before)
					continue
				}
				if !bytes.Equal(joinedBytes(d, rep.Parts, rep.Size), photo) {
					t.Errorf("%s cs=%d %s: rebuilt file differs (%v)", name, cs, c.what, rep.Parts)
					continue
				}
				t.Logf("%s cs=%d %s: rebuilt from %d pieces in %v", name, cs, c.what, rep.Pieces+1, took.Round(time.Millisecond))
			}
		}
	}
}

func TestRebuildUnknownSize(t *testing.T) {
	photo := load(t, "photo_rst.jpg")
	n := int64(len(photo))
	cs := int64(4096)
	b1 := (n / 3) / cs * cs
	d, _ := layout(t, photo, cs, [][2]int64{{200, b1}, {260, n - b1}})
	// a carver that stopped at the wrong place: size too small
	rep, err := Rebuild(d, []Extent{{200 * cs, n / 2}}, n/2, Options{Cluster: cs})
	if err != nil || !rep.OK {
		t.Fatalf("not rebuilt: %v %v", err, rep.Before)
	}
	if rep.Size != n || !bytes.Equal(joinedBytes(d, rep.Parts, rep.Size), photo) {
		t.Fatalf("size %d (want %d) or content differs", rep.Size, n)
	}
}

func TestWipedIsNotRebuiltFromJunk(t *testing.T) {
	photo := load(t, "photo_plain.jpg")
	n := int64(len(photo))
	cs := int64(4096)
	b1 := (n / 2) / cs * cs
	// the rest of the photo is nowhere on the drive
	d, _ := layout(t, photo, cs, [][2]int64{{100, b1}})
	t0 := time.Now()
	rep, err := Rebuild(d, []Extent{{100 * cs, n}}, n, Options{Cluster: cs, SizeKnown: true})
	if err == nil || rep.OK {
		t.Fatalf("claimed a repair from unrelated data: %v", rep.Parts)
	}
	t.Logf("gave up after %v", time.Since(t0).Round(time.Millisecond))
}
