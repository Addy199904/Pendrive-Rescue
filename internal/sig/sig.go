// Package sig recognises the start of known file types at the beginning
// of a disk sector. It is used by the full-drive scan (to note where files
// begin) and by the carver (which then works out how long each file is).
package sig

import (
	"bytes"
	"encoding/binary"
)

// Kind identifies a file format family.
type Kind uint8

const (
	None Kind = iota
	JPEG
	PNG
	GIF
	BMP
	TIFF // also camera RAW files built on TIFF (CR2, NEF, ARW, DNG ...)
	ORF
	RW2
	ISOBMFF // MP4, MOV, 3GP, M4A, HEIC, AVIF, CR3
	RIFF    // AVI, WAV, WEBP
	EBML    // MKV, WEBM
	ASF     // WMV, WMA
	FLV
	MP3 // MP3 starting with an ID3 tag
	OGG
	FLAC
	AMR
	PDF
	ZIP // also DOCX, XLSX, PPTX, ODT, EPUB, APK, JAR
	OLE // DOC, XLS, PPT, MSG
	RAR
	SEVENZ
	SQLITE
	PSD
	MPEGPS // MPG, VOB
	MPEGTS // TS
	M2TS   // MTS, M2TS (camcorders)
	GZIP
	RTF
	TAR
	ISO9660
	MIDI
	AIFF
	DJVU
	PST
	DWG
	AAC
	EPS
	EPSBIN
	KDBX
	RM
	CAB
	NumKinds
)

var names = [...]string{
	None: "none", JPEG: "JPEG", PNG: "PNG", GIF: "GIF", BMP: "BMP", TIFF: "TIFF/RAW",
	ORF: "ORF", RW2: "RW2", ISOBMFF: "MP4/MOV/HEIC", RIFF: "AVI/WAV/WEBP", EBML: "MKV/WEBM",
	ASF: "WMV/WMA", FLV: "FLV", MP3: "MP3", OGG: "OGG", FLAC: "FLAC", AMR: "AMR", PDF: "PDF",
	ZIP: "ZIP/Office", OLE: "Old Office", RAR: "RAR", SEVENZ: "7Z", SQLITE: "SQLite",
	PSD: "PSD", MPEGPS: "MPEG", MPEGTS: "TS", M2TS: "MTS", GZIP: "GZIP", RTF: "RTF",
	TAR: "TAR", ISO9660: "ISO", MIDI: "MIDI", AIFF: "AIFF", DJVU: "DjVu", PST: "Outlook PST",
	DWG: "AutoCAD DWG", AAC: "AAC", EPS: "EPS", EPSBIN: "EPS", KDBX: "KeePass", RM: "RealMedia", CAB: "CAB",
}

func (k Kind) String() string {
	if int(k) < len(names) {
		return names[k]
	}
	return "unknown"
}

var (
	oleMagic = []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}
	asfGUID  = []byte{0x30, 0x26, 0xB2, 0x75, 0x8E, 0x66, 0xCF, 0x11, 0xA6, 0xD9, 0x00, 0xAA, 0x00, 0x62, 0xCE, 0x6C}
)

// ISOOffset is how far before the matching sector an ISO 9660 image starts.
const ISOOffset = 0x8000

