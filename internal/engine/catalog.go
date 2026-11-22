package engine

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"pendriverescue/internal/carve"
	"pendriverescue/internal/fsys"
	"pendriverescue/internal/jpegfix"
	"pendriverescue/internal/scan"
	"pendriverescue/internal/sig"
)

// Item is one recoverable file in the catalog.
type Item struct {
	ID      int       `json:"id"`
	Cat     string    `json:"cat"`   // named, lost, found
	Group   string    `json:"group"` // photos, videos, music, documents, archives, text, other
	Vol     string    `json:"vol,omitempty"`
	Dir     []string  `json:"dir"`
	Name    string    `json:"name"`
	Ext     string    `json:"ext"`
	Size    int64     `json:"size"`
	MTime   time.Time `json:"mtime"`
	Deleted bool      `json:"deleted"`
	Hidden  bool      `json:"hidden"`
	Live    bool      `json:"live"`
	Cond    string    `json:"cond"` // "" (not checked yet), good, damaged, unknown, erased
	Note    string    `json:"note,omitempty"`
	Saved   string    `json:"saved,omitempty"`
	Off     int64     `json:"off"`
	Share   float64   `json:"share,omitempty"`   // damaged photos: part of the picture that is intact
	Preview bool      `json:"preview,omitempty"` // damaged photos: the small preview inside is intact
	Label   string    `json:"label"`             // perfect, partly, preview, damaged, unknown, wiped, empty, unchecked

	key   string
	path  []string // where it shows in the tree
	out   []string // sub folders in the output
	parts []fsys.Extent
	alt   []fsys.Extent
	data  []byte
	file  *fsys.File
	wide  bool
	sum   string             // why it may be damaged (from the file system)
	hdr   *jpegfix.HeaderFix // photos: a rebuilt start
}

// Tree labels.
const (
	topNamed = "Files with names"
	topLost  = "Lost folders"
	topFound = "Found by content"
)

var groupLabel = map[string]string{
	"photos": "Photos", "videos": "Videos", "music": "Music", "documents": "Documents",
	"archives": "Archives", "text": "Text files", "other": "Other",
}

func groupOfExt(ext string) string {
	switch strings.ToLower(ext) {
	case "jpg", "jpeg", "png", "gif", "bmp", "tif", "tiff", "heic", "heif", "avif", "webp", "cr2", "cr3", "nef",
		"arw", "dng", "orf", "rw2", "pef", "srw", "psd", "cdr", "eps", "ps", "svg", "ico", "djvu":
		return "photos"
	case "mp4", "mov", "3gp", "3g2", "m4v", "avi", "mkv", "webm", "wmv", "flv", "mpg", "mpeg", "vob", "ts", "mts",
		"m2ts", "ogv", "rm", "rmvb":
		return "videos"
	case "mp3", "wav", "ogg", "opus", "flac", "m4a", "aac", "amr", "awb", "wma", "oga", "mid", "midi", "aiff", "aif", "qcp":
		return "music"
	case "pdf", "doc", "docx", "xls", "xlsx", "ppt", "pptx", "odt", "ods", "odp", "rtf", "epub", "xps", "vsdx", "vsd",
		"msg", "pst", "dwg", "csv":
		return "documents"
	case "zip", "rar", "7z", "gz", "tgz", "tar", "iso", "apk", "jar", "cab":
		return "archives"
	case "txt", "html", "htm", "xml", "json", "vcf", "ics", "eml", "md", "log", "ini", "c", "py", "java", "js", "sh", "php":
		return "text"
	}
	return "other"
}

func extOf(name string) string {
	i := strings.LastIndexByte(name, '.')
	if i < 0 || i == len(name)-1 {
		return ""
	}
	return strings.ToLower(name[i+1:])
}

// itemFor returns the catalog entry for key, creating it if needed.
func (s *Session) itemFor(key string) (*Item, bool) {
	if id, ok := s.ids[key]; ok {
		return s.items[id-1], false
	}
	it := &Item{ID: len(s.items) + 1, key: key, Off: -1}
	s.items = append(s.items, it)
	s.ids[key] = it.ID
	return it, true
}

