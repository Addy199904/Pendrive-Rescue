package engine

import (
	"fmt"
	"html"
	"strings"
	"time"

	"pendriverescue/internal/disk"
	"pendriverescue/internal/health"
)

// ShopFile is the name of the note for a repair shop in the results folder.
const ShopFile = "For a repair shop.html"

// ShopHTML writes a one-page note to take to a repair shop: what is wrong
// with the pendrive, what was already done, what a shop could still do, and
// what to ask before paying. h, st and sum may each be missing.
func ShopHTML(drive string, h *health.Report, st *Status, sum *Summary) string {
	e := html.EscapeString
	var b strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&b, f, a...) }
	b.WriteString(`<!DOCTYPE html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>About this pendrive - a note for a repair shop</title><style>
:root{color-scheme:light}
body{margin:0;background:#FAF9F5;color:#1F1E1C;font:16px/1.6 Georgia,"Times New Roman",serif}
main{max-width:680px;margin:0 auto;padding:48px 28px 64px}
h1{font-weight:500;font-size:28px;line-height:1.25;margin:0 0 6px}
h2{font:600 13px/1.4 system-ui,"Segoe UI",sans-serif;color:#6B6A63;margin:36px 0 10px}
.meta{font:14px/1.5 system-ui,"Segoe UI",sans-serif;color:#6B6A63;margin:0}
.verdict{font-size:21px;line-height:1.4;margin:28px 0 8px}
table{border-collapse:collapse;width:100%;font:14px/1.45 system-ui,"Segoe UI",sans-serif}
td{padding:7px 0;border-bottom:1px solid #E6E3D8;vertical-align:top}td:first-child{color:#6B6A63;width:46%;padding-right:16px}
ul{padding-left:20px}li{margin:6px 0}
.lab{border-left:3px solid #B4532F;padding:2px 0 2px 14px}
footer{font:13px/1.5 system-ui,"Segoe UI",sans-serif;color:#6B6A63;margin-top:40px}
@media print{body{background:#fff}main{padding:0}}
</style></head><body><main>`)
	w(`<h1>About this pendrive</h1><p class="meta">A note for a repair shop, about %s`, e(drive))
	if !strings.Contains(drive, "(") {
		if h != nil && h.Size > 0 {
			w(` (%s)`, disk.HumanSize(h.Size))
		} else if st != nil && st.DriveSize > 0 {
			w(` (%s)`, disk.HumanSize(st.DriveSize))
		}
	}
	w(`, %s</p>`, time.Now().Format("2 January 2006"))

	if h != nil {
		third := strings.NewReplacer("Your pendrive", "This pendrive", "I read", "Pendrive Rescue read", "I was checking", "it was being checked", "I tried", "Pendrive Rescue tried")
		w(`<p class="verdict">%s</p><p>%s</p>`, e(third.Replace(h.Title)), e(third.Replace(h.Detail)))
		b.WriteString(`<h2>What the check measured</h2><table>`)
		row := func(k, v string) { w(`<tr><td>%s</td><td>%s</td></tr>`, e(k), e(v)) }
		row("Places read three times each", fmt.Sprint(h.Spots+h.Unreadable))
		row("Places that read differently on different tries", fmt.Sprint(h.UnstableAt))
		row("Places that could not be read", fmt.Sprint(h.Unreadable))
		row("Places slower than a second", fmt.Sprint(h.Slow))
		if h.Speed > 0 {
			row("Reading speed", disk.HumanSize(int64(h.Speed))+" per second")
		}
		if h.RealSize > 0 {
			row("Real capacity (fake drive)", "about "+disk.HumanSize(h.RealSize))
		}
		for _, f := range h.FS {
			v := fmt.Sprintf("%d files, %d deleted", f.Files, f.Deleted)
			if f.Encrypted {
				v = "encrypted with BitLocker"
			}
			row("File system "+strings.TrimSpace(f.Type+" "+f.Label), v)
		}
		if h.FSChecked && len(h.FS) == 0 {
			row("File system", "none found (formatted or damaged)")
		}
		b.WriteString(`</table>`)
	}

	if st != nil && st.Phase != "" {
		b.WriteString(`<h2>What has already been done</h2><ul>`)
		switch {
		case st.Careful:
			w(`<li>The whole pendrive was copied to a computer reading every part twice. Where the two reads disagreed, that part was read again up to 61 times and every bit kept the value most reads agreed on (%s small parts settled this way`, Thousands(st.Settled))
			if st.Unsure > 0 {
				w(`, %s of them still uncertain`, Thousands(st.Unsure))
			}
			b.WriteString(`).</li>`)
		case st.ImagePath != "":
			b.WriteString(`<li>The whole pendrive was copied to a computer once, and the parts that could not be read were tried again.</li>`)
		default:
			b.WriteString(`<li>The pendrive was read once from start to end. Nothing was written to it.</li>`)
		}
		if st.BadBytes > 0 {
			w(`<li>%s could not be read at all.</li>`, disk.HumanSize(st.BadBytes))
		}
		if st.Unstable > 0 {
			w(`<li>%d more small parts of damaged files were read again until the reads agreed.</li>`, st.Unstable)
		}
		if st.Joined > 0 {
			w(`<li>%d photos stored in pieces were put back together.</li>`, st.Joined)
		}
		if st.Headers > 0 {
			w(`<li>%d photos with a damaged header were repaired using a good photo from the same camera.</li>`, st.Headers)
		}
		if sum != nil && sum.Files > 0 {
			var kinds []string
			names := map[string][2]string{"photos": {"photo", "photos"}, "videos": {"video", "videos"}, "music": {"music file", "music files"},
				"documents": {"document", "documents"}, "archives": {"archive", "archives"}, "text": {"text file", "text files"}, "other": {"other file", "other files"}}
			for _, k := range []string{"photos", "videos", "documents", "music", "archives", "text", "other"} {
				if g := sum.Groups[k]; g != nil && g.Files > 0 {
					kinds = append(kinds, fmt.Sprintf("%s %s", Thousands(g.Files), plural(g.Files, names[k][0], names[k][1])))
				}
			}
			w(`<li>Found: %s.`, e(strings.Join(kinds, ", ")))
			if p := sum.Groups["photos"]; p != nil {
				w(` Of the photos, %s open perfectly`, Thousands(p.Labels["perfect"]))
				if d := p.Labels["partly"] + p.Labels["preview"] + p.Labels["damaged"]; d > 0 {
					w(` and %s are damaged`, Thousands(d))
				}
				b.WriteString(`.`)
			}
			b.WriteString(`</li>`)
			if sum.Saved > 0 {
				w(`<li>%s files were saved to the computer.</li>`, Thousands(sum.Saved))
			}
		}
		b.WriteString(`</ul>`)
	}

	if h != nil && h.Shop != "" {
		cls := ""
		if h.Lab {
			cls = ` class="lab"`
		}
		w(`<h2>What a shop could still do</h2><p%s>%s</p>`, cls, e(h.Shop))
	}
	b.WriteString(`<h2>Worth asking before you pay</h2><ul>
<li>Will you work on a copy of the pendrive, not on the pendrive itself?</li>
<li>Is this a software recovery (the easy case) or does the memory chip have to be taken off and read directly (chip-off)?</li>
<li>Do you charge anything if no files come back?</li>
<li>Can I see the list of recovered files before I pay?</li>
</ul>
<footer>Written by Pendrive Rescue, a free program that only reads the pendrive. Nothing on it was changed.</footer>
</main></body></html>`)
	return b.String()
}

// ShopReport is the note for this recovery.
func (s *Session) ShopReport() string {
	st := s.Status()
	sum := s.Summary()
	return ShopHTML(s.opt.SourceDesc, s.opt.Health, &st, &sum)
}
