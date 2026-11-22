// Package engine runs a recovery session: a quick look at the file systems
// (names appear within seconds), a full read of the drive (optionally into a
// resumable safe copy), deep analysis, and then saving the files the user
// picks. Both the browser app and the command line use it.
package engine

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"time"

	"pendriverescue/internal/carve"
	"pendriverescue/internal/disk"
	"pendriverescue/internal/fsys"
	"pendriverescue/internal/health"
	"pendriverescue/internal/jpegfix"
	"pendriverescue/internal/scan"
)

// Folder and file names in the output folder.
const (
	DirNamed   = "1 - Files with their names"
	DirLost    = "2 - Lost folders (after format)"
	DirFound   = "3 - Found by content (deep scan)"
	DirDamaged = "4 - Possibly damaged files"
	ImageName  = "pendrive-image.img"
)

// Options describe what to recover and how.
type Options struct {
	Source     string `json:"source"`
	SourceDesc string `json:"desc"`
	OutDir     string `json:"outDir"`
	MakeImage  bool   `json:"image"`
	Resume     bool   `json:"resume"`
	DeepScan   bool   `json:"deep"`
	TextSearch bool   `json:"text"`
	// Careful reads every part of the drive twice into the safe copy and
	// settles disagreements by majority (for worn flash).
	Careful bool `json:"careful"`
	// Limit reads only the first Limit bytes (fake drives: their real size).
	Limit int64 `json:"limit"`
	// Health is the check made before the recovery, if any.
	Health *health.Report `json:"-"`
}

// Phase of a session.
type Phase string

const (
	PhaseQuick     Phase = "quick"
	PhaseReading   Phase = "reading"
	PhaseRetrying  Phase = "retrying"
	PhaseAnalysing Phase = "analysing"
	PhaseReady     Phase = "ready"
	PhaseFailed    Phase = "failed"
)

// Diag is one finding of the diagnosis.
type Diag struct {
	Level string `json:"level"` // bad, warn, info, good
	Title string `json:"title"`
	Text  string `json:"text"`
}

// VolInfo describes a file system for the user.
type VolInfo struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Files   int      `json:"files"`
	Hidden  int      `json:"hidden"`
	Deleted int      `json:"deleted"`
	Lost    bool     `json:"lost"`
	Notes   []string `json:"notes,omitempty"`
}

// SaveStatus describes the saving of files.
type SaveStatus struct {
	Running   bool   `json:"running"`
	Done      int64  `json:"done"`
	Total     int64  `json:"total"`
	Files     int    `json:"files"`
	FilesDone int    `json:"filesDone"`
	Good      int    `json:"good"`
	Damaged   int    `json:"damaged"`
	Unknown   int    `json:"unknown"`
	Erased    int    `json:"erased"`
	Dupes     int    `json:"dupes"`
	Rebuilt   int    `json:"rebuilt"`
	Failed    int    `json:"failed"`
	Bytes     int64  `json:"bytes"`
	Full      bool   `json:"full"`
	Error     string `json:"error,omitempty"`
	Report    string `json:"report,omitempty"`
}

// Status is a snapshot of the session for the user interface.
type Status struct {
	Phase      Phase          `json:"phase"`
	Step       string         `json:"step"`
	Done       int64          `json:"done"`
	Total      int64          `json:"total"`
	Speed      float64        `json:"speed"`
	Left       int64          `json:"left"`
	Stall      int64          `json:"stall"`
	Live       map[string]int `json:"live"`
	Counts     map[string]int `json:"counts"`
	Files      int            `json:"files"`
	FilesBytes int64          `json:"filesBytes"`
	Map        []int          `json:"map"`
	BadBytes   int64          `json:"badBytes"`
	Notes      []string       `json:"notes"`
	Volumes    []VolInfo      `json:"volumes"`
	Diagnosis  []Diag         `json:"diagnosis"`
	Catalog    int            `json:"catalog"`
	Checked    int            `json:"checked"`
	ToCheck    int            `json:"toCheck"`
	Save       SaveStatus     `json:"save"`
	Error      string         `json:"error,omitempty"`
	ImagePath  string         `json:"imagePath,omitempty"`
	Resumed    int64          `json:"resumed"`
	Elapsed    int64          `json:"elapsed"`
	Drive      string         `json:"drive"`
	DriveSize  int64          `json:"driveSize"`
	OutDir     string         `json:"outDir"`
	Stopped    bool           `json:"stopped"`
	Unstable   int            `json:"unstable"` // pages the drive read differently on different tries
	Joined     int            `json:"joined"`   // photos rebuilt from pieces
	Careful    bool           `json:"careful"`  // the safe copy reads everything twice
	Settled    int            `json:"settled"`  // pages settled by majority during the careful copy
	Unsure     int            `json:"unsure"`   // of those, pages whose vote stayed close
	Headers    int            `json:"headers"`  // photo headers rebuilt from another photo
}