// rebuildCatalogLocked turns volumes and deep-scan results into items. IDs
// stay the same for the same file, so the user's selection survives.
func (s *Session) rebuildCatalogLocked() {
	named := 0
	for _, v := range s.vols {
		if len(v.Files) > 0 {
			named++
		}
	}
	lostVols := 0
	for _, v := range s.lost {
		if len(v.Files) > 0 {
			lostVols++
		}
	}
	addFS := func(v *fsys.Volume, cat string, multi bool) {
		for _, f := range v.Files {
			start := f.Start()
			key := fmt.Sprintf("%s|%d|%s|%s|%d", cat, v.Start, strings.Join(f.Dir, "/"), f.Name, start)
			it, _ := s.itemFor(key)
			it.Cat, it.Vol, it.Dir, it.Name = cat, v.Name, f.Dir, f.Name
			it.Ext = extOf(f.Name)
			it.Group = groupOfExt(it.Ext)
			it.Size, it.MTime, it.Deleted, it.Hidden, it.Live = f.Size, f.MTime, f.Deleted, f.Hidden, f.Live
			it.parts, it.alt, it.data = f.Extents, f.AltExtents, f.Data
			it.Off = start
			if f.Compressed {
				it.file = f
			}
			if it.Cond == "" && f.Damaged != "" {
				it.sum = f.Damaged
				if f.Damaged == "location of the data is unknown" {
					it.Cond = "damaged"
				}
			}
			if f.Note != "" {
				it.Note = f.Note
			}
			top := topNamed
			if cat == "lost" {
				top = topLost
			}
			it.out = append([]string{}, f.Dir...)
			if multi {
				it.out = append([]string{v.Name}, it.out...)
			}
			if cat == "named" {
				it.path = append([]string{top, v.Name}, f.Dir...)
			} else {
				it.path = append([]string{top}, it.out...)
			}
		}
	}
	for _, v := range s.vols {
		addFS(v, "named", named > 1)
	}
	for _, v := range s.lost {
		addFS(v, "lost", lostVols > 1)
	}
	seq := map[string]int{}
	for _, res := range s.carved {
		it, _ := s.itemFor(fmt.Sprintf("found|%d", res.Off))
		seq[res.Ext]++
		it.Cat = "found"
		it.Ext = res.Ext
		it.Group = groupOfExt(res.Ext)
		it.Name = carvedName(res, seq[res.Ext])
		it.Size, it.MTime, it.Off = res.Size, res.Time, res.Off
		it.parts = []fsys.Extent{{Off: res.Off, Len: res.Size}}
		if !res.Complete && it.Cond == "" {
			it.Cond = "damaged"
			it.Note = "the end of the file was not found"
		}
		if res.Estimated {
			it.Note = "size estimated - the file may have extra bytes at the end"
		}
		it.out = foundFolders(it)
		it.path = append([]string{topFound}, it.out...)
	}
	s.updateCountsLocked()
}

func foundFolders(it *Item) []string {
	out := []string{groupLabel[it.Group], strings.ToUpper(it.Ext)}
	if (it.Group == "photos" || it.Group == "videos") && !it.MTime.IsZero() {
		out = append(out, it.MTime.Format("2006-01"))
	}
	return out
}

func carvedName(res carve.Result, n int) string {
	prefix := map[string]string{"photos": "Photo", "videos": "Video", "music": "Audio", "documents": "Document", "archives": "Archive"}[groupOfExt(res.Ext)]
	if prefix == "" {
		prefix = "File"
	}
	if res.Title != "" {
		t := res.Title
		if strings.HasSuffix(strings.ToLower(t), "."+res.Ext) {
			t = t[:len(t)-len(res.Ext)-1]
		}
		return t + "." + res.Ext
	}
	if !res.Time.IsZero() {
		return fmt.Sprintf("%s %s.%s", prefix, res.Time.Format("2006-01-02 15.04.05"), res.Ext)
	}
	return fmt.Sprintf("%s %05d.%s", prefix, n, res.Ext)
}

func (s *Session) updateCountsLocked() {
	counts := map[string]int{}
	files := 0
	var bytes int64
	for _, it := range s.items {
		counts[it.Group]++
		files++
		bytes += it.Size
	}
	s.st.Counts = counts
	s.st.Files = files
	s.st.FilesBytes = bytes
	var vi []VolInfo
	for _, v := range s.vols {
		vi = append(vi, VolInfo{Name: v.Name, Type: v.Type, Files: v.Live, Hidden: v.Hidden, Deleted: v.Deleted, Notes: v.Notes})
	}
	for _, v := range s.lost {
		vi = append(vi, VolInfo{Name: v.Name, Type: v.Type, Files: len(v.Files), Lost: true})
	}
	s.st.Volumes = vi
	s.st.Catalog++
}

