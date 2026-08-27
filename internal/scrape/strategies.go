package scrape

import (
	"encoding/json"
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

// Strategy names, reported on every hit so a miss can be diagnosed.
const (
	StrategyMediaTag = "media-tag"
	StrategyAnchor   = "anchor"
	StrategyMeta     = "meta"
	StrategyJSONLD   = "json-ld"
	StrategyScript   = "script"
	StrategyIframe   = "iframe"
	StrategyFrame    = "turbo-frame"
)

// mediaAttrs are the attributes that carry a media URL on a media element.
// The data-* variants cover lazy-loading players, which leave src empty until
// JavaScript runs but ship the real URL in the markup.
var mediaAttrs = []string{
	"src", "data-src", "data-video-src", "data-mp4", "data-video",
	"data-url", "data-file", "data-source",
}

// metaVideoProps are the meta property/name values that carry a video URL.
var metaVideoProps = map[string]bool{
	"og:video":              true,
	"og:video:url":          true,
	"og:video:secure_url":   true,
	"twitter:player:stream": true,
}

// visitElement dispatches one element to whichever strategies apply.
func (d *discoverer) visitElement(n *html.Node) {
	switch strings.ToLower(n.Data) {
	case "title":
		if d.docTitle == "" {
			d.docTitle = text(n)
		}
	case "h1":
		if d.h1Title == "" {
			d.h1Title = text(n)
		}
	case "video", "audio", "embed":
		d.mediaTag(n)
	case "source":
		// <source> also appears inside <picture>, where it carries images.
		if p := n.Parent; p != nil && p.Type == html.ElementNode {
			switch strings.ToLower(p.Data) {
			case "video", "audio":
				d.mediaTag(n)
			}
		}
	case "object":
		if v, ok := attr(n, "data"); ok {
			d.addVideo(v, "", StrategyMediaTag, "")
		}
	case "a":
		d.anchor(n)
	case "meta":
		d.meta(n)
	case "iframe":
		d.iframe(n)
	case "turbo-frame":
		d.turboFrame(n)
	case "script":
		d.script(n)
	}
}

// mediaTag handles <video>, <audio>, <source> and <embed>.
func (d *discoverer) mediaTag(n *html.Node) {
	title, _ := attr(n, "title")
	for _, a := range mediaAttrs {
		if v, ok := attr(n, a); ok && strings.TrimSpace(v) != "" {
			d.addVideo(v, "", StrategyMediaTag, title)
		}
	}
}

// anchor treats a link as a video when its extension says so, and otherwise
// hands it to the crawler as a page that might contain one.
func (d *discoverer) anchor(n *html.Node) {
	href, ok := attr(n, "href")
	if !ok {
		return
	}
	abs := d.resolve(href)
	if abs == "" {
		return
	}
	title, rank := anchorTitle(n)
	if kind := Classify(abs); kind != "" {
		d.addVideo(href, kind, StrategyAnchor, title)
		return
	}
	d.addLink(href, LinkAnchor, title, rank)
}

// anchorTitle picks the best label available for a link, and reports where it
// came from so a repeat sighting of the same href can be compared against it.
func anchorTitle(n *html.Node) (string, titleRank) {
	for _, a := range []string{"title", "aria-label"} {
		if v, ok := attr(n, a); ok && strings.TrimSpace(v) != "" {
			return v, titleAttr
		}
	}
	if alt := descendantImgAlt(n); alt != "" {
		return alt, titleAlt
	}
	return text(n), titleText
}

// descendantImgAlt returns the alt text of the first image inside n, if any.
func descendantImgAlt(n *html.Node) string {
	var found string
	var rec func(*html.Node)
	rec = func(x *html.Node) {
		if found != "" {
			return
		}
		if x.Type == html.ElementNode && strings.EqualFold(x.Data, "img") {
			if v, ok := attr(x, "alt"); ok && strings.TrimSpace(v) != "" {
				found = strings.TrimSpace(v)
				return
			}
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			rec(c)
		}
	}
	rec(n)
	return found
}

// meta reads Open Graph and Twitter card video properties.
func (d *discoverer) meta(n *html.Node) {
	key, ok := attr(n, "property")
	if !ok {
		if key, ok = attr(n, "name"); !ok {
			return
		}
	}
	content, ok := attr(n, "content")
	if !ok || strings.TrimSpace(content) == "" {
		return
	}
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "og:title" {
		if d.ogTitle == "" {
			d.ogTitle = strings.TrimSpace(content)
		}
		return
	}
	if metaVideoProps[key] {
		d.addVideo(content, "", StrategyMeta, "")
	}
}

// iframe records the embedded document as a high-priority page to follow. This
// is the common "the video lives on another page" case: the player is the
// iframe's document, not the containing page.
func (d *discoverer) iframe(n *html.Node) {
	src, ok := attr(n, "src")
	if !ok {
		if src, ok = attr(n, "data-src"); !ok {
			return
		}
	}
	abs := d.resolve(src)
	if abs == "" {
		return
	}
	// An iframe pointing straight at a media file is a video, not a page.
	if kind := Classify(abs); kind != "" {
		d.addVideo(src, kind, StrategyIframe, "")
		return
	}
	title, _ := attr(n, "title")
	d.addLink(src, LinkIframe, title, titleAttr)
}

// turboFrame records a Hotwire <turbo-frame src> as a document to follow.
//
// A turbo-frame with a src is empty markup until the browser fetches that URL
// and swaps the response in. Server-rendered HTML therefore contains the
// frame's address but none of its content, which is why a page can look like
// it has no videos while displaying a full catalogue. Frames without a src
// are already-rendered content and carry nothing to follow.
func (d *discoverer) turboFrame(n *html.Node) {
	src, ok := attr(n, "src")
	if !ok || strings.TrimSpace(src) == "" {
		return
	}
	abs := d.resolve(src)
	if abs == "" {
		return
	}
	// A frame addressing a media file directly is a video, not a document.
	if kind := Classify(abs); kind != "" {
		d.addVideo(src, kind, StrategyFrame, "")
		return
	}
	id, _ := attr(n, "id")
	d.addLink(src, LinkFrame, id, titleNone)
}

// script handles both JSON-LD blocks and the inline-script regex sweep.
func (d *discoverer) script(n *html.Node) {
	body := rawText(n)
	if strings.TrimSpace(body) == "" {
		return
	}
	if t, ok := attr(n, "type"); ok && strings.Contains(strings.ToLower(t), "ld+json") {
		d.jsonLD(body)
		return
	}
	if d.opts.ScriptScan {
		d.scriptScan(body)
	}
}

// jsonLD walks a schema.org payload for video URLs. The structure is decoded
// into any rather than a fixed struct because these payloads are routinely
// arrays, or wrapped in @graph, or nested inside unrelated entities.
func (d *discoverer) jsonLD(body string) {
	var doc any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		return
	}
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			name, _ := t["name"].(string)
			for _, key := range []string{"contentUrl", "embedUrl"} {
				if s, ok := t[key].(string); ok && s != "" {
					kind := ""
					if key == "embedUrl" {
						// An embedUrl is a player page unless the extension
						// proves it is a media file.
						if kind = Classify(s); kind == "" {
							kind = KindEmbed
						}
					}
					d.addVideo(s, kind, StrategyJSONLD, name)
				}
			}
			for _, child := range t {
				walk(child)
			}
		case []any:
			for _, child := range t {
				walk(child)
			}
		}
	}
	walk(doc)
}

