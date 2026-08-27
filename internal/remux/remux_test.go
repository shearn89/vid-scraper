package remux

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubLookPath swaps the PATH lookup and clears the cache it feeds, so a test
// can pretend ffmpeg is missing (or is some other binary) without depending on
// what the machine running the suite happens to have installed.
func stubLookPath(t *testing.T, fn func(string) (string, error)) {
	t.Helper()
	prev := lookPath
	lookPath = fn
	resetLookupCache()
	t.Cleanup(func() {
		lookPath = prev
		resetLookupCache()
	})
}

func TestMP4Path(t *testing.T) {
	cases := []struct {
		src  string
		want string
	}{
		{"tyre-trouble.ts", "tyre-trouble.mp4"},
		{"tyre-trouble.mp4", "tyre-trouble.mp4"},
		{"noextension", "noextension.mp4"},
		{"a.b.ts", "a.b.mp4"},
		{"/tmp/out/spuds in mud.ts", "/tmp/out/spuds in mud.mp4"},
		{"trailing.dot.", "trailing.dot.mp4"},
		{".ts", ".mp4"},
	}
	for _, c := range cases {
		if got := mp4Path(c.src); got != c.want {
			t.Errorf("mp4Path(%q) = %q, want %q", c.src, got, c.want)
		}
	}
}

func TestAvailableMissing(t *testing.T) {
	stubLookPath(t, func(string) (string, error) { return "", exec.ErrNotFound })

	err := Available()
	if err == nil {
		t.Fatal("Available() = nil, want an error")
	}
	if !errors.Is(err, ErrNotInstalled) {
		t.Errorf("Available() = %v, does not wrap ErrNotInstalled", err)
	}
	if !strings.Contains(err.Error(), "ffmpeg") {
		t.Errorf("Available() = %q, want the message to name ffmpeg", err)
	}
}

func TestAvailablePresent(t *testing.T) {
	stubLookPath(t, func(string) (string, error) { return "/usr/bin/ffmpeg", nil })

	if err := Available(); err != nil {
		t.Fatalf("Available() = %v, want nil", err)
	}
}

func TestToMP4AlreadyMP4(t *testing.T) {
	// Deliberately a path that does not exist and a lookup that would fail:
	// an .mp4 source must short-circuit before either is consulted.
	stubLookPath(t, func(string) (string, error) { return "", exec.ErrNotFound })

	const src = "/does/not/exist/tyre-trouble.mp4"
	got, err := ToMP4(context.Background(), src, false)
	if err != nil {
		t.Fatalf("ToMP4(%q) = %v, want nil", src, err)
	}
	if got != src {
		t.Errorf("ToMP4(%q) = %q, want the path unchanged", src, got)
	}
}

func TestToMP4WithoutFFmpeg(t *testing.T) {
	stubLookPath(t, func(string) (string, error) { return "", exec.ErrNotFound })

	dir := t.TempDir()
	src := filepath.Join(dir, "clip.ts")
	if err := os.WriteFile(src, []byte("not really a stream"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := ToMP4(context.Background(), src, false); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("ToMP4 = %v, want an error wrapping ErrNotInstalled", err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("source was disturbed by a failed remux: %v", err)
	}
}

func TestToMP4RefusesExistingDestination(t *testing.T) {
	// The lookup succeeds but points at nothing runnable: the destination
	// check must reject before ffmpeg is ever started.
	stubLookPath(t, func(string) (string, error) { return "/nonexistent/ffmpeg", nil })

	dir := t.TempDir()
	src := filepath.Join(dir, "clip.ts")
	dst := filepath.Join(dir, "clip.mp4")
	for _, p := range []string{src, dst} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	_, err := ToMP4(context.Background(), src, false)
	if err == nil {
		t.Fatal("ToMP4 = nil, want an error naming the existing destination")
	}
	if !strings.Contains(err.Error(), dst) {
		t.Errorf("ToMP4 = %q, want the message to name %q", err, dst)
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("source was removed despite the failure: %v", err)
	}
}

func TestToMP4FailureLeavesNothingBehind(t *testing.T) {
	// "false" stands in for an ffmpeg that exits non-zero, so this covers the
	// cleanup path on a machine with no ffmpeg at all.
	bin, err := exec.LookPath("false")
	if err != nil {
		t.Skipf("no false(1) to stand in for a failing ffmpeg: %v", err)
	}
	stubLookPath(t, func(string) (string, error) { return bin, nil })

	dir := t.TempDir()
	src := filepath.Join(dir, "clip.ts")
	if err := os.WriteFile(src, []byte("not really a stream"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := ToMP4(context.Background(), src, false); err == nil {
		t.Fatal("ToMP4 = nil, want the ffmpeg failure reported")
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("source was removed despite the failure: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "clip.ts" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("failed remux left files behind: %v", names)
	}
}

// requireFFmpeg skips the calling test unless a real ffmpeg is installed, so
// the suite passes on machines without one.
func requireFFmpeg(t *testing.T) {
	t.Helper()
	resetLookupCache() // an earlier test may have cached a stub
	t.Cleanup(resetLookupCache)
	if err := Available(); err != nil {
		t.Skipf("ffmpeg required: %v", err)
	}
}

// writeTestTS generates a tiny MPEG-TS clip with ffmpeg itself.
func writeTestTS(t *testing.T, path string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-nostdin", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=duration=1:size=64x48:rate=5",
		"-c:v", "mpeg2video", path,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("could not generate a test clip with this ffmpeg build: %v: %s", err, out)
	}
}

func TestToMP4RoundTrip(t *testing.T) {
	requireFFmpeg(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "tyre-trouble.ts")
	writeTestTS(t, src)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	got, err := ToMP4(ctx, src, false)
	if err != nil {
		t.Fatalf("ToMP4 = %v, want nil", err)
	}
	want := filepath.Join(dir, "tyre-trouble.mp4")
	if got != want {
		t.Fatalf("ToMP4 = %q, want %q", got, want)
	}
	info, err := os.Stat(got)
	if err != nil {
		t.Fatalf("destination missing: %v", err)
	}
	if info.Size() == 0 {
		t.Error("destination is empty")
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("source still present with keepSource=false: %v", err)
	}
}

func TestToMP4KeepsSource(t *testing.T) {
	requireFFmpeg(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "clip.ts")
	writeTestTS(t, src)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := ToMP4(ctx, src, true); err != nil {
		t.Fatalf("ToMP4 = %v, want nil", err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("source removed with keepSource=true: %v", err)
	}
}

func TestToMP4CancelledContext(t *testing.T) {
	requireFFmpeg(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "clip.ts")
	writeTestTS(t, src)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ToMP4(ctx, src, false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ToMP4 = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "clip.mp4")); !os.IsNotExist(err) {
		t.Errorf("cancelled remux left a destination file: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("cancelled remux left %d files in %s, want just the source", len(entries), dir)
	}
}