// skipFunc tells the deep scan which hits belong to files already found
// with their names, so they are not listed twice.
func (s *Session) skipFunc() func(int64, sig.Kind) bool {
	s.mu.Lock()
	items := append([]*Item(nil), s.items...)
	r := s.r
	s.mu.Unlock()
	starts := map[int64]bool{}
	type rng struct {
		off, end int64
		ext      string
		live     bool
	}
	var rs []rng
	head := make([]byte, 1024)
	for _, it := range items {
		if it.Cat == "found" || len(it.parts) == 0 || it.parts[0].Off < 0 || it.Cond == "damaged" {
			continue
		}
		good := it.Live
		if !good {
			r.ReadAt(head, it.parts[0].Off)
			m, known := sig.ExtMatches(it.Ext, head)
			good = m || !known
		}
		if !good {
			continue
		}
		starts[it.parts[0].Off] = true
		for _, p := range it.parts {
			if p.Off >= 0 {
				rs = append(rs, rng{p.Off, p.Off + p.Len, it.Ext, it.Live})
			}
		}
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].off < rs[j].off })
	maxEnd := make([]int64, len(rs))
	for i := range rs {
		maxEnd[i] = rs[i].end
		if i > 0 && maxEnd[i-1] > maxEnd[i] {
			maxEnd[i] = maxEnd[i-1]
		}
	}
	return func(off int64, kind sig.Kind) bool {
		if starts[off] {
			return true
		}
		// ranges that start at or before off, checked backwards
		i := sort.Search(len(rs), func(i int) bool { return rs[i].off > off }) - 1
		for ; i >= 0 && maxEnd[i] > off; i-- {
			if off < rs[i].end {
				x := rs[i]
				if x.live || containerExt[x.ext] || sig.KindOfExt(x.ext) == kind {
					return true
				}
			}
		}
		return false
	}
}

// containerExt are file types that hold other files in aligned blocks
// (disk images, tar archives) or are streams with repeating sync marks.
var containerExt = map[string]bool{
	"iso": true, "tar": true, "img": true, "ima": true, "bin": true, "vhd": true, "vhdx": true,
	"vmdk": true, "vdi": true, "dmg": true, "dd": true, "raw": true, "wim": true, "esd": true,
	"nrg": true, "mdf": true, "ts": true, "mts": true, "m2ts": true, "mpg": true, "mpeg": true,
	"vob": true, "tgz": true,
}

// addTextRuns turns runs of text sectors into text files, leaving out text
// that is part of a file already found.
func (s *Session) addTextRuns(sr *scan.Result) {
	s.mu.Lock()
	type rng struct{ off, end int64 }
	var cov []rng
	for _, it := range s.items {
		for _, p := range it.parts {
			if p.Off >= 0 {
				cov = append(cov, rng{p.Off, p.Off + p.Len})
			}
		}
	}
	r := s.r
	s.mu.Unlock()
	sort.Slice(cov, func(i, j int) bool { return cov[i].off < cov[j].off })
	maxEnd := make([]int64, len(cov))
	for i := range cov {
		maxEnd[i] = cov[i].end
		if i > 0 && maxEnd[i-1] > maxEnd[i] {
			maxEnd[i] = maxEnd[i-1]
		}
	}
	covered := func(a, b int64) int64 { // bytes of [a,b) covered by known files
		var n int64
		i := sort.Search(len(cov), func(i int) bool { return cov[i].off >= b }) - 1
		for ; i >= 0 && maxEnd[i] > a; i-- {
			lo, hi := max(a, cov[i].off), min(b, cov[i].end)
			if hi > lo {
				n += hi - lo
			}
		}
		return n
	}
	head := make([]byte, 4096)
	seq := map[string]int{}
	added := 0
	for i, tr := range sr.TextRuns {
		if i%512 == 0 {
			s.progress(int64(i), int64(len(sr.TextRuns)))
		}
		if tr.Len < 64 || added >= 200000 {
			continue
		}
		if c := covered(tr.Off, tr.Off+tr.Len); c*2 > tr.Len {
			continue
		}
		n := min(int64(len(head)), tr.Len)
		r.ReadAt(head[:n], tr.Off)
		ext, title := carve.TextInfo(head[:n], tr.Wide)
		seq[ext]++
		name := title
		if name == "" {
			name = fmt.Sprintf("Text %05d", seq[ext])
		}
		s.mu.Lock()
		it, _ := s.itemFor(fmt.Sprintf("text|%d", tr.Off))
		it.Cat, it.Group, it.Ext = "found", "text", ext
		it.Name = name + "." + ext
		it.Size, it.Off, it.wide = tr.Len, tr.Off, tr.Wide
		it.parts = []fsys.Extent{{Off: tr.Off, Len: tr.Len}}
		it.out = []string{groupLabel["text"], strings.ToUpper(ext)}
		it.path = append([]string{topFound}, it.out...)
		s.mu.Unlock()
		added++
	}
	s.mu.Lock()
	s.updateCountsLocked()
	s.mu.Unlock()
}

