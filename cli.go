package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"pendriverescue/internal/disk"
	"pendriverescue/internal/engine"
	"pendriverescue/internal/health"
	"pendriverescue/internal/repair"
)

var in = bufio.NewReader(os.Stdin)

func ask(prompt string) string {
	fmt.Print(prompt)
	line, _ := in.ReadString('\n')
	return strings.Trim(strings.TrimSpace(line), `"'`)
}

func yes(prompt string, def bool) bool {
	for {
		switch strings.ToLower(ask(prompt)) {
		case "":
			return def
		case "y", "yes", "h", "haan", "ha":
			return true
		case "n", "no", "nahi":
			return false
		}
		fmt.Println("  Please type Y (yes) or N (no).")
	}
}

func pause(interactive bool) {
	if interactive {
		ask("\nPress Enter to close this window...")
	}
}

func banner() {
	fmt.Println("==============================================================")
	fmt.Printf("   PENDRIVE RESCUE %s  -  free data recovery for pendrives\n", version)
	fmt.Println("   It only READS your pendrive. Nothing on it is changed.")
	fmt.Println("==============================================================")
	if h := disk.DeviceHint(); h != "" {
		fmt.Println()
		fmt.Println(h)
	}
	fmt.Println()
}

func runCLI(f flags) {
	interactive := f.source == "" && !f.list && !f.yes
	banner()
	if f.list {
		drives, err := disk.ListDrives()
		if err != nil {
			fmt.Println("Error:", err)
			os.Exit(1)
		}
		printDrives(drives)
		return
	}
	opt := engine.Options{DeepScan: !f.noDeep, TextSearch: !f.noText, Resume: f.resume}
	var size int64
	if f.source != "" {
		s, err := disk.Open(f.source)
		if err != nil {
			fmt.Println("Cannot open", f.source+":", err)
			os.Exit(1)
		}
		size = s.Size()
		s.Close()
		opt.Source = f.source
		opt.SourceDesc = fmt.Sprintf("%s (%s)", f.source, disk.HumanSize(size))
	} else {
		d := chooseDrive()
		if d == nil {
			pause(true)
			return
		}
		opt.Source, size = d.Path, d.Size
		opt.SourceDesc = fmt.Sprintf("%s (%s)", d.Model, disk.HumanSize(d.Size))
	}

	stamp := time.Now().Format("2006-01-02 1504")
	if f.out != "" {
		opt.OutDir = f.out
	} else {
		def := filepath.Join(disk.DefaultOutputBase(opt.Source), "Pendrive Rescue "+stamp)
		for {
			fmt.Println()
			fmt.Println("Where should the recovered files be saved? (never on the pendrive itself)")
			fmt.Println("Suggested folder:", def)
			a := ask("Press Enter to use it, or type (or drag here) another folder: ")
			if a == "" {
				a = def
			} else if st, err := os.Stat(a); err == nil && st.IsDir() {
				a = filepath.Join(a, "Pendrive Rescue "+stamp)
			}
			if abs, err := filepath.Abs(a); err == nil {
				a = abs
			}
			if disk.IsDevicePath(opt.Source) && disk.SameDevice(existingParent(a), opt.Source) {
				fmt.Println("  That folder is ON THE PENDRIVE. Choose a folder on another drive.")
				continue
			}
			opt.OutDir = a
			break
		}
	}
	os.MkdirAll(opt.OutDir, 0755)

	// check the drive first: what is wrong decides how it is read
	var rep *health.Report
	if disk.IsDevicePath(opt.Source) {
		fmt.Println()
		fmt.Println("Checking the drive (about a minute, read-only)...")
		if src, err := disk.Open(opt.Source); err == nil {
			release, _ := disk.ProtectDrive(opt.Source)
			rep = health.Check(src, health.Options{})
			release()
			src.Close()
			fmt.Println()
			for _, l := range wrapText(rep.Title+" "+rep.Detail, 72) {
				fmt.Println("  " + l)
			}
			fmt.Println()
			for _, l := range wrapText(rep.Next, 72) {
				fmt.Println("  " + l)
			}
			opt.Health = rep
			if rep.RealSize > 0 {
				opt.Limit = rep.RealSize
				size = rep.RealSize
			}
		}
	}

	free, _ := disk.FreeSpace(opt.OutDir)
	fs := strings.ToUpper(disk.FSName(opt.OutDir))
	fat32 := strings.Contains(fs, "FAT") && !strings.Contains(fs, "EXFAT")
	canImage := disk.IsDevicePath(opt.Source) && free > 2*size+(1<<30) && !(fat32 && size >= 4<<30)
	wantCopy := rep != nil && (rep.Plan == health.PlanCopy || rep.Plan == health.PlanCareful)
	if wantCopy {
		canImage = disk.IsDevicePath(opt.Source) && free > size+(1<<30) && !(fat32 && size >= 4<<30)
	}
	switch {
	case !disk.IsDevicePath(opt.Source):
	case f.image:
		opt.MakeImage = true
	case f.noImage:
	case !interactive:
		opt.MakeImage = canImage && (wantCopy || rep == nil)
	default:
		fmt.Println()
		fmt.Printf("Free space in that folder: %s\n", disk.HumanSize(free))
		if done := engine.ResumeInfo(opt.Source, opt.OutDir); done > 0 {
			fmt.Printf("An unfinished safe copy of this pendrive is in that folder (%s copied).\n", disk.HumanSize(done))
			if yes("Continue it? [Y/n]: ", true) {
				opt.MakeImage, opt.Resume = true, true
			}
		} else if canImage && (wantCopy || rep == nil) {
			fmt.Printf("Recommended: first make a SAFE COPY of the pendrive on this computer (uses up to %s).\n", disk.HumanSize(size))
			opt.MakeImage = yes("Make the safe copy? [Y/n]: ", true)
		} else {
			fmt.Println("Not enough free space for a safe copy - the pendrive will be read directly. That is fine.")
		}
	}
	opt.Careful = opt.MakeImage && rep != nil && rep.Plan == health.PlanCareful
	if opt.Careful {
		fmt.Println("The copy reads every part twice and settles disagreements by reading again.")
	}
	if interactive {
		fmt.Println()
		ask("Press Enter to start (or close this window to cancel)...")
		fmt.Println()
	}

	sess, err := engine.Start(opt)
	if err != nil {
		fmt.Println("Cannot start:", err)
		pause(interactive)
		os.Exit(1)
	}
	defer sess.Close()
	for _, n := range sess.Status().Notes {
		fmt.Println("  " + n)
	}
	pb := &progress{}
	lastStep := ""
	var st engine.Status
	for {
		st = sess.Status()
		if st.Step != lastStep {
			pb.newline()
			fmt.Println(stepLine(st))
			lastStep = st.Step
		}
		if st.Phase == engine.PhaseReady || st.Phase == engine.PhaseFailed {
			break
		}
		if st.Total > 0 {
			pb.update(st)
		}
		time.Sleep(300 * time.Millisecond)
	}
	pb.newline()
	if st.Phase == engine.PhaseFailed {
		fmt.Println()
		fmt.Println("The recovery could not finish:", st.Error)
		for _, d := range st.Diagnosis {
			fmt.Println(" * " + d.Title + ": " + d.Text)
		}
		pause(interactive)
		os.Exit(1)
	}
	fmt.Printf("Found %s files (%s). Saving them...\n", engine.Thousands(st.Files), disk.HumanSize(st.FilesBytes))
	sess.Recover(engine.Selection{All: true})
	time.Sleep(200 * time.Millisecond)
	for {
		st = sess.Status()
		if !st.Save.Running && st.Save.Report != "" {
			break
		}
		pb.update(engine.Status{Step: "Saving", Done: st.Save.Done, Total: st.Save.Total, Speed: 0})
		time.Sleep(300 * time.Millisecond)
	}
	pb.newline()
	sv := st.Save
	fmt.Println()
	fmt.Println("==============================================================")
	fmt.Printf(" FINISHED: %s files saved (%s)\n", engine.Thousands(sv.Good+sv.Unknown+sv.Damaged), disk.HumanSize(sv.Bytes))
	fmt.Printf("   good: %d   not checkable: %d   possibly damaged: %d\n", sv.Good, sv.Unknown, sv.Damaged)
	if sv.Dupes+sv.Erased > 0 {
		fmt.Printf("   duplicates left out: %d   wiped (nothing to save): %d\n", sv.Dupes, sv.Erased)
	}
	fmt.Println(" Your files and REPORT.html are in:")
	fmt.Println("   " + opt.OutDir)
	fmt.Println("==============================================================")
	for _, d := range st.Diagnosis {
		fmt.Println()
		fmt.Println(" * " + d.Title)
		for _, l := range wrapText(d.Text, 70) {
			fmt.Println("   " + l)
		}
	}
	if interactive {
		fmt.Println()
		ask("Press Enter to open the folder with your files...")
		openFolder(opt.OutDir)
	}
}

