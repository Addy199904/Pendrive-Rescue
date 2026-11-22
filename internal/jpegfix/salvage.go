package jpegfix

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/jpeg"
	"io"
)

// Salvage is what can be saved from a damaged photo.
type Salvage struct {
	Info    *Info
	Result  Result
	Intact  []byte  // a smaller, valid JPEG with only the intact rows (nil if none)
	Share   float64 // part of the picture that is intact (0..1)
	Preview []byte  // the small preview picture stored inside the photo, upright (nil if none)
}

// SalvageFile works out what is intact in a JPEG.
func SalvageFile(r io.ReaderAt, size int64) (*Salvage, error) {
	info, err := Parse(r, size)
	if err != nil && info == nil {
		return nil, err
	}
	sv := &Salvage{Info: info}
	if info.ThumbLen > 0 {
		sv.Preview = preview(r, info)
	}
	if err != nil { // progressive: only the preview
		return sv, nil
	}
	sv.Result = Check(info, r, size)
	if sv.Result.OK {
		sv.Share = 1
		return sv, nil
	}
	sv.Share = float64(sv.Result.MCUs) / float64(max(sv.Result.Total, 1))
	sv.Intact = intact(r, info, sv.Result)
	return sv, nil
}

// intact cuts the photo after the last intact row of blocks and makes the
// picture that much shorter, so any viewer shows just the good part.
func intact(r io.ReaderAt, info *Info, res Result) []byte {
	c := res.rowCut
	if res.GoodRows < 1 || !c.ok || c.off <= info.ScanStart {
		return nil
	}
	maxV := 1
	for _, cp := range info.comps {
		maxV = max(maxV, cp.v)
	}
	h := res.GoodRows * 8 * maxV
	if h >= info.Height {
		return nil
	}
	out := make([]byte, c.off, c.off+4)
	if _, err := r.ReadAt(out, 0); err != nil {
		return nil
	}
	if c.used > 0 {
		b := make([]byte, 1)
		r.ReadAt(b, c.off)
		last := b[0] | byte(0xFF>>c.used) // keep the used bits, pad with ones
		out = append(out, last)
		if last == 0xFF {
			out = append(out, 0x00)
		}
	}
	out = append(out, 0xFF, 0xD9)
	if info.SOFAt+7 <= int64(len(out)) {
		binary.BigEndian.PutUint16(out[info.SOFAt+5:], uint16(h))
	}
	if _, err := jpeg.DecodeConfig(bytes.NewReader(out)); err != nil {
		return nil
	}
	return out
}

// preview returns the small picture that cameras and phones store inside
// the photo, turned upright.
func preview(r io.ReaderAt, info *Info) []byte {
	if info.ThumbLen > 1<<20 {
		return nil
	}
	b := make([]byte, info.ThumbLen)
	if _, err := r.ReadAt(b, info.ThumbOff); err != nil || len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return nil
	}
	img, err := jpeg.Decode(bytes.NewReader(b))
	if err != nil {
		return nil
	}
	if info.Orientation <= 1 || info.Orientation > 8 {
		return b
	}
	var out bytes.Buffer
	if jpeg.Encode(&out, Upright(img, info.Orientation), &jpeg.Options{Quality: 92}) != nil {
		return b
	}
	return out.Bytes()
}

// Upright turns a picture according to its EXIF orientation.
func Upright(img image.Image, orientation int) image.Image {
	bd := img.Bounds()
	w, h := bd.Dx(), bd.Dy()
	ow, oh := w, h
	if orientation >= 5 {
		ow, oh = h, w
	}
	out := image.NewRGBA(image.Rect(0, 0, ow, oh))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var nx, ny int
			switch orientation {
			case 2:
				nx, ny = w-1-x, y
			case 3:
				nx, ny = w-1-x, h-1-y
			case 4:
				nx, ny = x, h-1-y
			case 5:
				nx, ny = y, x
			case 6:
				nx, ny = h-1-y, x
			case 7:
				nx, ny = h-1-y, w-1-x
			case 8:
				nx, ny = y, w-1-x
			default:
				nx, ny = x, y
			}
			out.Set(nx, ny, img.At(bd.Min.X+x, bd.Min.Y+y))
		}
	}
	return out
}
