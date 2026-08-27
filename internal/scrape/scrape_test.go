package scrape

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

const baseURL = "https://example.com/dir/page.html"

// load parses a fixture from testdata against baseURL.
func load(t *testing.T, name string) *Result {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Discover(body, base, DefaultOptions())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	return res
}

func videoURLs(r *Result) []string {
	out := make([]string, 0, len(r.Videos))
	for _, v := range r.Videos {
		out = append(out, v.URL)
	}
	return out
}

func linkURLs(r *Result) []string {
	out := make([]string, 0, len(r.Links))
	for _, l := range r.Links {
		out = append(out, l.URL)
	}
	return out
}

func assertSameSet(t *testing.T, got, want []string) {
	t.Helper()
	gotSet := map[string]bool{}
	for _, g := range got {
		gotSet[g] = true
	}
	wantSet := map[string]bool{}
	for _, w := range want {
		wantSet[w] = true
	}
	for _, w := range want {
		if !gotSet[w] {
			t.Errorf("missing expected URL:\n  %s", w)
		}
	}
	for _, g := range got {
		if !wantSet[g] {
			t.Errorf("unexpected URL:\n  %s", g)
		}
	}
}

func TestDiscoverStrategies(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		videos  []string
	}{
		{
			name:    "media tags, including lazy-load attributes and nested sources",
			fixture: "media-tag.html",
			videos: []string{
				"https://example.com/clips/intro.mp4",
				"https://example.com/dir/lazy/loaded.webm",
				"https://cdn.example.net/hi.mp4",
				"https://example.com/rel/lo.webm",
				"https://example.com/legacy/movie.mov",
				"https://example.com/legacy/old.flv",
			},
		},
		{
			name:    "anchors classified by extension, ignoring non-http schemes",
			fixture: "anchor.html",
			videos: []string{
				"https://example.com/files/lecture.mp4",
				"https://cdn.example.net/talk.mkv?sig=abc123",
				"https://example.com/stream/live.m3u8",
				"https://example.com/manifest/dash.mpd",
			},
		},
		{
			name:    "open graph and twitter card properties",
			fixture: "meta.html",
			videos: []string{
				"https://cdn.example.net/og.mp4",
				"https://cdn.example.net/og-secure.mp4",
				"https://example.com/twitter/player.mp4",
			},
		},
		{
			name:    "json-ld VideoObject inside @graph and nested arrays",
			fixture: "json-ld.html",
			videos: []string{
				"https://cdn.example.net/keynote.mp4",
				"https://player.example.net/embed/keynote",
				"https://example.com/relative/nested.webm",
			},
		},
		{
			name:    "inline script urls, both escaped and plain",
			fixture: "script.html",
			videos: []string{
				"https://cdn.example.net/escaped/clip.mp4",
				"https://cdn.example.net/plain/clip2.webm",
				"https://cdn.example.net/stream/index.m3u8",
			},
		},
		{
			name:    "iframe pointing straight at media is a video, not a link",
			fixture: "iframe.html",
			videos:  []string{"https://cdn.example.net/direct.mp4"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertSameSet(t, videoURLs(load(t, tc.fixture)), tc.videos)
		})
	}
}

// TestClassification checks each video is tagged with the right kind, since
// kind is what routes a URL to the direct downloader, the HLS downloader, or
// the skip path.
func TestClassification(t *testing.T) {
	byURL := map[string]Video{}
	for _, v := range load(t, "anchor.html").Videos {
		byURL[v.URL] = v
	}
	want := map[string]string{
		"https://example.com/files/lecture.mp4":       KindDirect,
		"https://cdn.example.net/talk.mkv?sig=abc123": KindDirect,
		"https://example.com/stream/live.m3u8":        KindStream,
		"https://example.com/manifest/dash.mpd":       KindStream,
	}
	for u, kind := range want {
		v, ok := byURL[u]
		if !ok {
			t.Fatalf("expected to find %s", u)
		}
		if v.Kind != kind {
			t.Errorf("%s: kind = %q, want %q", u, v.Kind, kind)
		}
	}
}