func stepLine(st engine.Status) string {
	switch st.Phase {
	case engine.PhaseQuick:
		return "Step 1 of 4: " + st.Step
	case engine.PhaseReading:
		return "Step 2 of 4: " + st.Step
	case engine.PhaseRetrying:
		return "Step 2 of 4: " + st.Step
	case engine.PhaseAnalysing:
		return "Step 3 of 4: " + st.Step
	case engine.PhaseReady:
		return "Step 4 of 4: saving"
	}
	return st.Step
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

func printDrives(drives []disk.DriveInfo) {
	for i, d := range drives {
		var tags []string
		if d.USB {
			tags = append(tags, "USB")
		} else if d.Removable {
			tags = append(tags, "removable")
		}
		if d.System {
			tags = append(tags, "SYSTEM DISK - do not choose")
		}
		if d.Note != "" {
			tags = append(tags, d.Note)
		}
		model := d.Model
		if len(model) > 30 {
			model = model[:30]
		}
		fmt.Printf("  [%d]  %-30s %10s   %s %s\n", i+1, model, disk.HumanSize(d.Size), strings.Join(tags, ", "), strings.Join(d.Letters, " "))
		if len(d.FSInfo) > 0 && runtime.GOOS == "windows" {
			fmt.Printf("        %s\n", strings.Join(d.FSInfo, ";  "))
		}
	}
}

func chooseDrive() *disk.DriveInfo {
	for {
		drives, err := disk.ListDrives()
		if err != nil {
			fmt.Println("Could not list the drives:", err)
		}
		fmt.Println("Drives found on this computer:")
		fmt.Println()
		printDrives(drives)
		fmt.Println()
		fmt.Println("  [F]  Use an image file (a safe copy saved earlier)")
		fmt.Println("  [R]  Refresh the list (after plugging in the pendrive)")
		fmt.Println()
		a := strings.ToUpper(ask("Type the number of your PENDRIVE and press Enter: "))
		switch a {
		case "R", "":
			fmt.Println()
			continue
		case "F":
			p := ask("Type (or drag here) the image file: ")
			s, err := disk.OpenImage(p)
			if err != nil {
				fmt.Println("  Cannot open it:", err)
				continue
			}
			size := s.Size()
			s.Close()
			return &disk.DriveInfo{Path: p, Model: "Image file " + filepath.Base(p), Size: size}
		}
		n, err := strconv.Atoi(a)
		if err != nil || n < 1 || n > len(drives) {
			fmt.Println("  Please type one of the numbers shown.")
			fmt.Println()
			continue
		}
		d := drives[n-1]
		if d.Size <= 0 {
			fmt.Println()
			fmt.Println("  This drive reports 0 bytes / no media. Its controller chip has usually failed.")
			fmt.Println("  No software can read it; only a data recovery lab can. Try another USB port first.")
			fmt.Println()
			continue
		}
		if d.System && !yes("  That is the disk your computer runs from. Scan it anyway? [y/N]: ", false) {
			continue
		}
		if !d.System && !d.USB && !d.Removable && !yes("  This is not a USB drive. Continue anyway? [y/N]: ", false) {
			continue
		}
		return &d
	}
}

type progress struct {
	inLine   bool
	lastLine string
	last     time.Time
}

func (p *progress) newline() {
	if p.inLine {
		fmt.Println()
		p.inLine = false
		p.lastLine = ""
	}
}

func (p *progress) update(st engine.Status) {
	if time.Since(p.last) < 250*time.Millisecond && st.Done < st.Total {
		return
	}
	p.last = time.Now()
	frac := 0.0
	if st.Total > 0 {
		frac = float64(st.Done) / float64(st.Total)
	}
	if frac > 1 {
		frac = 1
	}
	const width = 16
	bar := strings.Repeat("#", int(frac*width)) + strings.Repeat(".", width-int(frac*width))
	line := fmt.Sprintf("  [%s] %3.0f%%", bar, frac*100)
	if st.Phase == engine.PhaseReading || st.Phase == engine.PhaseRetrying || st.Step == "Saving" {
		line += fmt.Sprintf("  %s of %s", disk.HumanSize(st.Done), disk.HumanSize(st.Total))
	}
	if st.Speed > 0 {
		line += fmt.Sprintf("  %.1f MB/s", st.Speed/1e6)
	}
	if st.Left > 0 {
		line += "  " + roundDur(time.Duration(st.Left)*time.Second) + " left"
	}
	if st.Stall > 0 {
		line += fmt.Sprintf("  (pendrive not responding for %d s)", st.Stall)
	}
	if len(line) < len(p.lastLine) {
		line += strings.Repeat(" ", len(p.lastLine)-len(line))
	}
	p.lastLine = line
	fmt.Print("\r" + line)
	p.inLine = true
}

func roundDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()+0.5))
	}
	return fmt.Sprintf("%d h %d min", int(d.Hours()), int(d.Minutes())%60)
}

