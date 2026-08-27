package hls

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/shearn89/vid-scraper/internal/fetch"
	"github.com/shearn89/vid-scraper/internal/progress"
)

// ErrLive reports a playlist with no EXT-X-ENDLIST.
var ErrLive = errors.New("live stream (no EXT-X-ENDLIST): not supported")

// ErrDRM reports a stream we deliberately will not attempt.
var ErrDRM = errors.New("DRM-protected stream: not supported")

// Options configures an HLS download.
type Options struct {
	Dir string
	// Quality selects a variant from a master playlist: "best", "worst", or a
	// target height such as "720".
	Quality      string
	Concurrency  int
	Progress     bool
	SkipExisting bool
	// Warnf receives non-fatal warnings, notably the separate-audio case.
	Warnf func(format string, args ...any)
	// Logf receives verbose progress.
	Logf func(format string, args ...any)
	// OnSegment, when set, is called as each segment lands, with the number
	// finished and the total expected. It exists so a caller drawing its own
	// display can have the same numbers the built-in bar would use; it is
	// called from the segment workers, so it must be safe for concurrent use.
	OnSegment func(done, total int)
}

// Downloader assembles HLS streams into a single file.
type Downloader struct {
	client *fetch.Client
	opts   Options
}

// New builds a Downloader.
func New(c *fetch.Client, opts Options) *Downloader {
	if opts.Concurrency <= 0 {
		opts.Concurrency = 4
	}
	if opts.Dir == "" {
		opts.Dir = "."
	}
	if opts.Quality == "" {
		opts.Quality = "best"
	}
	return &Downloader{client: c, opts: opts}
}

// Result describes a completed stream download.
type Result struct {
	Path     string
	Bytes    int64
	Segments int
	Existing bool
}

func (d *Downloader) warnf(format string, args ...any) {
	if d.opts.Warnf != nil {
		d.opts.Warnf(format, args...)
	}
}

func (d *Downloader) logf(format string, args ...any) {
	if d.opts.Logf != nil {
		d.opts.Logf(format, args...)
	}
}

// Download fetches an M3U8 stream and writes it as a single file.
func (d *Downloader) Download(ctx context.Context, rawURL string) (*Result, error) {
	return d.DownloadAs(ctx, rawURL, "")
}

