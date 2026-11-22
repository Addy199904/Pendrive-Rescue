// Command winres writes the Windows resources of Pendrive Rescue (manifest,
// icon and version information) as .syso object files that the Go linker
// puts into the .exe. It needs nothing outside the standard library.
//
//	go run ./tools/winres -version 3.0.0 -out .
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

const manifest = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<assembly xmlns="urn:schemas-microsoft-com:asm.v1" manifestVersion="1.0">
  <assemblyIdentity type="win32" name="PendriveRescue" version="%s" processorArchitecture="*"/>
  <description>Pendrive Rescue - free data recovery for pendrives</description>
  <trustInfo xmlns="urn:schemas-microsoft-com:asm.v3">
    <security>
      <requestedPrivileges>
        <requestedExecutionLevel level="requireAdministrator" uiAccess="false"/>
      </requestedPrivileges>
    </security>
  </trustInfo>
  <compatibility xmlns="urn:schemas-microsoft-com:compatibility.v1">
    <application>
      <supportedOS Id="{8e0f7a12-bfb3-4fe8-b9a5-48fd50a15a9a}"/>
      <supportedOS Id="{1f676c76-80e1-4239-95bb-83d0f6d0da78}"/>
      <supportedOS Id="{4a2f28e3-53b9-4441-ba9c-d69d4a4a6e38}"/>
      <supportedOS Id="{35138b9a-5d96-4fbd-8e2d-a2440225f93a}"/>
    </application>
  </compatibility>
  <application xmlns="urn:schemas-microsoft-com:asm.v3">
    <windowsSettings>
      <longPathAware xmlns="http://schemas.microsoft.com/SMI/2016/WindowsSettings">true</longPathAware>
    </windowsSettings>
  </application>