// ---------------------------------------------------------------- browsing

// Node is a folder in the results tree.
type Node struct {
	ID    string  `json:"id"`
	Name  string  `json:"name"`
	Count int     `json:"count"`
	Size  int64   `json:"size"`
	Kids  []*Node `json:"kids,omitempty"`
}

func nodeID(path []string) string {
	parts := make([]string, len(path))
	for i, p := range path {
		parts[i] = strings.ReplaceAll(p, "/", "∕")
	}
	return strings.Join(parts, "/")
}

// Tree returns the folder tree of everything found.
func (s *Session) Tree() *Node {
	s.mu.Lock()
	defer s.mu.Unlock()
	root := &Node{ID: "", Name: "All files"}
	index := map[string]*Node{"": root}
	order := map[string]int{topNamed: 0, topLost: 1, topFound: 2}
	for _, it := range s.items {
		root.Count++
		root.Size += it.Size
		for d := 1; d <= len(it.path); d++ {
			id := nodeID(it.path[:d])
			n := index[id]
			if n == nil {
				n = &Node{ID: id, Name: it.path[d-1]}
				parent := index[nodeID(it.path[:d-1])]
				parent.Kids = append(parent.Kids, n)
				index[id] = n
			}
			n.Count++
			n.Size += it.Size
		}
	}
	var sortKids func(n *Node, depth int)
	sortKids = func(n *Node, depth int) {
		sort.SliceStable(n.Kids, func(i, j int) bool {
			if depth == 0 {
				return order[n.Kids[i].Name] < order[n.Kids[j].Name]
			}
			return strings.ToLower(n.Kids[i].Name) < strings.ToLower(n.Kids[j].Name)
		})
		for _, k := range n.Kids {
			sortKids(k, depth+1)
		}
	}
	sortKids(root, 0)
	return root
}

// labelOf is the honest one-word state of an item, as the gallery shows it.
func labelOf(it *Item) string {
	switch it.Cond {
	case "good":
		return "perfect"
	case "damaged":
		switch {
		case it.Share >= 0.02:
			return "partly"
		case it.Preview:
			return "preview"
		}
		return "damaged"
	case "unknown":
		return "unknown"
	case "erased":
		return "wiped"
	}
	if it.Size == 0 {
		return "empty"
	}
	return "unchecked"
}

// GroupSum counts the items of one kind by label.
type GroupSum struct {
	Files  int            `json:"files"`
	Bytes  int64          `json:"bytes"`
	Labels map[string]int `json:"labels"`
}

// Summary is what was found, by kind and label, for the results screen.
type Summary struct {
	Files  int                  `json:"files"`
	Bytes  int64                `json:"bytes"`
	Groups map[string]*GroupSum `json:"groups"`
	First  time.Time            `json:"first"` // earliest and latest date of photos and videos
	Last   time.Time            `json:"last"`
	Saved  int                  `json:"saved"`
}

// Summary counts what was found.
func (s *Session) Summary() Summary {
	s.mu.Lock()
	defer s.mu.Unlock()
	sum := Summary{Groups: map[string]*GroupSum{}}
	for _, it := range s.items {
		g := sum.Groups[it.Group]
		if g == nil {
			g = &GroupSum{Labels: map[string]int{}}
			sum.Groups[it.Group] = g
		}
		g.Files++
		g.Bytes += it.Size
		g.Labels[labelOf(it)]++
		sum.Files++
		sum.Bytes += it.Size
		if it.Saved != "" && it.Saved != "-" {
			sum.Saved++
		}
		if (it.Group == "photos" || it.Group == "videos") && it.MTime.Year() > 1990 && it.MTime.Before(time.Now().Add(48*time.Hour)) {
			if sum.First.IsZero() || it.MTime.Before(sum.First) {
				sum.First = it.MTime
			}
			if it.MTime.After(sum.Last) {
				sum.Last = it.MTime
			}
		}
	}
	return sum
}

// Query selects items for the results list.
type Query struct {
	Node    string `json:"node"`
	Q       string `json:"q"`
	Group   string `json:"group"`
	Groups  string `json:"groups"` // several kinds, comma separated
	Label   string `json:"label"`  // several labels, comma separated
	Deleted bool   `json:"deleted"`
	Cond    string `json:"cond"`
	Sort    string `json:"sort"`
	Desc    bool   `json:"desc"`
	Offset  int    `json:"offset"`
	Limit   int    `json:"limit"`
}

// Page is one page of results.
type Page struct {
	Total int    `json:"total"`
	Size  int64  `json:"size"`
	Items []Item `json:"items"`
}

