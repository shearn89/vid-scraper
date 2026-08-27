package crawl

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"testing"

	"github.com/shearn89/vid-scraper/internal/fetch"
	"github.com/shearn89/vid-scraper/internal/scrape"
)

// site serves a small linked structure: an index linking to two detail pages
// and one off-topic page, each detail page holding one video.
func site(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/":
			fmt.Fprint(w, `<html><body>
				<a href="/videos/one">One</a>
				<a href="/videos/two">Two</a>
				<a href="/about">About</a>
				<a href="/direct.mp4">A direct file</a>
			</body></html>`)
		case "/videos/one":
			fmt.Fprint(w, `<html><body><video src="/media/one.mp4"></video></body></html>`)
		case "/videos/two":
			fmt.Fprint(w, `<html><body><video src="/media/two.mp4"></video></body></html>`)
		case "/about":
			fmt.Fprint(w, `<html><body><video src="/media/about.mp4"></video></body></html>`)
		default:
			http.NotFound(w, r)
		}
	}))
}

func run(t *testing.T, start string, opts Options) []scrape.Video {
	t.Helper()
	u, err := url.Parse(start)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Scrape == (scrape.Options{}) {
		opts.Scrape = scrape.DefaultOptions()
	}
	videos, errs := New(fetch.New(fetch.Options{}), opts).Run(context.Background(), u)
	for _, e := range errs {
		t.Logf("crawl warning: %v", e)
	}
	return videos
}

func urls(vs []scrape.Video) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		out = append(out, v.URL)
	}
	return out
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// TestDepthZeroStaysOnPage is the default: no link following at all.
func TestDepthZeroStaysOnPage(t *testing.T) {
	srv := site(t)
	defer srv.Close()

	got := urls(run(t, srv.URL+"/", Options{Depth: 0}))
	if len(got) != 1 || got[0] != srv.URL+"/direct.mp4" {
		t.Errorf("got %v, want only the directly-linked file", got)
	}
}

// TestDepthOneFollowsDetailPages covers the "video is on another page" case
// that motivated the tool.
func TestDepthOneFollowsDetailPages(t *testing.T) {
	srv := site(t)
	defer srv.Close()

	got := urls(run(t, srv.URL+"/", Options{Depth: 1}))
	for _, want := range []string{
		srv.URL + "/direct.mp4",
		srv.URL + "/media/one.mp4",
		srv.URL + "/media/two.mp4",
		srv.URL + "/media/about.mp4",
	} {
		if !contains(got, want) {
			t.Errorf("missing %s (got %v)", want, got)
		}
	}
}

// TestMatchFiltersFollowedLinks checks --match narrows the crawl.
func TestMatchFiltersFollowedLinks(t *testing.T) {
	srv := site(t)
	defer srv.Close()

	got := urls(run(t, srv.URL+"/", Options{Depth: 1, Match: regexp.MustCompile(`/videos/`)}))
	if contains(got, srv.URL+"/media/about.mp4") {
		t.Errorf("--match should have excluded /about, got %v", got)
	}
	if !contains(got, srv.URL+"/media/one.mp4") {
		t.Errorf("missing the matching page's video, got %v", got)
	}
}

// TestOffHostNotFollowed checks the default same-host restriction, and that
// --allow-host lifts it.
func TestOffHostNotFollowed(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body><video src="/offsite.mp4"></video></body></html>`)
	}))
	defer other.Close()

	main := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><body><a href="%s/page">Offsite</a></body></html>`, other.URL)
	}))
	defer main.Close()

	if got := urls(run(t, main.URL+"/", Options{Depth: 1})); len(got) != 0 {
		t.Errorf("off-host link should not be followed by default, got %v", got)
	}

	otherHost, _ := url.Parse(other.URL)
	got := urls(run(t, main.URL+"/", Options{Depth: 1, AllowHosts: []string{otherHost.Host}}))
	if !contains(got, other.URL+"/offsite.mp4") {
		t.Errorf("--allow-host should permit the hop, got %v", got)
	}
}

// TestIframeFollowedCrossHost covers the embedded-player case: the iframe is
// the hop the user wants, so it is followed even to another host and even when
// --match would exclude it.
func TestIframeFollowedCrossHost(t *testing.T) {
	player := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body><video src="/player/stream.mp4"></video></body></html>`)
	}))
	defer player.Close()

	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><body><iframe src="%s/embed/1"></iframe></body></html>`, player.URL)
	}))
	defer page.Close()

	got := urls(run(t, page.URL+"/", Options{Depth: 1, Match: regexp.MustCompile(`^never-matches$`)}))
	if !contains(got, player.URL+"/player/stream.mp4") {
		t.Errorf("iframe should be followed cross-host regardless of --match, got %v", got)
	}
}

// TestCycleDoesNotLoop checks the visited set stops two pages linking to each
// other from being fetched repeatedly.
func TestCycleDoesNotLoop(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/a":
			fmt.Fprint(w, `<html><body><a href="/b">b</a><a href="/a">a</a></body></html>`)
		default:
			fmt.Fprint(w, `<html><body><a href="/a">a</a><video src="/v.mp4"></video></body></html>`)
		}
	}))
	defer srv.Close()

	got := urls(run(t, srv.URL+"/a", Options{Depth: 3}))
	if !contains(got, srv.URL+"/v.mp4") {
		t.Errorf("expected to reach the video, got %v", got)
	}
	if hits > 2 {
		t.Errorf("fetched %d times; each page should be fetched once", hits)
	}
}

// TestDedupeAcrossPages checks one video linked from two pages is reported once.
func TestDedupeAcrossPages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/":
			fmt.Fprint(w, `<html><body><a href="/x">x</a><a href="/y">y</a></body></html>`)
		default:
			fmt.Fprint(w, `<html><body><video src="/shared.mp4"></video></body></html>`)
		}
	}))
	defer srv.Close()

	got := urls(run(t, srv.URL+"/", Options{Depth: 1}))
	if len(got) != 1 {
		t.Errorf("got %d videos, want 1 after dedupe: %v", len(got), got)
	}
}

// TestNonHTMLIsNotAnError checks a link to a PDF or image is skipped quietly:
// encountering one while crawling is normal, not a failure.
func TestNonHTMLIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><body><a href="/doc.pdf">doc</a></body></html>`)
			return
		}
		w.Header().Set("Content-Type", "application/pdf")
		w.Write([]byte("%PDF-1.4"))
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL + "/")
	_, errs := New(fetch.New(fetch.Options{}), Options{Depth: 1, Scrape: scrape.DefaultOptions()}).
		Run(context.Background(), u)
	if len(errs) != 0 {
		t.Errorf("a non-HTML link should not produce an error, got %v", errs)
	}
}

// TestFetchErrorsAreCollectedNotFatal checks one dead link does not abandon the
// rest of the crawl.
func TestFetchErrorsAreCollectedNotFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><body><a href="/dead">dead</a><a href="/good">good</a></body></html>`)
		case "/good":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><body><video src="/ok.mp4"></video></body></html>`)
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL + "/")
	videos, errs := New(fetch.New(fetch.Options{}), Options{Depth: 1, Scrape: scrape.DefaultOptions()}).
		Run(context.Background(), u)
	if len(errs) != 1 {
		t.Errorf("expected exactly one collected error, got %v", errs)
	}
	if !contains(urls(videos), srv.URL+"/ok.mp4") {
		t.Errorf("the working page's video should still be found, got %v", urls(videos))
	}
}
