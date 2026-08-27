package scrape

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
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

// TestUscreenCategory covers a Uscreen storefront catalogue listing, where each
// card wraps one destination in two anchors: a thumbnail and a caption. The
// listing itself holds no media URLs — the videos are one hop away on program
// pages — so the useful output is links, correctly labelled.
func TestUscreenCategory(t *testing.T) {
	res := load(t, "uscreen-category.html")

	if len(res.Videos) != 0 {
		t.Errorf("expected no videos on a catalogue listing, got %d", len(res.Videos))
	}

	assertSameSet(t, linkURLs(res), []string{
		"https://example.com/programs/ff-its-seed-time_master_stereo-b2c3d4?category_id=100200",
		"https://example.com/programs/silo-surprise-master-1-c3d4e5?category_id=100200",
		"https://example.com/programs/ff-flat-tyre_master_2k-mp4-a1b2c3?category_id=100200",
		"https://example.com/programs/ff-winter-special_master_2k-mp4-d4e5f6?category_id=100200",
	})

	// The thumbnail anchor precedes the caption anchor and contains only the
	// duration badge. Taking the first label seen would name every video after
	// its runtime.
	want := map[string]string{
		"https://example.com/programs/ff-its-seed-time_master_stereo-b2c3d4?category_id=100200":  "It's Seed Time",
		"https://example.com/programs/silo-surprise-master-1-c3d4e5?category_id=100200":          "Silo Surprise",
		"https://example.com/programs/ff-flat-tyre_master_2k-mp4-a1b2c3?category_id=100200":      "Flat Tyre",
		"https://example.com/programs/ff-winter-special_master_2k-mp4-d4e5f6?category_id=100200": "Pip's Winter Wish",
	}
	for _, l := range res.Links {
		if w, ok := want[l.URL]; ok && l.Title != w {
			t.Errorf("title for %s:\n  got  %q\n  want %q", l.URL, l.Title, w)
		}
	}
}

// TestAnchorTitlePrefersAuthoredLabel pins the label precedence directly:
// title= and aria-label= beat a nested image's alt=, which beats inner text.
func TestAnchorTitlePrefersAuthoredLabel(t *testing.T) {
	cases := []struct {
		name string
		html string
		want string
	}{
		{"title attr", `<a href="/p/1" title="Real Name">12:34</a>`, "Real Name"},
		{"aria-label", `<a href="/p/1" aria-label="Real Name">12:34</a>`, "Real Name"},
		{"img alt", `<a href="/p/1"><img alt="Real Name" src="t.jpg">12:34</a>`, "Real Name"},
		{"inner text only", `<a href="/p/1">Real Name</a>`, "Real Name"},
		{"second anchor upgrades", `<a href="/p/1"><span>12:34</span></a><a href="/p/1" title="Real Name">x</a>`, "Real Name"},
		{"better label does not lose to later worse one", `<a href="/p/1" title="Real Name">x</a><a href="/p/1"><span>12:34</span></a>`, "Real Name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, _ := url.Parse(baseURL)
			res, err := Discover([]byte("<html><body>"+tc.html+"</body></html>"), base, DefaultOptions())
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Links) != 1 {
				t.Fatalf("expected exactly 1 link, got %d", len(res.Links))
			}
			if res.Links[0].Title != tc.want {
				t.Errorf("got %q, want %q", res.Links[0].Title, tc.want)
			}
		})
	}
}

