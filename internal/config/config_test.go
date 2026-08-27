package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig puts a config file at the standard location under a temporary
// XDG_CONFIG_HOME, so tests never touch the user's real ~/.config.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	dir := filepath.Join(xdg, appDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(dir, fileName)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestPathUsesXDGConfigHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	got, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if want := filepath.Join("/xdg", appDir, fileName); got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
}

func TestPathFallsBackToHomeConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", home)
	got, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if want := filepath.Join(home, ".config", appDir, fileName); got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
}

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DefaultDir != "" {
		t.Errorf("DefaultDir = %q, want empty", cfg.DefaultDir)
	}
	// A nil map would be fine to read but panics on write, so Load must
	// always hand back an allocated one.
	if cfg.Sites == nil {
		t.Fatal("Sites is nil, want an empty map")
	}
	if len(cfg.Sites) != 0 {
		t.Errorf("Sites = %v, want empty", cfg.Sites)
	}
	if got := cfg.DirFor("example.com"); got != "." {
		t.Errorf("DirFor = %q, want %q", got, ".")
	}
}

func TestLoadExpandsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeConfig(t, `
default_dir = "~/Videos"

[sites."farmvids.example"]
dir = "~/Videos/farmyard-friends"

[sites."absolute.example"]
dir = "/srv/media"

[sites."other.example"]
dir = "~someone/Videos"
`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := filepath.Join(home, "Videos"); cfg.DefaultDir != want {
		t.Errorf("DefaultDir = %q, want %q", cfg.DefaultDir, want)
	}
	if want := filepath.Join(home, "Videos", "farmyard-friends"); cfg.Sites["farmvids.example"].Dir != want {
		t.Errorf("farmvids.example dir = %q, want %q", cfg.Sites["farmvids.example"].Dir, want)
	}
	if got := cfg.Sites["absolute.example"].Dir; got != "/srv/media" {
		t.Errorf("absolute.example dir = %q, want it unchanged", got)
	}
	// ~user is not ours to guess at, so it must survive untouched.
	if got := cfg.Sites["other.example"].Dir; got != "~someone/Videos" {
		t.Errorf("other.example dir = %q, want it unchanged", got)
	}
}

func TestDirForFallsBackToDefaultDir(t *testing.T) {
	writeConfig(t, `
default_dir = "/media/default"

[sites."farmvids.example"]
dir = "/media/farmyard-friends"
`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.DirFor("farmvids.example"); got != "/media/farmyard-friends" {
		t.Errorf("DirFor(configured) = %q", got)
	}
	if got := cfg.DirFor("unknown.example"); got != "/media/default" {
		t.Errorf("DirFor(unknown) = %q, want the default", got)
	}
}

func TestDirForIgnoresCaseAndWWW(t *testing.T) {
	writeConfig(t, `
[sites."WWW.FarmVids.example"]
dir = "/media/farmyard-friends"
`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, host := range []string{
		"farmvids.example",
		"www.farmvids.example",
		"WWW.FARMVIDS.EXAMPLE",
		"FarmVids.example",
		"farmvids.example:8443",
	} {
		if got := cfg.DirFor(host); got != "/media/farmyard-friends" {
			t.Errorf("DirFor(%q) = %q, want /media/farmyard-friends", host, got)
		}
	}
}

func TestDirForReturnsAbsolutePath(t *testing.T) {
	writeConfig(t, `default_dir = "videos"`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := cfg.DirFor("example.com")
	if !filepath.IsAbs(got) {
		t.Errorf("DirFor = %q, want an absolute path", got)
	}
	if filepath.Base(got) != "videos" {
		t.Errorf("DirFor = %q, want it to end in videos", got)
	}
}

func TestDirForNilConfig(t *testing.T) {
	var cfg *Config
	if got := cfg.DirFor("example.com"); got != "." {
		t.Errorf("DirFor on nil = %q, want %q", got, ".")
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	tests := map[string]string{
		"top level": `defalt_dir = "~/Videos"`,
		"site table": `
[sites."farmvids.example"]
dirr = "~/Videos"
`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			path := writeConfig(t, body)

			_, err := LoadFrom(path)
			if err == nil {
				t.Fatal("LoadFrom succeeded, want an unknown-key error")
			}
			if !strings.Contains(err.Error(), "unknown key") {
				t.Errorf("error = %v, want it to mention an unknown key", err)
			}
		})
	}
}

func TestLoadFromReportsMalformedTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.toml")
	if err := os.WriteFile(path, []byte("default_dir = \n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := LoadFrom(path); err == nil {
		t.Fatal("LoadFrom succeeded, want a parse error")
	}
}

func TestLoadFromRejectsDuplicateHosts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dupes.toml")
	body := `
[sites."farmvids.example"]
dir = "/a"

[sites."WWW.farmvids.example"]
dir = "/b"
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := LoadFrom(path)
	if err == nil {
		t.Fatal("LoadFrom succeeded, want a duplicate-host error")
	}
	if !strings.Contains(err.Error(), "duplicate host") {
		t.Errorf("error = %v, want it to mention a duplicate host", err)
	}
}

func TestLoadFromExampleFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg, err := LoadFrom(filepath.Join("..", "..", "config.example.toml"))
	if err != nil {
		t.Fatalf("LoadFrom(config.example.toml): %v", err)
	}
	if want := filepath.Join(home, "Videos"); cfg.DefaultDir != want {
		t.Errorf("DefaultDir = %q, want %q", cfg.DefaultDir, want)
	}
	if _, ok := cfg.Sites["farmvids.example"]; !ok {
		t.Errorf("Sites = %v, want an entry for farmvids.example", cfg.Sites)
	}
}
