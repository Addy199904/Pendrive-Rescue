package engine

import (
	"encoding/csv"
	"errors"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"pendriverescue/internal/disk"
	"pendriverescue/internal/scan"
)

func pct(a, b int64) float64 {
	if b == 0 {
		return 0
	}
	return 100 * float64(a) / float64(b)
}

// Thousands formats 1234567 as 1,234,567.
func Thousands(n int) string {
	s := fmt.Sprint(n)
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}

// diagnoseLocked explains, in plain words, what happened to the pendrive.
func (s *Session) diagnoseLocked() []Diag {
	sr := s.sr
	var d []Diag
	if sr == nil {
		return d
	}
	sectors := sr.Data + sr.Zero + sr.FF + sr.Bad
	found := len(s.items)
	interrupted := sr.Interrupted != nil && !errors.Is(sr.Interrupted, scan.ErrStopped)
	if interrupted {
		d = append(d, Diag{"bad", "The pendrive stopped responding",
			fmt.Sprintf("It stopped answering at %.0f%% of the reading. This usually means a hardware fault. Everything read up to then was used. "+
				"Unplug it, try another USB port or cable, and run Pendrive Rescue again with the safe copy: it continues where it stopped.",
				pct(sr.ScannedTo, s.st.DriveSize))})
	}
	if errors.Is(sr.Interrupted, scan.ErrStopped) {
		d = append(d, Diag{"info", "Reading was stopped early",
			fmt.Sprintf("Only %.0f%% of the pendrive was read, so files in the rest were not found. Run again to read it all (the safe copy continues where it stopped).",
				pct(sr.ScannedTo, s.st.DriveSize))})
	}
	if n := s.st.Unstable + s.st.Settled; n > 0 {
		text := fmt.Sprintf("In %s it handed back a few wrong bits without reporting an error. One wrong bit turns the rest of a photo grey. "+
			"Pendrive Rescue read those places again until the reads agreed. Copy everything you need off this pendrive and stop using it for anything important.",
			count(n, "small part", "small parts"))
		if s.st.Unsure > 0 {
			text += fmt.Sprintf(" In %s the reads still disagreed after many tries; files there may keep a few wrong bits.", count(s.st.Unsure, "part", "parts"))
		}
		d = append(d, Diag{"warn", "The pendrive's memory is wearing out", text})
	}
	if s.st.Headers > 0 {
		d = append(d, Diag{"good", fmt.Sprintf("%s rebuilt", count(s.st.Headers, "photo header was", "photo headers were")),
			"A few wrong bits in the first part of these photos made them unreadable. That part holds tables every photo from the same camera shares, so it was rebuilt from a good photo taken with the same camera."})
	}
	if s.st.Joined > 0 {
		d = append(d, Diag{"good", fmt.Sprintf("%s rebuilt from pieces", count(s.st.Joined, "photo was", "photos were")),
			"These photos were stored in pieces. The missing pieces were found elsewhere on the pendrive and joined back on; each one was checked."})
	}
	if bad := s.r.BadSectors(); bad > 0 {
		d = append(d, Diag{"bad", fmt.Sprintf("%s could not be read", disk.HumanSize(bad*512)),
			"Those areas of the pendrive are physically damaged. Files stored there are damaged or missing. No software can read damaged flash memory; " +
				"only a professional data recovery lab (\"chip-off\" recovery) might."})
	}
	for _, v := range s.vols {
		if v.Encrypted {
			d = append(d, Diag{"warn", "Part of the pendrive is encrypted with BitLocker",
				"Plug it into a Windows PC: Windows will ask for the password or the 48-digit recovery key (often saved in your Microsoft account at " +
					"account.microsoft.com/devices/recoverykey). After unlocking, the files are simply there. Without the password or key nobody can recover them."})
			break
		}
	}
	if s.fakeReal > 0 {
		d = append(d, Diag{"bad", "This pendrive is fake",
			fmt.Sprintf("It says it holds %s, but it really stores only about %s - everything after that repeats the beginning. "+
				"When more than %s was copied onto it, newer data silently overwrote older files. That data cannot be recovered by anyone. "+
				"Do not use this pendrive for important files.",
				disk.HumanSize(s.st.DriveSize), disk.HumanSize(s.fakeReal), disk.HumanSize(s.fakeReal))})
	}
	dataPct := pct(sr.Data, sectors)
	switch {
	case dataPct < 0.5 && found == 0:
		d = append(d, Diag{"bad", "The pendrive has been wiped",
			fmt.Sprintf("%.1f%% of it holds only zeros or blank values. This happens after a FULL format (not a quick one), after \"clean\" in Disk Management, "+
				"after formatting in a phone, camera or TV, or when the pendrive wipes itself. The old data was physically overwritten, so no software and no shop "+
				"can bring it back - please don't pay anyone who promises otherwise without checking first.", pct(sr.Zero+sr.FF, sectors))})
	case found == 0 && dataPct >= 5:
		d = append(d, Diag{"warn", "Data, but nothing recognisable",
			"The pendrive holds data, but no file system and no known file types were found. It may be encrypted (VeraCrypt or a pendrive's own security app) " +
				"or hold file types this program does not know."})
	}
	hidden, deleted := 0, 0
	for _, v := range s.vols {
		hidden += v.Hidden
		deleted += v.Deleted
	}
	if hidden > 0 {
		d = append(d, Diag{"info", fmt.Sprintf("%s hidden", count(hidden, "file was", "files were")),
			"Files hidden like this are often the work of the \"shortcut virus\". They are listed normally and can be recovered. Scan the pendrive with an antivirus before using it again."})
	}
	if deleted > 0 {
		d = append(d, Diag{"good", fmt.Sprintf("%s still %s", count(deleted, "deleted file", "deleted files"), plural(deleted, "has its name", "have their names")),
			"They are among the files found, with their names and folders, and are saved into \"" + DirNamed + "\"."})
	}
	lostN := 0
	for _, v := range s.lost {
		lostN += len(v.Files)
	}
	if lostN > 0 {
		d = append(d, Diag{"info", "The pendrive was formatted",
			fmt.Sprintf("%s found in folders that the format cut off. The names of the top-level folders are usually lost, so they are called "+
				"\"Lost folder 1\", \"Lost folder 2\" and so on, with what is inside them. Files and sub-folders inside keep their real names.", count(lostN, "file was", "files were"))})
	}
	carvedN, damaged, erased := 0, 0, 0
	for _, it := range s.items {
		if it.Cat == "found" {
			carvedN++
		}
		switch it.Cond {
		case "damaged":
			damaged++
		case "erased":
			erased++
		}
	}
	if carvedN > 0 {
		d = append(d, Diag{"info", fmt.Sprintf("The deep scan found %s by content", count(carvedN, "file", "files")),
			"Their original names are not stored in the files, so they are named by type and date, for example \"Photo 2023-05-12 10.15.33.jpg\". Some may be older copies of files deleted long ago."})
	}
	if damaged > 0 {
		d = append(d, Diag{"warn", fmt.Sprintf("%s damaged", count(damaged, "file is", "files are")),
			"They were partly overwritten or are incomplete. They are saved into the folder \"" + DirDamaged + "\"; many still open partly, such as half a photo."})
	}
	if erased > 0 {
		d = append(d, Diag{"warn", fmt.Sprintf("%s wiped", count(erased, "file was", "files were")),
			"They are still listed, but only zeros are left where their content was. There is nothing of them to recover."})
	}
	if found > 0 && !interrupted && s.r.BadSectors() == 0 && s.fakeReal == 0 {
		d = append(d, Diag{"good", "The pendrive was read without errors", ""})
	}
	return d
}

