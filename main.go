// Pendrive Rescue - free, read-only data recovery for pendrives, memory
// cards and other drives. Works on Windows and Linux.
//
// Started without options it opens its window in the web browser. With
// --cli it runs as a step-by-step console program instead.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"pendriverescue/internal/disk"
)

const version = "3.0"

type flags struct {
	cli, list, image, noImage, resume, noDeep, noText, yes, noBrowser bool
	source, out, repair                                               string
	port                                                              int
}

func main() {
	var f flags
	flag.BoolVar(&f.cli, "cli", false, "use the console instead of the browser window")
	flag.StringVar(&f.source, "source", "", "drive (\\\\.\\PhysicalDrive2 or /dev/sdb) or image file to recover from (console mode)")
	flag.StringVar(&f.out, "out", "", "folder where recovered files are saved (console mode)")
	flag.BoolVar(&f.list, "list", false, "only list the drives")
	flag.BoolVar(&f.image, "image", false, "first copy the drive to a safe copy (image file)")
	flag.BoolVar(&f.noImage, "no-image", false, "do not make a safe copy")
	flag.BoolVar(&f.resume, "resume", false, "continue an earlier, unfinished safe copy")
	flag.BoolVar(&f.noDeep, "no-deep", false, "skip the deep scan by file content")
	flag.BoolVar(&f.noText, "no-text", false, "do not look for text files")
	flag.BoolVar(&f.yes, "yes", false, "do not ask questions (console mode)")
	flag.BoolVar(&f.noBrowser, "no-browser", false, "do not open the browser automatically")
	flag.IntVar(&f.port, "port", 0, "port for the browser window (default: any free port)")
	flag.StringVar(&f.repair, "repair", "", "check and repair the JPEG photos in this folder (photos already copied off a drive)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Pendrive Rescue %s\n\nStarted without options it opens in your web browser.\n\nOptions:\n", version)
		flag.PrintDefaults()
	}
	flag.Parse()
	if f.repair != "" {
		repairFolder(f.repair)
		return
	}
	if f.source != "" || f.list {
		f.cli = true
	}

	needAdmin := f.source == "" || disk.IsDevicePath(f.source)
	if needAdmin && !disk.IsAdmin() {
		if runtime.GOOS == "windows" {
			fmt.Println("Asking Windows for administrator permission (needed to read the pendrive)...")
		} else {
			fmt.Println("Administrator (root) rights are needed to read the pendrive. Restarting with sudo...")
		}
		if err := disk.Relaunch(); err != nil {
			fmt.Println()
			fmt.Println("Could not get administrator rights:", err)
			if runtime.GOOS == "windows" {
				fmt.Println("Right-click the program and choose \"Run as administrator\".")
			} else {
				fmt.Println("Run it like this:  sudo ./" + filepath.Base(os.Args[0]))
			}
			pause(true)
			os.Exit(1)
		}
		return
	}
	if f.cli {
		runCLI(f)
		return
	}
	runGUI(f)
}

func openFolder(dir string) {
	switch runtime.GOOS {
	case "windows":
		exec.Command("explorer.exe", dir).Start()
	case "linux":
		asUser("xdg-open", dir)
	}
}

// asUser runs a desktop command as the real user when started with sudo.
func asUser(cmd string, args ...string) error {
	if u := os.Getenv("SUDO_USER"); u != "" && u != "root" {
		full := append([]string{"-u", u, "env"}, desktopEnv()...)
		full = append(full, cmd)
		full = append(full, args...)
		return exec.Command("sudo", full...).Start()
	}
	return exec.Command(cmd, args...).Start()
}

// desktopEnv keeps the variables a browser needs to find the user's screen.
func desktopEnv() []string {
	var out []string
	for _, k := range []string{"DISPLAY", "WAYLAND_DISPLAY", "XAUTHORITY", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
		if v := os.Getenv(k); v != "" {
			out = append(out, k+"="+v)
		}
	}
	if os.Getenv("XDG_RUNTIME_DIR") == "" {
		if uid := os.Getenv("SUDO_UID"); uid != "" {
			out = append(out, "XDG_RUNTIME_DIR=/run/user/"+uid)
		}
	}
	return out
}
