package hls

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/shearn89/vid-scraper/internal/fetch"
)

// segBody makes a distinctive, order-revealing body for segment i. If assembly
// concatenated segments out of order, the joined bytes would differ even though
// the total length matched.
func segBody(i int) []byte {
	return []byte(fmt.Sprintf("[segment %03d]%s", i, strings.Repeat("x", 64+i)))
}

// joined is what a correct download must produce.
func joined(n int) []byte {
	var b bytes.Buffer
	for i := 0; i < n; i++ {
		b.Write(segBody(i))
	}
	return b.Bytes()
}

func newTestDownloader(dir string, opts Options) *Downloader {
	opts.Dir = dir
	return New(fetch.New(fetch.Options{}), opts)
}

// streamServer serves a media playlist of n segments, plus the segments.
func streamServer(t *testing.T, n int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index.m3u8" {
			var b strings.Builder
			b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:10\n")
			for i := 0; i < n; i++ {
				fmt.Fprintf(&b, "#EXTINF:10.0,\nseg%d.ts\n", i)
			}
			b.WriteString("#EXT-X-ENDLIST\n")
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			fmt.Fprint(w, b.String())
			return
		}
		var i int
		if _, err := fmt.Sscanf(r.URL.Path, "/seg%d.ts", &i); err != nil || i < 0 || i >= n {
			http.NotFound(w, r)
			return
		}
		w.Write(segBody(i))
	}))
}

// TestDownloadAssemblesInOrder uses enough segments, fetched concurrently, that
// a race in assembly would show up as reordered output.
func TestDownloadAssemblesInOrder(t *testing.T) {
	const n = 25
	srv := streamServer(t, n)
	defer srv.Close()
	dir := t.TempDir()

	res, err := newTestDownloader(dir, Options{Concurrency: 8}).Download(context.Background(), srv.URL+"/index.m3u8")
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if res.Segments != n {
		t.Errorf("Segments = %d, want %d", res.Segments, n)
	}
	got, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, joined(n)) {
		t.Errorf("assembled output does not match the concatenated segments\n got %d bytes\nwant %d bytes",
			len(got), len(joined(n)))
	}
	if !strings.HasSuffix(res.Path, "index.ts") {
		t.Errorf("path = %q, want a .ts suffix for a transport stream", res.Path)
	}
	// The scratch directory must not survive a successful run.
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("expected only the output file, got %d entries", len(entries))
	}
}

// TestDownloadViaMaster checks variant selection and that variant and segment
// URIs resolve relative to the playlist that named them.
func TestDownloadViaMaster(t *testing.T) {
	const n = 4
	// The handler needs the server's own URL to build absolute segment links,
	// so the variable must exist before the server that closes over it. Both
	// directives say the same thing: staticcheck reads //lint:ignore,
	// golangci-lint reads //nolint.
	//lint:ignore S1021 declaration and assignment cannot be merged here
	var srv *httptest.Server //nolint:staticcheck // S1021: see above
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/master.m3u8":
			fmt.Fprint(w, "#EXTM3U\n"+
				"#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360\nlow/index.m3u8\n"+
				"#EXT-X-STREAM-INF:BANDWIDTH=5000000,RESOLUTION=1920x1080\nhigh/index.m3u8\n")
		case strings.HasSuffix(r.URL.Path, "/index.m3u8"):
			var b strings.Builder
			b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:10\n")
			for i := 0; i < n; i++ {
				fmt.Fprintf(&b, "#EXTINF:10.0,\nseg%d.ts\n", i)
			}
			b.WriteString("#EXT-X-ENDLIST\n")
			fmt.Fprint(w, b.String())
		default:
			// Segments must arrive under the selected variant's directory,
			// proving relative resolution used the media playlist's own URL.
			var i int
			if !strings.HasPrefix(r.URL.Path, "/high/") {
				http.Error(w, "wrong variant: "+r.URL.Path, http.StatusBadRequest)
				return
			}
			if _, err := fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/high"), "/seg%d.ts", &i); err != nil {
				http.NotFound(w, r)
				return
			}
			w.Write(segBody(i))
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	res, err := newTestDownloader(dir, Options{Quality: "best"}).Download(context.Background(), srv.URL+"/master.m3u8")
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, _ := os.ReadFile(res.Path)
	if !bytes.Equal(got, joined(n)) {
		t.Error("content mismatch after selecting a variant")
	}
}

// TestDownloadAES128 proves the decryption path recovers the exact plaintext,
// covering both an explicit IV and one derived from the sequence number.
func TestDownloadAES128(t *testing.T) {
	key := []byte("0123456789abcdef")
	const n = 5

	for _, explicitIV := range []bool{true, false} {
		name := "derived IV"
		if explicitIV {
			name = "explicit IV"
		}
		t.Run(name, func(t *testing.T) {
			iv := []byte("fedcba9876543210")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/index.m3u8":
					var b strings.Builder
					b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:10\n#EXT-X-MEDIA-SEQUENCE:0\n")
					if explicitIV {
						fmt.Fprintf(&b, "#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\",IV=0x%x\n", iv)
					} else {
						b.WriteString("#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\"\n")
					}
					for i := 0; i < n; i++ {
						fmt.Fprintf(&b, "#EXTINF:10.0,\nseg%d.ts\n", i)
					}
					b.WriteString("#EXT-X-ENDLIST\n")
					fmt.Fprint(w, b.String())
				case r.URL.Path == "/key.bin":
					w.Write(key)
				default:
					var i int
					if _, err := fmt.Sscanf(r.URL.Path, "/seg%d.ts", &i); err != nil {
						http.NotFound(w, r)
						return
					}
					segIV := iv
					if !explicitIV {
						segIV = make([]byte, 16)
						binary.BigEndian.PutUint64(segIV[8:], uint64(i))
					}
					w.Write(encryptAES128(t, segBody(i), key, segIV))
				}
			}))
			defer srv.Close()

			dir := t.TempDir()
			res, err := newTestDownloader(dir, Options{Concurrency: 4}).Download(context.Background(), srv.URL+"/index.m3u8")
			if err != nil {
				t.Fatalf("Download: %v", err)
			}
			got, _ := os.ReadFile(res.Path)
			if !bytes.Equal(got, joined(n)) {
				t.Errorf("decrypted output does not match the plaintext segments")
			}
		})
	}
}