// Session is one recovery.
type Session struct {
	opt Options

	mu         sync.Mutex
	st         Status
	started    time.Time
	phaseStart time.Time
	phaseBase  int64

	src  disk.Source
	devR *disk.Reader
	r    *disk.Reader

	sr       *scan.Result
	vols     []*fsys.Volume
	lost     []*fsys.Volume
	carved   []carve.Result
	items    []*Item
	ids      map[string]int
	hitsSeen int

	identity string
	stopCh   chan struct{}
	stopOnce sync.Once
	release  func()

	w      *writer
	saveMu sync.Mutex

	diagUnstable, diagJoined int
	mendMu                   sync.Mutex
	thumbs                   *thumbCache
	fakeReal                 int64
	samples                  *jpegfix.SampleSet // headers of good photos, for repairing broken ones
}

// openSource and isDevice can be replaced in tests to simulate failing drives.
var (
	openSource = disk.Open
	isDevice   = disk.IsDevicePath
)

// OpenSource opens a drive or image the way a recovery would.
func OpenSource(p string) (disk.Source, error) { return openSource(p) }

// IsDevice reports whether p is a drive (rather than a safe copy).
func IsDevice(p string) bool { return isDevice(p) }

// Start opens the drive and begins the recovery in the background.
func Start(opt Options) (*Session, error) {
	out, err := filepath.Abs(opt.OutDir)
	if err != nil {
		return nil, err
	}
	opt.OutDir = out
	if isDevice(opt.Source) && disk.SameDevice(existingParent(out), opt.Source) {
		return nil, errors.New("the folder for the results is on the pendrive itself - choose a folder on another drive")
	}
	if err := os.MkdirAll(out, 0755); err != nil {
		return nil, fmt.Errorf("cannot create the folder for the results: %v", err)
	}
	src, err := openSource(opt.Source)
	if err != nil {
		return nil, err
	}
	if src.Size() <= 0 {
		src.Close()
		return nil, errors.New("the drive reports a size of 0 bytes - its controller has probably failed")
	}
	if opt.SourceDesc == "" {
		opt.SourceDesc = opt.Source
	}
	if !isDevice(opt.Source) {
		opt.MakeImage = false
	}
	if !opt.MakeImage {
		opt.Careful = false // the settled data has to go somewhere: the safe copy
	}
	s := &Session{
		opt:     opt,
		src:     src,
		ids:     map[string]int{},
		stopCh:  make(chan struct{}),
		release: func() {},
		thumbs:  newThumbCache(4000),
		samples: jpegfix.NewSampleSet(),
	}
	s.devR = disk.NewReader(src)
	if opt.Limit > 0 && opt.Limit < src.Size() {
		s.devR.Truncate(opt.Limit)
		s.fakeReal = opt.Limit
	}
	s.r = s.devR
	s.identity = driveIdentity(s.devR)
	s.started = time.Now()
	s.st = Status{
		Phase: PhaseQuick, Live: map[string]int{}, Counts: map[string]int{}, Map: make([]int, scan.NumRegion),
		Drive: opt.SourceDesc, DriveSize: src.Size(), OutDir: out, Careful: opt.Careful,
	}
	if isDevice(opt.Source) {
		rel, notes := disk.ProtectDrive(opt.Source)
		s.release = rel
		s.st.Notes = append(s.st.Notes, notes...)
	}
	s.w = newWriter(out)
	go s.run()
	return s, nil
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

// driveIdentity fingerprints a drive so a safe copy is only resumed on the
// same pendrive.
func driveIdentity(r *disk.Reader) string {
	b := make([]byte, 64<<10)
	r.ReadAt(b, 0)
	h := sha1.Sum(b)
	return fmt.Sprintf("%d-%s", r.Size(), hex.EncodeToString(h[:8]))
}

// ResumeInfo reports how much of a safe copy of this drive already exists
// in outDir (0 if none).
func ResumeInfo(source, outDir string) int64 {
	src, err := openSource(source)
	if err != nil {
		return 0
	}
	defer src.Close()
	r := disk.NewReader(src)
	m, ok := scan.LoadMap(filepath.Join(outDir, ImageName)+".map", r.Size(), driveIdentity(r))
	if !ok {
		return 0
	}
	return m.CopiedBytes()
}

// Options returns the session's options.
func (s *Session) Options() Options { return s.opt }

// Stop ends the reading early; what was read so far is still analysed.
func (s *Session) Stop() {
	s.stopOnce.Do(func() { close(s.stopCh) })
	s.mu.Lock()
	s.st.Stopped = true
	s.mu.Unlock()
}

func (s *Session) stopped() bool {
	select {
	case <-s.stopCh:
		return true
	default:
		return false
	}
}

// Close releases the drive.
func (s *Session) Close() {
	s.Stop()
	s.release()
}

func (s *Session) setPhase(p Phase, step string, total int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Phase = p
	s.st.Step = step
	s.st.Done = 0
	s.st.Total = total
	s.st.Speed = 0
	s.st.Left = 0
	s.phaseStart = time.Now()
	s.phaseBase = 0
}

func (s *Session) progress(done, total int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setProgressLocked(done, total)
}

func (s *Session) setProgressLocked(done, total int64) {
	s.st.Done, s.st.Total = done, total
	el := time.Since(s.phaseStart).Seconds()
	if el > 2 && done > s.phaseBase {
		s.st.Speed = float64(done-s.phaseBase) / el
		if s.st.Speed > 0 && total > done {
			s.st.Left = int64(float64(total-done) / s.st.Speed)
		} else {
			s.st.Left = 0
		}
	}
}

// Status returns a snapshot for the user interface.
func (s *Session) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.st
	st.Elapsed = int64(time.Since(s.started).Seconds())
	if d := s.devR.Stalled(); d > 2*time.Second {
		st.Stall = int64(d.Seconds())
	}
	st.Live = copyMap(s.st.Live)
	st.Counts = copyMap(s.st.Counts)
	st.Map = append([]int(nil), s.st.Map...)
	st.Notes = append([]string(nil), s.st.Notes...)
	if s.st.Phase == PhaseReady && (s.st.Unstable != s.diagUnstable || s.st.Joined != s.diagJoined) {
		s.diagUnstable, s.diagJoined = s.st.Unstable, s.st.Joined
		s.st.Diagnosis = s.diagnoseLocked()
		st.Diagnosis = s.st.Diagnosis
	}
	return st
}

