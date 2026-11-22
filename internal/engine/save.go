package engine

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"pendriverescue/internal/disk"
	"pendriverescue/internal/verify"
)

// row is one line of the file list.
type row struct {
	category, original, savedAs, status, cond string
	size                                      int64
	deleted                                   bool
	mtime                                     time.Time
}

type writer struct {
	root   string
	used   map[string]bool
	hashes map[string]string
	rows   []row
	full   bool
	buf    []byte
}

func newWriter(root string) *writer {
	return &writer{root: root, used: map[string]bool{}, hashes: map[string]string{}, buf: make([]byte, 1<<20)}
}

var winReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// cleanName makes a name safe on Windows and Linux.
func cleanName(s string, maxRunes int) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 0x20 || r == 0x7F || r == utf8.RuneError:
			b.WriteRune('_')
		case strings.ContainsRune(`<>:"/\|?*`, r):
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimLeft(strings.TrimRight(b.String(), " ."), " ")
	if out == "" {
		out = "_"
	}
	base := out
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	if winReserved[strings.ToUpper(base)] {
		out = "_" + out
	}
	if r := []rune(out); len(r) > maxRunes {
		ext := filepath.Ext(out)
		if len([]rune(ext)) > 10 {
			ext = ""
		}
		out = strings.TrimRight(string(r[:maxRunes-len([]rune(ext))]), " .") + ext
	}
	return out
}

// destDir builds the folder for an item, keeping the whole path short
// enough for Windows Explorer (260 characters).
func (w *writer) destDir(top string, sub []string) string {
	parts := []string{w.root, top}
	for _, d := range sub {
		parts = append(parts, cleanName(d, 80))
	}
	dir := filepath.Join(parts...)
	if len(dir) > 200 {
		parts = []string{w.root, top}
		for _, d := range sub {
			parts = append(parts, cleanName(d, 24))
		}
		dir = filepath.Join(parts...)
	}
	return dir
}

func (w *writer) uniquePath(dir, name string) string {
	room := 250 - len(dir)
	if room < 24 {
		room = 24
	}
	name = cleanName(name, min(100, room))
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	if base == "" {
		base, ext = name, ""
	}
	p := filepath.Join(dir, name)
	for i := 2; ; i++ {
		key := strings.ToLower(p)
		if !w.used[key] {
			if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
				w.used[key] = true
				return p
			}
		}
		p = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", base, i, ext))
	}
}

func (w *writer) rel(p string) string {
	if r, err := filepath.Rel(w.root, p); err == nil {
		return r
	}
	return p
}

func isDiskFull(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "no space") || strings.Contains(s, "disk is full") || strings.Contains(s, "not enough space")
}

func topFolder(cat string) string {
	switch cat {
	case "named":
		return DirNamed
	case "lost":
		return DirLost
	}
	return DirFound
}

// osFile lets the checks read a saved file (missing bytes read as zeros).
type osFile struct {
	f    *os.File
	size int64
}

func (o osFile) Size() int64 { return o.size }
func (o osFile) ReadAt(p []byte, off int64) (int, error) {
	n, err := o.f.ReadAt(p, off)
	if err == io.EOF || n < len(p) {
		clear(p[n:])
		return len(p), nil
	}
	return n, err
}