// DownloadAs is Download with an explicit filename stem, used when the caller
// knows a better name than the URL carries. An HLS master playlist is exactly
// that case: its URL commonly ends in an opaque CDN asset ID, while the page
// that embedded it knows the human title.
//
// The extension still comes from the playlist's own segment format, never from
// the stem, so a caller passing "chip-chipping" gets ".ts" or ".mp4" according
// to what the stream actually contains.
func (d *Downloader) DownloadAs(ctx context.Context, rawURL, stem string) (*Result, error) {
	media, mediaURL, err := d.resolveMedia(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	if media.Live {
		return nil, ErrLive
	}
	if len(media.Segments) == 0 {
		return nil, errors.New("playlist contains no segments")
	}
	for i := range media.Segments {
		if media.Segments[i].Key.IsDRM() {
			return nil, ErrDRM
		}
	}

	name := streamName(rawURL, media)
	if stem != "" {
		name = sanitizeName(stem) + path.Ext(name)
	}
	finalPath := filepath.Join(d.opts.Dir, name)
	if d.opts.SkipExisting {
		if _, err := os.Stat(finalPath); err == nil {
			return &Result{Path: finalPath, Existing: true}, nil
		}
	}
	if err := os.MkdirAll(d.opts.Dir, 0o755); err != nil {
		return nil, err
	}

	// Segments are cached in a sidecar directory so an interrupted run resumes
	// at segment granularity rather than starting the stream over.
	partDir := filepath.Join(d.opts.Dir, "."+name+".parts")
	if err := os.MkdirAll(partDir, 0o755); err != nil {
		return nil, err
	}

	if err := d.fetchSegments(ctx, media, mediaURL, partDir, name); err != nil {
		return nil, err
	}
	total, err := d.assemble(media, partDir, finalPath)
	if err != nil {
		return nil, err
	}
	os.RemoveAll(partDir)
	return &Result{Path: finalPath, Bytes: total, Segments: len(media.Segments)}, nil
}

// resolveMedia fetches rawURL and, when it is a master playlist, selects a
// variant and fetches that. It returns the media playlist and the URL it came
// from, which is the base for resolving segment URIs.
func (d *Downloader) resolveMedia(ctx context.Context, rawURL string) (*Media, *url.URL, error) {
	body, finalURL, err := d.client.GetBytes(ctx, rawURL)
	if err != nil {
		return nil, nil, err
	}
	pl, err := Parse(body)
	if err != nil {
		return nil, nil, err
	}
	if !pl.IsMaster {
		return pl.Media, finalURL, nil
	}

	v, err := SelectVariant(pl.Master.Variants, d.opts.Quality)
	if err != nil {
		return nil, nil, err
	}
	d.logf("selected variant %s (%d bps)", describeVariant(v), v.Bandwidth)
	d.warnSeparateAudio(pl.Master, v, finalURL)

	variantURL, err := resolveRef(finalURL, v.URI)
	if err != nil {
		return nil, nil, fmt.Errorf("resolving variant URI: %w", err)
	}
	body, finalURL, err = d.client.GetBytes(ctx, variantURL.String())
	if err != nil {
		return nil, nil, err
	}
	pl, err = Parse(body)
	if err != nil {
		return nil, nil, err
	}
	if pl.IsMaster {
		return nil, nil, errors.New("variant URI pointed at another master playlist")
	}
	return pl.Media, finalURL, nil
}

// warnSeparateAudio reports the one case where concatenation is not enough.
//
// When the selected video variant references an audio group whose renditions
// carry their own URI, the audio is a separate stream that would have to be
// muxed in. Concatenating only the video segments would produce a silent file
// that looks perfectly fine, so this is surfaced loudly rather than discovered
// on playback.
func (d *Downloader) warnSeparateAudio(m *Master, v Variant, base *url.URL) {
	if v.AudioGroup == "" {
		return
	}
	for _, r := range m.Renditions {
		if !strings.EqualFold(r.Type, "AUDIO") || r.GroupID != v.AudioGroup || r.URI == "" {
			continue
		}
		audioURL := r.URI
		if u, err := resolveRef(base, r.URI); err == nil {
			audioURL = u.String()
		}
		d.warnf("this stream carries audio as a separate track (%q); the downloaded file will have video only.\n"+
			"    Muxing needs an external tool. The audio playlist is:\n    %s", r.Name, audioURL)
		return
	}
}

// SelectVariant picks a variant by quality preference. "best" and "worst" go by
// bandwidth; a numeric preference selects the variant whose height is closest
// to the request, preferring not to exceed it.
func SelectVariant(vs []Variant, quality string) (Variant, error) {
	if len(vs) == 0 {
		return Variant{}, errors.New("master playlist has no variants")
	}
	sorted := append([]Variant(nil), vs...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Bandwidth < sorted[j].Bandwidth })

	switch strings.ToLower(strings.TrimSpace(quality)) {
	case "", "best":
		return sorted[len(sorted)-1], nil
	case "worst":
		return sorted[0], nil
	}

	want, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(quality), "p"))
	if err != nil {
		return Variant{}, fmt.Errorf("invalid quality %q: use best, worst, or a height such as 720", quality)
	}
	best := sorted[0]
	for _, v := range sorted {
		if v.Height <= want && v.Height >= best.Height {
			best = v
		}
	}
	// Every variant is taller than requested; take the smallest available.
	if best.Height > want {
		for _, v := range sorted {
			if v.Height > 0 && v.Height < best.Height {
				best = v
			}
		}
	}
	return best, nil
}

func describeVariant(v Variant) string {
	if v.Height > 0 {
		return fmt.Sprintf("%dx%d", v.Width, v.Height)
	}
	return v.URI
}