// Detect reports which file type, if any, starts at b[0]. b should hold at
// least 512 bytes (the sector) and preferably a little more.
func Detect(b []byte) Kind {
	if len(b) < 512 {
		return None
	}
	switch b[0] {
	case 0xFF:
		if b[1] == 0xD8 && b[2] == 0xFF {
			m := b[3]
			if (m >= 0xE0 && m <= 0xEF) || m == 0xDB || m == 0xC0 || m == 0xC2 || m == 0xC4 || m == 0xFE || m == 0xDD {
				return JPEG
			}
		}
		if b[1]&0xF6 == 0xF0 && (b[2]>>2)&0xF < 12 && ADTSFrameLen(b) >= 7 {
			return AAC
		}
	case 0x89:
		if string(b[1:8]) == "PNG\r\n\x1a\n" {
			return PNG
		}
	case 'G': // 0x47 is also the MPEG transport stream sync byte
		if string(b[:6]) == "GIF87a" || string(b[:6]) == "GIF89a" {
			return GIF
		}
		if len(b) >= 4*188 && b[188] == 0x47 && b[376] == 0x47 && b[564] == 0x47 {
			return MPEGTS
		}
	case 'B':
		if b[1] == 'M' && bmpLooksValid(b) {
			return BMP
		}
	case 'I':
		if b[1] == 'I' {
			if b[2] == 0x2A && b[3] == 0 && tiffFirstIFDOK(b, binary.LittleEndian) {
				return TIFF
			}
			if b[2] == 'R' && (b[3] == 'O' || b[3] == 'S') {
				return ORF
			}
			if b[2] == 'U' && b[3] == 0 {
				return RW2
			}
		}
		if b[1] == 'D' && b[2] == '3' && b[3] >= 2 && b[3] <= 4 && b[4] != 0xFF &&
			(b[6]|b[7]|b[8]|b[9])&0x80 == 0 {
			return MP3
		}
	case 'M':
		if b[1] == 'M' && b[2] == 0 && b[3] == 0x2A && tiffFirstIFDOK(b, binary.BigEndian) {
			return TIFF
		}
		if string(b[:4]) == "MThd" && binary.BigEndian.Uint32(b[4:8]) == 6 {
			return MIDI
		}
		if string(b[:4]) == "MSCF" && binary.LittleEndian.Uint32(b[4:8]) == 0 && b[24] <= 3 {
			return CAB
		}
	case 'R':
		if string(b[:4]) == "RIFF" {
			switch string(b[8:12]) {
			case "AVI ", "WAVE", "WEBP", "RMID", "CDXA", "ACON", "QLCM":
				return RIFF
			}
			if string(b[8:11]) == "CDR" || string(b[8:11]) == "cdr" {
				return RIFF // CorelDRAW
			}
		}
		if string(b[:7]) == "Rar!\x1a\x07\x00" || string(b[:8]) == "Rar!\x1a\x07\x01\x00" {
			return RAR
		}
	case 0x1A:
		if b[1] == 0x45 && b[2] == 0xDF && b[3] == 0xA3 {
			return EBML
		}
	case 0x30:
		if bytes.Equal(b[:16], asfGUID) {
			return ASF
		}
	case 'F':
		if b[1] == 'L' && b[2] == 'V' && b[3] == 1 && b[4]&0xFA == 0 && binary.BigEndian.Uint32(b[5:9]) == 9 {
			return FLV
		}
		if string(b[:4]) == "FORM" && (string(b[8:12]) == "AIFF" || string(b[8:12]) == "AIFC") {
			return AIFF
		}
	case 'O':
		if string(b[:4]) == "OggS" && b[4] == 0 && b[5]&0x02 != 0 {
			return OGG
		}
	case 'f':
		if string(b[:4]) == "fLaC" && b[4]&0x7F == 0 && b[5] == 0 && b[6] == 0 && b[7] == 34 {
			return FLAC
		}
	case '#':
		if string(b[:6]) == "#!AMR\n" {
			return AMR
		}
	case '%':
		if string(b[:5]) == "%PDF-" {
			return PDF
		}
		if string(b[:11]) == "%!PS-Adobe-" {
			return EPS
		}
	case 'A':
		if string(b[:8]) == "AT&TFORM" {
			return DJVU
		}
		if string(b[:4]) == "AC10" && b[4] >= '0' && b[4] <= '3' && b[5] >= '0' && b[5] <= '9' && b[6] == 0 {
			return DWG
		}
	case '!':
		if string(b[:4]) == "!BDN" && string(b[8:10]) == "SM" {
			return PST
		}
	case 0xC5:
		if b[1] == 0xD0 && b[2] == 0xD3 && b[3] == 0xC6 {
			return EPSBIN
		}
	case 0x03:
		if b[1] == 0xD9 && b[2] == 0xA2 && b[3] == 0x9A && b[5] == 0xFB && b[6] == 0x4B && b[7] == 0xB5 {
			return KDBX
		}
	case '.':
		if string(b[:4]) == ".RMF" && binary.BigEndian.Uint32(b[4:8]) >= 18 && binary.BigEndian.Uint32(b[4:8]) < 64 {
			return RM
		}
	case 'P':
		if b[1] == 'K' && b[2] == 3 && b[3] == 4 {
			ver := binary.LittleEndian.Uint16(b[4:6])
			nameLen := binary.LittleEndian.Uint16(b[26:28])
			if ver <= 0x3F && nameLen > 0 && nameLen < 512 {
				return ZIP
			}
		}
	case 0xD0:
		if bytes.Equal(b[:8], oleMagic) {
			return OLE
		}
	case '7':
		if string(b[:6]) == "7z\xbc\xaf\x27\x1c" {
			return SEVENZ
		}
	case 'S':
		if string(b[:16]) == "SQLite format 3\x00" {
			return SQLITE
		}
	case '8':
		if string(b[:4]) == "8BPS" && (b[5] == 1 || b[5] == 2) && b[4] == 0 {
			return PSD
		}
	case 0x00:
		if b[1] == 0 && b[2] == 1 && b[3] == 0xBA {
			return MPEGPS
		}
	case 0x1F:
		if b[1] == 0x8B && b[2] == 8 && b[3]&0xE0 == 0 {
			return GZIP
		}
	case '{':
		if string(b[:5]) == "{\\rtf" {
			return RTF
		}
	case 0x01:
		if string(b[1:7]) == "CD001\x01" {
			return ISO9660
		}
	}
	// ISO base media (MP4 / MOV / 3GP / HEIC ...): size + "ftyp" + brand
	if b[4] == 'f' && b[5] == 't' && b[6] == 'y' && b[7] == 'p' {
		size := binary.BigEndian.Uint32(b[0:4])
		if size >= 12 && size <= 1024 && printable(b[8:12]) {
			return ISOBMFF
		}
	}
	// Camcorder AVCHD (.MTS): 4-byte timestamp + 0x47 every 192 bytes
	if len(b) >= 4*192+5 && b[4] == 0x47 && b[196] == 0x47 && b[388] == 0x47 && b[580] == 0x47 {
		return M2TS
	}
	// TAR (POSIX ustar)
	if string(b[257:262]) == "ustar" && b[0] != 0 {
		return TAR
	}
	return None
}

