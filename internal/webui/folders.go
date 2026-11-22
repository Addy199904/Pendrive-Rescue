package webui

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"pendriverescue/internal/disk"
)

type dirEntry struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Size   int64  `json:"size,omitempty"`
	Free   int64  `json:"free,omitempty"`
	Rescue bool   `json:"rescue,omitempty"` // holds a safe copy from an earlier recovery
}

type folderResp struct {
	Path   string     `json:"path"`
	Parent string     `json:"parent"`
	Dirs   []dirEntry `json:"dirs"`
	Files  []dirEntry `json:"files"`
	Roots  []dirEntry `json:"roots"`
	Free   int64      `json:"free"`
	Error  string     `json:"error,omitempty"`
}

var imageExts = map[string]bool{".img": true, ".dd": true, ".bin": true, ".raw": true, ".ima": true, ".iso": true, ".001": true}

// folders lists the sub-folders of a folder for the folder picker, and with
// files=1 also the disk image files in it.
func (s *Server) folders(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p := q.Get("path")
	if p == "" {
		p = disk.UserHome()
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	out := folderResp{Path: p, Dirs: []dirEntry{}, Files: []dirEntry{}, Roots: roots()}
	if parent := filepath.Dir(p); parent != p {
		out.Parent = parent
	}
	out.Free, _ = disk.FreeSpace(p)
	ents, err := os.ReadDir(p)
	if err != nil {
		out.Error = "This folder cannot be opened: " + err.Error()
	}
	for _, e := range ents {
		name := e.Name()
		if hiddenName(name) {
			continue
		}
		full := filepath.Join(p, name)
		isDir := e.IsDir()
		if e.Type()&os.ModeSymlink != 0 {
			if st, err := os.Stat(full); err == nil {
				isDir = st.IsDir()
			}
		}
		if isDir {
			out.Dirs = append(out.Dirs, dirEntry{Name: name, Path: full, Rescue: hasImage(full)})
		} else if q.Get("files") == "1" && imageExts[strings.ToLower(filepath.Ext(name))] {
			var size int64
			if info, err := e.Info(); err == nil {
				size = info.Size()
			}
			out.Files = append(out.Files, dirEntry{Name: name, Path: full, Size: size})
		}
	}
	byName := func(l []dirEntry) {
		sort.Slice(l, func(i, j int) bool { return strings.ToLower(l[i].Name) < strings.ToLower(l[j].Name) })
	}
	byName(out.Dirs)
	byName(out.Files)
	writeJSON(w, out)
}

func hiddenName(n string) bool {
	if strings.HasPrefix(n, ".") || strings.HasPrefix(n, "$") {
		return true
	}
	switch strings.ToLower(n) {
	case "system volume information", "recovery", "config.msi", "msocache", "lost+found", "proc", "sys", "dev", "run":
		return true
	}
	return false
}

// roots are the places to start from: drive letters on Windows; the home
// folder, mounted drives and / on Linux.
func roots() []dirEntry {
	var out []dirEntry
	add := func(name, p string) {
		for _, r := range out {
			if r.Path == p {
				return
			}
		}
		free, _ := disk.FreeSpace(p)
		out = append(out, dirEntry{Name: name, Path: p, Free: free})
	}
	if runtime.GOOS == "windows" {
		for c := 'C'; c <= 'Z'; c++ {
			p := string(c) + `:\`
			if _, err := os.Stat(p); err == nil {
				add(string(c)+":", p)
			}
		}
		if h := disk.UserHome(); h != "" {
			add("Your folders", h)
		}
		return out
	}
	if h := disk.UserHome(); h != "" {
		add("Home", h)
	}
	for _, base := range []string{"/media", "/run/media", "/mnt"} {
		ents, _ := os.ReadDir(base)
		for _, e := range ents {
			if !e.IsDir() {
				continue
			}
			p := filepath.Join(base, e.Name())
			// /media/<user>/<drive>
			if sub, _ := os.ReadDir(p); base != "/mnt" && len(sub) > 0 && isUserDir(e.Name()) {
				for _, s := range sub {
					if s.IsDir() {
						add(s.Name(), filepath.Join(p, s.Name()))
					}
				}
				continue
			}
			add(e.Name(), p)
		}
	}
	add("Whole computer", "/")
	return out
}

func isUserDir(name string) bool {
	if u := os.Getenv("SUDO_USER"); u != "" && u == name {
		return true
	}
	if u := os.Getenv("USER"); u != "" && u == name {
		return true
	}
	_, err := os.Stat(filepath.Join("/home", name))
	return err == nil || name == "root"
}