// scriptURLPattern matches absolute media URLs inside JavaScript. Regex is the
// right tool here precisely because the content is not markup: player configs
// embed URLs in JSON or string literals with no structure to parse. The script
// body is unescaped before matching (see unescapeJS), so this pattern only ever
// sees plain URLs.
var scriptURLPattern = regexp.MustCompile(
	"(?i)https?://[^\\s\"'`<>(){}\\[\\],;\\\\]{1,900}?\\.(?:mp4|m4v|webm|mov|mkv|ogv|flv|m3u8|mpd)(?:\\?[^\\s\"'`<>(){}\\[\\],;\\\\]{0,900})?",
)

// jsUnescaper reverses the escape forms that wrap URLs in embedded JSON and JS
// string literals. Normalising the whole body before matching is what lets one
// simple pattern handle "https:\/\/x/a.mp4" and "https://x/a.mp4"
// alike, rather than the pattern having to anticipate each encoding.
var jsUnescaper = strings.NewReplacer(
	`\/`, `/`,
	`\u002F`, `/`,
	`\u002f`, `/`,
	`\x2F`, `/`,
	`\x2f`, `/`,
	`\u0026`, `&`,
	`\u003D`, `=`,
	`\u003d`, `=`,
	`\u003F`, `?`,
	`\u003f`, `?`,
	`\u003A`, `:`,
	`\u003a`, `:`,
)

// scriptScan sweeps a script body for media URLs. It is the lowest-confidence
// strategy and is reported as such, so noisy hits can be identified and the
// sweep disabled with --no-script-scan.
func (d *discoverer) scriptScan(body string) {
	for _, m := range scriptURLPattern.FindAllString(jsUnescaper.Replace(body), -1) {
		// Trailing punctuation swept up from the surrounding source.
		u := strings.TrimRight(m, `.,'"`)
		if Classify(u) == "" {
			continue
		}
		d.addVideo(u, "", StrategyScript, "")
	}
}

// rawText returns a script element's literal contents. The HTML parser stores
// script bodies as a single raw text child.
func rawText(n *html.Node) string {
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
		}
	}
	return b.String()
}