func (s *Session) match(it *Item, q Query, node []string) bool {
	if len(node) > len(it.path) {
		return false
	}
	for i := range node {
		if strings.ReplaceAll(it.path[i], "/", "∕") != node[i] {
			return false
		}
	}
	if q.Group != "" && it.Group != q.Group {
		return false
	}
	if q.Groups != "" && !inList(q.Groups, it.Group) {
		return false
	}
	if q.Label != "" && !inList(q.Label, labelOf(it)) {
		return false
	}
	if q.Deleted && !it.Deleted && it.Cat == "named" {
		return false
	}
	if q.Cond != "" {
		c := it.Cond
		if c == "" {
			c = "unchecked"
		}
		if c != q.Cond {
			return false
		}
	}
	if q.Q != "" && !strings.Contains(strings.ToLower(it.Name), strings.ToLower(q.Q)) {
		return false
	}
	return true
}

func inList(list, v string) bool {
	for _, x := range strings.Split(list, ",") {
		if x == v {
			return true
		}
	}
	return false
}

func splitNode(id string) []string {
	if id == "" {
		return nil
	}
	return strings.Split(id, "/")
}

// Items returns a page of items matching q.
func (s *Session) Items(q Query) Page {
	s.mu.Lock()
	defer s.mu.Unlock()
	node := splitNode(q.Node)
	var sel []*Item
	var size int64
	for _, it := range s.items {
		if s.match(it, q, node) {
			sel = append(sel, it)
			size += it.Size
		}
	}
	less := func(a, b *Item) bool { return strings.ToLower(a.Name) < strings.ToLower(b.Name) }
	switch q.Sort {
	case "size":
		less = func(a, b *Item) bool { return a.Size < b.Size }
	case "date":
		less = func(a, b *Item) bool { return a.MTime.Before(b.MTime) || a.MTime.Equal(b.MTime) && a.Name < b.Name }
	case "type":
		less = func(a, b *Item) bool { return a.Ext < b.Ext || a.Ext == b.Ext && a.Name < b.Name }
	case "folder":
		less = func(a, b *Item) bool { return strings.Join(a.path, "/") < strings.Join(b.path, "/") }
	}
	sort.SliceStable(sel, func(i, j int) bool {
		if q.Desc {
			return less(sel[j], sel[i])
		}
		return less(sel[i], sel[j])
	})
	if q.Limit <= 0 || q.Limit > 1000 {
		q.Limit = 200
	}
	p := Page{Total: len(sel), Size: size}
	for i := q.Offset; i < len(sel) && i < q.Offset+q.Limit; i++ {
		c := *sel[i]
		c.Dir = append([]string(nil), sel[i].path...)
		c.Label = labelOf(sel[i])
		p.Items = append(p.Items, c)
	}
	return p
}

// IDs returns the id and size of every item matching q (for "select all").
func (s *Session) IDs(q Query) ([]int, []int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	node := splitNode(q.Node)
	var ids []int
	var sizes []int64
	for _, it := range s.items {
		if s.match(it, q, node) {
			ids = append(ids, it.ID)
			sizes = append(sizes, it.Size)
		}
	}
	return ids, sizes
}

// Conds returns the condition, note, saved path and label of the given
// items, for refreshing a list while the background check runs.
func (s *Session) Conds(ids []int) map[int][4]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[int][4]string, len(ids))
	for _, id := range ids {
		if id >= 1 && id <= len(s.items) {
			it := s.items[id-1]
			out[id] = [4]string{it.Cond, it.Note, it.Saved, labelOf(it) + "|" + fmt.Sprintf("%.3f", it.Share)}
		}
	}
	return out
}

// Item returns one item by id.
func (s *Session) Item(id int) (Item, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id < 1 || id > len(s.items) {
		return Item{}, false
	}
	c := *s.items[id-1]
	c.Dir = append([]string(nil), s.items[id-1].path...)
	c.Label = labelOf(s.items[id-1])
	return c, true
}

// Selection names the files to recover.
type Selection struct {
	IDs   []int  `json:"ids"`
	All   bool   `json:"all"`
	Query *Query `json:"query"`
}

func (s *Session) selectLocked(sel Selection) []*Item {
	var out []*Item
	switch {
	case sel.All:
		out = append(out, s.items...)
	case sel.Query != nil:
		node := splitNode(sel.Query.Node)
		for _, it := range s.items {
			if s.match(it, *sel.Query, node) {
				out = append(out, it)
			}
		}
	default:
		for _, id := range sel.IDs {
			if id >= 1 && id <= len(s.items) {
				out = append(out, s.items[id-1])
			}
		}
	}
	return out
}