// fetchSegments downloads every segment (and the init segment, if any) into
// partDir concurrently. Files are named by index, which is what lets assembly
// restore strict playlist order regardless of completion order.
func (d *Downloader) fetchSegments(ctx context.Context, m *Media, base *url.URL, partDir, label string) error {
	bar := progress.New(label, int64(len(m.Segments)), d.opts.Progress)
	defer bar.Done()

	if m.InitURI != "" {
		if err := d.fetchOne(ctx, base, m.InitURI, m.InitRange, nil, nil, 0, filepath.Join(partDir, "init")); err != nil {
			return fmt.Errorf("init segment: %w", err)
		}
	}

	var (
		keys     = &keyCache{client: d.client}
		sem      = make(chan struct{}, d.opts.Concurrency)
		wg       sync.WaitGroup
		mu       sync.Mutex
		done     int64
		errFirst error
	)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	fail := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if errFirst == nil {
			errFirst = err
			// Stop the remaining workers: one bad segment makes the whole
			// stream unusable, so there is nothing to gain by finishing.
			cancel()
		}
	}
	complete := func() {
		mu.Lock()
		done++
		n := done
		mu.Unlock()
		bar.Set(n)
		if d.opts.OnSegment != nil {
			d.opts.OnSegment(int(n), len(m.Segments))
		}
	}

	for i, seg := range m.Segments {
		wg.Add(1)
		go func(i int, seg Segment) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()

			dst := filepath.Join(partDir, fmt.Sprintf("seg-%06d", i))
			if _, err := os.Stat(dst); err == nil {
				complete() // already fetched by an earlier, interrupted run
				return
			}

			// The key is resolved here, through the shared cache, so a
			// playlist whose segments share one key fetches it exactly once.
			rawKey, err := keys.get(ctx, base, seg.Key)
			if err != nil {
				fail(fmt.Errorf("segment %d key: %w", i, err))
				return
			}
			if err := d.fetchOne(ctx, base, seg.URI, seg.Range, rawKey, ivFor(seg), seg.Seq, dst); err != nil {
				fail(fmt.Errorf("segment %d (%s): %w", i, seg.URI, err))
				return
			}
			complete()
		}(i, seg)
	}
	wg.Wait()
	return errFirst
}

// ivFor returns the explicit IV declared for a segment, or nil when the
// playlist supplies none and it must be derived from the sequence number.
func ivFor(seg Segment) []byte {
	if seg.Key == nil {
		return nil
	}
	return seg.Key.IV
}

