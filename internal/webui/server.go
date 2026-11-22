// Package webui is the window of Pendrive Rescue: a small web server on
// 127.0.0.1 that serves the app to the browser it opens, plus a JSON API over
// the recovery engine.
//
// Only that browser can use it. The address it opens carries a random key,
// which is swapped for a cookie; every request must carry the cookie and the
// right Host (against DNS rebinding), and every change must also carry a
// custom header that other web sites cannot send without permission.
// Recovered files are served with a sandbox policy, so a web page or SVG
// found on the pendrive cannot run in the app's origin.
package webui

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"pendriverescue/internal/disk"
	"pendriverescue/internal/engine"
	"pendriverescue/internal/health"
	"pendriverescue/internal/repair"
)

//go:embed static
var staticFS embed.FS

// Config connects the server to the desktop.
type Config struct {
	Version string
	// OpenPath shows a folder in the file manager or opens a file with its
	// usual program.
	OpenPath func(p string)
}

// Server is the local web app.
type Server struct {
	cfg   Config
	token string
	port  int
	hosts map[string]bool

	mu     sync.Mutex
	sess   *engine.Session
	srv    *http.Server
	repair *repair.Job
	check  *checkJob

	done     chan struct{}
	quitOnce sync.Once
}

// New makes a server with a fresh random key.
func New(cfg Config) *Server {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return &Server{cfg: cfg, token: hex.EncodeToString(b), done: make(chan struct{})}
}

// Listen opens a port on 127.0.0.1 (0 = any free port) and returns the
// address the browser must open.
func (s *Server) Listen(port int) (net.Listener, string, error) {
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, "", err
	}
	s.port = l.Addr().(*net.TCPAddr).Port
	s.hosts = map[string]bool{
		fmt.Sprintf("127.0.0.1:%d", s.port): true,
		fmt.Sprintf("localhost:%d", s.port): true,
	}
	return l, fmt.Sprintf("http://127.0.0.1:%d/?k=%s", s.port, s.token), nil
}

// Serve answers requests until the user quits.
func (s *Server) Serve(l net.Listener) error {
	s.srv = &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second}
	err := s.srv.Serve(l)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Done is closed when the user quits from the browser.
func (s *Server) Done() <-chan struct{} { return s.done }

// Close stops the recovery session (a running save finishes its file).
func (s *Server) Close() {
	s.mu.Lock()
	if s.sess != nil {
		s.sess.Close()
		s.sess = nil
	}
	s.mu.Unlock()
}

func (s *Server) keyOK(k string) bool {
	return len(k) == len(s.token) && subtle.ConstantTimeCompare([]byte(k), []byte(s.token)) == 1
}

func (s *Server) session() *engine.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sess
}