func copyMap(m map[string]int) map[string]int {
	o := make(map[string]int, len(m))
	for k, v := range m {
		o[k] = v
	}
	return o
}

func (s *Session) note(format string, a ...any) {
	s.mu.Lock()
	s.st.Notes = append(s.st.Notes, fmt.Sprintf(format, a...))
	s.mu.Unlock()
}

// run is the recovery pipeline.
func (s *Session) run() {
	defer func() {
		if p := recover(); p != nil {
			s.mu.Lock()
			s.st.Phase = PhaseFailed
			s.st.Error = fmt.Sprintf("internal error: %v", p)
			s.mu.Unlock()
			os.WriteFile(filepath.Join(s.opt.OutDir, "crash-details.txt"), []byte(fmt.Sprintf("%v\n%s", p, debug.Stack())), 0644)
		}
	}()

	// 1. quick look: file systems from the partition table (seconds)
	s.setPhase(PhaseQuick, "Reading the file systems", 0)
	vols, _ := fsys.FindVolumes(s.r, nil)
	s.mu.Lock()
	s.vols = vols
	s.rebuildCatalogLocked()
	s.mu.Unlock()

	// 2. read the whole drive once (into the safe copy when chosen)
	step := "Reading the whole pendrive"
	if s.opt.Careful {
		step = "Copying the pendrive carefully"
	} else if s.opt.MakeImage {
		step = "Copying the pendrive"
	}
	s.setPhase(PhaseReading, step, s.r.Size())
	imgPath := ""
	if s.opt.MakeImage {
		imgPath = filepath.Join(s.opt.OutDir, ImageName)
	}
	sr, err := scan.Run(s.devR, scan.Options{
		ImagePath: imgPath, Resume: s.opt.Resume, Identity: s.identity, Text: s.opt.TextSearch,
		Careful: s.opt.Careful, Stop: s.stopCh, Progress: s.onScan,
	})
	if sr == nil {
		s.fail(err)
		return
	}
	s.mu.Lock()
	s.sr = sr
	s.st.Resumed = sr.Resumed
	s.mu.Unlock()
	if err != nil && !errors.Is(err, scan.ErrStopped) {
		s.note("The reading stopped at %.0f%%: %v", 100*float64(sr.ScannedTo)/float64(s.r.Size()), err)
	}
	gone := errors.Is(err, disk.ErrDeviceGone) || s.devR.Gone()

	// 3. retry unreadable areas (safe copy only), then work from the copy
	if imgPath != "" && sr.ScannedTo > 0 {
		if !gone && !s.stopped() && s.devR.BadSectors() > 0 {
			if got := s.retry(imgPath); got > 0 {
				s.note("Retrying recovered %s of the unreadable areas.", disk.HumanSize(got))
				sr2 := s.rescanImage(imgPath)
				if sr2 != nil {
					sr2.Interrupted = sr.Interrupted
					sr2.ScannedTo = sr.ScannedTo
					sr2.Duration = sr.Duration
					sr = sr2
				}
			}
		}
		if img, err := disk.OpenImage(imgPath); err == nil {
			bad := s.devR.BadAreas()
			ir := disk.NewReader(img)
			for _, b := range bad {
				ir.MarkBadRange(b[0], b[1])
			}
			if sr.ScannedTo < ir.Size() {
				ir.MarkBadRange(sr.ScannedTo, ir.Size()-sr.ScannedTo)
			}
			s.mu.Lock()
			s.r = ir
			s.sr = sr
			s.st.ImagePath = imgPath
			s.mu.Unlock()
			s.thumbs.clear()
		}
	} else if gone {
		s.fail(fmt.Errorf("the pendrive stopped responding at %.0f%% - unplug it, plug it into another USB port and try again (choose the safe copy)", 100*float64(sr.ScannedTo)/float64(s.r.Size())))
		return
	}

	// 4. analysis
	s.setPhase(PhaseAnalysing, "Looking for file systems and lost folders", 0)
	if s.fakeReal > 0 {
		s.note("This pendrive is FAKE: it really holds only about %s, not %s. Only that part was read.", disk.HumanSize(s.fakeReal), disk.HumanSize(s.st.DriveSize))
	} else if real, fake := fakeCapacity(s.r, sr); fake {
		s.note("This pendrive is FAKE: it really holds only about %s, not %s.", disk.HumanSize(real), disk.HumanSize(s.r.Size()))
		s.mu.Lock()
		s.st.Diagnosis = nil
		s.fakeReal = real
		s.mu.Unlock()
		s.r.Truncate(real)
		limitScan(sr, real)
	}
	vols, _ = fsys.FindVolumes(s.r, sr)
	hx := fsys.NewHitIndex(sr.Hits)
	lost := fsys.FindLost(s.r, sr, vols, hx)
	s.mu.Lock()
	s.vols, s.lost = vols, lost
	s.rebuildCatalogLocked()
	s.mu.Unlock()

	if s.opt.DeepScan {
		s.setPhase(PhaseAnalysing, "Deep scan: finding files by their content", int64(len(sr.Hits)))
		skip := s.skipFunc()
		res, err := carve.Run(s.r, sr.Hits, carve.Options{
			Skip: skip,
			Progress: func(d, t int) {
				s.progress(int64(d), int64(t))
			},
		})
		if err != nil {
			s.note("Deep scan stopped: %v", err)
		}
		s.mu.Lock()
		s.carved = res
		s.rebuildCatalogLocked()
		s.mu.Unlock()
	}
	if s.opt.TextSearch {
		s.setPhase(PhaseAnalysing, "Looking for text files", int64(len(sr.TextRuns)))
		s.addTextRuns(sr)
	}

	s.mu.Lock()
	s.st.Phase = PhaseReady
	s.st.Step = "Ready"
	s.st.Diagnosis = s.diagnoseLocked()
	s.mu.Unlock()

	// 5. check files in the background so the user sees which are good
	go s.checkAll()
}

