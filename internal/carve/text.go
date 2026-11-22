package carve

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

var (
	reHTMLTitle = regexp.MustCompile(`(?is)<title[^>]*>\s*([^<]{2,100}?)\s*</title>`)
	reSubject   = regexp.MustCompile(`(?im)^Subject:\s*(.{2,100})$`)
	reVCardName = regexp.MustCompile(`(?im)^FN[;:][^:\r\n]*?:?([^\r\n]{2,60})$`)
)

// DecodeText turns the start of a text file into a Go string.
func DecodeText(b []byte, wide bool) string {
	if !wide {
		if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
			b = b[3:]
		}
		for len(b) > 0 && !utf8.Valid(b) {
			b = b[:len(b)-1]
		}
		return string(b)
	}
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		c := uint16(b[i]) | uint16(b[i+1])<<8
		if c == 0xFEFF && i == 0 {
			continue
		}
		u = append(u, c)
	}
	return string(utf16.Decode(u))
}

// TextInfo works out what kind of text file this is (by its first few KB)
// and a title to name it by.
func TextInfo(head []byte, wide bool) (ext, title string) {
	t := strings.TrimLeft(DecodeText(head, wide), " \t\r\n\uFEFF")
	low := strings.ToLower(firstN(t, 4096))
	switch {
	case strings.HasPrefix(low, "<?xml") && strings.Contains(low, "<svg"):
		ext = "svg"
	case strings.HasPrefix(low, "<?xml") && strings.Contains(low, "<html"):
		ext = "html"
	case strings.HasPrefix(low, "<?xml"):
		ext = "xml"
	case strings.HasPrefix(low, "<!doctype html") || strings.HasPrefix(low, "<html") || strings.Contains(firstN(low, 512), "<head>"):
		ext = "html"
	case strings.HasPrefix(low, "<svg"):
		ext = "svg"
	case strings.HasPrefix(low, "begin:vcard"):
		ext = "vcf"
	case strings.HasPrefix(low, "begin:vcalendar"):
		ext = "ics"
	case strings.HasPrefix(low, "<?php"):
		ext = "php"
	case strings.HasPrefix(low, "#!") && strings.Contains(firstLine(low), "python"):
		ext = "py"
	case strings.HasPrefix(low, "#!"):
		ext = "sh"
	case (strings.HasPrefix(low, "{") || strings.HasPrefix(low, "[")) && (strings.Contains(low, "\":") || strings.Contains(low, "\",")):
		ext = "json"
	case isEmail(low):
		ext = "eml"
	case isCSV(t):
		ext = "csv"
	case strings.Contains(low, "#include <") || strings.Contains(low, "int main("):
		ext = "c"
	case strings.Contains(low, "public class ") || strings.Contains(low, "public static void main"):
		ext = "java"
	case strings.Contains(low, "\ndef ") || strings.HasPrefix(low, "import ") && strings.Contains(low, "def "):
		ext = "py"
	default:
		ext = "txt"
	}
	switch ext {
	case "html":
		if m := reHTMLTitle.FindStringSubmatch(t); m != nil {
			title = m[1]
		}
	case "eml":
		if m := reSubject.FindStringSubmatch(firstN(t, 8192)); m != nil {
			title = m[1]
		}
	case "vcf":
		n := strings.Count(strings.ToUpper(t), "BEGIN:VCARD")
		if n > 1 {
			title = "Contacts"
		} else if m := reVCardName.FindStringSubmatch(t); m != nil {
			title = "Contact " + m[1]
		}
	case "txt":
		title = firstLine(t)
	}
	return ext, cleanLine(title)
}

func firstN(s string, n int) string {
	if len(s) > n {
		s = s[:n]
		for len(s) > 0 && !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s
}

func firstLine(s string) string {
	for _, l := range strings.SplitN(firstN(s, 2048), "\n", 8) {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

// cleanLine keeps a title only if it reads like words.
func cleanLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 50 {
		s = strings.TrimSpace(string(r[:50]))
	}
	letters, others := 0, 0
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ':
			letters++
		default:
			others++
		}
	}
	if len([]rune(s)) < 3 || letters < 3 || others*3 > letters {
		return ""
	}
	return s
}

func isEmail(low string) bool {
	head := firstN(low, 4096)
	n := 0
	for _, k := range []string{"\nfrom:", "\nto:", "\nsubject:", "\ndate:", "\nreceived:", "\nmime-version:", "\nmessage-id:"} {
		if strings.Contains("\n"+head, k) {
			n++
		}
	}
	return n >= 3
}

func isCSV(t string) bool {
	lines := strings.SplitN(firstN(t, 4096), "\n", 8)
	if len(lines) < 4 {
		return false
	}
	for _, sep := range []string{",", ";", "\t"} {
		want := strings.Count(lines[0], sep)
		if want < 1 {
			continue
		}
		ok := true
		for _, l := range lines[1 : len(lines)-1] {
			if strings.Count(l, sep) != want {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
