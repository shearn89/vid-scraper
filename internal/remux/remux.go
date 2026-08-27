// Package remux rewraps downloaded media into an MP4 container using ffmpeg.
//
// HLS streams whose segments are MPEG-TS land on disk as a .ts file that is
// byte-correct but in a container many media servers refuse to index. ffmpeg
// can move the same elementary streams into MP4 without touching the codecs,
// which is both lossless and far faster than a re-encode.
package remux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// ErrNotInstalled reports that no usable ffmpeg was found on PATH.
var ErrNotInstalled = errors.New("ffmpeg not found")

// maxStderr caps how much of ffmpeg's diagnostics reach the returned error. At
// -loglevel error the output is a line or two, so anything beyond this is a
// pathological stream spewing per-packet complaints.
const maxStderr = 2000

// lookPath is a variable so tests can simulate a machine without ffmpeg.
var lookPath = exec.LookPath

// The PATH lookup is stable for the lifetime of the process, and ToMP4 may be
// called once per downloaded file, so resolve it at most once.
var (
	lookupOnce sync.Mutex
	lookupDone bool
	ffmpegBin  string
	lookupErr  error
)

// ffmpegPath returns the resolved ffmpeg binary, or an error wrapping
// ErrNotInstalled.
func ffmpegPath() (string, error) {
	lookupOnce.Lock()
	defer lookupOnce.Unlock()
	if !lookupDone {
		ffmpegBin, lookupErr = lookPath("ffmpeg")
		if lookupErr != nil {
			lookupErr = fmt.Errorf("%w on PATH: %w", ErrNotInstalled, lookupErr)
		}
		lookupDone = true
	}
	return ffmpegBin, lookupErr
}

// resetLookupCache forgets a previous lookup. Only tests need it, as only they
// change what the lookup would return.
func resetLookupCache() {
	lookupOnce.Lock()
	defer lookupOnce.Unlock()
	lookupDone = false
	ffmpegBin = ""
	lookupErr = nil
}

// Available reports whether ffmpeg can be used, returning a descriptive error
// wrapping ErrNotInstalled when it cannot.
func Available() error {
	_, err := ffmpegPath()
	return err
}

// mp4Path derives the MP4 name for src by replacing its extension, so
// "tyre-trouble.ts" becomes "tyre-trouble.mp4" rather than growing a second
// extension.
func mp4Path(src string) string {
	return strings.TrimSuffix(src, filepath.Ext(src)) + ".mp4"
}

// ToMP4 rewraps src into an MP4 beside it and returns the new path. It never
// re-encodes. On success and when keepSource is false, src is removed. If src
// is already an .mp4 it is returned unchanged with no work.
//
// Should the rewrap succeed but src prove unremovable, the destination path is
// returned alongside the error: the MP4 is real and usable.
func ToMP4(ctx context.Context, src string, keepSource bool) (string, error) {
	if strings.EqualFold(filepath.Ext(src), ".mp4") {
		return src, nil
	}
	bin, err := ffmpegPath()
	if err != nil {
		return "", err
	}

	dst := mp4Path(src)
	switch _, err := os.Lstat(dst); {
	case err == nil:
		return "", fmt.Errorf("remux target already exists: %s", dst)
	case !errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("checking remux target %s: %w", dst, err)
	}

	// Build in a temporary file in the destination's own directory: the final
	// rename is then atomic and never crosses a filesystem, so a killed ffmpeg
	// cannot leave a truncated file wearing the real name. The ".mp4" suffix
	// matters too, as ffmpeg picks its muxer from the output extension.
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".remux-*.mp4")
	if err != nil {
		return "", fmt.Errorf("creating temporary file for %s: %w", dst, err)
	}
	tmpName := tmp.Name()
	// ffmpeg opens the path itself; we only wanted the reservation.
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("closing temporary file %s: %w", tmpName, err)
	}
	defer os.Remove(tmpName) // no-op once the rename below has moved it

	// -c copy keeps every stream bit-identical, and +faststart relocates the
	// moov atom to the head of the file, which is what lets a player start
	// before the whole download arrives.
	cmd := exec.CommandContext(ctx, bin,
		"-nostdin",
		"-loglevel", "error",
		"-y",
		"-i", src,
		"-c", "copy",
		"-movflags", "+faststart",
		tmpName,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("remuxing %s: %w", src, ctxErr)
		}
		return "", fmt.Errorf("remuxing %s: %w%s", src, err, stderrSuffix(stderr.String()))
	}

	if err := os.Rename(tmpName, dst); err != nil {
		return "", fmt.Errorf("moving remuxed file into place at %s: %w", dst, err)
	}
	if !keepSource {
		if err := os.Remove(src); err != nil {
			return dst, fmt.Errorf("removing source %s after remux: %w", src, err)
		}
	}
	return dst, nil
}

// stderrSuffix formats ffmpeg's diagnostics for inclusion in an error, since
// the exit status alone says nothing about what ffmpeg disliked.
func stderrSuffix(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) > maxStderr {
		s = s[:maxStderr] + "... (truncated)"
	}
	return ": " + s
}