func (s *Session) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Phase = PhaseFailed
	if err != nil {
		s.st.Error = err.Error()
	}
	if s.sr != nil {
		s.st.Diagnosis = s.diagnoseLocked()
	}
}

// onScan receives progress from the full read.
func (s *Session) onScan(res *scan.Result, done, total, bad int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phaseBase == 0 && res.Resumed > 0 {
		s.phaseBase = res.Resumed
	}
	s.setProgressLocked(done, total)
	s.st.BadBytes = bad
	s.st.Resumed = res.Resumed
	s.st.Settled = res.Settled
	s.st.Unsure = res.Unsure
	for i := s.hitsSeen; i < len(res.Hits); i++ {
		s.st.Live[groupOfExt(res.Hits[i].Kind.Ext())]++
	}
	s.hitsSeen = len(res.Hits)
	s.st.Live["text"] = len(res.TextRuns)
	for i, g := range res.Regions {
		s.st.Map[i] = cellState(g)
	}
}

func cellState(g scan.Region) int {
	tot := g.Data + g.Zero + g.FF + g.Bad
	switch {
	case tot == 0:
		return 0 // not read yet
	case g.Bad > 0:
		return 4
	case g.Data*2 >= tot:
		return 3
	case g.Data*20 >= tot:
		return 2
	case g.FF > g.Zero:
		return 5
	}
	return 1
}
