//go:build !linux && !windows

package disk

import (
	"errors"
	"os"
	"strings"
	"syscall"
)

// On other systems only image files are supported (raw devices may still
// open as files if permissions allow).

func IsDevicePath(p string) bool { return strings.HasPrefix(p, "/dev/") }

func OpenDevice(path string) (Source, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	end, err := f.Seek(0, 2)
	if err != nil || end <= 0 {
		f.Close()
		return nil, errors.New("cannot determine the size of " + path)
	}
	return &imageSource{f: f, size: end, name: path}, nil
}

func ListDrives() ([]DriveInfo, error) { return nil, nil }
func IsAdmin() bool                    { return os.Geteuid() == 0 }
func Relaunch() error                  { return errors.New("please run with sudo") }
func FreeSpace(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
func FSName(path string) string               { return "" }
func SameDevice(outDir, devPath string) bool  { return false }
func UserHome() string                        { h, _ := os.UserHomeDir(); return h }
func GiveToUser(root string)                  {}
func DefaultOutputBase(exclude string) string { return UserHome() }
func SetSparse(f *os.File)                    {}
func DeviceHint() string                      { return "" }

func ProtectDrive(devPath string) (release func(), notes []string) { return func() {}, nil }
func RunningFromTemp() bool                                        { return false }