// writeReports writes REPORT.html, REPORT.txt and the file list.
func (s *Session) writeReports() string {
	s.mu.Lock()
	s.st.Diagnosis = s.diagnoseLocked()
	st := s.st
	rows := append([]row(nil), s.w.rows...)
	sr := s.sr
	s.mu.Unlock()
	out := s.opt.OutDir

	// file list
	if f, err := os.Create(filepath.Join(out, "File list.csv")); err == nil {
		f.Write([]byte{0xEF, 0xBB, 0xBF})
		cw := csv.NewWriter(f)
		cw.UseCRLF = true
		cw.Write([]string{"Result", "Condition", "Category", "Original location", "Saved as", "Size (bytes)", "Deleted", "Last changed"})
		for _, r := range rows {
			del, mt := "", ""
			if r.deleted {
				del = "yes"
			}
			if !r.mtime.IsZero() {
				mt = r.mtime.Format("2006-01-02 15:04")
			}
			cw.Write([]string{r.status, r.cond, r.category, r.original, r.savedAs, fmt.Sprint(r.size), del, mt})
		}
		cw.Flush()
		f.Close()
	}

	sv := st.Save
	// text report
	var b strings.Builder
	p := func(f string, a ...any) { fmt.Fprintf(&b, f+"\r\n", a...) }
	p("PENDRIVE RESCUE - RECOVERY REPORT")
	p("=================================")
	p("Date:      %s", time.Now().Format("2006-01-02 15:04"))
	p("Drive:     %s", st.Drive)
	p("Size:      %s", disk.HumanSize(st.DriveSize))
	p("")
	p("SAVED")
	p("  Good files (checked):        %s", Thousands(sv.Good))
	p("  Saved, could not be checked: %s", Thousands(sv.Unknown))
	p("  Possibly damaged:            %s  (folder \"%s\")", Thousands(sv.Damaged), DirDamaged)
	p("  Rebuilt from pieces:         %s", Thousands(sv.Rebuilt))
	p("  Duplicates left out:         %s", Thousands(sv.Dupes))
	p("  Wiped (nothing to save):     %s", Thousands(sv.Erased))
	p("  Could not be saved:          %s", Thousands(sv.Failed))
	p("  Total saved:                 %s", disk.HumanSize(sv.Bytes))
	p("")
	p("WHAT PENDRIVE RESCUE FOUND OUT")
	for _, d := range st.Diagnosis {
		p("  * %s", d.Title)
		for _, l := range wrap(d.Text, 70) {
			p("    %s", l)
		}
	}
	for _, n := range st.Notes {
		p("  * %s", n)
	}
	p("")
	if sr != nil {
		sectors := sr.Data + sr.Zero + sr.FF + sr.Bad
		p("THE PENDRIVE FROM START (left) TO END (right)")
		p("  # = data   + = a little data   . = empty   F = blank (FF)   X = unreadable")
		chars := " .+#XF"
		var line strings.Builder
		for i, v := range st.Map {
			if i == 50 {
				p("  [%s]", line.String())
				line.Reset()
			}
			line.WriteByte(chars[v])
		}
		p("  [%s]", line.String())
		p("  Data %.1f%%, empty %.1f%%, blank %.1f%%, unreadable %s", pct(sr.Data, sectors), pct(sr.Zero, sectors), pct(sr.FF, sectors), disk.HumanSize(s.r.BadSectors()*512))
		p("")
	}
	p("FILE SYSTEMS")
	for _, v := range st.Volumes {
		if v.Lost {
			p("  * %s: %s files", v.Name, Thousands(v.Files))
		} else {
			p("  * %s: %s files (%s hidden), %s deleted", v.Name, Thousands(v.Files), Thousands(v.Hidden), Thousands(v.Deleted))
		}
	}
	p("")
	p("The full list of files is in \"File list.csv\". The pendrive was only read; nothing on it was changed.")
	os.WriteFile(filepath.Join(out, "REPORT.txt"), []byte(b.String()), 0644)

	// HTML report, and the note for a repair shop
	os.WriteFile(filepath.Join(out, "REPORT.html"), []byte(s.htmlReport(st, sr)), 0644)
	os.WriteFile(filepath.Join(out, ShopFile), []byte(s.ShopReport()), 0644)
	return filepath.Join(out, "REPORT.html")
}