// TestUscreenProgram covers a Hotwire player page: the media URL sits on a
// <source> inside a <video-player> custom element, reachable only by following
// the turbo-frames that address it.
func TestUscreenProgram(t *testing.T) {
	res := load(t, "uscreen-program.html")

	assertSameSet(t, videoURLs(res), []string{
		"https://stream.video.example/Zx01PLACEHOLDERvideoID000example.m3u8?token=TOKEN_ELIDED",
	})
	if len(res.Videos) != 1 {
		t.Fatalf("expected exactly 1 video, got %d: %v", len(res.Videos), videoURLs(res))
	}
	if got := res.Videos[0].Kind; got != KindStream {
		t.Errorf("kind: got %q, want %q", got, KindStream)
	}
	if got := res.Videos[0].Strategy; got != StrategyMediaTag {
		t.Errorf("strategy: got %q, want %q", got, StrategyMediaTag)
	}

	// Every frame carrying a src becomes a link; program_player has none.
	var frames []string
	for _, l := range res.Links {
		if l.Kind == LinkFrame {
			frames = append(frames, l.URL)
		}
	}
	assertSameSet(t, frames, []string{
		"https://example.com/programs/ff-flat-tyre_master_2k-mp4-a1b2c3.turbo_stream?category_id=100200&playlist_position=sidebar&preview=false",
		"https://example.com/programs/ff-flat-tyre_master_2k-mp4-a1b2c3/program_content?category_id=100200&preview=false",
		"https://example.com/programs/ff-flat-tyre_master_2k-mp4-a1b2c3/trailer",
		"https://farmvids.example/programs/ff-flat-tyre_master_2k-mp4-a1b2c3/resources",
		"https://farmvids.example/programs/ff-flat-tyre_master_2k-mp4-a1b2c3/related",
	})
}

// TestBlobAndDataURLsIgnored pins that a MediaSource handle on <video src> is
// not mistaken for a fetchable URL. The player assigns a blob: handle once
// JavaScript runs, and it is meaningless outside that browser session.
func TestBlobAndDataURLsIgnored(t *testing.T) {
	res := load(t, "uscreen-program.html")
	for _, v := range res.Videos {
		if strings.HasPrefix(v.URL, "blob:") || strings.HasPrefix(v.URL, "data:") {
			t.Errorf("non-fetchable URL reported as a video: %s", v.URL)
		}
	}
	for _, l := range res.Links {
		if strings.HasPrefix(l.URL, "blob:") || strings.HasPrefix(l.URL, "data:") {
			t.Errorf("non-fetchable URL reported as a link: %s", l.URL)
		}
	}
}

// TestTurboFrameWithoutSrcIgnored pins that an already-rendered frame carries
// nothing to fetch.
func TestTurboFrameWithoutSrcIgnored(t *testing.T) {
	base, _ := url.Parse(baseURL)
	res, err := Discover([]byte(`<html><body>
		<turbo-frame id="rendered">already here</turbo-frame>
		<turbo-frame id="lazy" src="/frag"></turbo-frame>
		<turbo-frame id="blank" src="  "></turbo-frame>
	</body></html>`), base, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	assertSameSet(t, linkURLs(res), []string{"https://example.com/frag"})
	if len(res.Links) != 1 {
		t.Fatalf("expected exactly 1 link, got %d: %v", len(res.Links), linkURLs(res))
	}
}

// TestSelfReferentialEmbedDropped pins that a page's own address, reported as
// its embedUrl, is not offered as a video. Uscreen storefronts do exactly this
// on every programme page, which would otherwise double the result list with
// entries nothing can download.
func TestSelfReferentialEmbedDropped(t *testing.T) {
	base, _ := url.Parse(baseURL)
	doc := `<html><head><script type="application/ld+json">
	{"@type":"VideoObject","name":"Flat Tyre",
	 "embedUrl":"` + baseURL + `",
	 "contentUrl":"https://cdn.example.com/real.m3u8"}
	</script></head><body></body></html>`

	res, err := Discover([]byte(doc), base, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	assertSameSet(t, videoURLs(res), []string{"https://cdn.example.com/real.m3u8"})
	for _, v := range res.Videos {
		if v.URL == baseURL {
			t.Errorf("page reported itself as a video: %s", v.URL)
		}
	}

	// An embedUrl pointing somewhere else is still a legitimate find.
	doc2 := `<html><head><script type="application/ld+json">
	{"@type":"VideoObject","embedUrl":"https://player.example.net/watch/7"}
	</script></head><body></body></html>`
	res2, err := Discover([]byte(doc2), base, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	assertSameSet(t, videoURLs(res2), []string{"https://player.example.net/watch/7"})
}
