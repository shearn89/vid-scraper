package scrape

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sitemapBase is the URL a sitemap is treated as having been fetched from, so
// relative <loc> values resolve against its directory.
const sitemapBase = "https://example.com/dir/sitemap.xml"

// discoverSitemap parses body against sitemapBase, failing the test on error.
func discoverSitemap(t *testing.T, body string) *Result {
	t.Helper()
	base, err := url.Parse(sitemapBase)
	if err != nil {
		t.Fatal(err)
	}
	res, err := DiscoverSitemap([]byte(body), base)
	if err != nil {
		t.Fatalf("DiscoverSitemap: %v", err)
	}
	return res
}

func assertOrder(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d links, want %d:\n got: %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("link %d:\n got: %s\nwant: %s", i, got[i], want[i])
		}
	}
}

func TestDiscoverSitemapShapes(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "urlset with the default namespace",
			body: `<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url><loc>https://example.com/a</loc><lastmod>2026-01-01</lastmod></url>
  <url><loc>https://example.com/b</loc></url>
</urlset>`,
			want: []string{"https://example.com/a", "https://example.com/b"},
		},
		{
			name: "urlset with a prefixed namespace",
			body: `<?xml version="1.0"?>
<sm:urlset xmlns:sm="http://www.sitemaps.org/schemas/sitemap/0.9">
  <sm:url><sm:loc>https://example.com/a</sm:loc></sm:url>
  <sm:url><sm:loc>https://example.com/b</sm:loc></sm:url>
</sm:urlset>`,
			want: []string{"https://example.com/a", "https://example.com/b"},
		},
		{
			name: "urlset with no namespace at all",
			body: `<urlset>
  <url><loc>https://example.com/a</loc></url>
  <url><loc>https://example.com/b</loc></url>
</urlset>`,
			want: []string{"https://example.com/a", "https://example.com/b"},
		},
		{
			name: "sitemapindex with the default namespace",
			body: `<?xml version="1.0" encoding="UTF-8"?>
<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <sitemap><loc>https://example.com/sitemap-1.xml</loc><lastmod>2026-01-01</lastmod></sitemap>
  <sitemap><loc>https://example.com/sitemap-2.xml</loc></sitemap>
</sitemapindex>`,
			want: []string{"https://example.com/sitemap-1.xml", "https://example.com/sitemap-2.xml"},
		},
		{
			name: "sitemapindex with a prefixed namespace",
			body: `<sm:sitemapindex xmlns:sm="http://www.sitemaps.org/schemas/sitemap/0.9">
  <sm:sitemap><sm:loc>https://example.com/sitemap-1.xml</sm:loc></sm:sitemap>
</sm:sitemapindex>`,
			want: []string{"https://example.com/sitemap-1.xml"},
		},
		{
			name: "relative and absolute locations resolve against the base",
			body: `<urlset>
  <url><loc>programs/one</loc></url>
  <url><loc>/programs/two</loc></url>
  <url><loc>//example.org/three</loc></url>
  <url><loc>https://other.example/four</loc></url>
</urlset>`,
			want: []string{
				"https://example.com/dir/programs/one",
				"https://example.com/programs/two",
				"https://example.org/three",
				"https://other.example/four",
			},
		},
		{
			name: "repeats are dropped and first-seen order preserved",
			body: `<urlset>
  <url><loc>https://example.com/c</loc></url>
  <url><loc>https://example.com/a</loc></url>
  <url><loc>https://example.com/c</loc></url>
  <url><loc>https://example.com/b</loc></url>
  <url><loc>https://example.com/a#top</loc></url>
</urlset>`,
			want: []string{
				"https://example.com/c",
				"https://example.com/a",
				"https://example.com/b",
			},
		},
		{
			name: "non-http schemes, empty and malformed locations are dropped",
			body: `<urlset>
  <url><loc>ftp://example.com/a</loc></url>
  <url><loc>mailto:someone@example.com</loc></url>
  <url><loc>javascript:void(0)</loc></url>
  <url><loc></loc></url>
  <url><loc>   </loc></url>
  <url><loc>://malformed</loc></url>
  <url><loc>https://example.com/keep</loc></url>
</urlset>`,
			want: []string{"https://example.com/keep"},
		},
		{
			name: "locations are trimmed and entity references decoded",
			body: `<urlset>
  <url>
    <loc>
      https://example.com/search?q=a&amp;p=1
    </loc>
  </url>
</urlset>`,
			want: []string{"https://example.com/search?q=a&p=1"},
		},
		{
			name: "extension elements nested in an entry are not mistaken for locations",
			body: `<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9"
        xmlns:video="http://www.google.com/schemas/sitemap-video/1.1">
  <url>
    <loc>https://example.com/watch</loc>
    <video:video>
      <video:content_loc>https://cdn.example.com/a.mp4</video:content_loc>
      <video:player_loc>https://example.com/player</video:player_loc>
    </video:video>
  </url>
</urlset>`,
			want: []string{"https://example.com/watch"},
		},
		{
			name: "empty urlset yields no links",
			body: `<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9"></urlset>`,
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := discoverSitemap(t, tt.body)
			if len(res.Videos) != 0 {
				t.Errorf("expected no videos, got %v", videoURLs(res))
			}
			assertOrder(t, linkURLs(res), tt.want)
		})
	}
}

func TestDiscoverSitemapLinkFields(t *testing.T) {
	res := discoverSitemap(t, `<urlset><url><loc>https://example.com/a</loc></url></urlset>`)
	if len(res.Links) != 1 {
		t.Fatalf("got %d links, want 1", len(res.Links))
	}
	got := res.Links[0]
	want := Link{URL: "https://example.com/a", Source: sitemapBase, Kind: LinkAnchor}
	if got != want {
		t.Errorf("link:\n got: %+v\nwant: %+v", got, want)
	}
}

