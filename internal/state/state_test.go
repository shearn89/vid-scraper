package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPathHonoursXDGStateHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)

	got, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "vid-scraper", "state.toml"); got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
}

func TestPathFallsBackToLocalState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	got, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	// ~/.local/state is where the XDG spec puts state, as distinct from
	// ~/.config: a lost state file costs a preference, not a configuration.
	if want := filepath.Join(home, ".local", "state", "vid-scraper", "state.toml"); got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
}

// A missing state file is the normal first run, not a failure.
func TestLoadMissingFileIsEmpty(t *testing.T) {
	s := LoadFrom(filepath.Join(t.TempDir(), "nope.toml"))
	if s == nil {
		t.Fatal("LoadFrom returned nil")
	}
	if _, known := s.ShowDownloaded("example.com"); known {
		t.Error("a missing file should remember nothing")
	}
}

// A corrupt file costs the preference, never the run.
func TestLoadCorruptFileIsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.toml")
	if err := os.WriteFile(path, []byte("this is not = valid toml ["), 0o644); err != nil {
		t.Fatal(err)
	}
	s := LoadFrom(path)
	if _, known := s.ShowDownloaded("example.com"); known {
		t.Error("a corrupt file should remember nothing")
	}
	// It must still be usable and writable afterwards.
	s.SetShowDownloaded("example.com", false)
	if err := s.Save(); err != nil {
		t.Fatalf("Save after a corrupt load: %v", err)
	}
	if v, known := LoadFrom(path).ShowDownloaded("example.com"); !known || v {
		t.Error("state did not recover after a corrupt file")
	}
}

func TestRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.toml")
	s := LoadFrom(path)
	s.SetShowDownloaded("example.com", false)
	s.SetShowDownloaded("other.test", true)
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	again := LoadFrom(path)
	for _, tc := range []struct {
		host string
		want bool
	}{{"example.com", false}, {"other.test", true}} {
		got, known := again.ShowDownloaded(tc.host)
		if !known {
			t.Errorf("%s: not remembered", tc.host)
			continue
		}
		if got != tc.want {
			t.Errorf("%s = %v, want %v", tc.host, got, tc.want)
		}
	}
}

// "false" must survive as a deliberate choice rather than reading as unset,
// which is the whole reason the field is a pointer.
func TestFalseIsDistinguishableFromUnset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.toml")
	s := LoadFrom(path)
	s.SetShowDownloaded("example.com", false)
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	again := LoadFrom(path)
	got, known := again.ShowDownloaded("example.com")
	if !known {
		t.Fatal("a stored false read back as unset")
	}
	if got {
		t.Error("a stored false read back as true")
	}
	if _, known := again.ShowDownloaded("never-seen.test"); known {
		t.Error("an unseen host read back as known")
	}
}

// Hosts key the same way they do in the config, so one site is one entry.
func TestHostNormalisation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.toml")
	s := LoadFrom(path)
	s.SetShowDownloaded("WWW.Example.COM", false)
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	again := LoadFrom(path)
	for _, host := range []string{"example.com", "Example.com", "www.example.com", "WWW.EXAMPLE.COM"} {
		if _, known := again.ShowDownloaded(host); !known {
			t.Errorf("%s did not match the stored entry", host)
		}
	}
	if len(again.Sites) != 1 {
		t.Errorf("stored %d entries for one host, want 1", len(again.Sites))
	}
}

func TestSaveCreatesDirectoryAndIsReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deep", "nested", "state.toml")
	s := LoadFrom(path)
	s.SetShowDownloaded("example.com", true)
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("state file not written: %v", err)
	}
	// The header exists so someone finding this file knows what wrote it and
	// that deleting it is safe.
	if !strings.Contains(string(body), "vid-scraper") {
		t.Errorf("state file lacks an explanatory header:\n%s", body)
	}
	if !strings.Contains(string(body), "show_downloaded") {
		t.Errorf("state file lacks the setting:\n%s", body)
	}
	// No temporary files left behind by the atomic write.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("left %d files in the state directory, want 1", len(entries))
	}
}

// A nil *State is what a caller gets from a failed lookup; it must be inert
// rather than a panic.
func TestNilStateIsSafe(t *testing.T) {
	var s *State
	if _, known := s.ShowDownloaded("example.com"); known {
		t.Error("nil state claimed to know something")
	}
	s.SetShowDownloaded("example.com", true)
	if err := s.Save(); err != nil {
		t.Errorf("Save on nil state: %v", err)
	}
}
