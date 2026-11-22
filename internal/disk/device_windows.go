//go:build windows

package disk

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

var (
	kernel32                  = syscall.NewLazyDLL("kernel32.dll")
	shell32                   = syscall.NewLazyDLL("shell32.dll")
	procGetLogicalDrives      = kernel32.NewProc("GetLogicalDrives")
	procGetDriveTypeW         = kernel32.NewProc("GetDriveTypeW")
	procGetDiskFreeSpaceExW   = kernel32.NewProc("GetDiskFreeSpaceExW")
	procGetVolumePathNameW    = kernel32.NewProc("GetVolumePathNameW")
	procGetVolumeInformationW = kernel32.NewProc("GetVolumeInformationW")
	procShellExecuteW         = shell32.NewProc("ShellExecuteW")
	procSetErrorMode          = kernel32.NewProc("SetErrorMode")
)

const (
	ioctlStorageQueryProperty   = 0x2D1400
	ioctlStorageGetDeviceNumber = 0x2D1080
	ioctlDiskGetDriveGeometryEx = 0x700A0
	ioctlDiskGetLengthInfo      = 0x7405C
	fsctlSetSparse              = 0x900C4

	driveRemovable = 2
	driveFixed     = 3
)

// IsDevicePath reports whether p names a raw device rather than a file.
func IsDevicePath(p string) bool {
	return strings.HasPrefix(p, `\\.\`)
}

type winDevice struct {
	h    syscall.Handle
	size int64
	ss   int
	name string
}

func utf16(s string) *uint16 {
	p, _ := syscall.UTF16PtrFromString(s)
	return p
}

func openRaw(path string, access uint32) (syscall.Handle, error) {
	return syscall.CreateFile(utf16(path), access,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, nil, syscall.OPEN_EXISTING, 0, 0)
}

func ioctl(h syscall.Handle, code uint32, in, out []byte) (uint32, error) {
	var ret uint32
	var ip, op *byte
	if len(in) > 0 {
		ip = &in[0]
	}
	if len(out) > 0 {
		op = &out[0]
	}
	err := syscall.DeviceIoControl(h, code, ip, uint32(len(in)), op, uint32(len(out)), &ret, nil)
	return ret, err
}

func geometry(h syscall.Handle) (size int64, ss int) {
	out := make([]byte, 256)
	if n, err := ioctl(h, ioctlDiskGetDriveGeometryEx, nil, out); err == nil && n >= 32 {
		ss = int(binary.LittleEndian.Uint32(out[20:24]))
		size = int64(binary.LittleEndian.Uint64(out[24:32]))
	}
	if size <= 0 {
		l := make([]byte, 8)
		if n, err := ioctl(h, ioctlDiskGetLengthInfo, nil, l); err == nil && n >= 8 {
			size = int64(binary.LittleEndian.Uint64(l))
		}
	}
	if ss <= 0 || ss&(ss-1) != 0 {
		ss = 512
	}
	return
}

// OpenDevice opens a physical drive strictly read-only.
func OpenDevice(path string) (Source, error) {
	h, err := openRaw(path, syscall.GENERIC_READ)
	if err != nil {
		if err == syscall.ERROR_ACCESS_DENIED {
			return nil, errors.New("access denied - the program must be run as Administrator")
		}
		return nil, fmt.Errorf("cannot open %s: %v", path, err)
	}
	size, ss := geometry(h)
	if size <= 0 {
		syscall.CloseHandle(h)
		return nil, fmt.Errorf("%s reports a size of 0 bytes (no media)", path)
	}
	return &winDevice{h: h, size: size, ss: ss, name: path}, nil
}

func (d *winDevice) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	var done uint32
	ov := syscall.Overlapped{Offset: uint32(off), OffsetHigh: uint32(off >> 32)}
	err := syscall.ReadFile(d.h, p, &done, &ov)
	if err != nil {
		return int(done), err
	}
	if int(done) != len(p) {
		return int(done), io.ErrUnexpectedEOF
	}
	return int(done), nil
}
func (d *winDevice) Size() int64     { return d.size }
func (d *winDevice) SectorSize() int { return d.ss }
func (d *winDevice) Name() string    { return d.name }
func (d *winDevice) Close() error    { return syscall.CloseHandle(d.h) }

func cstr(b []byte, off uint32) string {
	if off == 0 || int(off) >= len(b) {
		return ""
	}
	end := int(off)
	for end < len(b) && b[end] != 0 {
		end++
	}
	return strings.TrimSpace(string(b[off:end]))
}

func describe(h syscall.Handle) (model string, removable bool, bus uint32) {
	in := make([]byte, 12)
	out := make([]byte, 1024)
	n, err := ioctl(h, ioctlStorageQueryProperty, in, out)
	if err != nil || n < 36 {
		return "", false, 0
	}
	removable = out[10] != 0
	vendor := cstr(out, binary.LittleEndian.Uint32(out[12:16]))
	product := cstr(out, binary.LittleEndian.Uint32(out[16:20]))
	bus = binary.LittleEndian.Uint32(out[28:32])
	return strings.TrimSpace(vendor + " " + product), removable, bus
}

func deviceNumberOfVolume(letter string) (int, bool) {
	h, err := openRaw(`\\.\`+letter+":", 0)
	if err != nil {
		return -1, false
	}
	defer syscall.CloseHandle(h)
	out := make([]byte, 12)
	if _, err := ioctl(h, ioctlStorageGetDeviceNumber, nil, out); err != nil {
		return -1, false
	}
	if binary.LittleEndian.Uint32(out[0:4]) != 7 { // FILE_DEVICE_DISK
		return -1, false
	}
	return int(binary.LittleEndian.Uint32(out[4:8])), true
}

func volumeInfo(root string) (label, fs string) {
	lb := make([]uint16, 261)
	fb := make([]uint16, 261)
	var serial, maxLen, flags uint32
	r, _, _ := procGetVolumeInformationW.Call(uintptr(unsafe.Pointer(utf16(root))),
		uintptr(unsafe.Pointer(&lb[0])), 261,
		uintptr(unsafe.Pointer(&serial)), uintptr(unsafe.Pointer(&maxLen)), uintptr(unsafe.Pointer(&flags)),
		uintptr(unsafe.Pointer(&fb[0])), 261)
	if r == 0 {
		return "", "RAW/unreadable"
	}
	return syscall.UTF16ToString(lb), syscall.UTF16ToString(fb)
}

func driveType(root string) uint32 {
	r, _, _ := procGetDriveTypeW.Call(uintptr(unsafe.Pointer(utf16(root))))
	return uint32(r)
}

// ListDrives lists the physical disks of this computer.
func ListDrives() ([]DriveInfo, error) {
	// never show "There is no disk in the drive" pop-ups for empty card readers
	procSetErrorMode.Call(0x0001 | 0x8000)
	letters := map[int][]string{}
	fsinfo := map[int][]string{}
	sysNum := -1
	sysDrive := strings.ToUpper(os.Getenv("SystemDrive"))
	if sysDrive == "" {
		sysDrive = "C:"
	}
	mask, _, _ := procGetLogicalDrives.Call()
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		L := string(rune('A' + i))
		t := driveType(L + `:\`)
		if t != driveRemovable && t != driveFixed {
			continue
		}
		num, ok := deviceNumberOfVolume(L)
		if !ok {
			continue
		}
		label, fs := volumeInfo(L + `:\`)
		letters[num] = append(letters[num], L+":")
		info := fs
		if label != "" {
			info = fs + ` "` + label + `"`
		}
		fsinfo[num] = append(fsinfo[num], L+": "+info)
		if L+":" == sysDrive {
			sysNum = num
		}
	}
	var out []DriveInfo
	for n := 0; n < 64; n++ {
		path := `\\.\PhysicalDrive` + strconv.Itoa(n)
		h, err := openRaw(path, 0)
		if err != nil {
			continue
		}
		model, removable, bus := describe(h)
		size, ss := geometry(h)
		syscall.CloseHandle(h)
		if model == "" {
			model = "Disk " + strconv.Itoa(n)
		}
		d := DriveInfo{
			Path: path, Number: n, Model: model, Size: size, SectorSize: ss,
			USB:       bus == 7 || bus == 0xC || bus == 0xD,
			Removable: removable, System: n == sysNum,
			Letters: letters[n], FSInfo: fsinfo[n],
		}
		if size <= 0 {
			d.Note = "no media / 0 bytes"
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

// IsAdmin reports whether the program runs elevated.
func IsAdmin() bool {
	p, err := syscall.GetCurrentProcess()
	if err != nil {
		return false
	}
	var tok syscall.Token
	if err := syscall.OpenProcessToken(p, syscall.TOKEN_QUERY, &tok); err != nil {
		return false
	}
	defer tok.Close()
	var elevated, n uint32
	if err := syscall.GetTokenInformation(tok, 20 /* TokenElevation */, (*byte)(unsafe.Pointer(&elevated)), 4, &n); err != nil {
		return false
	}
	return elevated != 0
}

// Relaunch starts the program again with administrator rights (UAC prompt).
func Relaunch() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	var args []string
	for _, a := range os.Args[1:] {
		args = append(args, syscall.EscapeArg(a))
	}
	cwd, _ := os.Getwd()
	r, _, _ := procShellExecuteW.Call(0, uintptr(unsafe.Pointer(utf16("runas"))),
		uintptr(unsafe.Pointer(utf16(exe))), uintptr(unsafe.Pointer(utf16(strings.Join(args, " ")))),
		uintptr(unsafe.Pointer(utf16(cwd))), 1)
	if r <= 32 {
		return errors.New("administrator permission was not given")
	}
	os.Exit(0)
	return nil
}

// FreeSpace returns the free bytes available at path.
func FreeSpace(path string) (int64, error) {
	var avail, total, free uint64
	r, _, err := procGetDiskFreeSpaceExW.Call(uintptr(unsafe.Pointer(utf16(path))),
		uintptr(unsafe.Pointer(&avail)), uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&free)))
	if r == 0 {
		return 0, err
	}
	return int64(avail), nil
}

func volumeRoot(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	buf := make([]uint16, 1024)
	r, _, _ := procGetVolumePathNameW.Call(uintptr(unsafe.Pointer(utf16(abs))), uintptr(unsafe.Pointer(&buf[0])), 1024)
	if r == 0 {
		return filepath.VolumeName(abs) + `\`
	}
	return syscall.UTF16ToString(buf)
}

// FSName returns the file system name of the volume holding path.
func FSName(path string) string {
	_, fs := volumeInfo(volumeRoot(path))
	return fs
}

// SameDevice reports whether folder outDir is on physical drive devPath.
func SameDevice(outDir, devPath string) bool {
	low := strings.ToLower(devPath)
	if !strings.HasPrefix(low, `\\.\physicaldrive`) {
		return false
	}
	num, err := strconv.Atoi(devPath[len(`\\.\PhysicalDrive`):])
	if err != nil {
		return false
	}
	root := volumeRoot(outDir)
	if len(root) >= 2 && root[1] == ':' {
		if n, ok := deviceNumberOfVolume(strings.ToUpper(root[:1])); ok {
			return n == num
		}
	}
	return false
}

// UserHome returns the user's home folder.
func UserHome() string {
	h, _ := os.UserHomeDir()
	return h
}

// GiveToUser is not needed on Windows.
func GiveToUser(root string) {}

// DefaultOutputBase picks the fixed drive with the most free space that is
// not the source drive, e.g. `D:\`.
func DefaultOutputBase(exclude string) string {
	best, bestFree := "", int64(-1)
	mask, _, _ := procGetLogicalDrives.Call()
	for i := 2; i < 26; i++ { // skip A: and B:
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		root := string(rune('A'+i)) + `:\`
		if driveType(root) != driveFixed {
			continue
		}
		if exclude != "" && SameDevice(root, exclude) {
			continue
		}
		free, err := FreeSpace(root)
		if err != nil {
			continue
		}
		if free > bestFree {
			best, bestFree = root, free
		}
	}
	if best == "" {
		return UserHome()
	}
	return best
}

// SetSparse marks a new image file as sparse so empty areas take no space.
func SetSparse(f *os.File) {
	var ret uint32
	syscall.DeviceIoControl(syscall.Handle(f.Fd()), fsctlSetSparse, nil, 0, nil, 0, &ret, nil)
}

// DeviceHint gives OS-specific advice text.
func DeviceHint() string {
	return "IMPORTANT: if Windows asks you to FORMAT the pendrive, always click CANCEL."
}

const (
	fsctlLockVolume     = 0x00090018
	fsctlUnlockVolume   = 0x0009001C
	fsctlDismountVolume = 0x00090020
)

// ProtectDrive stops Windows from writing to the drive's volumes while the
// recovery runs: each volume is locked and dismounted (best effort, nothing
// is written). It returns a function that releases the locks, plus notes
// for the user.
func ProtectDrive(devPath string) (release func(), notes []string) {
	release = func() {}
	low := strings.ToLower(devPath)
	if !strings.HasPrefix(low, `\\.\physicaldrive`) {
		return
	}
	num, err := strconv.Atoi(devPath[len(`\\.\PhysicalDrive`):])
	if err != nil {
		return
	}
	var held []syscall.Handle
	mask, _, _ := procGetLogicalDrives.Call()
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		L := string(rune('A' + i))
		if n, ok := deviceNumberOfVolume(L); !ok || n != num {
			continue
		}
		h, err := openRaw(`\\.\`+L+":", syscall.GENERIC_READ)
		if err != nil {
			notes = append(notes, L+": could not be protected - close any window that shows it")
			continue
		}
		if _, err := ioctl(h, fsctlLockVolume, nil, nil); err != nil {
			syscall.CloseHandle(h)
			notes = append(notes, L+": is in use, so Windows could still write to it - close any window that shows it")
			continue
		}
		ioctl(h, fsctlDismountVolume, nil, nil)
		held = append(held, h)
		notes = append(notes, L+": locked - Windows cannot write to it while Pendrive Rescue runs")
	}
	release = func() {
		for _, h := range held {
			ioctl(h, fsctlUnlockVolume, nil, nil)
			syscall.CloseHandle(h)
		}
		held = nil
	}
	return
}

// RunningFromTemp reports whether the program was started from a temporary
// folder (for example straight from inside a ZIP file).
func RunningFromTemp() bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	low := strings.ToLower(exe)
	tmp := strings.ToLower(os.TempDir())
	return (tmp != "" && strings.HasPrefix(low, tmp)) || strings.Contains(low, `\temp\`) && strings.Contains(low, ".zip")
}
