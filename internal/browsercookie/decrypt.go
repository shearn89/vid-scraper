package browsercookie

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/sha1"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Chrome on Linux derives its cookie key from these parameters. The password is
// the fixed string below for v10; for v11 it comes from the system keyring.
// Linux uses a single PBKDF2 iteration for both (it is macOS that uses 1003).
const (
	v10Password = "peanuts"
	keySalt     = "saltysalt"
	keyIters    = 1
	keyLen      = 16 // AES-128
)

// Version tags Chrome writes ahead of the ciphertext in encrypted_value.
const (
	v10Prefix = "v10" // key derived from the fixed password above
	v11Prefix = "v11" // key derived from a password in the system keyring
)

// errKeyringEncrypted marks a v11 value whose key could not be obtained or did
// not work. It is unexported because callers match on *KeyringError, which
// carries the cookie names the failure applies to.
var errKeyringEncrypted = errors.New("cookie key is held in the system keyring")

// keyringUnavailableError explains why a v11 cookie could not be decrypted. It
// satisfies errors.Is(err, errKeyringEncrypted) so the caller can group these
// rows without special-casing each reason.
type keyringUnavailableError struct {
	reason string
}

func (e *keyringUnavailableError) Error() string { return e.reason }
func (e *keyringUnavailableError) Unwrap() error { return errKeyringEncrypted }

// KeyringError reports that every matching cookie was encrypted with a key from
// the system keyring that could not be used. Only cookie names appear in the
// message; neither the keyring password nor a cookie value ever does.
type KeyringError struct {
	Names  []string
	Reason string // why the keyring key was unusable; never contains a secret
}

func (e *KeyringError) Error() string {
	msg := fmt.Sprintf("cookie(s) %s are encrypted with a key from the system keyring", strings.Join(e.Names, ", "))
	if e.Reason != "" {
		msg += " (" + e.Reason + ")"
	}
	return msg + "; read the value from the browser and pass it with --cookie instead"
}

func (e *KeyringError) Unwrap() error { return errKeyringEncrypted }

// v10Key derives Chrome's fixed Linux cookie key once; PBKDF2 with a single
// iteration is cheap, but the result is reused for every row regardless.
var v10Key = sync.OnceValues(func() ([]byte, error) {
	return deriveKey([]byte(v10Password))
})

// keyringPasswords is the seam tests replace so they never reach the real
// keyring or session bus.
var keyringPasswords = secretServicePasswords

// v11KeyCache memoises the keyring round trip: the daemon is asked once per
// process, not once per cookie. sync.OnceValues would do, except that tests
// need to reset it, so the state is explicit.
var v11KeyCache struct {
	sync.Mutex
	done bool
	keys [][]byte
	err  error
}

// v11Keys derives a key from every Chrome Safe Storage password the keyring
// offers, caching the result. There can be more than one (Chrome and Chrome
// Beta each store one), and nothing in the entry says which build it belongs
// to, so decryption tries them in turn.
func v11Keys() ([][]byte, error) {
	v11KeyCache.Lock()
	defer v11KeyCache.Unlock()
	if v11KeyCache.done {
		return v11KeyCache.keys, v11KeyCache.err
	}
	v11KeyCache.done = true

	passwords, err := keyringPasswords()
	if err != nil {
		v11KeyCache.err = err
		return nil, err
	}
	for _, pw := range passwords {
		key, err := deriveKey(pw)
		if err != nil {
			v11KeyCache.err = fmt.Errorf("deriving key: %w", err)
			return nil, v11KeyCache.err
		}
		v11KeyCache.keys = append(v11KeyCache.keys, key)
	}
	return v11KeyCache.keys, nil
}

func deriveKey(password []byte) ([]byte, error) {
	return pbkdf2.Key(sha1.New, string(password), []byte(keySalt), keyIters, keyLen)
}

// decrypt turns an encrypted_value blob into the cookie's plaintext value.
func decrypt(enc []byte) ([]byte, error) {
	if len(enc) < 3 {
		return nil, errors.New("encrypted value too short")
	}
	switch string(enc[:3]) {
	case v10Prefix:
		key, err := v10Key()
		if err != nil {
			return nil, fmt.Errorf("deriving key: %w", err)
		}
		return decryptCBC(key, enc[3:])
	case v11Prefix:
		return decryptV11(enc[3:])
	default:
		return nil, fmt.Errorf("unsupported encryption version %q", enc[:3])
	}
}

// decryptV11 tries each key the keyring yielded and returns the first plaintext
// that decrypts cleanly. A cookie value is printable ASCII, so that is the test
// for having picked the right key when several are on offer.
func decryptV11(ct []byte) ([]byte, error) {
	keys, err := v11Keys()
	if err != nil {
		return nil, &keyringUnavailableError{reason: err.Error()}
	}

	var fallback []byte
	for _, key := range keys {
		plain, err := decryptCBC(key, ct)
		if err != nil {
			continue
		}
		if isPrintableASCII(plain) {
			return plain, nil
		}
		// Padding checked out, so this key is probably right even though the
		// value is not text. Keep it in case no key does better.
		if fallback == nil {
			fallback = plain
		}
	}
	if fallback != nil {
		return fallback, nil
	}
	return nil, &keyringUnavailableError{reason: "the keyring password did not decrypt the value"}
}

// decryptCBC undoes Chrome's AES-128-CBC layer: fixed IV, PKCS#7 padding, and
// on newer versions a domain hash ahead of the value.
func decryptCBC(key, ct []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("initialising cipher: %w", err)
	}
	if len(ct) == 0 || len(ct)%block.BlockSize() != 0 {
		return nil, fmt.Errorf("ciphertext is %d bytes, not a whole number of blocks", len(ct))
	}
	// Chrome uses a constant IV of 16 spaces on Linux.
	iv := []byte(strings.Repeat(" ", block.BlockSize()))
	plain := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, ct)

	plain, err = unpadPKCS7(plain, block.BlockSize())
	if err != nil {
		return nil, err
	}
	return stripDomainHash(plain), nil
}

// unpadPKCS7 strips the padding CBC requires. A bad pad means the wrong key, so
// it is reported rather than papered over.
func unpadPKCS7(b []byte, blockSize int) ([]byte, error) {
	if len(b) == 0 {
		return nil, errors.New("empty plaintext")
	}
	pad := int(b[len(b)-1])
	if pad == 0 || pad > blockSize || pad > len(b) {
		return nil, errors.New("invalid padding: wrong decryption key?")
	}
	for _, c := range b[len(b)-pad:] {
		if int(c) != pad {
			return nil, errors.New("invalid padding: wrong decryption key?")
		}
	}
	return b[:len(b)-pad], nil
}

// stripDomainHash removes the SHA-256 of the cookie's domain that newer Chrome
// versions prepend to the plaintext. There is no version flag for it, but the
// hash is binary while a cookie value is printable ASCII, so an unprintable
// leading block is the tell.
func stripDomainHash(plain []byte) []byte {
	if len(plain) > sha256.Size && !isPrintableASCII(plain[:sha256.Size]) {
		return plain[sha256.Size:]
	}
	return plain
}

func isPrintableASCII(b []byte) bool {
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}

// nowChromeMicros is the current time on Chrome's clock: microseconds since
// 1601-01-01 UTC.
func nowChromeMicros() int64 {
	return time.Now().UnixMicro() + chromeEpochOffsetMicros
}