</assembly>
`

const (
	rtIcon      = 3
	rtGroupIcon = 14
	rtVersion   = 16
	rtManifest  = 24
	langEnUS    = 0x0409
)

type resource struct {
	typ, id uint32
	data    []byte
}

func main() {
	ver := flag.String("version", "3.0.0", "program version")
	out := flag.String("out", ".", "folder for the .syso files")
	icoOut := flag.String("ico", "", "also write the icon as an .ico file here")
	flag.Parse()

	v := parseVersion(*ver)
	vs := fmt.Sprintf("%d.%d.%d.%d", v[0], v[1], v[2], v[3])

	sizes := []int{256, 48, 32, 16}
	var pngs [][]byte
	for _, s := range sizes {
		var b bytes.Buffer
		if err := png.Encode(&b, drawIcon(s)); err != nil {
			log.Fatal(err)
		}
		pngs = append(pngs, b.Bytes())
	}

	var res []resource
	group := new(bytes.Buffer)
	binary.Write(group, binary.LittleEndian, [3]uint16{0, 1, uint16(len(sizes))})
	ico := new(bytes.Buffer)
	binary.Write(ico, binary.LittleEndian, [3]uint16{0, 1, uint16(len(sizes))})
	offset := 6 + 16*len(sizes)
	for i, s := range sizes {
		w := uint8(s)
		if s >= 256 {
			w = 0
		}
		res = append(res, resource{rtIcon, uint32(i + 1), pngs[i]})
		group.Write([]byte{w, w, 0, 0})
		binary.Write(group, binary.LittleEndian, uint16(1))
		binary.Write(group, binary.LittleEndian, uint16(32))
		binary.Write(group, binary.LittleEndian, uint32(len(pngs[i])))
		binary.Write(group, binary.LittleEndian, uint16(i+1))

		ico.Write([]byte{w, w, 0, 0})
		binary.Write(ico, binary.LittleEndian, uint16(1))
		binary.Write(ico, binary.LittleEndian, uint16(32))
		binary.Write(ico, binary.LittleEndian, uint32(len(pngs[i])))
		binary.Write(ico, binary.LittleEndian, uint32(offset))
		offset += len(pngs[i])
	}
	for _, p := range pngs {
		ico.Write(p)
	}
	res = append(res,
		resource{rtGroupIcon, 1, group.Bytes()},
		resource{rtVersion, 1, versionInfo(v, *ver)},
		resource{rtManifest, 1, []byte(fmt.Sprintf(manifest, vs))},
	)

	for _, a := range []struct {
		arch    string
		machine uint16
		reloc   uint16
	}{
		{"amd64", 0x8664, 3}, // IMAGE_REL_AMD64_ADDR32NB
		{"386", 0x14c, 7},    // IMAGE_REL_I386_DIR32NB
	} {
		obj := coff(res, a.machine, a.reloc)
		name := filepath.Join(*out, "rsrc_windows_"+a.arch+".syso")
		if err := os.WriteFile(name, obj, 0644); err != nil {
			log.Fatal(err)
		}
		fmt.Println("wrote", name, len(obj), "bytes")
	}
	if *icoOut != "" {
		if err := os.WriteFile(*icoOut, ico.Bytes(), 0644); err != nil {
			log.Fatal(err)
		}
	}
}

func parseVersion(s string) [4]uint16 {
	var v [4]uint16
	for i, p := range strings.SplitN(s, ".", 4) {
		n, _ := strconv.Atoi(p)
		v[i] = uint16(n)
	}
	return v
}

// ---------------------------------------------------------------- the icon

// drawIcon draws the app's mark - a grey plug and a clay board with light
// cells - at the given size, smoothing edges by drawing 4x4 samples per pixel.
func drawIcon(size int) *image.NRGBA {
	steel := color.NRGBA{0xCF, 0xCA, 0xC0, 0xFF}
	hole := color.NRGBA{0x5C, 0x57, 0x4E, 0xFF}
	board := color.NRGBA{0xB4, 0x53, 0x2F, 0xFF}
	gold := color.NRGBA{0xFA, 0xF3, 0xEC, 0xFF}
	pale := color.NRGBA{0xE2, 0xA6, 0x8C, 0xFF}

	type rect struct {
		x0, y0, x1, y1, r float64
		c                 color.NRGBA
	}
	// drawn on a 32 x 32 grid
	shapes := []rect{
		{1, 11, 10.5, 21, 1.6, steel},
		{3.4, 13.4, 6.6, 15.4, 0.3, hole},
		{3.4, 16.6, 6.6, 18.6, 0.3, hole},
		{9, 5, 31, 27, 5, board},
		{13, 9.5, 19, 15.5, 1.1, gold},
		{21, 9.5, 27, 15.5, 1.1, gold},
		{13, 17, 19, 23, 1.1, gold},
		{21, 17, 27, 23, 1.1, pale},
	}
	if size <= 16 { // fewer, bolder parts when tiny
		shapes = []rect{
			{0.5, 11, 10, 21, 1.5, steel},
			{9, 4, 31.5, 28, 5, board},
			{13, 8.5, 19.5, 15, 1.2, gold},
			{21, 8.5, 27.5, 15, 1.2, gold},
			{13, 17, 19.5, 23.5, 1.2, gold},
			{21, 17, 27.5, 23.5, 1.2, pale},
		}
	}
	inside := func(s rect, x, y float64) bool {
		if x < s.x0 || x > s.x1 || y < s.y0 || y > s.y1 {
			return false
		}
		cx := math.Max(s.x0+s.r, math.Min(x, s.x1-s.r))
		cy := math.Max(s.y0+s.r, math.Min(y, s.y1-s.r))
		return (x-cx)*(x-cx)+(y-cy)*(y-cy) <= s.r*s.r
	}
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	const ss = 4
	scale := 32.0 / float64(size)
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			var r, g, b, a float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x := (float64(px) + (float64(sx)+0.5)/ss) * scale
					y := (float64(py) + (float64(sy)+0.5)/ss) * scale
					for i := len(shapes) - 1; i >= 0; i-- {
						if inside(shapes[i], x, y) {
							c := shapes[i].c
							r += float64(c.R)
							g += float64(c.G)
							b += float64(c.B)
							a++
							break
						}
					}
				}
			}
			if a == 0 {
				continue
			}
			img.SetNRGBA(px, py, color.NRGBA{uint8(r / a), uint8(g / a), uint8(b / a), uint8(255 * a / (ss * ss))})
		}
	}
	return img
}

// ---------------------------------------------------------------- version information

type vnode struct {
	key      string
	binary   bool
	value    []byte // raw bytes (binary) or UTF-16 text with its 0
	valueLen int    // in bytes for binary, in characters for text
	kids     []*vnode
}

func utf16z(s string) []byte {
	u := utf16.Encode([]rune(s + "\x00"))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[2*i:], c)
	}
	return b
}

func pad4(b *bytes.Buffer) {
	for b.Len()%4 != 0 {
		b.WriteByte(0)
	}
}

func (n *vnode) bytes() []byte {
	var b bytes.Buffer
	b.Write(make([]byte, 6)) // length, value length, type
	b.Write(utf16z(n.key))
	pad4(&b)
	b.Write(n.value)
	for _, k := range n.kids {
		pad4(&b)
		b.Write(k.bytes())
	}
	out := b.Bytes()
	typ := uint16(1)
	if n.binary {
		typ = 0
	}
	binary.LittleEndian.PutUint16(out[0:], uint16(len(out)))
	binary.LittleEndian.PutUint16(out[2:], uint16(n.valueLen))
	binary.LittleEndian.PutUint16(out[4:], typ)
	return out
}

func versionInfo(v [4]uint16, text string) []byte {
	ms := uint32(v[0])<<16 | uint32(v[1])
	ls := uint32(v[2])<<16 | uint32(v[3])
	var fixed bytes.Buffer
	binary.Write(&fixed, binary.LittleEndian, []uint32{
		0xFEEF04BD, 0x00010000, ms, ls, ms, ls,
		0x3F, 0, // flags mask, flags
		0x00040004, 1, 0, // NT Windows32, application
		0, 0,
	})
	str := func(k, val string) *vnode {
		return &vnode{key: k, value: utf16z(val), valueLen: len(utf16.Encode([]rune(val))) + 1}
	}
	table := &vnode{key: "040904B0", kids: []*vnode{
		str("CompanyName", "Pendrive Rescue"),
		str("FileDescription", "Pendrive Rescue - free data recovery"),
		str("FileVersion", text),
		str("InternalName", "PendriveRescue"),
		str("LegalCopyright", "Free software"),
		str("OriginalFilename", "PendriveRescue.exe"),
		str("ProductName", "Pendrive Rescue"),
		str("ProductVersion", text),
	}}
	trans := make([]byte, 4)
	binary.LittleEndian.PutUint16(trans[0:], langEnUS)
	binary.LittleEndian.PutUint16(trans[2:], 1200)
	root := &vnode{key: "VS_VERSION_INFO", binary: true, value: fixed.Bytes(), valueLen: fixed.Len(), kids: []*vnode{
		{key: "StringFileInfo", kids: []*vnode{table}},
		{key: "VarFileInfo", kids: []*vnode{{key: "Translation", binary: true, value: trans, valueLen: 4}}},
	}}
	return root.bytes()
}

// ---------------------------------------------------------------- COFF object with a .rsrc section

// coff lays out the resource tree (type / id / language) and wraps it in a
// COFF object. Each data entry's address is relocated by the linker.
func coff(res []resource, machine, relocType uint16) []byte {
	sort.Slice(res, func(i, j int) bool {
		if res[i].typ != res[j].typ {
			return res[i].typ < res[j].typ
		}
		return res[i].id < res[j].id
	})
	var types []uint32
	byType := map[uint32][]resource{}
	for _, r := range res {
		if len(byType[r.typ]) == 0 {
			types = append(types, r.typ)
		}
		byType[r.typ] = append(byType[r.typ], r)
	}
	dirSize := func(n int) int { return 16 + 8*n }

	// sizes of the three directory levels, then the data entries and data
	off := dirSize(len(types))
	typeDirOff := map[uint32]int{}
	for _, t := range types {
		typeDirOff[t] = off
		off += dirSize(len(byType[t]))
	}
	langDirOff := make([]int, len(res))
	for i := range res {
		langDirOff[i] = off
		off += dirSize(1)
	}
	entryOff := make([]int, len(res))
	for i := range res {
		entryOff[i] = off
		off += 16
	}
	dataOff := make([]int, len(res))
	for i, r := range res {
		off = (off + 7) &^ 7
		dataOff[i] = off
		off += len(r.data)
	}
	sec := make([]byte, (off+3)&^3)
	le := binary.LittleEndian
	dir := func(at, n int) {
		le.PutUint16(sec[at+14:], uint16(n)) // all entries are numbered
	}
	entry := func(at int, id uint32, target int, sub bool) {
		le.PutUint32(sec[at:], id)
		v := uint32(target)
		if sub {
			v |= 0x80000000
		}
		le.PutUint32(sec[at+4:], v)
	}
	dir(0, len(types))
	idx := 0
	for ti, t := range types {
		entry(16+8*ti, t, typeDirOff[t], true)
		dir(typeDirOff[t], len(byType[t]))
		for ri, r := range byType[t] {
			entry(typeDirOff[t]+16+8*ri, r.id, langDirOff[idx], true)
			dir(langDirOff[idx], 1)
			entry(langDirOff[idx]+16, langEnUS, entryOff[idx], false)
			le.PutUint32(sec[entryOff[idx]:], uint32(dataOff[idx])) // relocated
			le.PutUint32(sec[entryOff[idx]+4:], uint32(len(r.data)))
			copy(sec[dataOff[idx]:], r.data)
			idx++
		}
	}

	const fileHdr, secHdr = 20, 40
	rawOff := fileHdr + secHdr
	relocOff := rawOff + len(sec)
	symOff := relocOff + 10*len(res)
	var b bytes.Buffer
	binary.Write(&b, le, machine)
	binary.Write(&b, le, uint16(1))          // sections
	binary.Write(&b, le, uint32(0))          // time stamp
	binary.Write(&b, le, uint32(symOff))     // symbol table
	binary.Write(&b, le, uint32(1))          // symbols
	binary.Write(&b, le, uint16(0))          // optional header
	binary.Write(&b, le, uint16(0))          // characteristics
	b.WriteString(".rsrc\x00\x00\x00")       // name
	binary.Write(&b, le, uint32(0))          // virtual size
	binary.Write(&b, le, uint32(0))          // virtual address
	binary.Write(&b, le, uint32(len(sec)))   // raw size
	binary.Write(&b, le, uint32(rawOff))     // raw data
	binary.Write(&b, le, uint32(relocOff))   // relocations
	binary.Write(&b, le, uint32(0))          // line numbers
	binary.Write(&b, le, uint16(len(res)))   // relocation count
	binary.Write(&b, le, uint16(0))          // line number count
	binary.Write(&b, le, uint32(0x40000040)) // initialised data, readable
	b.Write(sec)
	for i := range res {
		binary.Write(&b, le, uint32(entryOff[i])) // where
		binary.Write(&b, le, uint32(0))           // symbol 0: the section
		binary.Write(&b, le, relocType)
	}
	b.WriteString(".rsrc\x00\x00\x00")
	binary.Write(&b, le, uint32(0)) // value
	binary.Write(&b, le, uint16(1)) // section number
	binary.Write(&b, le, uint16(0)) // type
	b.WriteByte(3)                  // static
	b.WriteByte(0)                  // aux symbols
	binary.Write(&b, le, uint32(4)) // empty string table
	return b.Bytes()
}
