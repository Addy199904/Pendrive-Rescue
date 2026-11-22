package verify

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"testing"
)

type memFile []byte

func (m memFile) Size() int64 { return int64(len(m)) }
func (m memFile) ReadAt(p []byte, off int64) (int, error) {
	n := copy(p, m[min(off, int64(len(m))):])
	return n, nil
}

func boxOf(typ string, body []byte) []byte {
	b := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(b, uint32(8+len(body)))
	copy(b[4:], typ)
	return append(b, body...)
}

// heif builds a small HEIF file: ftyp, meta with an iloc table (version 1,
// 4-byte offsets and lengths) pointing at two items in mdat.
func heif(dataLen int, shiftEnd int) []byte {
	ftyp := boxOf("ftyp", append([]byte("heic"), 0, 0, 0, 0, 'm', 'i', 'f', '1', 'h', 'e', 'i', 'c'))
	mk := func(mdatAt int) []byte {
		il := []byte{1, 0, 0, 0, 0x44, 0x00, 0, 2}
		for i := 0; i < 2; i++ {
			e := make([]byte, 2+2+2+2+4+4)
			binary.BigEndian.PutUint16(e[0:], uint16(i+1))
			binary.BigEndian.PutUint16(e[2:], 0) // construction method 0: in the file
			binary.BigEndian.PutUint16(e[6:], 1) // one extent
			binary.BigEndian.PutUint32(e[8:], uint32(mdatAt+8+i*dataLen/2))
			binary.BigEndian.PutUint32(e[12:], uint32(dataLen/2+shiftEnd*i))
			il = append(il, e...)
		}
		meta := boxOf("meta", append([]byte{0, 0, 0, 0}, append(boxOf("hdlr", make([]byte, 24)), boxOf("iloc", il)...)...))
		return meta
	}
	meta := mk(0)
	at := len(ftyp) + len(meta)
	meta = mk(at)
	data := make([]byte, dataLen)
	rand.New(rand.NewSource(1)).Read(data)
	return bytes.Join([][]byte{ftyp, meta, boxOf("mdat", data)}, nil)
}

func TestHEIF(t *testing.T) {
	good := heif(4000, 0)
	if r := checkHEIF(memFile(good)); r.Cond != Good {
		t.Errorf("a good HEIF: %v %s", r.Cond, r.Note)
	}
	cut := good[:len(good)-1000]
	if r := checkHEIF(memFile(cut)); r.Cond != Damaged {
		t.Errorf("a HEIF missing its end: %v", r.Cond)
	}
	wiped := heif(4000, 0)
	for i := len(wiped) - 4000; i < len(wiped); i++ {
		wiped[i] = 0
	}
	if r := checkHEIF(memFile(wiped)); r.Cond != Damaged {
		t.Errorf("a wiped HEIF: %v", r.Cond)
	}
	if r := checkHEIF(memFile(heif(4000, 3000))); r.Cond != Damaged {
		t.Errorf("a HEIF whose table points past the end: %v", r.Cond)
	}
}
