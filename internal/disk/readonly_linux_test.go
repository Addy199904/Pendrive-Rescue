package disk

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestDeviceIsOpenedReadOnly does the same for a real block device (a loop
// device over a temporary image). It needs root and losetup.
func TestDeviceIsOpenedReadOnly(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	if _, err := exec.LookPath("losetup"); err != nil {
		t.Skip("needs losetup")
	}
	path := filepath.Join(t.TempDir(), "stick.img")
	if err := os.WriteFile(path, make([]byte, 4<<20), 0644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("losetup", "--find", "--show", path).Output()
	if err != nil {
		t.Skip("no free loop device: ", err)
	}
	dev := strings.TrimSpace(string(out))
	defer exec.Command("losetup", "-d", dev).Run()
	src, err := OpenDevice(dev)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	d, ok := src.(*linuxDevice)
	if !ok {
		t.Fatalf("OpenDevice returned %T", src)
	}
	if _, err := d.f.WriteAt(AlignedBuf(512), 0); err == nil {
		t.Fatal("the device handle accepted a write")
	}
	if d.cached {
		t.Error("the device was not opened for uncached reads (O_DIRECT)")
	}
	buf := AlignedBuf(4096)
	if _, err := src.ReadAt(buf, 0); err != nil {
		t.Fatal("read failed: ", err)
	}
	// a second read must reach the device again, not the kernel's cache
	os.WriteFile(path, append([]byte("changed"), make([]byte, 4<<20-7)...), 0644)
	exec.Command("losetup", "-c", dev).Run()
	if _, err := src.ReadAt(buf, 0); err != nil {
		t.Fatal("second read failed: ", err)
	}
	if string(buf[:7]) != "changed" {
		t.Error("the second read came from a cache, not from the device")
	}
}
