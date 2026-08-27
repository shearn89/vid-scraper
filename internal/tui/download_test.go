package tui

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func newTestModel(titles ...string) *downloadModel {
	items := make([]DownloadItem, len(titles))
	for i, t := range titles {
		items[i] = DownloadItem{Title: t, Dest: "/tmp/" + t}
	}
	return &downloadModel{
		dir:   "~/Videos/farmyard-friends",
		items: items,
		rows:  make([]rowState, len(items)),
	}
}

// Progress arrives piecemeal: a stage change should not wipe counters an
// earlier update already reported.
func TestDownloadApplyKeepsKnownCounters(t *testing.T) {
	m := newTestModel("A")
	m.apply(Update{Index: 0, Stage: StageDownloading, Segments: 40, TotalSegs: 150})
	m.apply(Update{Index: 0, Stage: StageRemuxing})

	if got := m.rows[0].stage; got != StageRemuxing {
		t.Errorf("stage = %v, want StageRemuxing", got)
	}
	if m.rows[0].segments != 40 || m.rows[0].totalSegs != 150 {
		t.Errorf("counters lost: %+v", m.rows[0])
	}
}

func TestDownloadApplyIgnoresOutOfRange(t *testing.T) {
	m := newTestModel("A")
	m.apply(Update{Index: 7, Stage: StageDone})
	m.apply(Update{Index: -1, Stage: StageDone})
	if m.rows[0].stage != StagePending {
		t.Errorf("an out-of-range update touched a real row: %+v", m.rows[0])
	}
}

func TestDownloadCounts(t *testing.T) {
	m := newTestModel("A", "B", "C", "D")
	m.apply(Update{Index: 0, Stage: StageDone, Bytes: 1 << 20})
	m.apply(Update{Index: 1, Stage: StageSkipped})
	m.apply(Update{Index: 2, Stage: StageFailed, Err: errors.New("nope")})
	m.apply(Update{Index: 3, Stage: StageDownloading, Bytes: 512 << 10})

	complete, failed, bytes := m.counts()
	if complete != 2 {
		t.Errorf("complete = %d, want 2 (done + skipped)", complete)
	}
	if failed != 1 {
		t.Errorf("failed = %d, want 1", failed)
	}
	if want := int64(1<<20 + 512<<10); bytes != want {
		t.Errorf("bytes = %d, want %d", bytes, want)
	}
}

// Segment counts are preferred over byte counts: an HLS download knows its
// segment total up front, while its byte total only firms up as it goes.
func TestRowFraction(t *testing.T) {
	cases := []struct {
		name  string
		row   rowState
		want  float64
		known bool
	}{
		{"nothing known", rowState{stage: StageDownloading}, 0, false},
		{"by segments", rowState{stage: StageDownloading, segments: 75, totalSegs: 150}, 0.5, true},
		{"by bytes", rowState{stage: StageDownloading, bytes: 25, totalBytes: 100}, 0.25, true},
		{"segments win", rowState{stage: StageDownloading, segments: 1, totalSegs: 4, bytes: 99, totalBytes: 100}, 0.25, true},
		{"done is full", rowState{stage: StageDone}, 1, true},
		{"skipped is full", rowState{stage: StageSkipped}, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, known := tc.row.fraction()
			if known != tc.known {
				t.Fatalf("known = %v, want %v", known, tc.known)
			}
			if known && got != tc.want {
				t.Errorf("fraction = %v, want %v", got, tc.want)
			}
		})
	}
}

// The screen exists to replace signed CDN URLs with something readable, so it
// must never render one.
func TestDownloadViewShowsTitlesNotURLs(t *testing.T) {
	m := newTestModel("Tyre Trouble", "Guess Who Moo")
	m.apply(Update{Index: 0, Stage: StageDone, Bytes: 589 << 20, TotalSegs: 177})
	m.apply(Update{Index: 1, Stage: StageDownloading, Segments: 75, TotalSegs: 150})

	view := m.View()
	for _, want := range []string{"Tyre Trouble", "Guess Who Moo", "177 segments", "75/150 segments", "1/2 complete"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q:\n%s", want, view)
		}
	}
	for _, unwanted := range []string{"http", "token=", ".m3u8"} {
		if strings.Contains(view, unwanted) {
			t.Errorf("view leaked %q:\n%s", unwanted, view)
		}
	}
}

// A finished screen renders nothing, so the caller's summary is not printed
// under a frame implying work is still in flight.
func TestDownloadViewEmptyWhenDone(t *testing.T) {
	m := newTestModel("A")
	m.done = true
	if got := m.View(); got != "" {
		t.Errorf("finished view = %q, want empty", got)
	}
}

func TestDownloadFailureIsVisible(t *testing.T) {
	m := newTestModel("Broken One")
	m.apply(Update{Index: 0, Stage: StageFailed, Err: errors.New("http 500 Internal Server Error")})
	view := m.View()
	if !strings.Contains(view, "Broken One") || !strings.Contains(view, "1 failed") {
		t.Errorf("failure not surfaced:\n%s", view)
	}
}

// Without a terminal the screen must not animate; a log wants one line per
// finished file.
func TestRunDownloadsPlainPath(t *testing.T) {
	var buf bytes.Buffer
	items := []DownloadItem{{Title: "One"}, {Title: "Two"}}
	err := RunDownloads(&buf, "/out", items, func(ch chan<- Update) error {
		ch <- Update{Index: 0, Stage: StageDownloading, Segments: 5, TotalSegs: 10}
		ch <- Update{Index: 0, Stage: StageDone, Bytes: 1 << 20}
		ch <- Update{Index: 1, Stage: StageSkipped}
		return nil
	})
	if err != nil {
		t.Fatalf("RunDownloads: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"Downloading 2 files to /out", "One", "Two", "skipped"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.ContainsRune(out, '\x1b') {
		t.Errorf("plain output contains an escape sequence:\n%q", out)
	}
	// In-flight updates are not worth a line each; only outcomes are.
	if strings.Count(out, "One") != 1 {
		t.Errorf("expected one line for a finished file:\n%s", out)
	}
}

func TestRunDownloadsEmptyReturnsImmediately(t *testing.T) {
	var buf bytes.Buffer
	called := false
	err := RunDownloads(&buf, "/out", nil, func(chan<- Update) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("work ran for an empty item list")
	}
	if buf.Len() != 0 {
		t.Errorf("wrote output for an empty list: %q", buf.String())
	}
}

func TestRunDownloadsReturnsWorkError(t *testing.T) {
	sentinel := errors.New("download exploded")
	var buf bytes.Buffer
	err := RunDownloads(&buf, "/out", []DownloadItem{{Title: "A"}}, func(chan<- Update) error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("got %v, want %v", err, sentinel)
	}
}