func wrapText(s string, width int) []string {
	var lines []string
	var cur string
	for _, w := range strings.Fields(s) {
		if cur == "" {
			cur = w
		} else if len(cur)+1+len(w) <= width {
			cur += " " + w
		} else {
			lines = append(lines, cur)
			cur = w
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}

// repairFolder checks and repairs the photos in a folder (console mode).
func repairFolder(dir string) {
	banner()
	job, err := repair.Start(dir)
	if err != nil {
		fmt.Println("That folder cannot be opened:", dir)
		os.Exit(1)
	}
	fmt.Println("Checking the photos in", dir)
	shown := 0
	for {
		st := job.State()
		for ; shown < len(st.Photos); shown++ {
			p := st.Photos[shown]
			switch p.Status {
			case "fine":
			case "repaired":
				fmt.Printf("  repaired   %s (%s)\n", p.Name, p.Note)
			case "partial":
				fmt.Printf("  partly     %s: %.0f%% of the picture is intact\n", p.Name, 100*p.Share)
			case "preview":
				fmt.Printf("  preview    %s: only the small preview could be saved\n", p.Name)
			default:
				fmt.Printf("  failed     %s: %s\n", p.Name, p.Note)
			}
		}
		if !st.Running {
			fmt.Println()
			if st.Error != "" {
				fmt.Println(st.Error)
				os.Exit(1)
			}
			fmt.Printf("%d fine, %d repaired, %d partly saved, %d failed. Results: %s\n", st.Fine, st.Fixed, st.Partial, st.Failed, st.Out)
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
}
