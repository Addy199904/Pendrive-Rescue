package carve

import (
	"bytes"
	"encoding/binary"

	"pendriverescue/internal/sig"
)

// ---------------------------------------------------------------- MIDI

func (c *carver) midi(res *Result, limit int64) bool {
	h := c.read(res.Off, 14)
	tracks := int(binary.BigEndian.Uint16(h[10:12]))
	pos := res.Off + 14
	n := 0
	for ; n < tracks && pos+8 <= limit; n++ {
		t := c.read(pos, 8)
		if string(t[:4]) != "MTrk" {
			break
		}
		pos += 8 + int64(binary.BigEndian.Uint32(t[4:8]))
	}
	if n == 0 || pos > limit {
		return false
	}
	res.Size = pos - res.Off
	res.Complete = n == tracks
	return true
}

// ---------------------------------------------------------------- AIFF / DjVu (IFF forms)

func (c *carver) aiff(res *Result, limit int64) bool {
	size := int64(c.be32(res.Off+4)) + 8
	size += size & 1
	if size < 12 || res.Off+size > limit {
		return false
	}
	res.Size, res.Complete = size, true
	return true
}

func (c *carver) djvu(res *Result, limit int64) bool {
	size := 12 + int64(c.be32(res.Off+8))
	if size < 16 || res.Off+size > limit {
		return false
	}
	res.Size, res.Complete = size, true
	return true
}

// ---------------------------------------------------------------- Outlook PST

func (c *carver) pst(res *Result, limit int64) bool {
	h := c.readNew(res.Off, 0xC0)
	ver := binary.LittleEndian.Uint16(h[10:12])
	var size int64
	switch {
	case ver >= 23: // Unicode PST
		size = int64(binary.LittleEndian.Uint64(h[0xB8:0xC0]))
	case ver == 14 || ver == 15: // ANSI PST
		size = int64(binary.LittleEndian.Uint32(h[0xA8:0xAC]))
	default:
		return false
	}
	if size < 0x4400 || res.Off+size > limit {
		return false
	}
	res.Size, res.Complete = size, true
	return true
}

// ---------------------------------------------------------------- formats without a length

// estimated keeps everything up to the next file, minus trailing empty
// sectors (AutoCAD DWG, KeePass): such files still open with extra bytes.
func (c *carver) estimated(res *Result, limit int64) bool {
	end := c.nextHit(res.Off)
	if end > limit {
		end = limit
	}
	// trim trailing all-zero sectors
	for end-512 > res.Off+4096 {
		if !isZeroBytes(c.read(end-512, 512)) {
			break
		}
		end -= 512
	}
	if end-res.Off < 1024 {
		return false
	}
	res.Size = end - res.Off
	res.Estimated = true
	res.Complete = true
	return true
}

// ---------------------------------------------------------------- AAC (ADTS)

func (c *carver) aac(res *Result, limit int64) bool {
	pos := res.Off
	frames := 0
	for pos+7 <= limit {
		fl := sig.ADTSFrameLen(c.read(pos, 7))
		if fl < 7 {
			break
		}
		pos += int64(fl)
		frames++
	}
	if frames < 20 {
		return false
	}
	if pos > limit {
		pos = limit
	}
	res.Size, res.Complete = pos-res.Off, true
	return true
}

// ---------------------------------------------------------------- EPS / PostScript

func (c *carver) eps(res *Result, limit int64) bool {
	stop := c.nextHit(res.Off)
	if stop > limit {
		stop = limit
	}
	eofs := c.pdfEOFs(res.Off+11, stop)
	if len(eofs) > 0 {
		at := eofs[len(eofs)-1]
		res.Size = at + 5 + eolLen(c.read(at+5, 2)) - res.Off
		res.Complete = true
	} else {
		if stop-res.Off < 512 {
			return false
		}
		res.Size = stop - res.Off
		res.Estimated = true
	}
	head := c.readNew(res.Off, 512)
	if !bytes.Contains(head, []byte("EPSF")) {
		res.Ext = "ps"
	}
	return true
}

func (c *carver) epsbin(res *Result, limit int64) bool {
	h := c.readNew(res.Off, 30)
	var end int64
	for i := 4; i+8 <= 28; i += 8 {
		off := int64(binary.LittleEndian.Uint32(h[i:]))
		n := int64(binary.LittleEndian.Uint32(h[i+4:]))
		if n > 0 && off+n > end {
			end = off + n
		}
	}
	if end < 30 || res.Off+end > limit {
		return false
	}
	res.Size, res.Complete = end, true
	return true
}

// ---------------------------------------------------------------- RealMedia (RM / RMVB)

var rmChunks = map[string]bool{".RMF": true, "PROP": true, "MDPR": true, "CONT": true, "DATA": true, "INDX": true, "RMMD": true, "RJMD": true}

func (c *carver) realmedia(res *Result, limit int64) bool {
	pos := res.Off
	chunks := 0
	for pos+10 <= limit {
		h := c.read(pos, 8)
		id := string(h[:4])
		size := int64(binary.BigEndian.Uint32(h[4:8]))
		if !rmChunks[id] || size < 8 {
			break
		}
		pos += size
		chunks++
	}
	if chunks < 3 {
		return false
	}
	if pos > limit {
		pos = limit
	}
	res.Size, res.Complete = pos-res.Off, true
	return true
}

// ---------------------------------------------------------------- Windows cabinet

func (c *carver) cab(res *Result, limit int64) bool {
	size := int64(c.le32(res.Off + 8))
	if size < 44 || res.Off+size > limit {
		return false
	}
	res.Size, res.Complete = size, true
	return true
}
