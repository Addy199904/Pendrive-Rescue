package fsys

import (
	"fmt"
	"sort"
	"strings"

	"pendriverescue/internal/disk"
	"pendriverescue/internal/scan"
)

// Partition is an entry of the drive's partition table.
type Partition struct {
	Index int
	Start int64
	Size  int64
	Type  string
}

// Layout describes the partition table found on the drive.
type Layout struct {
	Scheme     string // "MBR", "GPT", "none"
	Partitions []Partition
}

func readLayout(r *disk.Reader) Layout {
	ss := int64(512)
	s0, _ := readAt(r, 0, 512)
	s1, _ := readAt(r, ss, 512)
	lay := Layout{Scheme: "none"}
	if string(s1[0:8]) == "EFI PART" {
		lay.Scheme = "GPT"
		entLBA := int64(le64(s1[72:80]))
		count := int(le32(s1[80:84]))
		esz := int(le32(s1[84:88]))
		if count > 256 {
			count = 256
		}
		if esz >= 128 && esz <= 1024 {
			tbl, _ := readAt(r, entLBA*ss, count*esz)
			for i := 0; i < count; i++ {
				e := tbl[i*esz : i*esz+esz]
				if isAllZero(e[0:16]) {
					continue
				}
				first := int64(le64(e[32:40]))
				last := int64(le64(e[40:48]))
				if last < first {
					continue
				}
				lay.Partitions = append(lay.Partitions, Partition{
					Index: len(lay.Partitions) + 1, Start: first * ss, Size: (last - first + 1) * ss,
					Type: decodeUTF16(e[56:128]),
				})
			}
		}
		return lay
	}
	if s0[510] != 0x55 || s0[511] != 0xAA {
		return lay
	}
	if bt := bootTypeOf(s0); bt != 0 {
		return lay // the drive starts directly with a file system ("superfloppy")
	}
	ok := true
	var parts []Partition
	for i := 0; i < 4; i++ {
		e := s0[446+i*16 : 446+i*16+16]
		if e[0] != 0 && e[0] != 0x80 {
			ok = false
			break
		}
		typ := e[4]
		start := int64(le32(e[8:12])) * ss
		n := int64(le32(e[12:16])) * ss
		if typ == 0 || n == 0 {
			continue
		}
		if typ == 0x05 || typ == 0x0F || typ == 0x85 {
			parts = append(parts, extendedParts(r, start)...)
			continue
		}
		parts = append(parts, Partition{Start: start, Size: n, Type: fmt.Sprintf("0x%02X", typ)})
	}
	if ok && len(parts) > 0 {
		lay.Scheme = "MBR"
		for i := range parts {
			parts[i].Index = i + 1
		}
		lay.Partitions = parts
	}
	return lay
}

func extendedParts(r *disk.Reader, extStart int64) []Partition {
	var out []Partition
	cur := extStart
	for i := 0; i < 64; i++ {
		s, _ := readAt(r, cur, 512)
		if s[510] != 0x55 || s[511] != 0xAA {
			break
		}
		e := s[446:462]
		if e[4] != 0 && le32(e[12:16]) != 0 {
			out = append(out, Partition{Start: cur + int64(le32(e[8:12]))*512, Size: int64(le32(e[12:16])) * 512, Type: fmt.Sprintf("0x%02X", e[4])})
		}
		n := s[462:478]
		if n[4] == 0 || le32(n[8:12]) == 0 {
			break
		}
		cur = extStart + int64(le32(n[8:12]))*512
	}
	return out
}

func bootTypeOf(s []byte) scan.BootType {
	// reuse the scanner's checks by building a tiny result
	switch string(s[3:11]) {
	case "EXFAT   ":
		return scan.BootExFAT
	case "NTFS    ":
		return scan.BootNTFS
	case "-FVE-FS-":
		return scan.BootBitLocker
	}
	if (s[0] == 0xEB || s[0] == 0xE9) && s[510] == 0x55 && s[511] == 0xAA {
		if _, ok := parseFATBoot(s); ok {
			bps := le16(s[11:13])
			if bps == 512 || bps == 1024 || bps == 2048 || bps == 4096 {
				return scan.BootFAT
			}
		}
	}
	return 0
}

type volCand struct {
	start    int64
	boot     []byte
	typ      scan.BootType
	part     int // partition number or 0
	backup   bool
	priority int
}

