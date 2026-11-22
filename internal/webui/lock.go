package webui

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A small file remembers the address of the open window, so that starting
// Pendrive Rescue a second time shows the running one instead of starting a
// second recovery.

func lockPath() string { return filepath.Join(os.TempDir(), "pendrive-rescue-window.txt") }

// Running returns the address of a Pendrive Rescue window that is already
// open on this computer, or "".
func Running() string {
	b, err := os.ReadFile(lockPath())
	if err != nil {
		return ""
	}
	addr := strings.TrimSpace(string(b))
	u, err := url.Parse(addr)
	if err != nil || u.Hostname() != "127.0.0.1" {
		return ""
	}
	c := http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://" + u.Host + "/api/ping?k=" + url.QueryEscape(u.Query().Get("k")))
	if err != nil {
		return ""
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	return addr
}

// SaveLock records the address of this window.
func SaveLock(addr string) { os.WriteFile(lockPath(), []byte(addr), 0600) }

// RemoveLock forgets it again (if it is still ours).
func RemoveLock(addr string) {
	if b, err := os.ReadFile(lockPath()); err == nil && strings.TrimSpace(string(b)) == addr {
		os.Remove(lockPath())
	}
}
