// Package browsercookie reads a site's cookies out of a local Chrome or
// Chromium profile, so a session that already exists in the browser does not
// have to be copied out of DevTools by hand.
//
// Only Linux profile layouts are handled. The cookie database is SQLite and
// Chrome keeps it open, so it is always copied before being read; values are
// encrypted with the scheme described in decrypt.go. Cookie values are secrets:
// nothing here logs, prints or embeds one in an error, and errors name a cookie
// only by its name.
package browsercookie

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite" // pure-Go driver; keeps the binary CGO-free
)

// chromeEpochOffsetMicros converts Chrome's timestamps (microseconds since
// 1601-01-01 UTC) to Unix microseconds.
const chromeEpochOffsetMicros = 11644473600000000

// cookieDBName is the file Chrome stores cookies in, inside a profile
// directory.
const cookieDBName = "Cookies"

// Profile is a discovered browser profile.
type Profile struct {
	Browser string // short label, e.g. "chrome-beta"
	Name    string // profile directory name, e.g. "Default"
	Path    string // absolute path to the profile directory
}

// String renders a profile for verbose output. It never touches cookie data.
func (p Profile) String() string { return p.Browser + "/" + p.Name }

// browserRoot is a browser's user-data directory and the label reported for
// profiles found under it.
type browserRoot struct {
	label string
	dir   string
}

// browserRoots lists candidate user-data directories, most preferred first.
// Chrome Beta leads because that is the build this tool was written against;
// the rest follow the usual Debian/Ubuntu and Snap locations.
//
// Paths are resolved through os.UserConfigDir and os.UserHomeDir, which read
// the environment on every call, so tests can redirect them.
func browserRoots() []browserRoot {
	var (
		roots []browserRoot
		seen  = make(map[string]bool)
	)
	add := func(label, dir string) {
		if dir == "" || seen[dir] {
			return
		}
		seen[dir] = true
		roots = append(roots, browserRoot{label: label, dir: dir})
	}

	// Chrome honours XDG_CONFIG_HOME, so os.UserConfigDir finds a relocated
	// profile. But ~/.config is checked too, and not only as a fallback:
	// XDG_CONFIG_HOME may have been set to point this tool's own config
	// somewhere else, and that must not quietly hide the browser. Trying both
	// costs two stat calls and removes a confusing failure.
	var bases []string
	if cfg, err := os.UserConfigDir(); err == nil {
		bases = append(bases, cfg)
	}
	home, homeErr := os.UserHomeDir()
	if homeErr == nil {
		bases = append(bases, filepath.Join(home, ".config"))
	}
	for _, base := range bases {
		for _, name := range []string{"google-chrome-beta", "google-chrome", "chromium"} {
			add(strings.TrimPrefix(name, "google-"), filepath.Join(base, name))
		}
	}
	if homeErr == nil {
		add("chromium-snap", filepath.Join(home, "snap", "chromium", "common", "chromium"))
	}
	return roots
}

// Profiles lists candidate Chrome/Chromium profiles on this machine. A
// directory qualifies only if it actually holds a cookie database, so callers
// can try each in turn without re-checking.
func Profiles() []Profile {
	var found []Profile
	for _, root := range browserRoots() {
		entries, err := os.ReadDir(root.dir)
		if err != nil {
			continue
		}
		var names []string
		for _, e := range entries {
			if !e.IsDir() || !isProfileDir(e.Name()) {
				continue
			}
			if _, err := os.Stat(filepath.Join(root.dir, e.Name(), cookieDBName)); err != nil {
				continue
			}
			names = append(names, e.Name())
		}
		sortProfileNames(names)
		for _, n := range names {
			found = append(found, Profile{Browser: root.label, Name: n, Path: filepath.Join(root.dir, n)})
		}
	}
	return found
}

// isProfileDir reports whether a directory name is one of Chrome's profile
// directories. Sibling directories such as "Crash Reports" are not.
func isProfileDir(name string) bool {
	if name == "Default" {
		return true
	}
	rest, ok := strings.CutPrefix(name, "Profile ")
	if !ok {
		return false
	}
	_, err := strconv.Atoi(rest)
	return err == nil
}

// sortProfileNames orders Default first, then numbered profiles numerically so
// "Profile 10" does not sort ahead of "Profile 2".
func sortProfileNames(names []string) {
	rank := func(n string) int {
		if n == "Default" {
			return -1
		}
		i, err := strconv.Atoi(strings.TrimPrefix(n, "Profile "))
		if err != nil {
			return 1 << 30
		}
		return i
	}
	sort.Slice(names, func(i, j int) bool { return rank(names[i]) < rank(names[j]) })
}

// ErrNoCookies reports that no profile held a usable cookie for the domain.
var ErrNoCookies = errors.New("no cookies found for domain in any local browser profile")

// rawCookie is one row of the cookies table, before decryption.
type rawCookie struct {
	name      string
	value     string
	encrypted []byte
}