// ServeHTTP checks who is asking, then serves the app or the API.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	if !s.hosts[strings.ToLower(r.Host)] {
		http.Error(w, "Wrong address.", http.StatusForbidden)
		return
	}
	if r.URL.Path == "/api/ping" { // a second copy of the program checks if we run
		if s.keyOK(r.URL.Query().Get("k")) {
			io.WriteString(w, "ok")
		} else {
			http.Error(w, "no", http.StatusForbidden)
		}
		return
	}
	if k := r.URL.Query().Get("k"); k != "" && r.URL.Path == "/" {
		if !s.keyOK(k) {
			s.denied(w)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "pdr", Value: s.token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if c, err := r.Cookie("pdr"); err != nil || !s.keyOK(c.Value) {
		s.denied(w)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if r.Header.Get("X-PDR") != "1" {
			http.Error(w, "Forbidden.", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" {
			if u, err := url.Parse(o); err != nil || !s.hosts[strings.ToLower(u.Host)] {
				http.Error(w, "Forbidden.", http.StatusForbidden)
				return
			}
		}
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		h.Set("Cache-Control", "no-store")
		h.Set("X-Frame-Options", "DENY")
		s.api(w, r)
		return
	}
	s.static(w, r)
}

func (s *Server) denied(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.WriteHeader(http.StatusForbidden)
	io.WriteString(w, `<!doctype html><meta charset="utf-8"><title>Pendrive Rescue</title>
<body style="font:18px/1.5 system-ui,sans-serif;max-width:36em;margin:4em auto;padding:0 1em;color:#17211C">
<h1 style="font-size:24px">Open Pendrive Rescue from its own window</h1>
<p>This page only works when Pendrive Rescue opens it. Look at the Pendrive Rescue window
(the black console window) and copy the address shown there into your browser, or start Pendrive Rescue again.</p>`)
}

// ---------------------------------------------------------------- static files

const pageCSP = "default-src 'self'; img-src 'self' data: blob:; media-src 'self'; frame-src 'self'; " +
	"script-src 'self'; style-src 'self'; font-src 'self'; connect-src 'self'; object-src 'none'; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

var contentTypes = map[string]string{
	".html": "text/html; charset=utf-8", ".css": "text/css; charset=utf-8", ".js": "text/javascript; charset=utf-8",
	".svg": "image/svg+xml", ".woff2": "font/woff2", ".txt": "text/plain; charset=utf-8",
}

func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if p == "/" {
		p = "/index.html"
	}
	p = path.Clean(p)
	b, err := fs.ReadFile(staticFS, "static"+p)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ct := contentTypes[path.Ext(p)]
	if ct == "" {
		ct = "application/octet-stream"
	}
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("X-Frame-Options", "DENY")
	if strings.HasSuffix(p, ".html") {
		h.Set("Content-Security-Policy", pageCSP)
		h.Set("Cache-Control", "no-store")
	} else if strings.HasSuffix(p, ".woff2") {
		h.Set("Cache-Control", "private, max-age=86400")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	w.Write(b)
}

// ---------------------------------------------------------------- API

type apiError struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(apiError{msg})
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(v)
}

func (s *Server) api(w http.ResponseWriter, r *http.Request) {
	post := r.Method == http.MethodPost
	switch r.URL.Path {
	case "/api/hello":
		s.hello(w)
	case "/api/drives":
		s.drives(w)
	case "/api/plan":
		if post {
			s.plan(w, r)
			return
		}
	case "/api/folders":
		s.folders(w, r)
	case "/api/start":
		if post {
			s.start(w, r)
			return
		}
	case "/api/quit":
		if post {
			writeJSON(w, map[string]bool{"ok": true})
			go func() {
				time.Sleep(300 * time.Millisecond)
				s.Close()
				s.quitOnce.Do(func() { close(s.done) })
				if s.srv != nil {
					s.srv.Close()
				}
			}()
			return
		}
	case "/api/repair":
		s.repairAPI(w, r)
		return
	case "/api/check":
		s.checkAPI(w, r)
		return
	case "/api/shop":
		s.shop(w)
		return
	case "/api/reset":
		if post {
			s.Close()
			writeJSON(w, map[string]bool{"ok": true})
			return
		}
	default:
		s.sessionAPI(w, r)
		return
	}
	if !post && w.Header().Get("Content-Type") == "" {
		fail(w, http.StatusMethodNotAllowed, "use POST")
	}
}

func (s *Server) sessionAPI(w http.ResponseWriter, r *http.Request) {
	sess := s.session()
	if r.URL.Path == "/api/status" && sess == nil {
		writeJSON(w, map[string]string{"phase": "none"})
		return
	}
	if sess == nil {
		fail(w, http.StatusConflict, "no recovery is running - start one first")
		return
	}
	post := r.Method == http.MethodPost
	id, _ := strconv.Atoi(r.URL.Query().Get("id"))
	switch r.URL.Path {
	case "/api/status":
		writeJSON(w, sess.Status())
	case "/api/stop":
		if post {
			sess.Stop()
			writeJSON(w, map[string]bool{"ok": true})
		}
	case "/api/tree":
		writeJSON(w, sess.Tree())
	case "/api/summary":
		writeJSON(w, sess.Summary())
	case "/api/items":
		writeJSON(w, sess.Items(queryOf(r)))
	case "/api/ids":
		ids, sizes := sess.IDs(queryOf(r))
		writeJSON(w, map[string]any{"ids": ids, "sizes": sizes})
	case "/api/conds":
		var req struct {
			IDs []int `json:"ids"`
		}
		if err := readJSON(r, &req); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, sess.Conds(req.IDs))
	case "/api/item":
		it, ok := sess.Item(id)
		if !ok {
			fail(w, http.StatusNotFound, "no such file")
			return
		}
		writeJSON(w, it)
	case "/api/file":
		s.file(w, r, sess, id)
	case "/api/thumb":
		b, err := sess.Thumb(id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(b)
	case "/api/text":
		t, err := sess.Text(id)
		if err != nil {
			writeJSON(w, apiError{err.Error()})
			return
		}
		writeJSON(w, map[string]string{"text": t})
	case "/api/hex":
		t, err := sess.Hex(id)
		if err != nil {
			writeJSON(w, apiError{err.Error()})
			return
		}
		writeJSON(w, map[string]string{"hex": t})
	case "/api/recover":
		if !post {
			break
		}
		var sel engine.Selection
		if err := readJSON(r, &sel); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := sess.Recover(sel); err != nil {
			fail(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	case "/api/open":
		if !post {
			break
		}
		var req struct {
			What string `json:"what"`
		}
		readJSON(r, &req)
		st := sess.Status()
		p := st.OutDir
		if req.What == "report" && st.Save.Report != "" {
			p = st.Save.Report
		}
		if s.cfg.OpenPath != nil {
			s.cfg.OpenPath(p)
		}
		writeJSON(w, map[string]bool{"ok": true})
	case "/api/report":
		st := sess.Status()
		if st.Save.Report == "" {
			http.NotFound(w, r)
			return
		}
		b, err := os.ReadFile(st.Save.Report)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; style-src 'unsafe-inline'; img-src data:")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Write(b)
	default:
		http.NotFound(w, r)
		return
	}
	if !post && (r.URL.Path == "/api/stop" || r.URL.Path == "/api/recover" || r.URL.Path == "/api/open") {
		fail(w, http.StatusMethodNotAllowed, "use POST")
	}
}

func queryOf(r *http.Request) engine.Query {
	v := r.URL.Query()
	off, _ := strconv.Atoi(v.Get("offset"))
	lim, _ := strconv.Atoi(v.Get("limit"))
	return engine.Query{
		Node: v.Get("node"), Q: v.Get("q"), Group: v.Get("group"), Cond: v.Get("cond"),
		Groups: v.Get("groups"), Label: v.Get("label"),
		Deleted: v.Get("deleted") == "1", Sort: v.Get("sort"), Desc: v.Get("desc") == "1",
		Offset: off, Limit: lim,
	}
}

// ---------------------------------------------------------------- recovered files

// Kinds the browser may show inside the app. Everything else is offered as
// a download only.
var inlineTypes = map[string]string{
	"jpg": "image/jpeg", "jpeg": "image/jpeg", "jfif": "image/jpeg", "png": "image/png", "gif": "image/gif",
	"bmp": "image/bmp", "webp": "image/webp", "avif": "image/avif", "ico": "image/x-icon", "svg": "image/svg+xml",
	"mp4": "video/mp4", "m4v": "video/mp4", "mov": "video/mp4", "3gp": "video/3gpp", "webm": "video/webm",
	"mkv": "video/webm", "ogv": "video/ogg",
	"mp3": "audio/mpeg", "m4a": "audio/mp4", "aac": "audio/aac", "wav": "audio/wav", "ogg": "audio/ogg",
	"oga": "audio/ogg", "opus": "audio/ogg", "flac": "audio/flac", "amr": "audio/amr",
	"pdf": "application/pdf",
}

func (s *Server) file(w http.ResponseWriter, r *http.Request, sess *engine.Session, id int) {
	sr, it, err := sess.Open(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	h := w.Header()
	name := url.PathEscape(it.Name)
	ct, inline := inlineTypes[it.Ext]
	if inline && r.URL.Query().Get("download") == "" {
		h.Set("Content-Disposition", "inline; filename*=UTF-8''"+name)
	} else {
		ct = "application/octet-stream"
		h.Set("Content-Disposition", "attachment; filename*=UTF-8''"+name)
	}
	h.Set("Content-Type", ct)
	h.Set("X-Frame-Options", "SAMEORIGIN")
	if ct != "application/pdf" { // Chrome refuses to show PDFs in a sandbox
		h.Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src 'self' data:; media-src 'self'; style-src 'unsafe-inline'")
	}
	mt := it.MTime
	if mt.Year() < 1980 {
		mt = time.Time{}
	}
	http.ServeContent(w, r, "", mt, sr)
}

// ---------------------------------------------------------------- start screen

type helloResp struct {
	Version string `json:"version"`
	OS      string `json:"os"`
	Admin   bool   `json:"admin"`
	Hint    string `json:"hint"`
	Temp    bool   `json:"temp"`
	Session bool   `json:"session"`
}

func (s *Server) hello(w http.ResponseWriter) {
	writeJSON(w, helloResp{
		Version: s.cfg.Version, OS: runtime.GOOS, Admin: disk.IsAdmin() || extraDrives != nil, Hint: disk.DeviceHint(),
		Temp: disk.RunningFromTemp(), Session: s.session() != nil,
	})
}

type driveJSON struct {
	Path      string   `json:"path"`
	Number    int      `json:"number"`
	Model     string   `json:"model"`
	Size      int64    `json:"size"`
	USB       bool     `json:"usb"`
	Removable bool     `json:"removable"`
	System    bool     `json:"system"`
	Letters   []string `json:"letters"`
	FS        []string `json:"fs"`
	Note      string   `json:"note"`
}

// extraDrives adds simulated drives (demo builds only).
var extraDrives func() []disk.DriveInfo

func (s *Server) drives(w http.ResponseWriter) {
	list, err := disk.ListDrives()
	if extraDrives != nil {
		list = append(extraDrives(), list...)
		err = nil
	}
	out := struct {
		Drives []driveJSON `json:"drives"`
		Error  string      `json:"error,omitempty"`
	}{Drives: []driveJSON{}}
	if err != nil {
		out.Error = err.Error()
	}
	for _, d := range list {
		out.Drives = append(out.Drives, driveJSON{d.Path, d.Number, strings.TrimSpace(d.Model), d.Size, d.USB, d.Removable, d.System, d.Letters, d.FSInfo, d.Note})
	}
	writeJSON(w, out)
}

type planReq struct {
	Source string `json:"source"`
	Base   string `json:"base"`
	Need   int64  `json:"need"` // bytes a safe copy needs (fake drives: their real size)
}

type planResp struct {
	Base      string `json:"base"`
	OutDir    string `json:"outDir"`
	Free      int64  `json:"free"`
	FS        string `json:"fs"`
	Size      int64  `json:"size"`
	Device    bool   `json:"device"`
	OnDrive   bool   `json:"onDrive"`
	CanImage  bool   `json:"canImage"`
	Recommend bool   `json:"recommend"`
	ImageWhy  string `json:"imageWhy,omitempty"`
	Resume    int64  `json:"resume"`
	Warn      string `json:"warn,omitempty"`
	Error     string `json:"error,omitempty"`
}

// plan works out where the files will go and whether a safe copy fits.
func (s *Server) plan(w http.ResponseWriter, r *http.Request) {
	var req planReq
	if err := readJSON(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	var p planResp
	src, err := engine.OpenSource(req.Source)
	if err != nil {
		p.Error = "Cannot open this drive: " + err.Error()
		writeJSON(w, p)
		return
	}
	p.Size = src.Size()
	src.Close()
	p.Device = engine.IsDevice(req.Source)
	need := p.Size
	if req.Need > 0 && req.Need < need {
		need = req.Need
	}
	base := req.Base
	if base == "" {
		base = disk.DefaultOutputBase(req.Source)
	}
	if abs, err := filepath.Abs(base); err == nil {
		base = abs
	}
	p.Base = base
	if hasImage(base) {
		p.OutDir = base // an earlier recovery folder: continue in it
	} else {
		p.OutDir = freshFolder(base)
	}
	parent := existingParent(p.OutDir)
	p.Free, _ = disk.FreeSpace(parent)
	p.FS = strings.ToUpper(disk.FSName(parent))
	if p.Device && disk.SameDevice(parent, req.Source) {
		p.OnDrive = true
		p.Error = "This folder is on the drive you want to recover. Saving there would overwrite the lost files. Choose a folder on another drive."
		writeJSON(w, p)
		return
	}
	fat32 := strings.Contains(p.FS, "FAT") && !strings.Contains(p.FS, "EXFAT")
	switch {
	case !p.Device:
		p.ImageWhy = "This is already a safe copy."
	case fat32 && need >= 4<<30:
		p.ImageWhy = "The folder is on a FAT32 drive, which cannot hold files of 4 GB or more."
	case p.Free < need+(1<<30):
		p.ImageWhy = fmt.Sprintf("A copy needs %s of free space, and this drive has %s.", disk.HumanSize(need+(1<<30)), disk.HumanSize(p.Free))
	default:
		p.CanImage = true
		p.Recommend = p.Free > 2*need+(1<<30)
	}
	if p.Device {
		p.Resume = engine.ResumeInfo(req.Source, p.OutDir)
	}
	if p.Free > 0 && p.Free < 1<<30 {
		p.Warn = fmt.Sprintf("Only %s free here. Choose a folder on a drive with more space if you can.", disk.HumanSize(p.Free))
	}
	writeJSON(w, p)
}

func hasImage(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, engine.ImageName))
	return err == nil
}

// freshFolder names a new, empty results folder inside base.
func freshFolder(base string) string {
	name := "Pendrive Rescue " + time.Now().Format("2006-01-02 1504")
	p := filepath.Join(base, name)
	for i := 2; i < 100; i++ {
		if !nonEmpty(p) {
			return p
		}
		p = filepath.Join(base, fmt.Sprintf("%s (%d)", name, i))
	}
	return p
}

func nonEmpty(dir string) bool {
	f, err := os.Open(dir)
	if err != nil {
		return false
	}
	defer f.Close()
	names, _ := f.Readdirnames(1)
	return len(names) > 0
}

func existingParent(p string) string {
	for {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		q := filepath.Dir(p)
		if q == p {
			return p
		}
		p = q
	}
}

type startReq struct {
	Source  string `json:"source"`
	Desc    string `json:"desc"`
	OutDir  string `json:"outDir"`
	Image   bool   `json:"image"`
	Resume  bool   `json:"resume"`
	Deep    bool   `json:"deep"`
	Text    bool   `json:"text"`
	Careful bool   `json:"careful"`
	Limit   int64  `json:"limit"`
}

func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	var req startReq
	if err := readJSON(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Source == "" || req.OutDir == "" {
		fail(w, http.StatusBadRequest, "choose a drive and a folder first")
		return
	}
	s.Close()
	var rep *health.Report
	s.mu.Lock()
	if c := s.check; c != nil {
		if st := c.state(); !st.Running && st.Source == req.Source {
			rep = st.Report
		}
	}
	s.mu.Unlock()
	sess, err := engine.Start(engine.Options{
		Source: req.Source, SourceDesc: req.Desc, OutDir: req.OutDir,
		MakeImage: req.Image, Resume: req.Resume && req.Image, DeepScan: req.Deep, TextSearch: req.Text,
		Careful: req.Careful && req.Image, Limit: req.Limit, Health: rep,
	})
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	s.mu.Lock()
	s.sess = sess
	s.mu.Unlock()
	writeJSON(w, sess.Status())
}

// ---------------------------------------------------------------- repairing photos already copied

func (s *Server) repairAPI(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	job := s.repair
	s.mu.Unlock()
	if r.Method != http.MethodPost {
		if job == nil {
			writeJSON(w, map[string]bool{"none": true})
			return
		}
		writeJSON(w, job.State())
		return
	}
	var req struct {
		Folder string `json:"folder"`
		Action string `json:"action"` // start, stop, open
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	switch req.Action {
	case "stop":
		if job != nil {
			job.Stop()
		}
	case "open":
		if job != nil && s.cfg.OpenPath != nil {
			s.cfg.OpenPath(job.State().Out)
		}
	default:
		if job != nil && job.State().Running {
			fail(w, http.StatusConflict, "photos are being repaired already")
			return
		}
		j, err := repair.Start(req.Folder)
		if err != nil {
			fail(w, http.StatusBadRequest, "That folder cannot be opened.")
			return
		}
		s.mu.Lock()
		s.repair = j
		s.mu.Unlock()
		job = j
	}
	if job == nil {
		writeJSON(w, map[string]bool{"none": true})
		return
	}
	writeJSON(w, job.State())
}

// ---------------------------------------------------------------- the health check

type checkState struct {
	Running bool           `json:"running"`
	Source  string         `json:"source"`
	Desc    string         `json:"desc"`
	Step    string         `json:"step"`
	Done    int            `json:"done"`
	Total   int            `json:"total"`
	Report  *health.Report `json:"report,omitempty"`
	Error   string         `json:"error,omitempty"`
}

type checkJob struct {
	mu   sync.Mutex
	st   checkState
	stop chan struct{}
}

func (c *checkJob) state() checkState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st
}

func (s *Server) checkAPI(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	job := s.check
	s.mu.Unlock()
	if r.Method != http.MethodPost {
		if job == nil {
			writeJSON(w, map[string]bool{"none": true})
			return
		}
		writeJSON(w, job.state())
		return
	}
	var req struct {
		Source string `json:"source"`
		Desc   string `json:"desc"`
		Action string `json:"action"` // start (default), stop
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Action == "stop" {
		if job != nil {
			job.mu.Lock()
			select {
			case <-job.stop:
			default:
				close(job.stop)
			}
			job.mu.Unlock()
		}
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	if job != nil && job.state().Running {
		fail(w, http.StatusConflict, "a check is running already")
		return
	}
	if s.session() != nil {
		s.Close() // the drive is read by one thing at a time
	}
	src, err := engine.OpenSource(req.Source)
	if err != nil {
		fail(w, http.StatusBadRequest, "This drive cannot be opened: "+err.Error())
		return
	}
	job = &checkJob{stop: make(chan struct{}), st: checkState{Running: true, Source: req.Source, Desc: req.Desc, Step: "Starting"}}
	s.mu.Lock()
	s.check = job
	s.mu.Unlock()
	go func() {
		defer src.Close()
		release := func() {}
		if engine.IsDevice(req.Source) {
			release, _ = disk.ProtectDrive(req.Source)
		}
		defer release()
		rep := health.Check(src, health.Options{Stop: job.stop, Progress: func(step string, d, t int) {
			job.mu.Lock()
			job.st.Step, job.st.Done, job.st.Total = step, d, t
			job.mu.Unlock()
		}})
		job.mu.Lock()
		job.st.Running = false
		job.st.Report = rep
		job.mu.Unlock()
	}()
	writeJSON(w, job.state())
}

// shop serves the note for a repair shop: for the recovery running now, or
// for the last check.
func (s *Server) shop(w http.ResponseWriter) {
	var page string
	if sess := s.session(); sess != nil {
		page = sess.ShopReport()
	} else {
		s.mu.Lock()
		job := s.check
		s.mu.Unlock()
		if job == nil || job.state().Report == nil {
			http.Error(w, "Check a pendrive first.", http.StatusNotFound)
			return
		}
		st := job.state()
		page = engine.ShopHTML(st.Desc, st.Report, nil, nil)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "sandbox allow-modals; default-src 'none'; style-src 'unsafe-inline'")
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	io.WriteString(w, page)
}
