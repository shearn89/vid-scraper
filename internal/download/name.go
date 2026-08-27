package download

import (
	"crypto/sha256"
	"encoding/hex"
	"mime"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

// maxNameLen keeps derived names well inside the 255-byte limit common to
// Linux and macOS filesystems, with room for a collision suffix.
const maxNameLen = 180

// contentTypeExt maps response content types onto an extension, used only when
// the URL itself carries none.
var contentTypeExt = map[string]string{
	"video/mp4":                ".mp4",
	"video/webm":               ".webm",
	"video/quicktime":          ".mov",
	"video/x-matroska":         ".mkv",
	"video/x-msvideo":          ".avi",
	"video/ogg":                ".ogv",
	"video/mpeg":               ".mpeg",
	"video/x-flv":              ".flv",
	"video/mp2t":               ".ts",
	"application/octet-stream": ".bin",
}

// NameFromURL derives a filename from a URL's path. Falls back to the host
// plus a hash when the path carries nothing usable, so every URL yields a
// stable, distinct name.
func NameFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "video-" + shortHash(rawURL)
	}
	base := path.Base(u.Path)
	if unescaped, err := url.PathUnescape(base); err == nil {
		base = unescaped
	}
	base = Sanitize(base)
	if base == "" || base == "." || base == ".." {
		host := Sanitize(u.Host)
		if host == "" {
			host = "video"
		}
		return host + "-" + shortHash(rawURL)
	}
	return base
}

// Sanitize strips path separators, control characters and characters that are
// reserved on common filesystems, so a name derived from a remote URL can never
// escape the output directory or produce an unopenable file.
func Sanitize(name string) string {
	name = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20 || r == 0x7f:
			return -1
		case strings.ContainsRune(`/\:*?"<>|`, r):
			return '-'
		default:
			return r
		}
	}, name)
	name = strings.Trim(name, " .")
	if len(name) > maxNameLen {
		// Trim the stem, never the extension.
		ext := path.Ext(name)
		if len(ext) > 12 {
			ext = ""
		}
		keep := maxNameLen - len(ext)
		name = strings.Trim(name[:keep], " .") + ext
	}
	return name
}

// EnsureExt appends an extension derived from the content type when the name
// has none, so downloads land as playable files rather than extensionless blobs.
func EnsureExt(name, contentType string) string {
	if path.Ext(name) != "" {
		return name
	}
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return name
	}
	if ext, ok := contentTypeExt[strings.ToLower(mt)]; ok && ext != ".bin" {
		return name + ext
	}
	return name
}

// NameFromDisposition extracts a filename from a Content-Disposition header,
// returning "" when the header is absent or carries no usable name.
func NameFromDisposition(header string) string {
	if header == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(header)
	if err != nil {
		return ""
	}
	return Sanitize(params["filename"])
}

// UniquePath returns a path in dir for name that does not yet exist, appending
// -1, -2 and so on before the extension on collision.
func UniquePath(dir, name string) string {
	candidate := filepath.Join(dir, name)
	if !exists(candidate) {
		return candidate
	}
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; ; i++ {
		candidate = filepath.Join(dir, stem+"-"+strconv.Itoa(i)+ext)
		if !exists(candidate) {
			return candidate
		}
	}
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// shortHash gives a stable, filename-safe discriminator for a URL.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:4])
}

// mediaExts are the extensions ExistingFile peels off when comparing a file
// against a stem. HLS downloads land as "name.ts.mp4", so peeling repeats.
var mediaExts = map[string]bool{
	".mp4":  true,
	".ts":   true,
	".mkv":  true,
	".webm": true,
	".mov":  true,
	".m4v":  true,
	".avi":  true,
}

// asciiFold maps accented Latin letters onto plain ASCII. Hand-rolled rather
// than depending on golang.org/x/text: full Unicode normalisation is a heavy
// dependency for the handful of runes a video title actually carries.
var asciiFold = foldTable(map[string]string{
	"àáâãäåāăą":  "a",
	"æ":          "ae",
	"çćĉċč":      "c",
	"ďđ":         "d",
	"èéêëēĕėęě":  "e",
	"ĝğġģ":       "g",
	"ĥħ":         "h",
	"ìíîïĩīĭįı":  "i",
	"ĵ":          "j",
	"ķ":          "k",
	"ĺļľŀł":      "l",
	"ñńņňŉ":      "n",
	"òóôõöøōŏő":  "o",
	"œ":          "oe",
	"ŕŗř":        "r",
	"śŝşš":       "s",
	"ß":          "ss",
	"ţťŧ":        "t",
	"þ":          "th",
	"ùúûüũūŭůűų": "u",
	"ŵ":          "w",
	"ýÿŷ":        "y",
	"źżž":        "z",
})

// foldTable expands the grouped fold spec into a per-rune lookup, adding each
// key's uppercase form so callers need not case-fold first.
func foldTable(groups map[string]string) map[rune]string {
	table := make(map[rune]string)
	for src, dst := range groups {
		for _, r := range src {
			table[r] = dst
			table[unicode.ToUpper(r)] = dst
		}
	}
	return table
}

// NameFromTitle converts a human video title into a filename stem: lowercase,
// ASCII, hyphen-separated. "Chip Chipping" -> "chip-chipping". Returns "" when
// the title yields nothing usable. CDN-hosted playlists carry opaque IDs in
// their URLs, so the page's title is the only name a human would recognise.
func NameFromTitle(title string) string {
	var b strings.Builder
	b.Grow(len(title))
	// Separators are held back and emitted only before the next kept rune, so
	// leading, trailing and repeated runs collapse without a second pass.
	sep := false
	for _, r := range title {
		// Apostrophes vanish rather than splitting a word: the library holds
		// "its-sow-time", not "it-s-sow-time".
		if r == '\'' || r == '’' {
			continue
		}
		folded, ok := asciiFold[r]
		if !ok {
			folded = string(r)
		}
		for _, c := range folded {
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
				if sep && b.Len() > 0 {
					b.WriteByte('-')
				}
				sep = false
				b.WriteByte(byte(c))
				continue
			}
			sep = true
		}
	}
	// Sanitize applies the shared truncation policy; that cut can land on a
	// hyphen, hence the trim.
	return strings.Trim(Sanitize(b.String()), "-")
}

// ExistingFile reports the path of a file in dir that already holds the video
// named by stem, ignoring extension differences, and whether one was found. It
// exists so a download can be skipped when the library already has the video
// under a slightly different extension.
func ExistingFile(dir, stem string) (string, bool) {
	target := strings.ToLower(stem)
	if target == "" {
		return "", false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := strings.ToLower(e.Name())
		// A partial download is not the video; matching one would skip a real
		// download and leave the library with a truncated file.
		if strings.HasSuffix(name, ".part") || strings.HasSuffix(name, ".parts") {
			continue
		}
		// Whole-stem equality only: "munchy-crunchy-1" is a collision-suffixed
		// sibling, a different video, not this one.
		if trimMediaExts(name) == target {
			return filepath.Join(dir, e.Name()), true
		}
	}
	return "", false
}

// trimMediaExts strips every trailing media extension, so "clip.ts.mp4",
// "clip.ts" and "clip.mp4" all reduce to "clip".
func trimMediaExts(name string) string {
	for {
		ext := path.Ext(name)
		if ext == "" || !mediaExts[ext] {
			return name
		}
		name = strings.TrimSuffix(name, ext)
	}
}