// Cookies returns a Cookie request-header value for domain, e.g.
// "_uscreen2_session=abc; other=def", read from the given profile.
//
// Cookies whose key lives in the system keyring cannot be decrypted here; if
// every match is one of those, the returned error is a *KeyringError so callers
// can suggest passing the cookie directly instead.
func Cookies(profile Profile, domain string) (string, error) {
	domain = normalizeDomain(domain)
	if domain == "" {
		return "", errors.New("empty domain")
	}

	rows, err := queryCookies(filepath.Join(profile.Path, cookieDBName), domain)
	if err != nil {
		return "", err
	}

	var (
		pairs    []string
		seen     = make(map[string]bool)
		keyring  []string
		unusable []string
	)
	for _, r := range rows {
		if seen[r.name] {
			// Rows arrive most-specific first, so the first match for a name
			// is the one Chrome would prefer.
			continue
		}
		value := r.value
		if len(r.encrypted) > 0 {
			plain, err := decrypt(r.encrypted)
			if errors.Is(err, errKeyringEncrypted) {
				keyring = append(keyring, r.name)
				continue
			}
			if err != nil {
				// A row we cannot read is not a reason to discard the rows we
				// can: profiles accumulate cookies written by older browser
				// versions with schemes this build does not know.
				unusable = append(unusable, r.name)
				continue
			}
			value = string(plain)
		}
		// A value that cannot go in a header would make net/http reject every
		// request built from this jar, so one malformed cookie must not take
		// the rest down with it.
		if value == "" || !validHeaderValue(value) {
			continue
		}
		seen[r.name] = true
		pairs = append(pairs, r.name+"="+value)
	}

	if len(pairs) == 0 {
		if len(keyring) > 0 {
			return "", &KeyringError{Names: keyring}
		}
		if len(unusable) > 0 {
			return "", fmt.Errorf("cookie(s) %s could not be decrypted", strings.Join(unusable, ", "))
		}
		return "", ErrNoCookies
	}
	return strings.Join(pairs, "; "), nil
}

// CookiesFromAny tries each profile in turn, returning the first that yields
// cookies for domain. A keyring failure is reported only if no other profile
// produced anything, since it is the one error a caller can act on.
func CookiesFromAny(domain string) (string, error) {
	profiles := Profiles()
	if len(profiles) == 0 {
		return "", errors.New("no Chrome or Chromium profile found")
	}
	var keyringErr *KeyringError
	for _, p := range profiles {
		header, err := Cookies(p, domain)
		if err == nil {
			return header, nil
		}
		var ke *KeyringError
		if errors.As(err, &ke) && keyringErr == nil {
			keyringErr = ke
		}
	}
	if keyringErr != nil {
		return "", keyringErr
	}
	return "", fmt.Errorf("%w (tried %d profile(s))", ErrNoCookies, len(profiles))
}

// normalizeDomain reduces user input to a bare hostname: a full URL, a leading
// dot or a port would all otherwise fail to match host_key.
func normalizeDomain(domain string) string {
	d := strings.TrimSpace(strings.ToLower(domain))
	if _, rest, ok := strings.Cut(d, "://"); ok {
		d = rest
	}
	d, _, _ = strings.Cut(d, "/")
	if host, _, ok := strings.Cut(d, ":"); ok {
		d = host
	}
	return strings.TrimPrefix(d, ".")
}

// queryCookies reads unexpired cookies for domain out of the database at
// dbPath. Chrome holds a lock on the live file, so a copy is read instead.
func queryCookies(dbPath, domain string) ([]rawCookie, error) {
	copyPath, cleanup, err := copyDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	db, err := sql.Open("sqlite", "file:"+copyPath+"?_pragma=busy_timeout(2000)")
	if err != nil {
		return nil, fmt.Errorf("opening cookie database: %w", err)
	}
	defer db.Close()

	// Longer host_key and path values are the more specific match, which is the
	// one to keep when the same cookie name is set at several scopes.
	const q = `SELECT name, value, encrypted_value, expires_utc
		FROM cookies
		WHERE host_key = ? OR host_key = ? OR host_key LIKE ?
		ORDER BY LENGTH(host_key) DESC, LENGTH(path) DESC`
	rows, err := db.Query(q, domain, "."+domain, "%."+domain)
	if err != nil {
		return nil, fmt.Errorf("querying cookies: %w", err)
	}
	defer rows.Close()

	nowChrome := nowChromeMicros()
	var out []rawCookie
	for rows.Next() {
		var (
			name    string
			value   sql.NullString
			enc     []byte
			expires sql.NullInt64
		)
		if err := rows.Scan(&name, &value, &enc, &expires); err != nil {
			return nil, fmt.Errorf("reading cookie row: %w", err)
		}
		// expires_utc of 0 marks a session cookie, which has not expired.
		if expires.Valid && expires.Int64 > 0 && expires.Int64 <= nowChrome {
			continue
		}
		out = append(out, rawCookie{name: name, value: value.String, encrypted: enc})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading cookies: %w", err)
	}
	return out, nil
}

// copyDB copies the cookie database, and any write-ahead log beside it, to a
// temporary directory. The WAL matters: without it a cookie written in the
// current browser session may not be visible in the copy.
func copyDB(dbPath string) (path string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "vid-scraper-cookies-")
	if err != nil {
		return "", nil, fmt.Errorf("creating temp dir: %w", err)
	}
	cleanup = func() { os.RemoveAll(dir) }

	dst := filepath.Join(dir, cookieDBName)
	if err := copyFile(dbPath, dst); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("copying cookie database: %w", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		// Absent sidecar files are normal, not an error.
		if err := copyFile(dbPath+suffix, dst+suffix); err != nil && !os.IsNotExist(err) {
			cleanup()
			return "", nil, fmt.Errorf("copying %s: %w", filepath.Base(dbPath+suffix), err)
		}
	}
	return dst, cleanup, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	// 0600: the copy holds the same secrets as the original.
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// validHeaderValue reports whether s may appear as an HTTP header value.
// net/http rejects control bytes other than tab, and a cookie carrying one is
// far more likely to be a decryption artefact than a value a server set.
func validHeaderValue(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < 0x20 && c != '\t') || c == 0x7f {
			return false
		}
	}
	return true
}
