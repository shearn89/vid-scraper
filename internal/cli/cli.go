// Package cli wires flag parsing to discovery and downloading.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/shearn89/vid-scraper/internal/crawl"
	"github.com/shearn89/vid-scraper/internal/download"
	"github.com/shearn89/vid-scraper/internal/fetch"
	"github.com/shearn89/vid-scraper/internal/hls"
	"github.com/shearn89/vid-scraper/internal/progress"
	"github.com/shearn89/vid-scraper/internal/scrape"
)

// Exit codes.
const (
	ExitOK       = 0
	ExitError    = 1
	ExitNotFound = 2
)

// repeatable collects a flag that may be given more than once.
type repeatable []string

func (r *repeatable) String() string { return strings.Join(*r, ",") }
func (r *repeatable) Set(v string) error {
	*r = append(*r, v)
	return nil
}

// config holds parsed flags.
type config struct {
	output       string
	list         bool
	asJSON       bool
	depth        int
	follow       bool
	match        string
	allowHosts   repeatable
	headers      repeatable
	concurrency  int
	skipExisting bool
	userAgent    string
	cookie       string
	timeout      time.Duration
	quality      string
	noScriptScan bool
	render       bool
	verbose      bool
	version      bool
}

// Version is overridable at build time with -ldflags.
var Version = "dev"

const usage = `vid-scraper - find and download videos linked from a webpage

Usage:
  vid-scraper [flags] <url>

Discovery:
  -d, --depth N         follow links this many hops beyond the page (default 0)
  -f, --follow          shorthand for --depth 1
      --match REGEX     only follow links whose URL matches (anchors only;
                        iframes are always followed, being embedded players)
      --allow-host H    also follow links to this host (repeatable)
      --no-script-scan  disable the inline-<script> URL sweep

Output:
  -o, --output DIR      download directory (default ".")
  -l, --list            list discovered videos instead of downloading
      --json            list as JSON (url, source, kind, strategy, title)
  -v, --verbose         report each fetch and which strategy found each hit

Downloading:
  -c, --concurrency N   parallel fetches and segment downloads (default 4)
      --skip-existing   do not re-download files already present
      --quality S       HLS variant: best (default), worst, or a height (720)

Requests:
      --user-agent S    override the User-Agent
      --header K:V      extra request header (repeatable)
      --cookie S        Cookie header value, for pages behind a login
      --timeout D       per-request timeout (default 30s)
      --render          render JavaScript before scraping (not yet implemented)

  -h, --help            show this help
      --version         show version

Notes:
  HLS (.m3u8) streams are downloaded natively. DASH (.mpd) is reported but not
  downloaded: it requires muxing separate audio and video streams, which needs
  an external tool.

Examples:
  vid-scraper --list https://example.com/gallery
  vid-scraper --follow --match '/videos/' -o ./out https://example.com/gallery
  vid-scraper --list --json https://example.com/page | jq -r '.[].url'
`

// Run parses arguments and executes, returning a process exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	var c config
	fs := flag.NewFlagSet("vid-scraper", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}

	str := func(p *string, def string, names ...string) {
		for _, n := range names {
			fs.StringVar(p, n, def, "")
		}
	}
	bl := func(p *bool, names ...string) {
		for _, n := range names {
			fs.BoolVar(p, n, false, "")
		}
	}
	in := func(p *int, def int, names ...string) {
		for _, n := range names {
			fs.IntVar(p, n, def, "")
		}
	}

	str(&c.output, ".", "o", "output")
	bl(&c.list, "l", "list")
	bl(&c.asJSON, "json")
	in(&c.depth, 0, "d", "depth")
	bl(&c.follow, "f", "follow")
	str(&c.match, "", "match")
	in(&c.concurrency, 4, "c", "concurrency")
	bl(&c.skipExisting, "skip-existing")
	str(&c.userAgent, "", "user-agent")
	str(&c.cookie, "", "cookie")
	str(&c.quality, "best", "quality")
	bl(&c.noScriptScan, "no-script-scan")
	bl(&c.render, "render")
	bl(&c.verbose, "v", "verbose")
	bl(&c.version, "version")
	fs.Var(&c.allowHosts, "allow-host", "")
	fs.Var(&c.headers, "header", "")
	fs.DurationVar(&c.timeout, "timeout", 30*time.Second, "")

	positional, err := parseArgs(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, usage)
			return ExitOK
		}
		fmt.Fprintf(stderr, "vid-scraper: %v\n\nRun 'vid-scraper --help' for usage.\n", err)
		return ExitError
	}
	if c.version {
		fmt.Fprintf(stdout, "vid-scraper %s\n", Version)
		return ExitOK
	}
	if len(positional) != 1 {
		fmt.Fprint(stderr, usage)
		return ExitError
	}
	if c.render {
		fmt.Fprintln(stderr, "vid-scraper: --render is not implemented in this build.\n"+
			"The fetcher is behind an interface so a headless-browser backend can be added;\n"+
			"until then only server-returned HTML is scraped.")
		return ExitError
	}

	start, err := parseStartURL(positional[0])
	if err != nil {
		fmt.Fprintf(stderr, "vid-scraper: %v\n", err)
		return ExitError
	}
	if c.follow && c.depth == 0 {
		c.depth = 1
	}

	var matchRe *regexp.Regexp
	if c.match != "" {
		if matchRe, err = regexp.Compile(c.match); err != nil {
			fmt.Fprintf(stderr, "vid-scraper: invalid --match pattern: %v\n", err)
			return ExitError
		}
	}

	logf := func(string, ...any) {}
	if c.verbose {
		logf = func(format string, a ...any) { fmt.Fprintf(stderr, format+"\n", a...) }
	}

	client := fetch.New(fetch.Options{
		UserAgent: c.userAgent,
		Headers:   c.headers,
		Cookie:    c.cookie,
		Timeout:   c.timeout,
	})

	crawler := crawl.New(client, crawl.Options{
		Depth:       c.depth,
		AllowHosts:  c.allowHosts,
		Match:       matchRe,
		Concurrency: c.concurrency,
		Delay:       100 * time.Millisecond,
		Scrape:      scrape.Options{ScriptScan: !c.noScriptScan},
		Logf:        logf,
	})

	videos, crawlErrs := crawler.Run(ctx, start)
	for _, e := range crawlErrs {
		fmt.Fprintf(stderr, "warning: %v\n", e)
	}

	if len(videos) == 0 {
		fmt.Fprintln(stderr, "No videos found.")
		if !c.verbose {
			fmt.Fprintln(stderr, "Try --follow to look one page deeper, or --verbose to see what was fetched.")
		}
		return ExitNotFound
	}

	if c.asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(videos); err != nil {
			fmt.Fprintf(stderr, "vid-scraper: %v\n", err)
			return ExitError
		}
		return ExitOK
	}
	if c.list {
		for _, v := range videos {
			fmt.Fprintln(stdout, v.URL)
		}
		return ExitOK
	}

	return downloadAll(ctx, client, c, videos, stdout, stderr, logf)
}

