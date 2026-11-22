//go:build linux

package disk

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// IsDevicePath reports whether p names a block device rather than a file.
func IsDevicePath(p string) bool {
	if !strings.HasPrefix(p, "/dev/") {
		return false
	}
	st, err := os.Stat(p)
	return err == nil && st.Mode()&os.ModeDevice != 0
}

type linuxDevice struct {
	f      *os.File
	size   int64
	ss     int
	name   string
	cached bool // opened without O_DIRECT: the kernel's cache must be emptied before each read
}

const (
	blkGetSize64 = 0x80081272
	blkSSZGet    = 0x1268
	blkFlsBuf    = 0x1261
)

// OpenDevice opens a block device strictly read-only. Reads bypass the
// kernel's cache (O_DIRECT): reading a place again must really ask the
// pendrive again, or worn flash that answers differently each time could
// not be outvoted.
func OpenDevice(path string) (Source, error) {
	cached := false
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECT, 0)
	if err != nil && !errors.Is(err, os.ErrPermission) && !errors.Is(err, os.ErrNotExist) {
		cached = true
		f, err = os.OpenFile(path, os.O_RDONLY, 0)
	}
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return nil, fmt.Errorf("permission denied for %s - run this program with sudo", path)
		}
		return nil, err
	}
	var size uint64
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), blkGetSize64, uintptr(unsafe.Pointer(&size))); e != 0 {
		end, err := f.Seek(0, io.SeekEnd)
		if err != nil {
			f.Close()
			return nil, err
		}
		size = uint64(end)
	}
	var ss int32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), blkSSZGet, uintptr(unsafe.Pointer(&ss))); e != 0 || ss <= 0 {
		ss = 512
	}
	return &linuxDevice{f: f, size: int64(size), ss: int(ss), name: path, cached: cached}, nil
}

func (d *linuxDevice) ReadAt(p []byte, off int64) (int, error) {
	if d.cached {
		// forget what the kernel kept from earlier reads (needs root, which
		// reading a drive needs anyway; it writes nothing to the drive)
		syscall.Syscall(syscall.SYS_IOCTL, d.f.Fd(), blkFlsBuf, 0)
	}
	n, err := d.f.ReadAt(p, off)
	if err == io.EOF && n == len(p) {
		err = nil
	}
	return n, err
}
func (d *linuxDevice) Size() int64     { return d.size }
func (d *linuxDevice) SectorSize() int { return d.ss }
func (d *linuxDevice) Name() string    { return d.name }
func (d *linuxDevice) Close() error    { return d.f.Close() }

func readSys(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

type mountEntry struct {
	dev, dir, fstype string
}

func readMounts() []mountEntry {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []mountEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		parts := strings.SplitN(line, " - ", 2)
		if len(parts) != 2 {
			continue
		}
		left := strings.Fields(parts[0])
		right := strings.Fields(parts[1])
		if len(left) < 5 || len(right) < 2 {
			continue
		}
		out = append(out, mountEntry{dev: unescapeMount(right[1]), dir: unescapeMount(left[4]), fstype: right[0]})
	}
	return out
}

func unescapeMount(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func partitionsOf(name string) []string {
	var parts []string
	ents, _ := os.ReadDir("/sys/block/" + name)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), name) {
			parts = append(parts, e.Name())
		}
	}
	return parts
}