// TestResumeFromPartialSegments checks an interrupted run reuses the segments
// it already has and still assembles a byte-correct file.
func TestResumeFromPartialSegments(t *testing.T) {
	const n = 6
	// Atomic: the handler runs on concurrent server goroutines (Concurrency > 1).
	var served atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index.m3u8" {
			var b strings.Builder
			b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:10\n")
			for i := 0; i < n; i++ {
				fmt.Fprintf(&b, "#EXTINF:10.0,\nseg%d.ts\n", i)
			}
			b.WriteString("#EXT-X-ENDLIST\n")
			fmt.Fprint(w, b.String())
			return
		}
		var i int
		fmt.Sscanf(r.URL.Path, "/seg%d.ts", &i)
		served.Add(1)
		w.Write(segBody(i))
	}))
	defer srv.Close()

	dir := t.TempDir()
	// Plant the first three segments as if a previous run had fetched them.
	partDir := dir + "/.index.ts.parts"
	if err := os.MkdirAll(partDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := os.WriteFile(fmt.Sprintf("%s/seg-%06d", partDir, i), segBody(i), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	res, err := newTestDownloader(dir, Options{Concurrency: 2}).Download(context.Background(), srv.URL+"/index.m3u8")
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if got := served.Load(); got != 3 {
		t.Errorf("fetched %d segments, want 3 (the other 3 were already on disk)", got)
	}
	got, _ := os.ReadFile(res.Path)
	if !bytes.Equal(got, joined(n)) {
		t.Error("resumed stream does not match the expected concatenation")
	}
}

func TestLiveStreamRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6.0,\nseg0.ts\n")
	}))
	defer srv.Close()

	_, err := newTestDownloader(t.TempDir(), Options{}).Download(context.Background(), srv.URL+"/live.m3u8")
	if !errors.Is(err, ErrLive) {
		t.Errorf("err = %v, want ErrLive", err)
	}
}

func TestDRMRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n"+
			`#EXT-X-KEY:METHOD=SAMPLE-AES,URI="skd://x",KEYFORMAT="com.apple.streamingkeydelivery"`+"\n"+
			"#EXTINF:6.0,\nseg0.ts\n#EXT-X-ENDLIST\n")
	}))
	defer srv.Close()

	_, err := newTestDownloader(t.TempDir(), Options{}).Download(context.Background(), srv.URL+"/drm.m3u8")
	if !errors.Is(err, ErrDRM) {
		t.Errorf("err = %v, want ErrDRM", err)
	}
}

// TestSeparateAudioWarns is the check that stops a silent file being handed
// over as if it were fine.
func TestSeparateAudioWarns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/master.m3u8":
			b, err := os.ReadFile("../../testdata/separate-audio.m3u8")
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			w.Write(b)
		case "/video/720p.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:10\n#EXTINF:10.0,\nseg0.ts\n#EXT-X-ENDLIST\n")
		default:
			w.Write(segBody(0))
		}
	}))
	defer srv.Close()

	var warnings []string
	d := newTestDownloader(t.TempDir(), Options{
		Warnf: func(f string, a ...any) { warnings = append(warnings, fmt.Sprintf(f, a...)) },
	})
	if _, err := d.Download(context.Background(), srv.URL+"/master.m3u8"); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if len(warnings) == 0 {
		t.Fatal("expected a warning that audio is a separate track")
	}
	if !strings.Contains(warnings[0], "audio/en.m3u8") {
		t.Errorf("warning should name the audio playlist URL, got: %s", warnings[0])
	}
}

func TestSegmentFailureIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index.m3u8" {
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:10\n#EXTINF:10.0,\nseg0.ts\n"+
				"#EXTINF:10.0,\nmissing.ts\n#EXT-X-ENDLIST\n")
			return
		}
		if r.URL.Path == "/seg0.ts" {
			w.Write(segBody(0))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	_, err := newTestDownloader(t.TempDir(), Options{}).Download(context.Background(), srv.URL+"/index.m3u8")
	if err == nil {
		t.Fatal("expected an error when a segment 404s")
	}
	if !strings.Contains(err.Error(), "missing.ts") {
		t.Errorf("error should name the failing segment, got: %v", err)
	}
}

// encryptAES128 mirrors the decryption path, so a round trip proves the
// padding and IV handling agree with a standard encryptor.
func encryptAES128(t *testing.T, plain, key, iv []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	padded := append(append([]byte{}, plain...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)
	return out
}
