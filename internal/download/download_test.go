package download

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/shearn89/vid-scraper/internal/fetch"
)

// payload is deliberately larger than one buffer so io.Copy loops.
var payload = []byte(strings.Repeat("vid-scraper test payload 0123456789\n", 2000))

// rangeServer serves payload with byte-range support, the behaviour resume
// depends on.
func rangeServer(t *testing.T, supportRanges bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		rng := r.Header.Get("Range")
		if !supportRanges || rng == "" {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			w.WriteHeader(http.StatusOK)
			w.Write(payload)
			return
		}
		var start int64
		fmt.Sscanf(rng, "bytes=%d-", &start)
		if start >= int64(len(payload)) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(payload)-1, len(payload)))
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)-int(start)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(payload[start:])
	}))
}

func newDownloader(t *testing.T, dir string, opts Options) *Downloader {
	t.Helper()
	opts.Dir = dir
	return New(fetch.New(fetch.Options{}), opts)
}

func TestDownloadWholeFile(t *testing.T) {
	srv := rangeServer(t, true)
	defer srv.Close()
	dir := t.TempDir()

	res, err := newDownloader(t, dir, Options{}).Download(context.Background(), srv.URL+"/clip.mp4")
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if filepath.Base(res.Path) != "clip.mp4" {
		t.Errorf("path = %q, want basename clip.mp4", res.Path)
	}
	got, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Errorf("content mismatch: got %d bytes, want %d", len(got), len(payload))
	}
	if res.Bytes != int64(len(payload)) {
		t.Errorf("Bytes = %d, want %d", res.Bytes, len(payload))
	}
	// No .part file should survive a successful download.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".part") {
			t.Errorf("leftover part file: %s", e.Name())
		}
	}
}

// TestResume is the important one: an interrupted transfer must produce a file
// byte-identical to a clean download, not a file that is merely the right size.
func TestResume(t *testing.T) {
	srv := rangeServer(t, true)
	defer srv.Close()
	dir := t.TempDir()
	rawURL := srv.URL + "/clip.mp4"

	// Simulate an interrupted run by planting a part file holding the first
	// third of the payload, named exactly as the downloader would name it.
	head := len(payload) / 3
	partPath := filepath.Join(dir, "."+NameFromURL(rawURL)+"."+shortHash(rawURL)+".part")
	if err := os.WriteFile(partPath, payload[:head], 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := newDownloader(t, dir, Options{}).Download(context.Background(), rawURL)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if !res.Resumed {
		t.Error("expected Resumed = true")
	}
	got, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Errorf("resumed content differs from source (got %d bytes, want %d)", len(got), len(payload))
	}
	if res.Bytes != int64(len(payload)) {
		t.Errorf("Bytes = %d, want %d", res.Bytes, len(payload))
	}
}

// TestResumeAgainstServerIgnoringRanges checks the downloader restarts cleanly
// rather than appending a full body onto a partial file, which would corrupt it.
func TestResumeAgainstServerIgnoringRanges(t *testing.T) {
	srv := rangeServer(t, false)
	defer srv.Close()
	dir := t.TempDir()
	rawURL := srv.URL + "/clip.mp4"

	partPath := filepath.Join(dir, "."+NameFromURL(rawURL)+"."+shortHash(rawURL)+".part")
	if err := os.WriteFile(partPath, payload[:100], 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := newDownloader(t, dir, Options{}).Download(context.Background(), rawURL)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Errorf("content corrupted: got %d bytes, want %d", len(got), len(payload))
	}
}

// TestCompletedPartFile covers a part file that already holds the whole
// payload: the server answers 416 and the file is simply promoted.
func TestCompletedPartFile(t *testing.T) {
	srv := rangeServer(t, true)
	defer srv.Close()
	dir := t.TempDir()
	rawURL := srv.URL + "/clip.mp4"

	partPath := filepath.Join(dir, "."+NameFromURL(rawURL)+"."+shortHash(rawURL)+".part")
	if err := os.WriteFile(partPath, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := newDownloader(t, dir, Options{}).Download(context.Background(), rawURL)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, _ := os.ReadFile(res.Path)
	if string(got) != string(payload) {
		t.Error("promoted part file does not match payload")
	}
}

func TestRedirect(t *testing.T) {
	target := rangeServer(t, true)
	defer target.Close()
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/final.mp4", http.StatusFound)
	}))
	defer front.Close()

	dir := t.TempDir()
	res, err := newDownloader(t, dir, Options{}).Download(context.Background(), front.URL+"/start")
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, _ := os.ReadFile(res.Path)
	if string(got) != string(payload) {
		t.Error("redirected download content mismatch")
	}
}