// fetchOne downloads a single segment, decrypting it when a key applies. The
// download goes to a ".part" sidecar and is renamed on success, so a file
// present in partDir is always known-complete and can be skipped on resume.
func (d *Downloader) fetchOne(ctx context.Context, base *url.URL, ref string, br *ByteRange, key, iv []byte, seq uint64, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	u, err := resolveRef(base, ref)
	if err != nil {
		return err
	}
	req, err := d.client.NewRequest(ctx, http.MethodGet, u.String())
	if err != nil {
		return err
	}
	if br != nil {
		req.Header.Set("Range", br.Header())
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("http %s", resp.Status)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if key != nil {
		if data, err = decryptAES128(data, key, iv, seq); err != nil {
			return err
		}
	}

	tmp := dst + ".part"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// assemble concatenates the init segment and every media segment, in playlist
// order, into the output file.
func (d *Downloader) assemble(m *Media, partDir, finalPath string) (int64, error) {
	out, err := os.Create(finalPath)
	if err != nil {
		return 0, err
	}
	defer out.Close()

	var total int64
	appendFile := func(p string) error {
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		n, err := io.Copy(out, f)
		total += n
		return err
	}

	if m.InitURI != "" {
		if err := appendFile(filepath.Join(partDir, "init")); err != nil {
			return total, fmt.Errorf("init segment: %w", err)
		}
	}
	for i := range m.Segments {
		if err := appendFile(filepath.Join(partDir, fmt.Sprintf("seg-%06d", i))); err != nil {
			return total, fmt.Errorf("segment %d: %w", i, err)
		}
	}
	return total, out.Sync()
}

// keyCache fetches and caches AES keys, which are typically shared across every
// segment in a playlist.
type keyCache struct {
	client *fetch.Client
	mu     sync.Mutex
	cache  map[string][]byte
}

func (kc *keyCache) get(ctx context.Context, base *url.URL, k *Key) ([]byte, error) {
	if k == nil || !strings.EqualFold(k.Method, MethodAES128) {
		return nil, nil
	}
	u, err := resolveRef(base, k.URI)
	if err != nil {
		return nil, err
	}
	id := u.String()

	kc.mu.Lock()
	if kc.cache == nil {
		kc.cache = make(map[string][]byte)
	}
	if v, ok := kc.cache[id]; ok {
		kc.mu.Unlock()
		return v, nil
	}
	kc.mu.Unlock()

	data, _, err := kc.client.GetBytes(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("fetching key %s: %w", id, err)
	}
	if len(data) != 16 {
		return nil, fmt.Errorf("key %s: expected 16 bytes, got %d", id, len(data))
	}

	kc.mu.Lock()
	kc.cache[id] = data
	kc.mu.Unlock()
	return data, nil
}

// decryptAES128 reverses AES-128-CBC segment encryption. When the playlist
// supplies no IV, the specification derives it from the segment's media
// sequence number as a 128-bit big-endian value.
func decryptAES128(data, key, iv []byte, seq uint64) ([]byte, error) {
	if len(data)%aes.BlockSize != 0 || len(data) == 0 {
		return nil, fmt.Errorf("encrypted segment length %d is not a multiple of the block size", len(data))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(iv) != aes.BlockSize {
		derived := make([]byte, aes.BlockSize)
		binary.BigEndian.PutUint64(derived[8:], seq)
		iv = derived
	}
	out := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, data)
	return stripPKCS7(out)
}

// stripPKCS7 removes the padding block appended before encryption.
func stripPKCS7(b []byte) ([]byte, error) {
	if len(b) == 0 {
		return nil, errors.New("empty plaintext")
	}
	pad := int(b[len(b)-1])
	if pad == 0 || pad > aes.BlockSize || pad > len(b) {
		return nil, fmt.Errorf("invalid PKCS#7 padding byte %d", pad)
	}
	for _, c := range b[len(b)-pad:] {
		if int(c) != pad {
			return nil, errors.New("invalid PKCS#7 padding")
		}
	}
	return b[:len(b)-pad], nil
}

// resolveRef resolves a possibly-relative playlist reference.
func resolveRef(base *url.URL, ref string) (*url.URL, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, errors.New("empty URI")
	}
	if base == nil {
		return url.Parse(ref)
	}
	return base.Parse(ref)
}

// streamName derives an output filename from the playlist URL, with an
// extension matching the segment container: fMP4 streams get .mp4, transport
// streams get .ts.
func streamName(rawURL string, m *Media) string {
	stem := "stream"
	if u, err := url.Parse(rawURL); err == nil {
		if b := strings.TrimSuffix(path.Base(u.Path), path.Ext(u.Path)); b != "" && b != "." && b != "/" {
			stem = b
		}
	}
	ext := ".ts"
	if m.InitURI != "" {
		ext = ".mp4"
	} else if len(m.Segments) > 0 {
		switch strings.ToLower(path.Ext(segPath(m.Segments[0].URI))) {
		case ".mp4", ".m4s":
			ext = ".mp4"
		case ".aac":
			ext = ".aac"
		}
	}
	return sanitizeName(stem) + ext
}

func segPath(ref string) string {
	if u, err := url.Parse(ref); err == nil {
		return u.Path
	}
	return ref
}

func sanitizeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`/\:*?"<>|`, r) {
			return '-'
		}
		return r
	}, s)
	s = strings.Trim(s, " .-")
	if s == "" {
		return "stream"
	}
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}
