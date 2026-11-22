//go:build windows

package disk

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWindowsLock needs a test drive Windows has mounted (the CI makes a
// virtual one): PDR_TEST_DISK=\\.\PhysicalDrive2 and PDR_TEST_LETTER=R.
// It checks that while Pendrive Rescue holds the lock, Windows cannot write
// to the drive, that the raw disk can still be read, and that the drive is
// usable again afterwards.
func TestWindowsLock(t *testing.T) {
	dev, letter := os.Getenv("PDR_TEST_DISK"), os.Getenv("PDR_TEST_LETTER")
	if dev == "" || letter == "" {
		t.Skip("set PDR_TEST_DISK and PDR_TEST_LETTER to a test drive")
	}
	root := letter + `:\`
	if err := os.WriteFile(filepath.Join(root, "before.txt"), []byte("x"), 0644); err != nil {
		t.Fatalf("the test drive is not writable to begin with: %v", err)
	}
	src, err := OpenDevice(dev)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	buf := AlignedBuf(4096)
	if _, err := src.ReadAt(buf, 0); err != nil {
		t.Fatal("cannot read the drive: ", err)
	}

	release, notes := ProtectDrive(dev)
	t.Logf("notes: %q", notes)
	locked := false
	for _, n := range notes {
		locked = locked || strings.Contains(n, "locked")
	}
	if !locked {
		t.Errorf("the volume %s: was not locked: %q", letter, notes)
	}
	if err := os.WriteFile(filepath.Join(root, "during.txt"), []byte("x"), 0644); err == nil {
		t.Error("Windows could still write to the drive while it was locked")
	}
	if _, err := src.ReadAt(buf, 0); err != nil {
		t.Error("reading the raw disk failed while the volume was locked: ", err)
	}
	release()

	var werr error
	for i := 0; i < 40; i++ {
		if werr = os.WriteFile(filepath.Join(root, "after.txt"), []byte("x"), 0644); werr == nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if werr != nil {
		t.Errorf("the drive was not usable after the lock was released: %v", werr)
	}
	os.Remove(filepath.Join(root, "before.txt"))
	os.Remove(filepath.Join(root, "after.txt"))
}