// downloadAll fetches every discovered video, routing each to the downloader
// that suits its kind.
func downloadAll(ctx context.Context, client *fetch.Client, c config, videos []scrape.Video, stdout, stderr io.Writer, logf func(string, ...any)) int {
	direct := download.New(client, download.Options{
		Dir:          c.output,
		SkipExisting: c.skipExisting,
		Progress:     true,
	})
	streams := hls.New(client, hls.Options{
		Dir:          c.output,
		Quality:      c.quality,
		Concurrency:  c.concurrency,
		Progress:     true,
		SkipExisting: c.skipExisting,
		Warnf:        func(f string, a ...any) { fmt.Fprintf(stderr, "  warning: "+f+"\n", a...) },
		Logf:         logf,
	})

	var failures, skipped int
	fmt.Fprintf(stderr, "Found %d video(s).\n", len(videos))

	for i, v := range videos {
		label := fmt.Sprintf("[%d/%d]", i+1, len(videos))

		switch {
		case v.Kind == scrape.KindEmbed:
			fmt.Fprintf(stderr, "%s skipping embed (no direct media URL): %s\n", label, v.URL)
			skipped++
			continue
		case scrape.IsDASH(v.URL):
			fmt.Fprintf(stderr, "%s skipping DASH manifest (needs muxing, not supported): %s\n", label, v.URL)
			skipped++
			continue
		}

		fmt.Fprintf(stderr, "%s %s\n", label, v.URL)

		if scrape.IsHLS(v.URL) {
			res, err := streams.Download(ctx, v.URL)
			if err != nil {
				fmt.Fprintf(stderr, "  failed: %v\n", err)
				failures++
				continue
			}
			if res.Existing {
				fmt.Fprintf(stderr, "  exists: %s\n", download.DisplayPath(res.Path))
				continue
			}
			fmt.Fprintf(stderr, "  saved:  %s (%s, %d segments)\n",
				download.DisplayPath(res.Path), progress.Bytes(res.Bytes), res.Segments)
			fmt.Fprintln(stdout, res.Path)
			continue
		}

		res, err := direct.Download(ctx, v.URL)
		if err != nil {
			fmt.Fprintf(stderr, "  failed: %v\n", err)
			failures++
			continue
		}
		if res.Existing {
			fmt.Fprintf(stderr, "  exists: %s\n", download.DisplayPath(res.Path))
			continue
		}
		verb := "saved"
		if res.Resumed {
			verb = "resumed"
		}
		fmt.Fprintf(stderr, "  %s:  %s (%s)\n", verb, download.DisplayPath(res.Path), progress.Bytes(res.Bytes))
		fmt.Fprintln(stdout, res.Path)
	}

	if skipped > 0 {
		fmt.Fprintf(stderr, "%d video(s) skipped as undownloadable; their URLs are listed above.\n", skipped)
	}
	if failures > 0 {
		fmt.Fprintf(stderr, "%d download(s) failed.\n", failures)
		return ExitError
	}
	return ExitOK
}

// parseArgs allows flags to appear before or after the URL, which stdlib flag
// does not do on its own but users reasonably expect.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// parseStartURL validates the target and supplies a scheme when the user gave
// a bare host, which is how URLs are usually pasted.
func parseStartURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("no URL given")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid URL %q: %w", raw, err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("invalid URL %q: no host", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported scheme %q: only http and https are supported", u.Scheme)
	}
	return u, nil
}
