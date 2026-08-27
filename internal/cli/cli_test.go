package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shearn89/vid-scraper/internal/scrape"
)

// gallery serves an index page linking to a detail page that holds a video,
// plus the video itself.
func gallery(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><head><title>Gallery</title></head><body>
				<a href="/videos/ep1">Episode 1</a>
				<video src="/media/promo.mp4"></video>
			</body></html>`)
		case "/videos/ep1":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><body><video src="/media/ep1.mp4"></video></body></html>`)
		default:
			w.Header().Set("Content-Type", "video/mp4")
			fmt.Fprintf(w, "fake video bytes for %s", r.URL.Path)
		}
	}))
}

// exec runs the CLI and returns its exit code plus captured streams.
func exec(args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := Run(context.Background(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestListMode(t *testing.T) {
	srv := gallery(t)
	defer srv.Close()

	code, out, _ := exec("--list", srv.URL+"/")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d", code, ExitOK)
	}
	if got := strings.TrimSpace(out); got != srv.URL+"/media/promo.mp4" {
		t.Errorf("stdout = %q, want just the promo URL", got)
	}
}

// TestFollowReachesSecondHop is the headline behaviour: a video that is only
// reachable via a detail page.
func TestFollowReachesSecondHop(t *testing.T) {
	srv := gallery(t)
	defer srv.Close()

	code, out, _ := exec("--list", "--follow", srv.URL+"/")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "/media/ep1.mp4") {
		t.Errorf("expected the second-hop video in output:\n%s", out)
	}
}

func TestJSONMode(t *testing.T) {
	srv := gallery(t)
	defer srv.Close()

	code, out, _ := exec("--json", srv.URL+"/")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	var videos []scrape.Video
	if err := json.Unmarshal([]byte(out), &videos); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if len(videos) != 1 {
		t.Fatalf("got %d videos, want 1", len(videos))
	}
	if videos[0].Strategy != scrape.StrategyMediaTag || videos[0].Kind != scrape.KindDirect {
		t.Errorf("video = %+v", videos[0])
	}
	if videos[0].Source != srv.URL+"/" {
		t.Errorf("source = %q, want the page URL", videos[0].Source)
	}
}

// TestDownloadEndToEnd exercises the default mode: discover, then write files.
func TestDownloadEndToEnd(t *testing.T) {
	srv := gallery(t)
	defer srv.Close()
	dir := t.TempDir()

	code, _, errOut := exec("--follow", "-o", dir, srv.URL+"/")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr:\n%s", code, errOut)
	}
	for _, name := range []string{"promo.mp4", "ep1.mp4"} {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("expected %s to be downloaded: %v", name, err)
			continue
		}
		if !strings.Contains(string(body), "fake video bytes") {
			t.Errorf("%s has unexpected content: %q", name, body)
		}
	}
}

// TestFlagsAfterURL checks the argument reordering, since users routinely put
// the URL first.
func TestFlagsAfterURL(t *testing.T) {
	srv := gallery(t)
	defer srv.Close()

	code, out, errOut := exec(srv.URL+"/", "--list")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr:\n%s", code, errOut)
	}
	if !strings.Contains(out, "/media/promo.mp4") {
		t.Errorf("stdout = %q", out)
	}
}

func TestNothingFoundExitCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body><p>No videos here.</p></body></html>`)
	}))
	defer srv.Close()

	code, out, errOut := exec("--list", srv.URL+"/")
	if code != ExitNotFound {
		t.Errorf("exit = %d, want %d", code, ExitNotFound)
	}
	if out != "" {
		t.Errorf("stdout should be empty, got %q", out)
	}
	if !strings.Contains(errOut, "No videos found") {
		t.Errorf("stderr = %q", errOut)
	}
}

// TestDASHIsReportedNotDownloaded checks the deliberate limitation is surfaced
// clearly rather than producing a broken file.
func TestDASHIsReportedNotDownloaded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body><a href="/stream/manifest.mpd">Watch</a></body></html>`)
	}))
	defer srv.Close()
	dir := t.TempDir()

	code, _, errOut := exec("-o", dir, srv.URL+"/")
	if code != ExitOK {
		t.Errorf("exit = %d; a skipped DASH manifest is not a failure", code)
	}
	if !strings.Contains(errOut, "DASH") || !strings.Contains(errOut, "manifest.mpd") {
		t.Errorf("stderr should name DASH and the URL, got:\n%s", errOut)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("nothing should have been written, found %v", entries)
	}
}

// TestEmbedIsReportedNotDownloaded covers a player page with no media URL.
func TestEmbedIsReportedNotDownloaded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><head>
			<meta property="og:video" content="https://player.example.net/embed/xyz">
		</head><body></body></html>`)
	}))
	defer srv.Close()
	dir := t.TempDir()

	code, _, errOut := exec("-o", dir, srv.URL+"/")
	if code != ExitOK {
		t.Errorf("exit = %d", code)
	}
	if !strings.Contains(errOut, "embed") {
		t.Errorf("stderr should explain the embed was skipped, got:\n%s", errOut)
	}
}

func TestHLSDownloadThroughCLI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><body><video src="/stream/index.m3u8"></video></body></html>`)
		case r.URL.Path == "/stream/index.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:10\n"+
				"#EXTINF:10.0,\nseg0.ts\n#EXTINF:10.0,\nseg1.ts\n#EXT-X-ENDLIST\n")
		default:
			fmt.Fprintf(w, "<%s>", filepath.Base(r.URL.Path))
		}
	}))
	defer srv.Close()
	dir := t.TempDir()

	code, _, errOut := exec("-o", dir, srv.URL+"/")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr:\n%s", code, errOut)
	}
	got, err := os.ReadFile(filepath.Join(dir, "index.ts"))
	if err != nil {
		t.Fatalf("expected index.ts: %v", err)
	}
	if string(got) != "<seg0.ts><seg1.ts>" {
		t.Errorf("assembled stream = %q, want the two segments in order", got)
	}
}

func TestBadArguments(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"no url", []string{"--list"}},
		{"unknown flag", []string{"--nope", "https://example.com"}},
		{"bad match regex", []string{"--match", "([", "https://example.com"}},
		{"unsupported scheme", []string{"ftp://example.com/x"}},
		{"render not implemented", []string{"--render", "https://example.com"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if code, _, _ := exec(tc.args...); code != ExitError {
				t.Errorf("exit = %d, want %d", code, ExitError)
			}
		})
	}
}

func TestHelpAndVersion(t *testing.T) {
	code, out, _ := exec("--help")
	if code != ExitOK || !strings.Contains(out, "Usage:") {
		t.Errorf("--help: exit = %d, out = %q", code, out)
	}
	if code, out, _ := exec("--version"); code != ExitOK || !strings.Contains(out, "vid-scraper") {
		t.Errorf("--version: exit = %d, out = %q", code, out)
	}
}

func TestParseStartURL(t *testing.T) {
	// A bare host is how URLs are usually pasted; it should just work.
	u, err := parseStartURL("example.com/page")
	if err != nil {
		t.Fatalf("parseStartURL: %v", err)
	}
	if u.Scheme != "https" || u.Host != "example.com" {
		t.Errorf("got %s", u)
	}
	for _, bad := range []string{"", "   ", "ftp://x/y", "https://"} {
		if _, err := parseStartURL(bad); err == nil {
			t.Errorf("parseStartURL(%q) should have failed", bad)
		}
	}
}