func printable(b []byte) bool {
	for _, c := range b {
		if c < 0x20 || c > 0x7E {
			return false
		}
	}
	return true
}

func bmpLooksValid(b []byte) bool {
	size := binary.LittleEndian.Uint32(b[2:6])
	dataOff := binary.LittleEndian.Uint32(b[10:14])
	dib := binary.LittleEndian.Uint32(b[14:18])
	switch dib {
	case 12, 40, 52, 56, 64, 108, 124:
	default:
		return false
	}
	if binary.LittleEndian.Uint32(b[6:10]) != 0 {
		return false
	}
	if dataOff < 14+dib || dataOff > 1<<20 {
		return false
	}
	if size != 0 && (size < dataOff || size > 1<<30) {
		return false
	}
	if dib >= 40 {
		w := int32(binary.LittleEndian.Uint32(b[18:22]))
		h := int32(binary.LittleEndian.Uint32(b[22:26]))
		bpp := binary.LittleEndian.Uint16(b[28:30])
		if w <= 0 || w > 100000 || h == 0 || h > 100000 || h < -100000 {
			return false
		}
		switch bpp {
		case 1, 4, 8, 16, 24, 32:
		default:
			return false
		}
	}
	return true
}

func tiffFirstIFDOK(b []byte, bo binary.ByteOrder) bool {
	off := bo.Uint32(b[4:8])
	return off >= 8 && off < 1<<28
}

// Ext gives a default file extension for a kind (the carver refines it).
func (k Kind) Ext() string {
	switch k {
	case JPEG:
		return "jpg"
	case PNG:
		return "png"
	case GIF:
		return "gif"
	case BMP:
		return "bmp"
	case TIFF:
		return "tif"
	case ORF:
		return "orf"
	case RW2:
		return "rw2"
	case ISOBMFF:
		return "mp4"
	case RIFF:
		return "avi"
	case EBML:
		return "mkv"
	case ASF:
		return "wmv"
	case FLV:
		return "flv"
	case MP3:
		return "mp3"
	case OGG:
		return "ogg"
	case FLAC:
		return "flac"
	case AMR:
		return "amr"
	case PDF:
		return "pdf"
	case ZIP:
		return "zip"
	case OLE:
		return "doc"
	case RAR:
		return "rar"
	case SEVENZ:
		return "7z"
	case SQLITE:
		return "sqlite"
	case PSD:
		return "psd"
	case MPEGPS:
		return "mpg"
	case MPEGTS:
		return "ts"
	case M2TS:
		return "mts"
	case GZIP:
		return "gz"
	case RTF:
		return "rtf"
	case TAR:
		return "tar"
	case ISO9660:
		return "iso"
	case MIDI:
		return "mid"
	case AIFF:
		return "aiff"
	case DJVU:
		return "djvu"
	case PST:
		return "pst"
	case DWG:
		return "dwg"
	case AAC:
		return "aac"
	case EPS, EPSBIN:
		return "eps"
	case KDBX:
		return "kdbx"
	case RM:
		return "rm"
	case CAB:
		return "cab"
	}
	return "bin"
}

// ADTSFrameLen returns the length of an AAC ADTS frame starting at b.
func ADTSFrameLen(b []byte) int {
	if len(b) < 7 || b[0] != 0xFF || b[1]&0xF6 != 0xF0 {
		return 0
	}
	return int(b[3]&3)<<11 | int(b[4])<<3 | int(b[5])>>5
}

