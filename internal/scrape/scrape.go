// Package scrape extracts video URLs, and links worth following, from HTML.
//
// Discovery is a set of independent strategies run over one parsed document.
// Every hit records the strategy that produced it, which is what makes tuning
// against a new site tractable: --verbose shows exactly which heuristic fired.
package scrape

import (
	"bytes"
	"net/url"
	"path"
	"strings"

	"golang.org/x/net/html"
)

// Video kinds.
const (
	KindDirect = "direct" // a media file we can download byte-for-byte
	KindStream = "stream" // an HLS or DASH manifest
	KindEmbed  = "embed"  // a player page; no media URL of its own
)

// Link kinds.
const (
	LinkAnchor = "anchor"
	LinkIframe = "iframe"
)

// Video is a discovered video URL.
type Video struct {
	URL      string `json:"url"`
	Source   string `json:"source"`   // page it was found on
	Kind     string `json:"kind"`     // direct | stream | embed
	Strategy string `json:"strategy"` // which heuristic found it
	Title    string `json:"title,omitempty"`
}

// Link is a page worth following when depth allows.
type Link struct {
	URL    string
	Source string
	Kind   string // anchor | iframe
	Title  string
}

// Result is the outcome of discovery over one document.
type Result struct {
	Videos []Video
	Links  []Link
}

// Options tunes which strategies run.
type Options struct {
	// ScriptScan enables the inline-<script> regex sweep. It is the noisiest
	// strategy, hence separately disableable.
	ScriptScan bool
}

// DefaultOptions enables every strategy.
func DefaultOptions() Options { return Options{ScriptScan: true} }

// directExts are containers we can fetch as a single file.
var directExts = map[string]bool{
	".mp4": true, ".m4v": true, ".webm": true, ".mov": true, ".mkv": true,
	".avi": true, ".ogv": true, ".flv": true, ".wmv": true, ".mpg": true,
	".mpeg": true, ".3gp": true, ".ts": true,
}

// streamExts are adaptive-streaming manifests.
var streamExts = map[string]bool{".m3u8": true, ".mpd": true}

// Classify reports the kind implied by a URL's file extension, or "" when the
// extension tells us nothing. Query strings and fragments are ignored, since
// media URLs routinely carry signing parameters.
func Classify(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	ext := strings.ToLower(path.Ext(u.Path))
	switch {
	case directExts[ext]:
		return KindDirect
	case streamExts[ext]:
		return KindStream
	default:
		return ""
	}
}

// IsHLS reports whether a URL points at an M3U8 playlist.
func IsHLS(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return strings.EqualFold(path.Ext(u.Path), ".m3u8")
}

// IsDASH reports whether a URL points at a DASH manifest.
func IsDASH(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return strings.EqualFold(path.Ext(u.Path), ".mpd")
}

// Discover parses body as HTML and runs every enabled strategy over it. All
// URLs are resolved against base, which must be the document's final URL.
func Discover(body []byte, base *url.URL, opts Options) (*Result, error) {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	d := &discoverer{
		base:      base,
		source:    base.String(),
		opts:      opts,
		seenVideo: make(map[string]bool),
		seenLink:  make(map[string]bool),
	}
	d.walk(doc)
	// The page title is a better fallback than nothing for videos found by
	// strategies with no local text to draw on, but it is only known after the
	// walk, so it is applied last. og:title is preferred over the document
	// title regardless of which appeared first in the markup: it is the label
	// the page author chose for sharing, and is usually cleaner.
	fallback := d.ogTitle
	if fallback == "" {
		fallback = d.docTitle
	}
	for i := range d.result.Videos {
		if d.result.Videos[i].Title == "" {
			d.result.Videos[i].Title = fallback
		}
	}
	return &d.result, nil
}

// discoverer accumulates results across one document walk.
type discoverer struct {
	base      *url.URL
	source    string
	opts      Options
	result    Result
	seenVideo map[string]bool
	seenLink  map[string]bool
	docTitle  string
	ogTitle   string
}

// resolve turns a possibly-relative reference into an absolute http(s) URL,
// dropping the fragment. It returns "" for anything we cannot or should not
// follow: mailto:, javascript:, data:, and empty or malformed references.
func (d *discoverer) resolve(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.HasPrefix(ref, "#") {
		return ""
	}
	u, err := d.base.Parse(ref)
	if err != nil {
		return ""
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	u.Fragment = ""
	return u.String()
}

// addVideo records a video, deduping on the resolved URL. The first strategy to
// find a URL keeps it, which is why strategies run in descending order of
// reliability.
func (d *discoverer) addVideo(rawRef, kind, strategy, title string) {
	abs := d.resolve(rawRef)
	if abs == "" || d.seenVideo[abs] {
		return
	}
	if kind == "" {
		if kind = Classify(abs); kind == "" {
			kind = KindEmbed
		}
	}
	d.seenVideo[abs] = true
	d.result.Videos = append(d.result.Videos, Video{
		URL:      abs,
		Source:   d.source,
		Kind:     kind,
		Strategy: strategy,
		Title:    strings.TrimSpace(title),
	})
}

func (d *discoverer) addLink(rawRef, kind, title string) {
	abs := d.resolve(rawRef)
	if abs == "" || d.seenLink[abs] {
		return
	}
	d.seenLink[abs] = true
	d.result.Links = append(d.result.Links, Link{
		URL:    abs,
		Source: d.source,
		Kind:   kind,
		Title:  strings.TrimSpace(title),
	})
}

// walk visits every node once, dispatching elements to the strategies that
// care about them.
func (d *discoverer) walk(n *html.Node) {
	if n.Type == html.ElementNode {
		d.visitElement(n)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		d.walk(c)
	}
}

// attr returns the value of the named attribute, and whether it was present.
func attr(n *html.Node, name string) (string, bool) {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, name) {
			return a.Val, true
		}
	}
	return "", false
}

// text returns the concatenated text content of a node, collapsed to a single
// line and truncated. Used for anchor titles.
func text(n *html.Node) string {
	var b strings.Builder
	var rec func(*html.Node)
	rec = func(x *html.Node) {
		if x.Type == html.TextNode {
			b.WriteString(x.Data)
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			rec(c)
		}
	}
	rec(n)
	s := strings.Join(strings.Fields(b.String()), " ")
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}
