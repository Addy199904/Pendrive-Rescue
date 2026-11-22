package jpegfix

import (
	"bytes"
	"image/jpeg"
	"math/rand"
	"testing"
	"time"
)

// worn makes a copy with random wrong bits in a quarter of the pages.
func worn(b []byte, seed int64, q float64) []byte {
	out := append([]byte(nil), b...)
	rng := rand.New(rand.NewSource(seed))
	for pg := 4096; pg < len(out); pg += 4096 {
		if (int64(pg/4096)*2654435761)%100 >= 28 {
			continue
		}
		for i := pg; i < pg+4096 && i < len(out); i++ {
			if rng.Float64() < q {
				out[i] ^= 1 << rng.Intn(8)
			}
		}
	}
	return out
}

func TestCombineThreeCopies(t *testing.T) {
	orig := load(t, "photo_plain.jpg")
	copies := [][]byte{worn(orig, 1, 0.03), worn(orig, 2, 0.03), worn(orig, 3, 0.03)}
	out, res, err := Combine(copies, time.Now().Add(time.Minute))
	wrong, before := 0, 0
	for i := range out {
		if out[i] != orig[i] {
			wrong++
		}
		if copies[0][i] != orig[i] {
			before++
		}
	}
	t.Logf("three copies: %v, %d wrong bytes left (each copy had about %d)", res, wrong, before)
	// voting fixes almost everything; bits that two of the three copies
	// got wrong the same way are left (the drive itself can be read again)
	if err != nil || wrong > before/20 {
		t.Fatalf("not combined well: %v %v", err, res)
	}
}

func TestCombineTwoCopies(t *testing.T) {
	orig := load(t, "photo_plain.jpg")
	copies := [][]byte{worn(orig, 4, 0.01), worn(orig, 5, 0.01)}
	t0 := time.Now()
	out, res, err := Combine(copies, time.Now().Add(2*time.Minute))
	wrong := 0
	for i := range out {
		if out[i] != orig[i] {
			wrong++
		}
	}
	before := 0
	for i := range orig {
		if copies[0][i] != orig[i] {
			before++
		}
	}
	t.Logf("two copies: %v, %d wrong bytes left (copy had %d), took %v", res, wrong, before, time.Since(t0).Round(time.Millisecond))
	if err != nil || wrong >= before {
		t.Fatalf("no better than a single copy: %v %v", err, res)
	}
	if res.OK {
		if _, err := jpeg.Decode(bytes.NewReader(out)); err != nil {
			t.Fatalf("Go cannot open it: %v", err)
		}
	}
}
