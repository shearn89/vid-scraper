package hls

import (
	"os"
	"path/filepath"
	"testing"
)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return b
}

func TestParseMaster(t *testing.T) {
	pl, err := Parse(loadFixture(t, "master.m3u8"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !pl.IsMaster {
		t.Fatal("expected a master playlist")
	}
	if got := len(pl.Master.Variants); got != 3 {
		t.Fatalf("got %d variants, want 3", got)
	}
	v := pl.Master.Variants[2]
	if v.Bandwidth != 5000000 || v.Width != 1920 || v.Height != 1080 {
		t.Errorf("variant 2 = %+v", v)
	}
	// CODECS contains a comma inside quotes; splitting must not break on it.
	if v.Codecs != "avc1.640028,mp4a.40.2" {
		t.Errorf("codecs = %q, want the full quoted value", v.Codecs)
	}
	if v.URI != "1080p/index.m3u8" {
		t.Errorf("uri = %q", v.URI)
	}
}

func TestSelectVariant(t *testing.T) {
	vs := []Variant{
		{URI: "low", Bandwidth: 800000, Height: 360},
		{URI: "mid", Bandwidth: 2000000, Height: 720},
		{URI: "high", Bandwidth: 5000000, Height: 1080},
	}
	tests := []struct{ quality, want string }{
		{"best", "high"},
		{"", "high"},
		{"worst", "low"},
		{"720", "mid"},
		{"720p", "mid"},
		{"1080", "high"},
		{"480", "low"}, // nearest without exceeding
		{"240", "low"}, // below everything: smallest available
		{"4000", "high"},
	}
	for _, tc := range tests {
		got, err := SelectVariant(vs, tc.quality)
		if err != nil {
			t.Fatalf("SelectVariant(%q): %v", tc.quality, err)
		}
		if got.URI != tc.want {
			t.Errorf("SelectVariant(%q) = %q, want %q", tc.quality, got.URI, tc.want)
		}
	}
	if _, err := SelectVariant(vs, "enormous"); err == nil {
		t.Error("expected an error for an unparseable quality")
	}
	if _, err := SelectVariant(nil, "best"); err == nil {
		t.Error("expected an error for an empty variant list")
	}
}

func TestParseMedia(t *testing.T) {
	pl, err := Parse(loadFixture(t, "media.m3u8"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if pl.IsMaster {
		t.Fatal("expected a media playlist")
	}
	m := pl.Media
	if m.Live {
		t.Error("EXT-X-ENDLIST present, so Live should be false")
	}
	if got := len(m.Segments); got != 4 {
		t.Fatalf("got %d segments, want 4", got)
	}
	if m.TargetDuration != 10 {
		t.Errorf("target duration = %v", m.TargetDuration)
	}
	if m.Segments[0].URI != "seg0.ts" || m.Segments[0].Duration != 9.5 {
		t.Errorf("segment 0 = %+v", m.Segments[0])
	}
	// EXT-X-MEDIA-SEQUENCE seeds the sequence numbers used for IV derivation.
	if m.MediaSequence != 100 || m.Segments[0].Seq != 100 || m.Segments[3].Seq != 103 {
		t.Errorf("sequence numbering wrong: base=%d first=%d last=%d",
			m.MediaSequence, m.Segments[0].Seq, m.Segments[3].Seq)
	}
	for _, s := range m.Segments {
		if s.Key != nil {
			t.Errorf("unencrypted playlist produced a key on %s", s.URI)
		}
	}
}

func TestParseLiveRejected(t *testing.T) {
	pl, err := Parse(loadFixture(t, "live.m3u8"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !pl.Media.Live {
		t.Error("a playlist with no EXT-X-ENDLIST must be marked Live")
	}
}

func TestParseEncrypted(t *testing.T) {
	pl, err := Parse(loadFixture(t, "encrypted.m3u8"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	segs := pl.Media.Segments
	if len(segs) != 3 {
		t.Fatalf("got %d segments, want 3", len(segs))
	}
	if segs[0].Key == nil || segs[0].Key.Method != MethodAES128 {
		t.Fatalf("segment 0 key = %+v", segs[0].Key)
	}
	if segs[0].Key.URI != "key.bin" {
		t.Errorf("key uri = %q", segs[0].Key.URI)
	}
	if len(segs[0].Key.IV) != 16 {
		t.Errorf("IV length = %d, want 16", len(segs[0].Key.IV))
	}
	// METHOD=NONE mid-playlist must clear the key for later segments.
	if segs[2].Key != nil {
		t.Errorf("EXT-X-KEY:METHOD=NONE did not clear the key: %+v", segs[2].Key)
	}
}

func TestParseFMP4(t *testing.T) {
	pl, err := Parse(loadFixture(t, "fmp4.m3u8"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if pl.Media.InitURI != "init.mp4" {
		t.Errorf("init uri = %q", pl.Media.InitURI)
	}
	if got := streamName("https://x/stream/index.m3u8", pl.Media); got != "index.mp4" {
		t.Errorf("streamName = %q, want index.mp4 for an fMP4 stream", got)
	}
}

func TestParseRejectsNonPlaylist(t *testing.T) {
	if _, err := Parse([]byte("<html><body>not a playlist</body></html>")); err == nil {
		t.Error("expected an error for HTML input")
	}
	if _, err := Parse([]byte("#EXTM3U\n")); err == nil {
		t.Error("expected an error for a playlist with no variants or segments")
	}
}

func TestIsDRM(t *testing.T) {
	tests := []struct {
		key  *Key
		want bool
	}{
		{nil, false},
		{&Key{Method: MethodAES128}, false},
		{&Key{Method: MethodAES128, KeyFormat: "identity"}, false},
		{&Key{Method: MethodSampleAES}, true},
		{&Key{Method: MethodAES128, KeyFormat: "urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed"}, true},
	}
	for _, tc := range tests {
		if got := tc.key.IsDRM(); got != tc.want {
			t.Errorf("IsDRM(%+v) = %v, want %v", tc.key, got, tc.want)
		}
	}
}

func TestParseByteRange(t *testing.T) {
	if br := parseByteRange("1024@512", 0); br == nil || br.Length != 1024 || br.Offset != 512 {
		t.Errorf("explicit offset parsed as %+v", br)
	}
	// With no @offset, the range continues from the previous one.
	if br := parseByteRange("1024", 2048); br == nil || br.Offset != 2048 {
		t.Errorf("implicit offset parsed as %+v", br)
	}
	if br := parseByteRange("1024@512", 0); br.Header() != "bytes=512-1535" {
		t.Errorf("Header() = %q", br.Header())
	}
}
