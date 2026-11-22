// Package repair checks and repairs photos that were already copied off a
// drive: it finds the ones that turn grey partway, combines several copies
// of the same photo when there are any, and otherwise saves the part that is
// still intact and the small preview stored inside the photo.
package repair

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"pendriverescue/internal/jpegfix"
)

// OutName is the folder the results go into, inside the chosen folder.
const OutName = "Repaired by Pendrive Rescue"

// Photo is the outcome for one photo.
type Photo struct {
	Name   string  `json:"name"`
	Copies int     `json:"copies"`
	Status string  `json:"status"` // fine, repaired, partial, preview, failed
	Share  float64 `json:"share"`  // part that is intact (partial)
	Note   string  `json:"note"`
}

// Job is a repair running in the background.
type Job struct {
	mu      sync.Mutex
	running bool
	done    int
	total   int
	photos  []Photo
	out     string
	err     string
	stop    chan struct{}
	once    sync.Once
	samples *jpegfix.SampleSet
}

// State is a snapshot of a job.
type State struct {
	Running bool    `json:"running"`
	Done    int     `json:"done"`
	Total   int     `json:"total"`
	Photos  []Photo `json:"photos"`
	Out     string  `json:"out"`
	Error   string  `json:"error,omitempty"`
	Fine    int     `json:"fine"`
	Fixed   int     `json:"fixed"`
	Partial int     `json:"partial"`
	Failed  int     `json:"failed"`
}

// State returns the progress and results so far.
func (j *Job) State() State {
	j.mu.Lock()
	defer j.mu.Unlock()
	st := State{Running: j.running, Done: j.done, Total: j.total, Out: j.out, Error: j.err,
		Photos: append([]Photo{}, j.photos...)}
	for _, p := range j.photos {
		switch p.Status {
		case "fine":
			st.Fine++
		case "repaired":
			st.Fixed++
		case "partial", "preview":
			st.Partial++
		default:
			st.Failed++
		}
	}
	return st
}

// Stop ends the job after the current photo.
func (j *Job) Stop() { j.once.Do(func() { close(j.stop) }) }

var copySuffix = regexp.MustCompile(`(?i)( - copy( \(\d+\))?| \(\d+\)|_copy\d*| copy\d*)$`)

// key groups copies of the same photo: same name (without " - Copy (2)"
// and the like) and same size.
func key(path string, size int64) string {
	base := filepath.Base(path)
	ext := filepath.Ext(base)
	name := copySuffix.ReplaceAllString(strings.TrimSuffix(base, ext), "")
	return strings.ToLower(name) + "|" + strconv.FormatInt(size, 10)
}

// Start checks every JPEG photo in folder (and its sub-folders).
func Start(folder string) (*Job, error) {
	st, err := os.Stat(folder)
	if err != nil || !st.IsDir() {
		return nil, os.ErrNotExist
	}
	out := filepath.Join(folder, OutName)
	groups := map[string][]string{}
	var order []string
	filepath.Walk(folder, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if fi.IsDir() {
			if p == out {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(p))
		if ext != ".jpg" && ext != ".jpeg" && ext != ".jfif" {
			return nil
		}
		if strings.Contains(fi.Name(), "(intact part)") || strings.Contains(fi.Name(), "(small preview)") {
			return nil
		}
		k := key(p, fi.Size())
		if groups[k] == nil {
			order = append(order, k)
		}
		groups[k] = append(groups[k], p)
		return nil
	})
	sort.Strings(order)
	j := &Job{running: true, total: len(order), out: out, stop: make(chan struct{}), samples: jpegfix.NewSampleSet()}
	go j.run(groups, order)
	return j, nil
}

