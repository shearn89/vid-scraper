package browsercookie

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/sha1"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// encryptV10 produces a blob in the shape Chrome writes: the "v10" tag, then
// AES-128-CBC over PKCS#7-padded plaintext under Chrome's fixed Linux key.
func encryptV10(t *testing.T, plain []byte) []byte {
	t.Helper()
	key, err := v10Key()
	if err != nil {
		t.Fatalf("v10Key: %v", err)
	}
	return encryptWithKey(t, key, v10Prefix, plain)
}

// encryptWithKey builds a Chrome-shaped encrypted_value for an arbitrary key
// and version tag, so v10 and v11 fixtures share one implementation.
func encryptWithKey(t *testing.T, key []byte, prefix string, plain []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("aes.NewCipher: %v", err)
	}
	bs := block.BlockSize()
	padded := append(append([]byte{}, plain...), padBytes(bs-len(plain)%bs)...)
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, []byte(strings.Repeat(" ", bs))).CryptBlocks(out, padded)
	return append([]byte(prefix), out...)
}

// testCookie is a row to seed into a synthetic cookie database.
type testCookie struct {
	host    string
	name    string
	value   string // plaintext column; left empty when enc is set
	enc     []byte
	path    string
	expires int64 // Chrome epoch microseconds; 0 is a session cookie
}

// writeDB builds a cookie database with Chrome's column layout in dir and
// returns a Profile pointing at it. No real browser profile is ever touched.
func writeDB(t *testing.T, cookies []testCookie) Profile {
	t.Helper()
	return writeDBIn(t, t.TempDir(), cookies)
}

