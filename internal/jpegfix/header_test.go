package jpegfix

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"testing"
)

func encode(t *testing.T, q int, seed int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 320, 240))
	for y := 0; y < 240; y++ {
		for x := 0; x < 320; x++ {
			img.Set(x, y, color.RGBA{uint8(x*seed + y), uint8(y * 2), uint8((x + y + seed) % 256), 255})
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: q}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func readable(b []byte) bool {
	info, err := Parse(bytes.NewReader(b), int64(len(b)))
	return err == nil && Check(info, bytes.NewReader(b), int64(len(b))).OK
}

func TestRepairHeader(t *testing.T) {
	set := NewSampleSet()
	set.Add(encode(t, 85, 3), false)
	set.Add(encode(t, 85, 5), false) // the same tables twice: known
	photo := encode(t, 85, 7)
	a, z, ok := tablesOf(photo)
	if !ok {
		t.Fatal("no tables found")
	}
	broken := append([]byte(nil), photo...)
	broken[a+3] ^= 0x20 // the first table's length
	broken[a+40] ^= 0x04
	broken[z-5] ^= 0x01
	if readable(broken) {
		t.Fatal("the damage did not break the photo")
	}
	fix := RepairHeader(broken, set)
	if fix == nil {
		t.Fatal("not repaired")
	}
	fixed := append(append([]byte(nil), fix.Head...), broken[fix.From:]...)
	if !readable(fixed) || !bytes.Equal(fixed, photo) {
		t.Errorf("the repaired photo is not the original (bits fixed: %d)", fix.Bits)
	}

	// tables from another camera (another quality) must not be used
	other := NewSampleSet()
	other.Add(encode(t, 60, 3), true)
	if RepairHeader(broken, other) != nil {
		t.Error("repaired from tables that do not belong to the photo")
	}
	// a fine photo is left alone
	if RepairHeader(photo, set) != nil {
		t.Error("a fine photo was changed")
	}
}

func TestRepairAppLength(t *testing.T) {
	b, err := os.ReadFile("testdata/photo_plain.jpg")
	if err != nil {
		t.Skip("test photo missing")
	}
	set := NewSampleSet()
	set.Add(b, true)
	broken := append([]byte(nil), b...)
	broken[4] ^= 0x02 // the length of the first APP segment
	if readable(broken) {
		t.Fatal("the damage did not break the photo")
	}
	fix := RepairHeader(broken, set)
	if fix == nil || fix.Dropped {
		t.Fatalf("not repaired, or the EXIF block was dropped: %+v", fix)
	}
	fixed := append(append([]byte(nil), fix.Head...), broken[fix.From:]...)
	if !bytes.Equal(fixed, b) {
		t.Error("the repaired photo is not the original")
	}
}