// ExtMatches checks whether the first bytes of a file fit what its
// extension promises. It returns (match, known) - known is false when the
// extension is not one we can check.
func ExtMatches(ext string, head []byte) (match bool, known bool) {
	if len(head) < 16 {
		return false, false
	}
	k := Detect(pad512(head))
	switch ext {
	case "jpg", "jpeg", "jpe", "jfif":
		return k == JPEG, true
	case "png":
		return k == PNG, true
	case "gif":
		return k == GIF, true
	case "bmp":
		return k == BMP, true
	case "tif", "tiff", "cr2", "nef", "arw", "dng", "pef", "srw", "nrw":
		return k == TIFF, true
	case "mp4", "m4v", "m4a", "mov", "3gp", "3g2", "heic", "heif", "avif", "cr3":
		return k == ISOBMFF, true
	case "avi", "wav", "webp":
		return k == RIFF, true
	case "mkv", "webm":
		return k == EBML, true
	case "wmv", "wma", "asf":
		return k == ASF, true
	case "flv":
		return k == FLV, true
	case "pdf":
		return k == PDF, true
	case "docx", "xlsx", "pptx", "zip", "odt", "ods", "odp", "epub", "apk", "jar":
		return k == ZIP || (len(head) >= 4 && string(head[:4]) == "PK\x05\x06"), true
	case "doc", "xls", "ppt", "msg":
		// Very old or tiny Word/Excel files may not be OLE, but nearly all are.
		return k == OLE, true
	case "rar":
		return k == RAR, true
	case "7z":
		return k == SEVENZ, true
	case "psd":
		return k == PSD, true
	case "ogg", "opus", "oga":
		return k == OGG, true
	case "flac":
		return k == FLAC, true
	case "amr":
		return k == AMR, true
	case "rtf":
		return k == RTF, true
	case "mid", "midi":
		return k == MIDI, true
	case "aif", "aiff", "aifc":
		return k == AIFF, true
	case "djvu":
		return k == DJVU, true
	case "pst":
		return k == PST, true
	case "dwg":
		return k == DWG, true
	case "kdbx":
		return k == KDBX, true
	case "rm", "rmvb":
		return k == RM, true
	case "cab":
		return k == CAB, true
	case "eps":
		return k == EPS || k == EPSBIN, true
	case "cdr":
		return k == RIFF || k == ZIP, true
	case "aac":
		return k == AAC, true
	case "mp3":
		if k == MP3 {
			return true, true
		}
		// MP3 without ID3 tag starts with a frame sync
		return head[0] == 0xFF && head[1]&0xE0 == 0xE0, true
	}
	return false, false
}

func pad512(b []byte) []byte {
	if len(b) >= 1024 {
		return b
	}
	p := make([]byte, 1024)
	copy(p, b)
	return p
}

// KindOfExt maps a file extension to the signature its data starts with.
func KindOfExt(ext string) Kind {
	switch ext {
	case "jpg", "jpeg", "jpe", "jfif":
		return JPEG
	case "png":
		return PNG
	case "gif":
		return GIF
	case "bmp":
		return BMP
	case "tif", "tiff", "cr2", "nef", "arw", "dng", "pef", "srw":
		return TIFF
	case "mp4", "mov", "3gp", "3g2", "m4a", "m4v", "heic", "heif", "avif", "cr3":
		return ISOBMFF
	case "avi", "wav", "webp":
		return RIFF
	case "mkv", "webm":
		return EBML
	case "wmv", "wma", "asf":
		return ASF
	case "flv":
		return FLV
	case "mp3":
		return MP3
	case "ogg", "opus", "oga", "ogv":
		return OGG
	case "flac":
		return FLAC
	case "amr":
		return AMR
	case "pdf":
		return PDF
	case "docx", "xlsx", "pptx", "zip", "odt", "ods", "odp", "epub", "apk", "jar":
		return ZIP
	case "doc", "xls", "ppt", "msg":
		return OLE
	case "rar":
		return RAR
	case "7z":
		return SEVENZ
	case "psd":
		return PSD
	case "mpg", "mpeg", "vob":
		return MPEGPS
	case "ts":
		return MPEGTS
	case "mts", "m2ts":
		return M2TS
	case "gz", "tgz":
		return GZIP
	case "rtf":
		return RTF
	case "tar":
		return TAR
	case "iso":
		return ISO9660
	case "sqlite", "db":
		return SQLITE
	case "mid", "midi":
		return MIDI
	case "aif", "aiff", "aifc":
		return AIFF
	case "djvu", "djv":
		return DJVU
	case "pst", "ost":
		return PST
	case "dwg":
		return DWG
	case "aac":
		return AAC
	case "kdbx", "kdb":
		return KDBX
	case "rm", "rmvb":
		return RM
	case "cab":
		return CAB
	case "eps", "ps":
		return EPS
	}
	return None
}
