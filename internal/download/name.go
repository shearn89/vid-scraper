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
