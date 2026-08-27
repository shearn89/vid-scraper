package scrape

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
)

// sitemapSniffLimit caps how far IsSitemap looks for the root element. A
// sitemap can be 50MB, and the answer is always in the first few hundred bytes,
// so a document that has not revealed a root element by here is not one we want
// to keep scanning.
const sitemapSniffLimit = 4096

// IsSitemap reports whether a document looks like an XML sitemap, judged from
// its content type and leading bytes. Both must agree: servers hand out
// application/xml for RSS feeds and arbitrary payloads alike, so the root
// element is what actually settles it.
func IsSitemap(contentType string, body []byte) bool {
	if !strings.Contains(strings.ToLower(contentType), "xml") {
		return false
	}
	return hasSitemapRoot(body)
}

// hasSitemapRoot reports whether the first element in body is <urlset> or
// <sitemapindex>, with or without a namespace prefix. The prologue — a BOM, an
// <?xml?> declaration, comments and whitespace — is skipped, because all of it
// is optional in the wild and none of it identifies the document.
func hasSitemapRoot(body []byte) bool {
	b := bytes.TrimPrefix(body, []byte("\xef\xbb\xbf"))
	if len(b) > sitemapSniffLimit {
		b = b[:sitemapSniffLimit]
	}
	for {
		b = bytes.TrimLeft(b, " \t\r\n")
		switch {
		case bytes.HasPrefix(b, []byte("<?")):
			end := bytes.Index(b, []byte("?>"))
			if end < 0 {
				return false
			}
			b = b[end+2:]
		case bytes.HasPrefix(b, []byte("<!--")):
			end := bytes.Index(b, []byte("-->"))
			if end < 0 {
				return false
			}
			b = b[end+3:]
		default:
			return isSitemapRootTag(b)
		}
	}
}

// isSitemapRootTag reports whether b opens with a sitemap root element tag.
func isSitemapRootTag(b []byte) bool {
	if !bytes.HasPrefix(b, []byte("<")) {
		return false
	}
	rest := b[1:]
	// A namespace prefix may precede the name; only the local name identifies
	// the document. The colon is sought before any tag-ending byte so that
	// something like <rss version="x:y"> cannot be mistaken for a prefix.
	if i := bytes.IndexAny(rest, ":> \t\r\n/"); i >= 0 && i < len(rest) && rest[i] == ':' {
		rest = rest[i+1:]
	}
	for _, name := range []string{"urlset", "sitemapindex"} {
		if len(rest) < len(name) || !strings.EqualFold(string(rest[:len(name)]), name) {
			continue
		}
		// The name must end here, so that <urlsetfoo> does not match. A tag cut
		// off exactly at the end of the name is treated as a match: the sniff
		// buffer, not the document, is what ran out.
		if len(rest) == len(name) {
			return true
		}
		switch rest[len(name)] {
		case ' ', '\t', '\r', '\n', '>', '/':
			return true
		}
	}
	return false
}

// DiscoverSitemap parses an XML sitemap or sitemap index and returns every URL
// it lists as a Link, resolved against base.
//
// Both document types defined by sitemaps.org are handled, and both yield plain
// links: the entries of an index are further sitemaps, which the crawler
// follows at the next depth exactly as it follows any other link, so nothing
// needs fetching here.
func DiscoverSitemap(body []byte, base *url.URL) (*Result, error) {
	// resolve is a method on discoverer, but it reads nothing beyond the base
	// URL, so a bare value gives the XML path the same scheme filtering and
	// fragment stripping the HTML path uses.
	d := &discoverer{base: base, source: base.String()}

	dec := xml.NewDecoder(bytes.NewReader(body))
	var (
		res   Result
		seen  = make(map[string]bool)
		stack []string
		loc   strings.Builder
		inLoc bool
	)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			// A sitemap truncated in transit still lists real URLs in the part
			// that parsed, and those are worth crawling, so a cut-off document
			// that already yielded links is reported as a success. Anything
			// else — including truncation before the first entry — is a parse
			// failure. encoding/xml signals truncation only through the message
			// of a *xml.SyntaxError, which is why that is matched on.
			if len(res.Links) > 0 && isTruncatedXML(err) {
				break
			}
			return nil, fmt.Errorf("parsing sitemap: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			name := strings.ToLower(t.Name.Local)
			// Matching on the local name keeps the default, prefixed and
			// missing namespace forms all working. The parent is checked too,
			// because sitemap extensions nest their own elements inside <url>
			// and a bare "ends in loc" test would pick up video:content_loc.
			if name == "loc" && len(stack) > 0 {
				switch stack[len(stack)-1] {
				case "url", "sitemap":
					inLoc = true
					loc.Reset()
				}
			}
			stack = append(stack, name)
		case xml.CharData:
			if inLoc {
				loc.Write(t)
			}
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			if inLoc && strings.EqualFold(t.Name.Local, "loc") {
				inLoc = false
				abs := d.resolve(loc.String())
				if abs == "" || seen[abs] {
					continue
				}
				seen[abs] = true
				res.Links = append(res.Links, Link{
					URL:    abs,
					Source: d.source,
					Kind:   LinkAnchor,
				})
			}
		}
	}
	return &res, nil
}

// isTruncatedXML reports whether err is encoding/xml running out of input
// mid-document, as opposed to reading something malformed.
func isTruncatedXML(err error) bool {
	var se *xml.SyntaxError
	if !errors.As(err, &se) {
		return false
	}
	return se.Msg == "unexpected EOF"
}
