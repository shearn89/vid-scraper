package crawl

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"testing"
	"time"

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

// TestDestructiveLinksNotFollowed pins that a crawl carrying real credentials
// never follows a link that acts on the account. Signing the user out mid-crawl
// is the failure this prevents.
func TestDestructiveLinksNotFollowed(t *testing.T) {
	c := New(nil, Options{})
	start, _ := url.Parse("https://example.com/")

	blocked := []string{
		"https://example.com/sign_out",
		"https://example.com/signout",
		"https://example.com/sign-out",
		"https://example.com/logout",
		"https://example.com/log_out",
		"https://example.com/account/delete",
		"https://example.com/subscriptions/cancel",
		"https://example.com/items/42/destroy",
		"https://example.com/email/unsubscribe?t=1",
		"https://example.com/tokens/revoke",
	}
	for _, u := range blocked {
		if c.shouldFollow(scrape.Link{URL: u, Kind: scrape.LinkAnchor}, start) {
			t.Errorf("followed a destructive link: %s", u)
		}
	}

	// Ordinary pages whose names merely contain a blocked word as a substring
	// must still be crawled.
	allowed := []string{
		"https://example.com/programs/tt-tyre-rescue",
		"https://example.com/categories/removals-and-storage",
		"https://example.com/videos/cancellation-policy-explained",
		"https://example.com/deleted-scenes-episode-3",
	}
	for _, u := range allowed {
		if !c.shouldFollow(scrape.Link{URL: u, Kind: scrape.LinkAnchor}, start) {
			t.Errorf("blocked an ordinary page: %s", u)
		}
	}
}

// TestTaskQueueOrdersDeepestFirst pins the scheduling rule that makes the walk
// pipeline: the deepest pending page is taken next, so a chain is finished
// rather than every chain's first hop being started.
func TestTaskQueueOrdersDeepestFirst(t *testing.T) {
	q := &taskQueue{}
	for _, tk := range []task{
		{depth: 0, path: []int{0}},
		{depth: 2, path: []int{9}},
		{depth: 1, path: []int{5}},
		{depth: 2, path: []int{3}},
	} {
		q.push(tk)
	}
	var gotDepths []int
	var gotPaths []int
	for q.Len() > 0 {
		tk := q.pop()
		gotDepths = append(gotDepths, tk.depth)
		gotPaths = append(gotPaths, tk.path[0])
	}
	wantDepths := []int{2, 2, 1, 0}
	for i := range wantDepths {
		if gotDepths[i] != wantDepths[i] {
			t.Fatalf("depth order = %v, want %v", gotDepths, wantDepths)
		}
	}
	// Equal depths break on tree position, not on insertion order, so the
	// walk is reproducible.
	if gotPaths[0] != 3 || gotPaths[1] != 9 {
		t.Errorf("equal-depth ties = %v, want the lower path first", gotPaths[:2])
	}
}

// TestResultOrderIsDeterministic runs the same concurrent crawl repeatedly and
// requires identical output. Concurrency means completion order varies between
// runs; sorting by tree position is what keeps the result stable.
func TestResultOrderIsDeterministic(t *testing.T) {
	srv := site(t)
	defer srv.Close()

	var first []string
	for i := range 12 {
		got := urls(run(t, srv.URL+"/", Options{Depth: 1, Concurrency: 4}))
		if i == 0 {
			first = got
			continue
		}
		if len(got) != len(first) {
			t.Fatalf("run %d returned %d videos, run 0 returned %d", i, len(got), len(first))
		}
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("run %d differs at %d:\n got  %v\n want %v", i, j, got, first)
			}
		}
	}
}

// TestCancelledContextStopsTheWalk pins that cancellation ends the crawl
// rather than leaving workers parked on the condition variable, which would
// hang the process.
func TestCancelledContextStopsTheWalk(t *testing.T) {
	srv := site(t)
	defer srv.Close()

	u, err := url.Parse(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		New(fetch.New(fetch.Options{}), Options{
			Depth: 3, Concurrency: 4, Scrape: scrape.DefaultOptions(),
		}).Run(ctx, u)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
}
