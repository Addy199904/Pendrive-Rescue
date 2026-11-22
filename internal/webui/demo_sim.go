//go:build demosim

package webui

import (
	"os"

	"pendriverescue/internal/disk"
)

// Simulated pendrives in the drive list (demo builds only). The image comes
// from PDR_DEMO_IMAGE.
func init() {
	img := os.Getenv("PDR_DEMO_IMAGE")
	if img == "" {
		return
	}
	st, err := os.Stat(img)
	if err != nil {
		return
	}
	extraDrives = func() []disk.DriveInfo {
		var out []disk.DriveInfo
		for i, k := range [][2]string{{"worn", "SanDisk Cruzer Blade"}, {"healthy", "HP v236w"}, {"failing", "Kingston DataTraveler"}, {"fake", "Generic Flash Disk"}} {
			size := st.Size()
			if k[0] == "fake" {
				size *= 8
			}
			out = append(out, disk.DriveInfo{
				Path: "sim:" + k[0] + ":" + img, Number: 10 + i, Model: k[1], Size: size, SectorSize: 512,
				USB: true, Removable: true, Letters: []string{string(rune('E'+i)) + ":"}, FSInfo: []string{"FAT32"},
			})
		}
		return out
	}
}