// FindVolumes finds and opens every readable file system on the drive and
// lists its files. hits are the scan's file-type hits (used to skip boot
// sectors that belong to disk images stored inside files).
func FindVolumes(r *disk.Reader, sr *scan.Result) ([]*Volume, Layout) {
	lay := readLayout(r)
	var cands []volCand
	add := func(c volCand) {
		if c.start < 0 || c.start >= r.Size() {
			return
		}
		cands = append(cands, c)
	}
	s0, _ := readAt(r, 0, 512)
	if t := bootTypeOf(s0); t != 0 {
		add(volCand{start: 0, boot: s0, typ: t, priority: 0})
	}
	for _, p := range lay.Partitions {
		b, _ := readAt(r, p.Start, 512)
		t := bootTypeOf(b)
		add(volCand{start: p.Start, boot: b, typ: t, part: p.Index, priority: 1})
	}
	if sr != nil {
		for _, bc := range sr.Boot {
			add(volCand{start: bc.Off, boot: bc.Data, typ: bc.Type, priority: 2})
			// the sector may be a backup copy: work out where its volume starts
			switch bc.Type {
			case scan.BootFAT:
				bps := int64(le16(bc.Data[11:13]))
				if bk := int64(le16(bc.Data[50:52])); le16(bc.Data[22:24]) == 0 && bk > 0 && bk < 32 {
					add(volCand{start: bc.Off - bk*bps, boot: bc.Data, typ: bc.Type, backup: true, priority: 3})
				}
			case scan.BootExFAT:
				bps := int64(1) << uint(bc.Data[108]&15)
				add(volCand{start: bc.Off - 12*bps, boot: bc.Data, typ: bc.Type, backup: true, priority: 3})
			case scan.BootNTFS:
				bps := int64(le16(bc.Data[11:13]))
				tot := int64(le64(bc.Data[40:48]))
				add(volCand{start: bc.Off - tot*bps, boot: bc.Data, typ: bc.Type, backup: true, priority: 3})
			}
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].priority < cands[j].priority })

	var vols []*Volume
	opened := map[int64]bool{}
	var liveFiles rangeSet
	for _, c := range cands {
		if opened[c.start] {
			continue
		}
		if c.priority >= 2 && liveFiles.contains(c.start) {
			continue // a disk image stored inside a file
		}
		if c.part > 0 && c.typ == 0 {
			continue
		}
		v := openVolume(r, c)
		if v == nil {
			continue
		}
		opened[c.start] = true
		idx := len(vols) + 1
		label := ""
		if v.Label != "" {
			label = ", " + v.Label
		}
		switch {
		case c.part > 0:
			v.Name = fmt.Sprintf("Partition %d (%s%s)", c.part, v.Type, label)
		case c.start == 0:
			v.Name = fmt.Sprintf("Drive (%s%s)", v.Type, label)
		default:
			v.Name = fmt.Sprintf("Volume %d at %s (%s%s)", idx, disk.HumanSize(c.start), v.Type, label)
			v.Notes = append(v.Notes, "this file system is not listed in the partition table (older or hidden file system)")
		}
		if c.backup {
			v.Notes = append(v.Notes, "the main boot sector was destroyed; opened using its backup copy")
		}
		for _, f := range v.Files {
			f.Volume = v.Name
			if f.Live {
				for _, e := range f.Extents {
					if e.Off >= 0 {
						liveFiles.add(e.Off, e.Len)
					}
				}
			}
		}
		liveFiles.finish()
		vols = append(vols, v)
	}
	return vols, lay
}

func openVolume(r *disk.Reader, c volCand) *Volume {
	defer func() { recover() }()
	switch c.typ {
	case scan.BootFAT:
		f, err := openFAT(r, c.start, c.boot)
		if err != nil || f.fat == nil || !fatLooksValid(f.fat, f.bits) {
			return nil
		}
		if f.g.dataStart >= r.Size() {
			return nil
		}
		f.walk()
		return f.vol
	case scan.BootExFAT:
		x, err := openExFAT(r, c.start, c.boot)
		if err != nil || len(x.fat) < 2 || x.fat[0] != 0xFFFFFFF8 {
			return nil
		}
		x.walk()
		return x.vol
	case scan.BootNTFS:
		n, err := openNTFS(r, c.start, c.boot)
		if err != nil {
			return nil
		}
		n.readMFT()
		if m0 := n.recs[0]; m0 == nil || m0.bestName() == nil || m0.bestName().name != "$MFT" {
			return nil
		}
		n.vol.Label = n.volumeLabel()
		n.walkRecords(false)
		return n.vol
	case scan.BootBitLocker:
		v := &Volume{Type: "BitLocker", Start: c.start, Encrypted: true}
		v.Notes = append(v.Notes, "this part of the pendrive is encrypted with BitLocker. Unlock it in Windows with your password or 48-digit recovery key - then the files are simply there. Without the key nobody can recover them.")
		return v
	}
	return nil
}

// volumeLabel reads the label from the $Volume record (number 3).
func (n *ntfsFS) volumeLabel() string {
	if len(n.mftEx) == 0 || n.mftEx[0].Len < 4*n.recSize {
		return ""
	}
	b, err := readAt(n.r, n.mftEx[0].Off+3*n.recSize, int(n.recSize))
	if err != nil {
		return ""
	}
	if string(b[0:4]) != "FILE" {
		return ""
	}
	rec := append([]byte(nil), b...)
	fixup(rec)
	p := int(le16(rec[0x14:]))
	for p+24 <= len(rec) {
		typ := le32(rec[p:])
		if typ == 0xFFFFFFFF {
			break
		}
		alen := int(le32(rec[p+4:]))
		if alen < 16 || p+alen > len(rec) {
			break
		}
		if typ == 0x60 && rec[p+8] == 0 {
			vl := int(le32(rec[p+16:]))
			vo := int(le16(rec[p+20:]))
			if p+vo+vl <= len(rec) {
				return strings.TrimSpace(decodeUTF16(rec[p+vo : p+vo+vl]))
			}
		}
		p += alen
	}
	return ""
}