func TestDiscoverSitemapMalformed(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"mismatched end tag", `<urlset><url><loc>https://example.com/a</loc></ur></url></urlset>`},
		{"broken attribute syntax", `<urlset & broken><url><loc>https://example.com/a</loc></url></urlset>`},
		{"truncated before any entry completes", `<urlset><url><loc>https://exa`},
	}
	base, err := url.Parse(sitemapBase)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := DiscoverSitemap([]byte(tt.body), base); err == nil {
				t.Error("expected an error, got nil")
			}
		})
	}
}

// TestDiscoverSitemapTruncated pins the deliberate asymmetry with the malformed
// cases: a body cut short mid-transfer keeps the links its intact prefix listed.
func TestDiscoverSitemapTruncated(t *testing.T) {
	res := discoverSitemap(t, `<urlset>
  <url><loc>https://example.com/a</loc></url>
  <url><loc>https://example.com/b</loc></url>
  <url><loc>https://example.com/c`)
	assertOrder(t, linkURLs(res), []string{"https://example.com/a", "https://example.com/b"})
}

func TestIsSitemap(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
		want        bool
	}{
		{
			name:        "urlset with a declaration",
			contentType: "application/xml",
			body:        "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<urlset xmlns=\"http://www.sitemaps.org/schemas/sitemap/0.9\">",
			want:        true,
		},
		{
			name:        "sitemapindex with a charset on the content type",
			contentType: "text/xml; charset=utf-8",
			body:        "<?xml version=\"1.0\"?><sitemapindex>",
			want:        true,
		},
		{
			name:        "leading BOM, whitespace and comments",
			contentType: "application/xml",
			body:        "\xef\xbb\xbf  <?xml version=\"1.0\"?>\n<!-- generated nightly -->\n<!--another-->\n  <urlset>",
			want:        true,
		},
		{
			name:        "prefixed namespace on the root element",
			contentType: "application/xml",
			body:        `<sm:urlset xmlns:sm="http://www.sitemaps.org/schemas/sitemap/0.9">`,
			want:        true,
		},
		{
			name:        "no declaration and no namespace",
			contentType: "application/xml",
			body:        "<urlset>\n<url><loc>https://example.com/</loc></url>\n</urlset>",
			want:        true,
		},
		{
			name:        "self-closing empty urlset",
			contentType: "application/xml",
			body:        `<urlset/>`,
			want:        true,
		},
		{
			name:        "an RSS feed",
			contentType: "application/rss+xml",
			body:        "<?xml version=\"1.0\"?><rss version=\"2.0\"><channel><title>x</title></channel></rss>",
			want:        false,
		},
		{
			name:        "an Atom feed",
			contentType: "application/atom+xml",
			body:        `<feed xmlns="http://www.w3.org/2005/Atom"><title>x</title></feed>`,
			want:        false,
		},
		{
			name:        "an arbitrary XML document",
			contentType: "application/xml",
			body:        "<?xml version=\"1.0\"?><catalog><item id=\"1\"/></catalog>",
			want:        false,
		},
		{
			name:        "HTML",
			contentType: "text/html; charset=utf-8",
			body:        "<!doctype html><html><head><title>x</title></head></html>",
			want:        false,
		},
		{
			name:        "XHTML, whose content type contains xml",
			contentType: "application/xhtml+xml",
			body:        "<?xml version=\"1.0\"?><html xmlns=\"http://www.w3.org/1999/xhtml\"><body/></html>",
			want:        false,
		},
		{
			name:        "a sitemap served with the wrong content type",
			contentType: "text/plain",
			body:        "<urlset>",
			want:        false,
		},
		{
			name:        "an element merely starting with urlset",
			contentType: "application/xml",
			body:        `<urlsetting foo="bar">`,
			want:        false,
		},
		{
			name:        "an unterminated comment hiding the root element",
			contentType: "application/xml",
			body:        "<!-- <urlset>",
			want:        false,
		},
		{
			name:        "empty body",
			contentType: "application/xml",
			body:        "",
			want:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsSitemap(tt.contentType, []byte(tt.body)); got != tt.want {
				t.Errorf("IsSitemap(%q, ...) = %v, want %v", tt.contentType, got, tt.want)
			}
		})
	}
}

// TestDiscoverSitemapFixture is the regression test against a whole-file
// sitemap: testdata/sitemap.xml carries 54 entries of which 46 are /programs/
// pages, in the shape a Uscreen storefront serves. The URLs are invented; the
// document structure, element order and counts are what the test pins.
func TestDiscoverSitemapFixture(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "sitemap.xml"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	if !IsSitemap("application/xml", body) {
		t.Error("IsSitemap = false for the real sitemap fixture")
	}
	base, err := url.Parse("https://farmvids.example/sitemap.xml")
	if err != nil {
		t.Fatal(err)
	}
	res, err := DiscoverSitemap(body, base)
	if err != nil {
		t.Fatalf("DiscoverSitemap: %v", err)
	}
	if len(res.Links) != 54 {
		t.Errorf("got %d links, want 54", len(res.Links))
	}
	programs := 0
	for _, l := range res.Links {
		if strings.Contains(l.URL, "/programs/") {
			programs++
		}
		if l.Kind != LinkAnchor {
			t.Errorf("link %s: kind = %q, want %q", l.URL, l.Kind, LinkAnchor)
		}
	}
	if programs != 46 {
		t.Errorf("got %d /programs/ links, want 46", programs)
	}
}