// writeTo copies an item into a new file and returns its SHA-1 and whether
// it holds only zeros.
func (s *Session) writeTo(path string, it *Item, alt bool, onBytes func(int64)) (string, bool, error) {
	ir, err := s.readerFor(it, alt)
	if err != nil {
		return "", false, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return "", false, err
	}
	h := sha1.New()
	zero := true
	buf := s.w.buf
	for p := int64(0); p < ir.size; {
		n := min(int64(len(buf)), ir.size-p)
		b := buf[:n]
		if _, err := ir.ReadAt(b, p); err != nil {
			f.Close()
			os.Remove(path)
			return "", false, err
		}
		if zero {
			for _, c := range b {
				if c != 0 {
					zero = false
					break
				}
			}
		}
		h.Write(b)
		if _, err := f.Write(b); err != nil {
			f.Close()
			os.Remove(path)
			return "", false, err
		}
		p += n
		if onBytes != nil {
			onBytes(n)
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", false, err
	}
	if !it.MTime.IsZero() {
		os.Chtimes(path, it.MTime, it.MTime)
	}
	return hex.EncodeToString(h.Sum(nil)) + fmt.Sprint(ir.size), zero && ir.size > 0, nil
}

func checkSaved(path, ext string) verify.Result {
	f, err := os.Open(path)
	if err != nil {
		return verify.Result{Cond: verify.Unknown}
	}
	defer f.Close()
	st, _ := f.Stat()
	return verify.Check(ext, osFile{f, st.Size()}, 64<<20)
}

// Recover saves the selected files in the background.
func (s *Session) Recover(sel Selection) error {
	s.mu.Lock()
	if s.st.Save.Running {
		s.mu.Unlock()
		return errors.New("already saving")
	}
	items := s.selectLocked(sel)
	var todo []*Item
	var total int64
	for _, it := range items {
		if it.Saved == "" {
			todo = append(todo, it)
			total += it.Size
		}
	}
	s.st.Save = SaveStatus{Running: true, Total: total, Files: len(todo)}
	s.mu.Unlock()
	go s.save(todo)
	return nil
}

func (s *Session) save(items []*Item) {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	var done int64
	onBytes := func(n int64) {
		done += n
		s.mu.Lock()
		s.st.Save.Done = done
		s.mu.Unlock()
	}
	for _, it := range items {
		if s.w.full {
			break
		}
		st, cond, savedAs := s.saveOne(it, onBytes)
		s.mu.Lock()
		sv := &s.st.Save
		sv.FilesDone++
		switch {
		case strings.HasPrefix(st, "duplicate"):
			sv.Dupes++
		case cond == "erased":
			sv.Erased++
		case savedAs == "":
			sv.Failed++
		case cond == "damaged":
			sv.Damaged++
			sv.Bytes += it.Size
		case cond == "unknown" || cond == "":
			sv.Unknown++
			sv.Bytes += it.Size
		default:
			sv.Good++
			sv.Bytes += it.Size
		}
		if strings.Contains(st, "rebuilt") {
			sv.Rebuilt++
		}
		sv.Full = s.w.full
		it.Saved = savedAs
		if it.Saved == "" {
			it.Saved = "-" // tried; see the file list
		}
		s.mu.Unlock()
	}
	s.mu.Lock()
	s.st.Save.Running = false
	s.mu.Unlock()
	rep := s.writeReports()
	disk.GiveToUser(s.opt.OutDir)
	s.mu.Lock()
	s.st.Save.Report = rep
	s.mu.Unlock()
}

// saveOne writes one item, checks it and files it under the right folder.
func (s *Session) saveOne(it *Item, onBytes func(int64)) (status, cond, savedAs string) {
	s.mu.Lock()
	top := topFolder(it.Cat)
	out := append([]string(nil), it.out...)
	name, ext, size := it.Name, it.Ext, it.Size
	known := it.Cond
	orig := strings.Join(it.path, " / ") + " / " + it.Name
	if it.Cat == "found" {
		orig = fmt.Sprintf("found at %s from the start of the drive", disk.HumanSize(it.Off))
	}
	alt := len(it.alt) > 0
	s.mu.Unlock()
	record := func(status, cond, saved string) {
		s.w.rows = append(s.w.rows, row{category: top, original: orig, savedAs: saved, status: status, cond: cond,
			size: size, deleted: it.Deleted, mtime: it.MTime})
	}
	if size < 0 || size > s.r.Size()+(1<<30) {
		record("not saved: impossible size (damaged entry)", "damaged", "")
		return "failed", "damaged", ""
	}
	if known == "damaged" && len(it.parts) == 0 {
		record("not saved: the location of its data is unknown", "damaged", "")
		return "failed", "damaged", ""
	}
	dir := s.w.destDir(top, out)
	if err := os.MkdirAll(dir, 0755); err != nil {
		record("not saved: "+err.Error(), "", "")
		return "failed", "", ""
	}
	path := s.w.uniquePath(dir, name)
	hash, zero, err := s.writeTo(path, it, false, onBytes)
	if err != nil {
		if isDiskFull(err) {
			s.w.full = true
			record("not saved: the output drive is full", "", "")
		} else {
			record("not saved: "+err.Error(), "", "")
		}
		return "failed", "", ""
	}
	if zero {
		os.Remove(path)
		s.setCond(it, "erased", "only zeros left - the content was wiped")
		record("not saved: its content was erased (only zeros left)", "erased", "")
		return "erased", "erased", ""
	}
	if prev, dup := s.w.hashes[hash]; dup {
		os.Remove(path)
		record("duplicate of "+prev, "", prev)
		return "duplicate", "", prev
	}
	res := verify.Result{Cond: verify.Cond(known)}
	note := ""
	if known == "" || known == "damaged" {
		res = checkSaved(path, ext)
		note = res.Note
	}
	status = "saved"
	if res.Cond == verify.Damaged && alt {
		// the deleted file may have been placed around clusters in use
		tmp := path + ".alt"
		if h2, z2, err := s.writeTo(tmp, it, true, nil); err == nil && !z2 {
			if r2 := checkSaved(tmp, ext); r2.Cond == verify.Good {
				os.Remove(path)
				os.Rename(tmp, path)
				hash, res, note = h2, r2, "rebuilt from scattered pieces"
				status = "saved, rebuilt from scattered pieces"
				s.mu.Lock()
				it.parts, it.alt = it.alt, nil
				s.mu.Unlock()
			} else {
				os.Remove(tmp)
			}
		} else {
			os.Remove(tmp)
		}
	}
	if res.Cond == verify.Damaged {
		// worn flash: read the wrong parts again; photos in pieces: join them
		if r2, how, h2, ok := s.mendSaved(it, path, ext, res); ok {
			res, hash = r2, h2
			if r2.Cond != verify.Damaged {
				note = how
				status = "saved, " + how
			} else {
				note = r2.Note
			}
			if prev, dup := s.w.hashes[hash]; dup {
				os.Remove(path)
				s.setCond(it, string(res.Cond), note)
				record("duplicate of "+prev, string(res.Cond), prev)
				return "duplicate", "", prev
			}
		}
	}
	if res.Cond == verify.Damaged && isJPEG(ext) && s.fixHeader(it) {
		// a few wrong bits in the header: rebuild it from a photo of the same camera
		tmp := path + ".hdr"
		if h2, _, err := s.writeTo(tmp, it, false, nil); err == nil {
			r2 := checkSaved(tmp, ext)
			os.Remove(path)
			os.Rename(tmp, path)
			hash, res = h2, r2
			note = headerNote(it)
			if r2.Cond == verify.Damaged {
				note += "; the picture itself is damaged further on"
			} else {
				status = "saved, " + note
			}
		} else {
			os.Remove(tmp)
		}
	}
	s.w.hashes[hash] = s.w.rel(path)
	cond = string(res.Cond)
	if res.Cond == verify.Damaged {
		why := note
		if why == "" {
			why = it.sum
		}
		ddir := s.w.destDir(DirDamaged, append([]string{top}, out...))
		if os.MkdirAll(ddir, 0755) == nil {
			np := s.w.uniquePath(ddir, filepath.Base(path))
			if os.Rename(path, np) == nil {
				delete(s.w.used, strings.ToLower(path))
				path = np
			}
		}
		status = "saved, possibly damaged: " + why
		if isJPEG(ext) {
			n, share, preview := salvage(path)
			if n > 0 {
				status += " (its intact part and preview were saved next to it)"
			}
			s.mu.Lock()
			it.Share, it.Preview = share, preview
			s.mu.Unlock()
		}
	} else if res.Cond == verify.Good && isJPEG(ext) {
		if f, err := os.Open(path); err == nil {
			head := make([]byte, 128<<10)
			n, _ := f.ReadAt(head, 0)
			f.Close()
			s.learnPhoto(head[:n], true)
		}
	}
	s.setCond(it, cond, note)
	savedAs = s.w.rel(path)
	record(status, cond, savedAs)
	return status, cond, savedAs
}

func (s *Session) setCond(it *Item, cond, note string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cond != "" {
		it.Cond = cond
	}
	if note != "" {
		it.Note = note
	}
}
