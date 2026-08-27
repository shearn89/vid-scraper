// Package config loads the user's configuration file, which maps site hosts
// onto the directory their videos should be downloaded to.
//
// Configuration is entirely optional: a user who never writes a config file
// gets working defaults, so Load treats a missing file as success rather than
// forcing every caller to special-case it.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// appDir and fileName locate the config file beneath the config home.
const (
	appDir   = "vid-scraper"
	fileName = "config.toml"
)

// Site holds the per-host settings.
//
// Everything beyond Dir exists so a configured site can be run with no
// arguments at all: the settings that would otherwise be flags are recorded
// once, and the site picker turns them into a single keypress.
type Site struct {
	// Dir is the download directory for this host, with a leading ~ expanded.
	Dir string `toml:"dir"`
	// Name labels the site in the picker. Defaults to the host.
	Name string `toml:"name"`
	// URL is the page to start from when the site is chosen from the picker.
	// A site without one can still be configured for downloads; it just
	// cannot be launched without naming a URL on the command line.
	URL string `toml:"url"`
	// TitleTrim is a regular expression removed from a video's title before
	// the title becomes a filename. Sites routinely prefix every title with
	// the show's name, which is redundant once the files sit in a directory
	// named after that show: `title_trim = "^Farmyard Friends "` turns
	// "Farmyard Friends Go Digging" into "go-digging.mp4".
	TitleTrim string `toml:"title_trim"`
	// Match, Depth, CookieFromBrowser and Remux are defaults for the
	// equivalent flags. An explicitly given flag always wins.
	Match             string `toml:"match"`
	Depth             int    `toml:"depth"`
	CookieFromBrowser bool   `toml:"cookie_from_browser"`
	Remux             bool   `toml:"remux"`
}

// Launchable is a site that can be started from the picker: one with a URL.
type Launchable struct {
	Host string
	Site Site
}

// Label is what the picker shows for a site.
func (l Launchable) Label() string {
	if l.Site.Name != "" {
		return l.Site.Name
	}
	return l.Host
}

// Launchable returns every site carrying a URL, sorted by label, so a no-arg
// run has something to offer. Sites without a URL are omitted rather than
// listed and then failing on selection.
func (c *Config) Launchable() []Launchable {
	if c == nil {
		return nil
	}
	out := make([]Launchable, 0, len(c.Sites))
	for host, site := range c.Sites {
		if strings.TrimSpace(site.URL) == "" {
			continue
		}
		out = append(out, Launchable{Host: host, Site: site})
	}
	sort.Slice(out, func(i, j int) bool {
		if a, b := out[i].Label(), out[j].Label(); a != b {
			return a < b
		}
		return out[i].Host < out[j].Host
	})
	return out
}

// SiteFor returns the configured site for a host, and whether one exists.
func (c *Config) SiteFor(host string) (Site, bool) {
	if c == nil {
		return Site{}, false
	}
	key := normaliseHost(host)
	site, ok := c.Sites[key]
	if !ok {
		if h, _, found := strings.Cut(key, ":"); found {
			site, ok = c.Sites[h]
		}
	}
	return site, ok
}

// Config is the whole configuration file.
type Config struct {
	// DefaultDir is used for any host without its own entry.
	DefaultDir string `toml:"default_dir"`
	// Sites is keyed by host, e.g. "farmvids.example". Keys are normalised on
	// load (lowercased, "www." stripped) so lookups match however the user
	// happened to write them.
	Sites map[string]Site `toml:"sites"`
}

// Path returns the config file path, honouring XDG_CONFIG_HOME and falling
// back to ~/.config.
//
// os.UserConfigDir is deliberately not used: on macOS it resolves to
// ~/Library/Application Support, and we want one predictable location across
// platforms so a dotfiles repo can carry the same file everywhere.
func Path() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locating home directory: %w", err)
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, appDir, fileName), nil
}

// Load reads the config file from Path. A missing file is not an error: it
// yields defaults with an empty, non-nil Sites map.
func Load() (*Config, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	return LoadFrom(path)
}

// LoadFrom reads a specific config file, for tests and a --config flag.
func LoadFrom(path string) (*Config, error) {
	cfg := &Config{Sites: map[string]Site{}}

	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	var raw Config
	md, err := toml.Decode(string(data), &raw)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	// A silently ignored key means a typo like `dirr =` leaves the user
	// wondering why their setting had no effect, so refuse to load instead.
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("parsing %s: unknown key(s): %s", path, strings.Join(keys, ", "))
	}

	cfg.DefaultDir = expandHome(raw.DefaultDir)
	for host, site := range raw.Sites {
		key := normaliseHost(host)
		if key == "" {
			return nil, fmt.Errorf("parsing %s: empty host in [sites]", path)
		}
		if _, dup := cfg.Sites[key]; dup {
			return nil, fmt.Errorf("parsing %s: duplicate host %q in [sites]", path, key)
		}
		// Compiled here and discarded: a bad pattern must fail at load, where
		// the file being complained about is obvious, not mid-download.
		if site.TitleTrim != "" {
			if _, err := regexp.Compile(site.TitleTrim); err != nil {
				return nil, fmt.Errorf("parsing %s: site %q: invalid title_trim: %w", path, key, err)
			}
		}
		site.Dir = expandHome(site.Dir)
		cfg.Sites[key] = site
	}
	return cfg, nil
}

// DirFor returns the download directory for a host, falling back to
// DefaultDir and then to the current directory. Configured paths come back
// absolute; the "." fallback is left as-is so the caller downloads where it
// was invoked.
func (c *Config) DirFor(host string) string {
	if c == nil {
		return "."
	}
	key := normaliseHost(host)
	site, ok := c.Sites[key]
	// Callers pass a URL's Host, which carries a port for non-default ports.
	// Fall back to the bare hostname so an entry written without one matches.
	if !ok {
		if h, _, found := strings.Cut(key, ":"); found {
			site, ok = c.Sites[h]
		}
	}
	dir := c.DefaultDir
	if ok && site.Dir != "" {
		dir = site.Dir
	}
	if dir == "" {
		return "."
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	return abs
}

// normaliseHost puts a host into the form used as a Sites key: lowercase and
// without the "www." prefix, so example.com and WWW.Example.com are one site.
func normaliseHost(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	return strings.TrimPrefix(h, "www.")
}

// expandHome expands a leading "~/" to the user's home directory. A "~user"
// form is returned unchanged rather than guessed at, and so is any path when
// the home directory cannot be determined.
func expandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[len("~/"):])
}