// TestEmbedKind covers a JSON-LD embedUrl with no media extension: it is a
// player page, so it must not be handed to a downloader as if it were a file.
func TestEmbedKind(t *testing.T) {
	for _, v := range load(t, "json-ld.html").Videos {
		if v.URL == "https://player.example.net/embed/keynote" && v.Kind != KindEmbed {
			t.Errorf("embedUrl kind = %q, want %q", v.Kind, KindEmbed)
		}
	}
}

// TestLinksForCrawling checks that non-video anchors and iframes are offered to
// the crawler, and that unfollowable schemes are not.
func TestLinksForCrawling(t *testing.T) {
	assertSameSet(t, linkURLs(load(t, "anchor.html")), []string{
		"https://example.com/videos/detail-page",
		"https://other.example.org/elsewhere",
	})

	res := load(t, "iframe.html")
	assertSameSet(t, linkURLs(res), []string{
		"https://player.example.net/embed/abc",
		"https://example.com/local/player?id=7",
	})
	for _, l := range res.Links {
		if l.Kind != LinkIframe {
			t.Errorf("%s: kind = %q, want %q", l.URL, l.Kind, LinkIframe)
		}
	}
}

// TestDedupeAcrossStrategies proves one URL found by four strategies is
// reported once, and that the earliest-parsed strategy is the one credited.
func TestDedupeAcrossStrategies(t *testing.T) {
	res := load(t, "combined.html")
	urls := videoURLs(res)
	if len(urls) != 1 {
		t.Fatalf("got %d videos, want 1: %v", len(urls), urls)
	}
	if urls[0] != "https://cdn.example.net/same.mp4" {
		t.Errorf("url = %q", urls[0])
	}
	if res.Videos[0].Strategy != StrategyMeta {
		t.Errorf("strategy = %q, want %q (meta is in <head>, parsed first)",
			res.Videos[0].Strategy, StrategyMeta)
	}
}

// TestScriptScanDisabled checks --no-script-scan suppresses the
// lowest-confidence strategy and nothing else.
func TestScriptScanDisabled(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "script.html"))
	if err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse(baseURL)
	res, err := Discover(body, base, Options{ScriptScan: false})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Videos) != 0 {
		t.Errorf("expected no videos with ScriptScan off, got %v", videoURLs(res))
	}
}

// TestTitles checks the fallbacks that give a discovered video a human label.
func TestTitles(t *testing.T) {
	for _, v := range load(t, "anchor.html").Videos {
		if v.URL == "https://example.com/files/lecture.mp4" && v.Title != "Lecture recording" {
			t.Errorf("anchor title = %q, want %q", v.Title, "Lecture recording")
		}
	}
	// og:title wins over <title> as the page-level fallback.
	for _, v := range load(t, "meta.html").Videos {
		if v.Title != "The Real Title" {
			t.Errorf("title = %q, want %q", v.Title, "The Real Title")
		}
	}
}

func TestClassify(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://x/a.mp4", KindDirect},
		{"https://x/a.MP4", KindDirect},
		{"https://x/a.mp4?token=1&exp=2", KindDirect},
		{"https://x/a.m3u8", KindStream},
		{"https://x/a.mpd", KindStream},
		{"https://x/page", ""},
		{"https://x/image.jpg", ""},
		{"https://x/a.mp4#t=10", KindDirect},
	}
	for _, tc := range tests {
		if got := Classify(tc.in); got != tc.want {
			t.Errorf("Classify(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestRelativeResolution checks URLs resolve against the document's own URL,
// which is the final URL after redirects, not the one originally requested.
func TestRelativeResolution(t *testing.T) {
	base, _ := url.Parse("https://cdn.example.net/deep/nested/page.html")
	res, err := Discover([]byte(`<video src="../up.mp4"></video>`), base, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	want := "https://cdn.example.net/deep/up.mp4"
	if got := videoURLs(res); len(got) != 1 || got[0] != want {
		t.Errorf("got %v, want [%s]", got, want)
	}
}