func wrap(s string, width int) []string {
	var lines []string
	var cur string
	for _, w := range strings.Fields(s) {
		if cur == "" {
			cur = w
		} else if len(cur)+1+len(w) <= width {
			cur += " " + w
		} else {
			lines = append(lines, cur)
			cur = w
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}

func (s *Session) htmlReport(st Status, sr *scan.Result) string {
	e := html.EscapeString
	var b strings.Builder
	sv := st.Save
	b.WriteString(`<!DOCTYPE html><html lang="en"><head><meta charset="utf-8"><title>Pendrive Rescue report</title><style>
body{font:15px/1.5 "Segoe UI",system-ui,sans-serif;color:#1d2433;background:#f6f7f9;margin:0}
main{max-width:860px;margin:0 auto;padding:32px 24px 64px}
h1{font-size:26px;margin:0 0 4px}h2{font-size:17px;margin:32px 0 10px}
.sub{color:#5b6475}.tiles{display:grid;grid-template-columns:repeat(4,1fr);gap:12px;margin-top:20px}
.tile{background:#fff;border:1px solid #e3e6eb;border-radius:10px;padding:14px}
.tile b{display:block;font-size:24px}.tile span{color:#5b6475;font-size:13px}
.d{background:#fff;border:1px solid #e3e6eb;border-left:4px solid #8a93a6;border-radius:8px;padding:12px 14px;margin:8px 0}
.d.bad{border-left-color:#c2410c}.d.warn{border-left-color:#b7791f}.d.good{border-left-color:#2f855a}.d.info{border-left-color:#2b6cb0}
.d b{display:block}.map{display:grid;grid-template-columns:repeat(50,1fr);gap:2px;margin:8px 0}
.map i{height:14px;border-radius:2px;background:#e3e6eb}.c1{background:#e9edf3!important}.c2{background:#9ec5f8!important}
.c3{background:#2b6cb0!important}.c4{background:#c2410c!important}.c5{background:#cbd2dc!important}
table{border-collapse:collapse;width:100%;background:#fff}td,th{border-bottom:1px solid #e3e6eb;padding:8px;text-align:left;font-size:14px}
.legend span{margin-right:14px;font-size:13px;color:#5b6475}.legend i{display:inline-block;width:10px;height:10px;border-radius:2px;margin-right:4px;vertical-align:-1px}
</style></head><body><main>`)
	fmt.Fprintf(&b, `<h1>Pendrive Rescue report</h1><div class="sub">%s &middot; %s &middot; %s</div>`,
		e(st.Drive), disk.HumanSize(st.DriveSize), time.Now().Format("2 Jan 2006, 15:04"))
	fmt.Fprintf(&b, `<div class="tiles"><div class="tile"><b>%s</b><span>good files saved</span></div><div class="tile"><b>%s</b><span>saved, not checkable</span></div><div class="tile"><b>%s</b><span>possibly damaged</span></div><div class="tile"><b>%s</b><span>saved in total</span></div></div>`,
		Thousands(sv.Good), Thousands(sv.Unknown), Thousands(sv.Damaged), disk.HumanSize(sv.Bytes))
	b.WriteString(`<h2>What Pendrive Rescue found out</h2>`)
	for _, d := range st.Diagnosis {
		fmt.Fprintf(&b, `<div class="d %s"><b>%s</b>%s</div>`, e(d.Level), e(d.Title), e(d.Text))
	}
	for _, n := range st.Notes {
		fmt.Fprintf(&b, `<div class="d info">%s</div>`, e(n))
	}
	if sr != nil {
		b.WriteString(`<h2>The pendrive from start to end</h2><div class="map">`)
		for _, v := range st.Map {
			fmt.Fprintf(&b, `<i class="c%d"></i>`, v)
		}
		b.WriteString(`</div><div class="legend"><span><i class="c3"></i>data</span><span><i class="c2"></i>a little data</span><span><i class="c1"></i>empty</span><span><i class="c5"></i>blank</span><span><i class="c4"></i>unreadable</span></div>`)
	}
	b.WriteString(`<h2>File systems</h2><table><tr><th>Name</th><th>Files</th><th>Hidden</th><th>Deleted</th></tr>`)
	vols := append([]VolInfo(nil), st.Volumes...)
	sort.SliceStable(vols, func(i, j int) bool { return !vols[i].Lost && vols[j].Lost })
	for _, v := range vols {
		fmt.Fprintf(&b, `<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>`, e(v.Name), Thousands(v.Files), Thousands(v.Hidden), Thousands(v.Deleted))
	}
	b.WriteString(`</table><h2>Where your files are</h2><table>`)
	for _, r := range [][2]string{{DirNamed, "files with their original names and folders (existing, hidden and deleted)"},
		{DirLost, "folders cut off by a format"}, {DirFound, "files found by their content, sorted by type and date"},
		{DirDamaged, "files that are partly overwritten, unreadable or incomplete"}, {"File list.csv", "every file found, with its result (open with Excel)"}} {
		fmt.Fprintf(&b, `<tr><td>%s</td><td>%s</td></tr>`, e(r[0]), e(r[1]))
	}
	b.WriteString(`</table><p class="sub">The pendrive was only read; nothing on it was changed.</p></main></body></html>`)
	return b.String()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// count writes "1 file" / "12 files".
func count(n int, one, many string) string {
	return Thousands(n) + " " + plural(n, one, many)
}
