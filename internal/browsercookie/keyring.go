package browsercookie

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/godbus/dbus/v5"
)

// The Secret Service API Chrome stores its key in. See
// https://specifications.freedesktop.org/secret-service/.
const (
	secretServiceName = "org.freedesktop.secrets"
	secretServicePath = dbus.ObjectPath("/org/freedesktop/secrets")
	serviceIface      = "org.freedesktop.Secret.Service"
	sessionIface      = "org.freedesktop.Secret.Session"
	itemIface         = "org.freedesktop.Secret.Item"
)

// busAddrEnv names the session bus. Its absence means there is no session bus
// to talk to, which is worth failing on immediately: it keeps a headless run,
// and the test suite, from waiting on a connection that cannot be made.
const busAddrEnv = "DBUS_SESSION_BUS_ADDRESS"

// keyringTimeout bounds the whole Secret Service exchange. A stuck or prompting
// daemon must never hang the tool: the caller has a usable fallback in
// --cookie, so giving up quickly is better than waiting.
const keyringTimeout = 5 * time.Second

// noPrompt is the path the Secret Service returns when an operation completed
// without needing to prompt the user.
const noPrompt = dbus.ObjectPath("/")

// errNeedsPrompt reports that unlocking the keyring would require the user to
// type a password into a dialog, which a CLI cannot satisfy on their behalf.
var errNeedsPrompt = errors.New("the keyring is locked and unlocking it needs an interactive prompt")

// safeStorageAttrs are the attribute sets Chrome and Chromium builds have used
// for the "Chrome Safe Storage" entry, in the order worth trying. Chrome Beta
// stores under application=chrome; the schema attribute covers builds that key
// on the libsecret schema instead.
var safeStorageAttrs = []map[string]string{
	{"application": "chrome"},
	{"application": "chromium"},
	{"xdg:schema": "chrome_libsecret_os_crypt_password_v2"},
}

// secret is the Secret Service's secret struct: (session, parameters, value,
// content type).
type secret struct {
	Session     dbus.ObjectPath
	Parameters  []byte
	Value       []byte
	ContentType string
}

// secretServicePasswords returns every Chrome Safe Storage password the local
// keyring holds. More than one can match: a machine with both Chrome and Chrome
// Beta installed has an entry per build, and the attributes do not distinguish
// them, so the caller tries each key in turn.
//
// The returned values are secrets and are never logged or placed in an error.
func secretServicePasswords() ([][]byte, error) {
	if os.Getenv(busAddrEnv) == "" {
		return nil, fmt.Errorf("no D-Bus session bus (%s is unset)", busAddrEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), keyringTimeout)
	defer cancel()

	// Connecting and authenticating happen before any context-aware call, so
	// the lookup runs in its own goroutine and the deadline is enforced here.
	// The channel is buffered so a late goroutine can finish and exit.
	type outcome struct {
		passwords [][]byte
		err       error
	}
	done := make(chan outcome, 1)
	go func() {
		pw, err := lookupSafeStorage(ctx)
		done <- outcome{pw, err}
	}()

	select {
	case o := <-done:
		return o.passwords, o.err
	case <-ctx.Done():
		return nil, fmt.Errorf("secret service did not respond within %s", keyringTimeout)
	}
}

// lookupSafeStorage performs the Secret Service exchange: open a session,
// search for the Chrome entry, unlock it if that can be done without a prompt,
// and read the secrets.
func lookupSafeStorage(ctx context.Context) ([][]byte, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, fmt.Errorf("connecting to the session bus: %w", err)
	}
	defer conn.Close()

	svc := conn.Object(secretServiceName, secretServicePath)

	// The "plain" algorithm sends the secret over the bus unencrypted. That is
	// safe here: the session bus is a per-user socket owned by this very user,
	// so the secret never crosses a privilege boundary that the encrypted
	// (DH-negotiated) session would protect it against. Anything able to read
	// the socket can already read the cookie database itself.
	var (
		output  dbus.Variant
		session dbus.ObjectPath
	)
	if err := svc.CallWithContext(ctx, serviceIface+".OpenSession", 0, "plain", dbus.MakeVariant("")).
		Store(&output, &session); err != nil {
		return nil, fmt.Errorf("opening a secret service session: %w", err)
	}
	defer func() {
		// Best effort: the daemon drops the session when the connection closes
		// anyway, so a failure here is not worth reporting.
		_ = conn.Object(secretServiceName, session).CallWithContext(ctx, sessionIface+".Close", 0).Err
	}()

	items, err := searchItems(ctx, svc)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, errors.New("no Chrome Safe Storage entry found in the keyring")
	}

	var passwords [][]byte
	for _, item := range items {
		var s secret
		if err := conn.Object(secretServiceName, item).
			CallWithContext(ctx, itemIface+".GetSecret", 0, session).Store(&s); err != nil {
			// One unreadable item should not sink the others.
			continue
		}
		if len(s.Value) > 0 {
			passwords = append(passwords, s.Value)
		}
	}
	if len(passwords) == 0 {
		return nil, errors.New("the Chrome Safe Storage entry could not be read")
	}
	return passwords, nil
}

// searchItems collects the item paths matching any known attribute set,
// unlocking locked ones when that needs no prompt. Duplicates are dropped:
// the attribute sets overlap by design.
func searchItems(ctx context.Context, svc dbus.BusObject) ([]dbus.ObjectPath, error) {
	var (
		found  []dbus.ObjectPath
		seen   = make(map[dbus.ObjectPath]bool)
		locked []dbus.ObjectPath
	)
	add := func(paths []dbus.ObjectPath) {
		for _, p := range paths {
			if !seen[p] {
				seen[p] = true
				found = append(found, p)
			}
		}
	}

	for _, attrs := range safeStorageAttrs {
		var unlockedPaths, lockedPaths []dbus.ObjectPath
		if err := svc.CallWithContext(ctx, serviceIface+".SearchItems", 0, attrs).
			Store(&unlockedPaths, &lockedPaths); err != nil {
			return nil, fmt.Errorf("searching the keyring: %w", err)
		}
		add(unlockedPaths)
		locked = append(locked, lockedPaths...)
	}

	// Only bother unlocking when nothing readable turned up; an already-usable
	// entry is the common case and must not trigger a dialog.
	if len(found) == 0 && len(locked) > 0 {
		unlocked, err := unlock(ctx, svc, locked)
		if err != nil {
			return nil, err
		}
		add(unlocked)
	}
	return found, nil
}

// unlock asks the Secret Service to unlock paths. If it answers with a prompt
// path, the unlock needs the user at a dialog, which this cannot supply, so it
// gives up rather than driving the prompt and blocking.
func unlock(ctx context.Context, svc dbus.BusObject, paths []dbus.ObjectPath) ([]dbus.ObjectPath, error) {
	var (
		unlocked []dbus.ObjectPath
		prompt   dbus.ObjectPath
	)
	if err := svc.CallWithContext(ctx, serviceIface+".Unlock", 0, paths).Store(&unlocked, &prompt); err != nil {
		return nil, fmt.Errorf("unlocking the keyring: %w", err)
	}
	if len(unlocked) == 0 && prompt != noPrompt {
		return nil, errNeedsPrompt
	}
	return unlocked, nil
}