func TestContentDispositionName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="Episode 4 - Finale.mp4"`)
		w.Header().Set("Content-Type", "video/mp4")
		w.Write(payload)
	}))
	defer srv.Close()

	dir := t.TempDir()
	res, err := newDownloader(t, dir, Options{}).Download(context.Background(), srv.URL+"/download?id=9")
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if got := filepath.Base(res.Path); got != "Episode 4 - Finale.mp4" {
		t.Errorf("name = %q, want %q", got, "Episode 4 - Finale.mp4")
	}
}

// TestExtensionFromContentType covers a URL with no extension: the file must
// still land as something playable rather than an extensionless blob.
func TestExtensionFromContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/webm")
		w.Write(payload)
	}))
	defer srv.Close()

	dir := t.TempDir()
	res, err := newDownloader(t, dir, Options{}).Download(context.Background(), srv.URL+"/stream/12345")
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if got := filepath.Base(res.Path); !strings.HasSuffix(got, ".webm") {
		t.Errorf("name = %q, want a .webm suffix", got)
	}
}

func TestNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	dir := t.TempDir()
	if _, err := newDownloader(t, dir, Options{}).Download(context.Background(), srv.URL+"/missing.mp4"); err == nil {
		t.Fatal("expected an error for a 404")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("a failed download left files behind: %v", entries)
	}
}

func TestSkipExisting(t *testing.T) {
	srv := rangeServer(t, true)
	defer srv.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "clip.mp4"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := newDownloader(t, dir, Options{SkipExisting: true}).Download(context.Background(), srv.URL+"/clip.mp4")
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if !res.Existing {
		t.Error("expected Existing = true")
	}
	got, _ := os.ReadFile(filepath.Join(dir, "clip.mp4"))
	if string(got) != "old" {
		t.Error("existing file was overwritten")
	}
}

// TestCollisionSuffix checks a second download of a different URL with the same
// basename does not clobber the first.
func TestCollisionSuffix(t *testing.T) {
	srv := rangeServer(t, true)
	defer srv.Close()
	dir := t.TempDir()
	d := newDownloader(t, dir, Options{})

	first, err := d.Download(context.Background(), srv.URL+"/a/clip.mp4")
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.Download(context.Background(), srv.URL+"/b/clip.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if first.Path == second.Path {
		t.Fatalf("both downloads landed at %s", first.Path)
	}
	if got := filepath.Base(second.Path); got != "clip-1.mp4" {
		t.Errorf("second name = %q, want clip-1.mp4", got)
	}
}

func TestNameFromURL(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://x.example/a/b/clip.mp4", "clip.mp4"},
		{"https://x.example/a/clip.mp4?sig=1", "clip.mp4"},
		{"https://x.example/My%20Video.mp4", "My Video.mp4"},
		{"https://x.example/a/../../etc/passwd", "passwd"},
	}
	for _, tc := range tests {
		if got := NameFromURL(tc.in); got != tc.want {
			t.Errorf("NameFromURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// A path with nothing usable still yields a stable, distinct name.
	if got := NameFromURL("https://x.example/"); got == "" || strings.ContainsAny(got, `/\`) {
		t.Errorf("NameFromURL for a bare host gave %q", got)
	}
}

// TestSanitizeCannotEscapeDir is a safety check: a hostile remote name must
// never resolve outside the output directory.
func TestSanitizeCannotEscapeDir(t *testing.T) {
	for _, in := range []string{"../../etc/passwd", `..\..\windows\system32`, "a/b/c.mp4", "with\x00null.mp4"} {
		got := Sanitize(in)
		if strings.ContainsAny(got, `/\`) || strings.Contains(got, "\x00") {
			t.Errorf("Sanitize(%q) = %q, still contains a separator or null", in, got)
		}
		if filepath.Dir(filepath.Join("/out", got)) != "/out" {
			t.Errorf("Sanitize(%q) = %q escapes the output directory", in, got)
		}
	}
}