// ListDrives lists the disks the computer can see.
func ListDrives() ([]DriveInfo, error) {
	ents, err := os.ReadDir("/sys/block")
	if err != nil {
		return nil, err
	}
	mounts := readMounts()
	var out []DriveInfo
	for _, e := range ents {
		name := e.Name()
		skip := false
		for _, p := range []string{"loop", "ram", "zram", "dm-", "md", "sr", "fd", "nbd", "mtdblock", "zd"} {
			if strings.HasPrefix(name, p) {
				skip = true
			}
		}
		if skip {
			continue
		}
		base := "/sys/block/" + name
		sectors, _ := strconv.ParseInt(readSys(base+"/size"), 10, 64)
		ss, _ := strconv.Atoi(readSys(base + "/queue/logical_block_size"))
		if ss <= 0 {
			ss = 512
		}
		link, _ := filepath.EvalSymlinks(base)
		d := DriveInfo{
			Path:       "/dev/" + name,
			Number:     -1,
			Size:       sectors * 512,
			SectorSize: ss,
			Removable:  readSys(base+"/removable") == "1",
			USB:        strings.Contains(link, "/usb") || strings.HasPrefix(name, "mmcblk"),
		}
		vendor := readSys(base + "/device/vendor")
		if strings.HasPrefix(vendor, "0x") { // a PCI id (virtual disks), not a name
			vendor = ""
		}
		model := strings.TrimSpace(vendor + " " + readSys(base+"/device/model"))
		if model == "" {
			model = readSys(base + "/device/name")
		}
		if model == "" {
			model = name
		}
		d.Model = model
		devs := append([]string{name}, partitionsOf(name)...)
		for _, m := range mounts {
			b := filepath.Base(m.dev)
			for _, dv := range devs {
				if b == dv && strings.HasPrefix(m.dev, "/dev/") {
					d.Letters = append(d.Letters, m.dir)
					d.FSInfo = append(d.FSInfo, m.fstype)
					if m.dir == "/" || m.dir == "/boot" || m.dir == "/boot/efi" || m.dir == "/usr" || m.dir == "/home" {
						d.System = true
					}
				}
			}
		}
		if d.Size == 0 {
			d.Note = "no media / 0 bytes"
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// IsAdmin reports whether we can read raw disks.
func IsAdmin() bool { return os.Geteuid() == 0 }

// Relaunch restarts the program with administrator rights (sudo).
func Relaunch() error {
	sudo, err := exec.LookPath("sudo")
	if err != nil {
		return errors.New("please run this program as root (sudo)")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := append([]string{"sudo", exe}, os.Args[1:]...)
	return syscall.Exec(sudo, args, os.Environ())
}

// FreeSpace returns the free bytes on the file system holding path.
func FreeSpace(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// FSName returns the file system type that holds path ("ext4", "vfat" ...).
func FSName(path string) string {
	m := mountFor(path)
	if m == nil {
		return ""
	}
	return m.fstype
}

func mountFor(path string) *mountEntry {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		abs = r
	}
	var best *mountEntry
	for _, m := range readMounts() {
		m := m
		if abs == m.dir || strings.HasPrefix(abs, strings.TrimSuffix(m.dir, "/")+"/") || m.dir == "/" {
			if best == nil || len(m.dir) >= len(best.dir) {
				best = &m
			}
		}
	}
	return best
}

// SameDevice reports whether the folder outDir lives on the drive devPath.
func SameDevice(outDir, devPath string) bool {
	if !strings.HasPrefix(devPath, "/dev/") {
		// image file: refuse writing into the image's own folder only if identical file
		return false
	}
	name := filepath.Base(devPath)
	devs := append([]string{name}, partitionsOf(name)...)
	if m := mountFor(outDir); m != nil {
		b := filepath.Base(m.dev)
		for _, d := range devs {
			if b == d {
				return true
			}
		}
	}
	var st syscall.Stat_t
	if err := syscall.Stat(outDir, &st); err == nil {
		dev := uint64(st.Dev)
		major := (dev>>8)&0xfff | (dev>>32)&^0xfff
		minor := dev&0xff | (dev>>12)&^0xff
		want := fmt.Sprintf("%d:%d", major, minor)
		for _, d := range devs {
			p := "/sys/block/" + name + "/" + d + "/dev"
			if d == name {
				p = "/sys/block/" + name + "/dev"
			}
			if readSys(p) == want {
				return true
			}
		}
	}
	return false
}

// UserHome returns the home folder of the real user (not root when run with sudo).
func UserHome() string {
	if su := os.Getenv("SUDO_USER"); su != "" && su != "root" {
		if u, err := user.Lookup(su); err == nil {
			return u.HomeDir
		}
	}
	h, _ := os.UserHomeDir()
	return h
}

// GiveToUser hands ownership of everything under root back to the user who
// started sudo, so they can open and delete the recovered files.
func GiveToUser(root string) {
	uid, err1 := strconv.Atoi(os.Getenv("SUDO_UID"))
	gid, err2 := strconv.Atoi(os.Getenv("SUDO_GID"))
	if err1 != nil || err2 != nil || uid == 0 {
		return
	}
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil {
			os.Lchown(p, uid, gid)
		}
		return nil
	})
}

// DefaultOutputBase suggests where to put recovered files.
func DefaultOutputBase(exclude string) string {
	return UserHome()
}

// SetSparse is a no-op on Linux (holes are automatic).
func SetSparse(f *os.File) {}

// DeviceHint gives OS-specific advice text.
func DeviceHint() string {
	return "Tip: on Linux, do not open the pendrive in the file manager while the scan runs."
}

// ProtectDrive cannot lock a Linux block device without writing, so it only
// reports mounted partitions, which the desktop could still write to.
func ProtectDrive(devPath string) (release func(), notes []string) {
	release = func() {}
	if !strings.HasPrefix(devPath, "/dev/") {
		return
	}
	name := filepath.Base(devPath)
	devs := append([]string{name}, partitionsOf(name)...)
	for _, m := range readMounts() {
		b := filepath.Base(m.dev)
		for _, d := range devs {
			if b == d && strings.HasPrefix(m.dev, "/dev/") {
				notes = append(notes, fmt.Sprintf("%s is mounted at %s - the desktop could still write to it. Unmount (eject) it for the safest recovery.", m.dev, m.dir))
			}
		}
	}
	return
}

// RunningFromTemp is only meaningful on Windows.
func RunningFromTemp() bool { return false }