func (j *Job) run(groups map[string][]string, order []string) {
	defer func() {
		j.mu.Lock()
		j.running = false
		j.mu.Unlock()
	}()
	if err := os.MkdirAll(j.out, 0755); err != nil {
		j.mu.Lock()
		j.err = "The folder for the results cannot be made: " + err.Error()
		j.mu.Unlock()
		return
	}
	// learn the header tables of the cameras from all the photos first:
	// photos with a broken header are repaired from them
	for _, k := range order {
		if f, err := os.Open(groups[k][0]); err == nil {
			head := make([]byte, 128<<10)
			n, _ := f.ReadAt(head, 0)
			f.Close()
			j.samples.Add(head[:n], false)
		}
	}
	for _, k := range order {
		select {
		case <-j.stop:
			return
		default:
		}
		p := j.one(groups[k])
		j.mu.Lock()
		j.photos = append(j.photos, p)
		j.done++
		j.mu.Unlock()
	}
}

// one handles the copies of one photo.
func (j *Job) one(paths []string) Photo {
	name := filepath.Base(paths[0])
	ext := filepath.Ext(name)
	name = copySuffix.ReplaceAllString(strings.TrimSuffix(name, ext), "") + ext
	ph := Photo{Name: name, Copies: len(paths)}
	var copies [][]byte
	for _, p := range paths {
		if b, err := os.ReadFile(p); err == nil {
			copies = append(copies, b)
		}
	}
	if len(copies) == 0 {
		ph.Status, ph.Note = "failed", "it cannot be read"
		return ph
	}
	// a copy that is fine already?
	for _, c := range copies {
		if ok(c) {
			ph.Status = "fine"
			if len(copies) > 1 {
				os.WriteFile(filepath.Join(j.out, name), c, 0644)
			}
			return ph
		}
	}
	best := copies[0]
	if len(copies) > 1 {
		out, res, err := jpegfix.Combine(copies, time.Now().Add(90*time.Second))
		if err == nil && out != nil {
			best = out
			if res.OK {
				os.WriteFile(filepath.Join(j.out, name), out, 0644)
				ph.Status, ph.Note = "repaired", "combined from "+strconv.Itoa(len(copies))+" copies"
				return ph
			}
		}
	}
	// wrong bits in the header: rebuild it from a photo of the same camera
	if fix := jpegfix.RepairHeader(best, j.samples); fix != nil {
		fixed := append(append([]byte(nil), fix.Head...), best[fix.From:]...)
		note := "its broken header was rebuilt from another photo taken with the same camera"
		if fix.Dropped {
			note += " (its date and rotation were lost)"
		}
		if ok(fixed) {
			os.WriteFile(filepath.Join(j.out, name), fixed, 0644)
			ph.Status, ph.Note = "repaired", note
			return ph
		}
		best = fixed
		ph.Note = note
	}
	sv, err := jpegfix.SalvageFile(bytes.NewReader(best), int64(len(best)))
	if err != nil || sv == nil {
		ph.Status, ph.Note = "failed", "it is not a readable JPEG photo any more"
		return ph
	}
	base := strings.TrimSuffix(name, ext)
	if sv.Intact != nil && sv.Share > 0.01 {
		os.WriteFile(filepath.Join(j.out, base+" (intact part).jpg"), sv.Intact, 0644)
		ph.Status, ph.Share = "partial", sv.Share
	}
	if sv.Preview != nil {
		os.WriteFile(filepath.Join(j.out, base+" (small preview).jpg"), sv.Preview, 0644)
		if ph.Status == "" {
			ph.Status = "preview"
		}
	}
	if ph.Status == "" {
		if ph.Note != "" {
			ph.Status, ph.Note = "failed", ph.Note+", but the picture breaks right after it"
		} else {
			ph.Status, ph.Note = "failed", "nothing usable is left in this copy"
		}
	}
	return ph
}

func ok(b []byte) bool {
	info, err := jpegfix.Parse(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return err == jpegfix.ErrUnsupported // progressive photos: cannot be checked, assume fine
	}
	return jpegfix.Check(info, bytes.NewReader(b), int64(len(b))).OK
}
