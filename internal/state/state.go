// Package state stores the small preferences a run remembers for next time.
//
// It is deliberately separate from internal/config. A config file is written by
// the user and carries their comments and layout; rewriting it to record which
// way a toggle was left would destroy both. State is written by the program and
// read by the program, so it can be serialised freely.
package state

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/shearn89/vid-scraper/internal/config"
)

const (
	appDir   = "vid-scraper"
	fileName = "state.toml"
)

// Site is the remembered state for one host.
type Site struct {
	// ShowDownloaded is whether the picker last had already-downloaded rows
	// on screen. A pointer so that "never toggled" is distinguishable from
	// "deliberately set to false", which is what lets a configured default
	// apply until the user actually expresses a preference.
	ShowDownloaded *bool `toml:"show_downloaded"`
}

// State is the whole state file.
type State struct {
	Sites map[string]Site `toml:"sites"`
	// path is where this was loaded from, so Save need not resolve it again.
	path string
}

// Path returns the state file path, honouring XDG_STATE_HOME and falling back
// to ~/.local/state, which is where the XDG base directory specification puts
// data that persists between runs but is not worth backing up.
func Path() (string, error) {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locating the state directory: %w", err)
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, appDir, fileName), nil
}

// Load reads the state file. A missing or unreadable file is not an error:
// forgetting a preference must never stop a download.
func Load() *State {
	path, err := Path()
	if err != nil {
		return &State{Sites: map[string]Site{}}
	}
	return LoadFrom(path)
}

// LoadFrom reads a specific state file, for tests and unusual layouts.
func LoadFrom(path string) *State {
	s := &State{Sites: map[string]Site{}, path: path}

	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			// Deliberately swallowed beyond this: state is a convenience, and
			// a corrupt file should cost the user their preference, not their
			// run. It is rewritten on the next Save.
			return s
		}
		return s
	}

	var raw State
	if _, err := toml.Decode(string(data), &raw); err != nil {
		return s
	}
	for host, site := range raw.Sites {
		s.Sites[config.NormaliseHost(host)] = site
	}
	return s
}

// ShowDownloaded reports the remembered preference for host, and whether one
// was remembered at all.
func (s *State) ShowDownloaded(host string) (value, known bool) {
	if s == nil {
		return false, false
	}
	site, ok := s.Sites[config.NormaliseHost(host)]
	if !ok || site.ShowDownloaded == nil {
		return false, false
	}
	return *site.ShowDownloaded, true
}

// SetShowDownloaded records the preference for host in memory. Call Save to
// write it out.
func (s *State) SetShowDownloaded(host string, value bool) {
	if s == nil {
		return
	}
	if s.Sites == nil {
		s.Sites = map[string]Site{}
	}
	key := config.NormaliseHost(host)
	site := s.Sites[key]
	site.ShowDownloaded = &value
	s.Sites[key] = site
}

// Save writes the state file, creating its directory if needed.
//
// The write goes to a temporary file in the same directory and is renamed into
// place, so an interrupted save leaves the previous state intact rather than a
// half-written file that fails to parse.
func (s *State) Save() error {
	if s == nil {
		return nil
	}
	path := s.path
	if path == "" {
		var err error
		if path, err = Path(); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating the state directory: %w", err)
	}

	var b strings.Builder
	b.WriteString("# Written by vid-scraper to remember interface preferences.\n")
	b.WriteString("# Safe to delete; it will be recreated.\n\n")
	if err := toml.NewEncoder(&b).Encode(s); err != nil {
		return fmt.Errorf("encoding state: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*.toml")
	if err != nil {
		return fmt.Errorf("creating a temporary state file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.WriteString(b.String()); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing state: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replacing the state file: %w", err)
	}
	return nil
}
