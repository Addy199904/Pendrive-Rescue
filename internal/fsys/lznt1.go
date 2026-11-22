package fsys

import (
	"encoding/binary"
	"errors"
)

// LZNT1 decompresses one NTFS compression unit.
func LZNT1(src []byte, out []byte) (int, error) {
	o := 0
	for p := 0; p+2 <= len(src); {
		h := binary.LittleEndian.Uint16(src[p:])
		if h == 0 {
			break
		}
		size := int(h&0x0FFF) + 1
		p += 2
		if p+size > len(src) {
			size = len(src) - p
		}
		chunk := src[p : p+size]
		p += size
		if h&0x8000 == 0 {
			// stored chunk
			n := copy(out[o:], chunk)
			o += n
			continue
		}
		start := o
		for i := 0; i < len(chunk); {
			flags := chunk[i]
			i++
			for bit := 0; bit < 8 && i < len(chunk); bit++ {
				if flags&(1<<uint(bit)) == 0 {
					if o >= len(out) {
						return o, nil
					}
					out[o] = chunk[i]
					o++
					i++
					continue
				}
				if i+2 > len(chunk) {
					return o, errors.New("truncated LZNT1 data")
				}
				tok := int(binary.LittleEndian.Uint16(chunk[i:]))
				i += 2
				pos := o - start
				if pos <= 0 {
					return o, errors.New("bad LZNT1 data")
				}
				l := 12
				for x := pos - 1; x >= 0x10; x >>= 1 {
					l--
				}
				length := (tok & (1<<uint(l) - 1)) + 3
				disp := (tok >> uint(l)) + 1
				if disp > pos {
					return o, errors.New("bad LZNT1 reference")
				}
				for k := 0; k < length; k++ {
					if o >= len(out) {
						return o, nil
					}
					out[o] = out[o-disp]
					o++
				}
			}
		}
	}
	return o, nil
}
