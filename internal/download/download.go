// Package download fetches media files over HTTP, streaming to disk with
// resume support.
package download

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/shearn89/vid-scraper/internal/fetch"
	"github.com/shearn89/vid-scraper/internal/progress"
)

// Options configures the downloader.
type Options struct {
	Dir          string
	SkipExisting bool
	Progress     bool
}

// Downloader fetches media files.
type Downloader struct {
	client *fetch.Client
	opts   Options
}

// New builds a Downloader.
func New(c *fetch.Client, opts Options) *Downloader {
	if opts.Dir == "" {
		opts.Dir = "."
	}
	return &Downloader{client: c, opts: opts}
}

// Result describes a completed download.
type Result struct {
	Path     string
	Bytes    int64
	Resumed  bool
	Existing bool // already present; nothing transferred
}

// Download streams rawURL to a file under the configured directory.
//
// Transfer goes to a sidecar ".part" file that is renamed into place only on
// success, so an interrupted run never leaves a truncated file that looks
// complete. The part file is keyed on a hash of the URL, which keeps resume
// correct even when two different URLs derive the same display name.
func (d *Downloader) Download(ctx context.Context, rawURL string) (*Result, error) {
	if err := os.MkdirAll(d.opts.Dir, 0o755); err != nil {
		return nil, err
	}

	name := NameFromURL(rawURL)
	if d.opts.SkipExisting {
		if p := filepath.Join(d.opts.Dir, name); exists(p) {
			return &Result{Path: p, Existing: true}, nil
		}
	}

	partPath := filepath.Join(d.opts.Dir, "."+name+"."+shortHash(rawURL)+".part")
	var have int64
	if fi, err := os.Stat(partPath); err == nil {
		have = fi.Size()
	}

	req, err := d.client.NewRequest(ctx, http.MethodGet, rawURL)
	if err != nil {
		return nil, err
	}
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var (
		flags   = os.O_CREATE | os.O_WRONLY
		resumed bool
	)
	switch resp.StatusCode {
	case http.StatusPartialContent:
		flags |= os.O_APPEND
		resumed = true
	case http.StatusOK:
		// The server ignored the range request (or there was nothing to
		// resume): start over rather than appending onto a partial file, which
		// would silently corrupt the result.
		flags |= os.O_TRUNC
		have = 0
	case http.StatusRequestedRangeNotSatisfiable:
		// The part file is already the full length; nothing left to transfer.
		if err := os.Rename(partPath, filepath.Join(d.opts.Dir, name)); err == nil {
			return &Result{Path: filepath.Join(d.opts.Dir, name), Bytes: have, Resumed: true}, nil
		}
		return nil, fmt.Errorf("http %s", resp.Status)
	default:
		return nil, fmt.Errorf("http %s", resp.Status)
	}

	f, err := os.OpenFile(partPath, flags, 0o644)
	if err != nil {
		return nil, err
	}

	total := resp.ContentLength
	if total > 0 {
		total += have
	}
	bar := progress.New(name, total, d.opts.Progress)
	bar.Add(have)

	written, copyErr := io.Copy(io.MultiWriter(f, bar), resp.Body)
	closeErr := f.Close()
	bar.Done()
	if copyErr != nil {
		// The part file is kept deliberately: the next run resumes from here.
		return nil, copyErr
	}
	if closeErr != nil {
		return nil, closeErr
	}

	// The final name may improve on the URL-derived one now that headers are
	// known; the part file's identity does not change, only the rename target.
	finalName := name
	if fromHeader := NameFromDisposition(resp.Header.Get("Content-Disposition")); fromHeader != "" {
		finalName = fromHeader
	}
	finalName = EnsureExt(finalName, resp.Header.Get("Content-Type"))

	finalPath := filepath.Join(d.opts.Dir, finalName)
	if d.opts.SkipExisting && exists(finalPath) {
		os.Remove(partPath)
		return &Result{Path: finalPath, Existing: true}, nil
	}
	finalPath = UniquePath(d.opts.Dir, finalName)
	if err := os.Rename(partPath, finalPath); err != nil {
		return nil, err
	}
	return &Result{Path: finalPath, Bytes: have + written, Resumed: resumed}, nil
}

// DisplayPath shortens an absolute path for logging when it sits under the
// working directory.
func DisplayPath(p string) string {
	if wd, err := os.Getwd(); err == nil {
		if rel, err := filepath.Rel(wd, p); err == nil && !strings.HasPrefix(rel, "..") {
			return rel
		}
	}
	return p
}