// writeDBIn is writeDB against a caller-chosen directory, for tests that need
// the database to sit inside a fake browser profile tree.
func writeDBIn(t *testing.T, dir string, cookies []testCookie) Profile {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, cookieDBName))
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE cookies (
		host_key TEXT, name TEXT, value TEXT, encrypted_value BLOB,
		path TEXT, expires_utc INTEGER)`); err != nil {
		t.Fatalf("creating table: %v", err)
	}
	for _, c := range cookies {
		p := c.path
		if p == "" {
			p = "/"
		}
		if _, err := db.Exec(
			`INSERT INTO cookies (host_key, name, value, encrypted_value, path, expires_utc) VALUES (?, ?, ?, ?, ?, ?)`,
			c.host, c.name, c.value, c.enc, p, c.expires,
		); err != nil {
			t.Fatalf("inserting %s: %v", c.name, err)
		}
	}
	return Profile{Browser: "test", Name: "Default", Path: dir}
}

// future is a Chrome-epoch timestamp well ahead of now.
func future() int64 { return nowChromeMicros() + int64(24*time.Hour/time.Microsecond) }

func TestCookiesRoundTrip(t *testing.T) {
	p := writeDB(t, []testCookie{
		{host: "example.com", name: "_uscreen2_session", enc: encryptV10(t, []byte("abc123")), expires: 0},
		{host: ".example.com", name: "other", value: "def", expires: future()},
	})

	got, err := Cookies(p, "example.com")
	if err != nil {
		t.Fatalf("Cookies: %v", err)
	}
	if !strings.Contains(got, "_uscreen2_session=abc123") || !strings.Contains(got, "other=def") {
		t.Fatalf("header missing cookies: %q", got)
	}
	if strings.Count(got, "; ") != 1 {
		t.Errorf("expected two pairs joined by \"; \", got %q", got)
	}
}

func TestCookiesStripsDomainHash(t *testing.T) {
	// Chrome 130+ prepends a 32-byte SHA-256 of the domain to the plaintext.
	hash := make([]byte, 32)
	for i := range hash {
		hash[i] = byte(i) // binary, so unprintable
	}
	p := writeDB(t, []testCookie{
		{host: "example.com", name: "sess", enc: encryptV10(t, append(hash, []byte("value-after-hash")...))},
	})

	got, err := Cookies(p, "example.com")
	if err != nil {
		t.Fatalf("Cookies: %v", err)
	}
	if got != "sess=value-after-hash" {
		t.Fatalf("got %q, want the hash stripped", got)
	}
}

func TestCookiesSkipsExpired(t *testing.T) {
	p := writeDB(t, []testCookie{
		{host: "example.com", name: "stale", value: "old", expires: nowChromeMicros() - 1_000_000},
		{host: "example.com", name: "fresh", value: "new", expires: future()},
		{host: "example.com", name: "session", value: "live", expires: 0},
	})

	got, err := Cookies(p, "example.com")
	if err != nil {
		t.Fatalf("Cookies: %v", err)
	}
	if strings.Contains(got, "stale") {
		t.Errorf("expired cookie present in %q", got)
	}
	if !strings.Contains(got, "fresh=new") || !strings.Contains(got, "session=live") {
		t.Errorf("live cookies missing from %q", got)
	}
}

func TestCookiesDomainMatching(t *testing.T) {
	p := writeDB(t, []testCookie{
		{host: "example.com", name: "bare", value: "1"},
		{host: ".example.com", name: "dotted", value: "2"},
		{host: "videos.example.com", name: "sub", value: "3"},
		{host: "notexample.com", name: "lookalike", value: "4"},
		{host: "example.com.evil.net", name: "suffixed", value: "5"},
	})

	got, err := Cookies(p, "example.com")
	if err != nil {
		t.Fatalf("Cookies: %v", err)
	}
	for _, want := range []string{"bare=1", "dotted=2", "sub=3"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %q", want, got)
		}
	}
	for _, unwanted := range []string{"lookalike", "suffixed"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("unrelated domain %s matched in %q", unwanted, got)
		}
	}
}

func TestCookiesAcceptsURLAndPort(t *testing.T) {
	p := writeDB(t, []testCookie{{host: "example.com", name: "a", value: "1"}})

	for _, domain := range []string{"https://example.com/watch/1", ".Example.com", "example.com:443", " example.com "} {
		got, err := Cookies(p, domain)
		if err != nil {
			t.Fatalf("Cookies(%q): %v", domain, err)
		}
		if got != "a=1" {
			t.Errorf("Cookies(%q) = %q", domain, got)
		}
	}
}

func TestCookiesPrefersMostSpecificHost(t *testing.T) {
	p := writeDB(t, []testCookie{
		{host: ".example.com", name: "dup", value: "wide"},
		{host: "videos.example.com", name: "dup", value: "narrow"},
	})

	got, err := Cookies(p, "example.com")
	if err != nil {
		t.Fatalf("Cookies: %v", err)
	}
	if got != "dup=narrow" {
		t.Fatalf("got %q, want the most specific host_key to win", got)
	}
}

func TestCookiesNoMatch(t *testing.T) {
	p := writeDB(t, []testCookie{{host: "other.test", name: "a", value: "1"}})

	if _, err := Cookies(p, "example.com"); !errors.Is(err, ErrNoCookies) {
		t.Fatalf("err = %v, want ErrNoCookies", err)
	}
}

// stubKeyringPasswords swaps the keyring seam and clears the derived-key cache
// both before and after, so tests never reach a real secret service and never
// leak a cached key into one another.
func stubKeyringPasswords(t *testing.T, fn func() ([][]byte, error)) {
	t.Helper()
	resetV11Cache()
	prev := keyringPasswords
	keyringPasswords = fn
	t.Cleanup(func() {
		keyringPasswords = prev
		resetV11Cache()
	})
}

func resetV11Cache() {
	v11KeyCache.Lock()
	defer v11KeyCache.Unlock()
	v11KeyCache.done = false
	v11KeyCache.keys = nil
	v11KeyCache.err = nil
}

// stubKeyringUnavailable makes the v11 path behave as it does on a machine with
// no reachable keyring, so these tests assert on our logic rather than on
// whatever secret service the host happens to be running.
func stubKeyringUnavailable(t *testing.T) {
	t.Helper()
	_ = t
	stubKeyringPasswords(t, func() ([][]byte, error) {
		return nil, errors.New("no keyring in tests")
	})
}

func TestCookiesKeyringEncrypted(t *testing.T) {
	stubKeyringUnavailable(t)
	enc := append([]byte(v11Prefix), make([]byte, 16)...)
	p := writeDB(t, []testCookie{{host: "example.com", name: "sess", enc: enc}})

	_, err := Cookies(p, "example.com")
	var ke *KeyringError
	if !errors.As(err, &ke) {
		t.Fatalf("err = %v, want *KeyringError", err)
	}
	if len(ke.Names) != 1 || ke.Names[0] != "sess" {
		t.Errorf("names = %v, want [sess]", ke.Names)
	}
	if !strings.Contains(ke.Error(), "--cookie") {
		t.Errorf("error should point at the --cookie flag: %q", ke.Error())
	}
}

// A v11 cookie must not hide the v10 cookies alongside it.
func TestCookiesKeyringMixedWithV10(t *testing.T) {
	stubKeyringUnavailable(t)
	p := writeDB(t, []testCookie{
		{host: "example.com", name: "keyring", enc: append([]byte(v11Prefix), make([]byte, 16)...)},
		{host: "example.com", name: "plain", enc: encryptV10(t, []byte("ok"))},
	})

	got, err := Cookies(p, "example.com")
	if err != nil {
		t.Fatalf("Cookies: %v", err)
	}
	if got != "plain=ok" {
		t.Fatalf("got %q, want the decryptable cookie", got)
	}
}

func TestCookiesReadsWhileFileIsOpen(t *testing.T) {
	// The live database is copied before reading, so an open handle (Chrome's,
	// in practice) must not matter.
	p := writeDB(t, []testCookie{{host: "example.com", name: "a", value: "1"}})
	f, err := os.Open(filepath.Join(p.Path, cookieDBName))
	if err != nil {
		t.Fatalf("opening db: %v", err)
	}
	defer f.Close()

	if _, err := Cookies(p, "example.com"); err != nil {
		t.Fatalf("Cookies: %v", err)
	}
}

func TestCookiesMissingDatabase(t *testing.T) {
	p := Profile{Browser: "test", Name: "Default", Path: t.TempDir()}
	if _, err := Cookies(p, "example.com"); err == nil {
		t.Fatal("expected an error for a profile with no cookie database")
	}
	if _, err := Cookies(p, ""); err == nil {
		t.Fatal("expected an error for an empty domain")
	}
}

func TestUnpadPKCS7(t *testing.T) {
	tests := []struct {
		name    string
		in      []byte
		want    string
		wantErr bool
	}{
		{name: "full block of padding", in: append([]byte("0123456789abcdef"), padBytes(16)...), want: "0123456789abcdef"},
		{name: "single byte", in: append([]byte("abc"), 1), want: "abc"},
		{name: "empty", in: nil, wantErr: true},
		{name: "zero pad", in: []byte{'a', 0}, wantErr: true},
		{name: "pad longer than block", in: []byte{'a', 17}, wantErr: true},
		{name: "pad longer than input", in: []byte{'a', 5}, wantErr: true},
		{name: "inconsistent pad bytes", in: []byte{'a', 3, 2, 3}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := unpadPKCS7(tt.in, 16)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("got %q, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unpadPKCS7: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// padBytes returns the PKCS#7 padding for n missing bytes: n copies of n.
func padBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(n)
	}
	return b
}

func TestStripDomainHash(t *testing.T) {
	hash := make([]byte, 32)
	hash[0] = 0xff

	if got := stripDomainHash(append(hash, []byte("abc")...)); string(got) != "abc" {
		t.Errorf("hashed value: got %q, want %q", got, "abc")
	}
	// A 33+ byte value that is all printable is a real value, not a hash.
	long := strings.Repeat("a", 40)
	if got := stripDomainHash([]byte(long)); string(got) != long {
		t.Errorf("printable value was truncated: %q", got)
	}
	if got := stripDomainHash([]byte("short")); string(got) != "short" {
		t.Errorf("short value: got %q", got)
	}
}

func TestDecryptRejectsBadInput(t *testing.T) {
	if _, err := decrypt([]byte("v1")); err == nil {
		t.Error("expected an error for a truncated blob")
	}
	if _, err := decrypt([]byte("v09abcdef")); err == nil {
		t.Error("expected an error for an unknown version tag")
	}
	if _, err := decrypt([]byte("v10short")); err == nil {
		t.Error("expected an error for a partial block")
	}
	if _, err := decrypt(append([]byte(v10Prefix), make([]byte, 16)...)); err == nil {
		t.Error("expected a padding error for a blob encrypted with another key")
	}
}

func TestProfilesFindsProfilesWithCookieDBs(t *testing.T) {
	// HOME and XDG_CONFIG_HOME are redirected so the real browser profiles on
	// this machine are never opened.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	beta := filepath.Join(home, ".config", "google-chrome-beta")
	for _, name := range []string{"Default", "Profile 2", "Profile 10"} {
		mkProfile(t, beta, name, true)
	}
	mkProfile(t, beta, "Profile 3", false) // no Cookies file
	mkProfile(t, beta, "Crash Reports", true)
	mkProfile(t, filepath.Join(home, "snap", "chromium", "common", "chromium"), "Default", true)

	got := Profiles()
	var names []string
	for _, p := range got {
		names = append(names, p.String())
	}
	want := []string{"chrome-beta/Default", "chrome-beta/Profile 2", "chrome-beta/Profile 10", "chromium-snap/Default"}
	if len(names) != len(want) {
		t.Fatalf("got %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("got %v, want %v", names, want)
		}
	}
}

func mkProfile(t *testing.T, root, name string, withDB bool) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if withDB {
		if err := os.WriteFile(filepath.Join(dir, cookieDBName), []byte("not a real db"), 0o600); err != nil {
			t.Fatalf("writing %s: %v", dir, err)
		}
	}
}

func TestCookiesFromAny(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	// An earlier-ranked profile with nothing for the domain must not stop the
	// search at the profile that does have it.
	empty := filepath.Join(home, ".config", "google-chrome-beta", "Default")
	wanted := filepath.Join(home, ".config", "google-chrome-beta", "Profile 1")
	for _, d := range []string{empty, wanted} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	writeDBIn(t, empty, []testCookie{{host: "other.test", name: "a", value: "1"}})
	writeDBIn(t, wanted, []testCookie{{host: "example.com", name: "sess", enc: encryptV10(t, []byte("xyz"))}})

	got, err := CookiesFromAny("example.com")
	if err != nil {
		t.Fatalf("CookiesFromAny: %v", err)
	}
	if got != "sess=xyz" {
		t.Fatalf("got %q, want sess=xyz", got)
	}
}

func TestCookiesFromAnyWithNoProfiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	if _, err := CookiesFromAny("example.com"); err == nil {
		t.Fatal("expected an error when no browser profile exists")
	}
}

// TestCookiesV11RoundTrip exercises the v11 path end to end with a known
// keyring password, proving the only difference from v10 is where the password
// comes from.
func TestCookiesV11RoundTrip(t *testing.T) {
	const password = "pretend-this-came-from-gnome-keyring"
	key, err := pbkdf2.Key(sha1.New, password, []byte(keySalt), keyIters, keyLen)
	if err != nil {
		t.Fatalf("pbkdf2: %v", err)
	}

	stubKeyringPasswords(t, func() ([][]byte, error) {
		return [][]byte{[]byte(password)}, nil
	})

	enc := encryptWithKey(t, key, v11Prefix, []byte("v11-value"))
	p := writeDB(t, []testCookie{{host: "example.com", name: "sess", enc: enc}})

	got, err := Cookies(p, "example.com")
	if err != nil {
		t.Fatalf("Cookies: %v", err)
	}
	if got != "sess=v11-value" {
		t.Fatalf("got %q, want the decrypted v11 cookie", got)
	}
}
