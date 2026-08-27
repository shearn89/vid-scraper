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
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"golang.org/x/term"

	"github.com/shearn89/vid-scraper/internal/browsercookie"
	appcfg "github.com/shearn89/vid-scraper/internal/config"
	"github.com/shearn89/vid-scraper/internal/crawl"
	"github.com/shearn89/vid-scraper/internal/download"
	"github.com/shearn89/vid-scraper/internal/fetch"
	"github.com/shearn89/vid-scraper/internal/hls"
	"github.com/shearn89/vid-scraper/internal/progress"
	"github.com/shearn89/vid-scraper/internal/remux"
	"github.com/shearn89/vid-scraper/internal/scrape"
	"github.com/shearn89/vid-scraper/internal/state"
	"github.com/shearn89/vid-scraper/internal/tui"
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
	cookieBrowse bool
	selectMode   bool
	remux        bool
	// titleTrim is compiled from the site's config; nil when unset.
	titleTrim    *regexp.Regexp
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
  vid-scraper                 choose from the sites in your config file

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
  -s, --select          pick what to download in an interactive list; videos
                        already present in the target directory start unticked
      --json            list as JSON (url, source, kind, strategy, title)
  -v, --verbose         report each fetch and which strategy found each hit

Downloading:
  -c, --concurrency N   parallel fetches and segment downloads (default 4)
      --skip-existing   do not re-download files already present
      --quality S       HLS variant: best (default), worst, or a height (720)
      --remux           rewrap MPEG-TS downloads into MP4 with ffmpeg. No
                        re-encoding, so it is quick and lossless; requires
                        ffmpeg on PATH

Requests:
      --user-agent S    override the User-Agent
      --header K:V      extra request header (repeatable)
      --cookie S        Cookie header value, for pages behind a login
      --cookie-from-browser
                        read the site's cookies from a local Chrome/Chromium
                        profile instead of pasting them by hand
      --timeout D       per-request timeout (default 30s)
      --render          render JavaScript before scraping (not yet implemented)

  -h, --help            show this help
      --version         show version

Configuration:
  ~/.config/vid-scraper/config.toml sets a download directory per site, so the
  target host decides where files land when -o is not given. See
  config.example.toml. An explicit -o always wins.

  Interface preferences you change in the picker are remembered per site in
  ~/.local/state/vid-scraper/state.toml. That file is written by vid-scraper
  and safe to delete; your config is never written to.

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
	bl(&c.cookieBrowse, "cookie-from-browser")
	bl(&c.selectMode, "s", "select")
	bl(&c.remux, "remux")
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
	if len(positional) > 1 {
		fmt.Fprint(stderr, usage)
		return ExitError
	}
	if c.render {
		fmt.Fprintln(stderr, "vid-scraper: --render is not implemented in this build.\n"+
			"The fetcher is behind an interface so a headless-browser backend can be added;\n"+
			"until then only server-returned HTML is scraped.")
		return ExitError
	}

	logf := func(string, ...any) {}
	if c.verbose {
		logf = func(format string, a ...any) { fmt.Fprintf(stderr, format+"\n", a...) }
	}

	// A config file is a convenience, never a requirement: a broken one is
	// worth reporting, but it must not stop a run that specified everything
	// on the command line.
	userCfg, err := appcfg.Load()
	if err != nil {
		fmt.Fprintf(stderr, "warning: ignoring config: %v\n", err)
		userCfg = &appcfg.Config{}
	}

	target := ""
	if len(positional) == 1 {
		target = positional[0]
	} else {
		// No URL given. Offer the configured sites rather than printing usage:
		// a site recorded in the config already carries everything a run needs,
		// so choosing one is the whole invocation.
		sites := userCfg.Launchable()
		if len(sites) == 0 {
			fmt.Fprint(stderr, usage)
			return ExitError
		}
		chosen, err := tui.SelectSite(sites)
		if err != nil {
			if errors.Is(err, tui.ErrAborted) {
				return ExitOK
			}
			if errors.Is(err, tui.ErrNoTerminal) {
				fmt.Fprintln(stderr, "vid-scraper: choosing a site needs a terminal.\n"+
					"Give a URL on the command line instead, for example:\n"+
					"  vid-scraper "+sites[0].Site.URL)
				return ExitError
			}
			fmt.Fprintf(stderr, "vid-scraper: %v\n", err)
			return ExitError
		}
		target = chosen.Site.URL
		// Picking a site is a request to review what it holds, so the video
		// picker follows: crawl, mark what is already downloaded, and let the
		// user confirm. That is the two-keypress path.
		c.selectMode = true
	}

	start, err := parseStartURL(target)
	if err != nil {
		fmt.Fprintf(stderr, "vid-scraper: %v\n", err)
		return ExitError
	}

	// Per-site settings stand in for flags the user did not give, so a
	// configured site needs no arguments at all.
	if site, ok := userCfg.SiteFor(start.Host); ok {
		applySiteDefaults(&c, fs, site, logf)
	}
	if !flagGiven(fs, "o", "output") && c.output == "." {
		c.output = userCfg.DirFor(start.Host)
		logf("output directory from config: %s", c.output)
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

	// Checked before crawling: discovering that the rewrap tool is missing
	// after downloading gigabytes would be a poor way to find out.
	if c.remux {
		if err := remux.Available(); err != nil {
			fmt.Fprintf(stderr, "vid-scraper: --remux needs ffmpeg: %v\n", err)
			fmt.Fprintln(stderr, "Install it with your package manager (for example: sudo apt install ffmpeg),\n"+
				"or drop --remux to keep the downloaded .ts files as they are.")
			return ExitError
		}
	}

	if c.cookieBrowse {
		if c.cookie != "" {
			fmt.Fprintln(stderr, "vid-scraper: --cookie and --cookie-from-browser are mutually exclusive.")
			return ExitError
		}
		hdr, err := browsercookie.CookiesFromAny(start.Host)
		if err != nil {
			fmt.Fprintf(stderr, "vid-scraper: %v\n", err)
			return ExitError
		}
		c.cookie = hdr
		logf("loaded browser cookies for %s", start.Host)
	}

	client := fetch.New(fetch.Options{
		UserAgent: c.userAgent,
		Headers:   c.headers,
		Cookie:    c.cookie,
		Timeout:   c.timeout,
	})

	var (
		videos    []scrape.Video
		crawlErrs []error
	)
	// A crawl of a whole catalogue runs for minutes. Behind a spinner that is
	// visibly working; without one it is indistinguishable from a hang, which
	// is what it looked like before. --verbose already prints every fetch, so
	// the spinner would only fight with it for the cursor.
	crawlWork := func(status tui.Status) error {
		crawler := crawl.New(client, crawl.Options{
			Depth:       c.depth,
			AllowHosts:  c.allowHosts,
			Match:       matchRe,
			Concurrency: c.concurrency,
			Delay:       100 * time.Millisecond,
			Scrape:      scrape.Options{ScriptScan: !c.noScriptScan},
			Logf:        logf,
			Progress: func(pages, found int) {
				status("%d page%s searched, %d video%s found",
					pages, plural(pages, "", "s"), found, plural(found, "", "s"))
			},
		})
		videos, crawlErrs = crawler.Run(ctx, start)
		return nil
	}
	if c.verbose {
		_ = crawlWork(func(string, ...any) {})
	} else if err := tui.WithSpinner(stderr, "Searching "+start.Host, crawlWork); err != nil {
		if errors.Is(err, tui.ErrAborted) {
			return ExitOK
		}
		fmt.Fprintf(stderr, "vid-scraper: %v\n", err)
		return ExitError
	}
	// A page that would not load is rarely the user's problem: sites carry
	// widgets and endpoints a crawler is not meant to reach, and a 403 from one
	// of them says nothing about whether the videos were found. In interactive
	// use they are noise above the thing actually being asked about, so they
	// are counted rather than listed; --verbose still shows each one.
	switch {
	case c.verbose:
		for _, e := range crawlErrs {
			fmt.Fprintf(stderr, "warning: %v\n", e)
		}
	case !c.selectMode:
		for _, e := range crawlErrs {
			fmt.Fprintf(stderr, "warning: %v\n", e)
		}
	case len(crawlErrs) > 0:
		logf("%d page%s could not be read", len(crawlErrs), plural(len(crawlErrs), "", "s"))
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

	if c.selectMode {
		site, _ := userCfg.SiteFor(start.Host)
		videos, err = selectVideos(c.output, start.Host, videos, c.titleTrim, site, stderr, logf)
		if err != nil {
			if errors.Is(err, tui.ErrAborted) {
				fmt.Fprintln(stderr, "Aborted; nothing downloaded.")
				return ExitOK
			}
			if errors.Is(err, tui.ErrNoTerminal) {
				fmt.Fprintln(stderr, "vid-scraper: --select needs a terminal.\n"+
					"Without one, use --list to see what was found, or drop --select to\n"+
					"download everything (--skip-existing avoids repeating what you have).")
				return ExitError
			}
			fmt.Fprintf(stderr, "vid-scraper: %v\n", err)
			return ExitError
		}
		if len(videos) == 0 {
			fmt.Fprintln(stderr, "Nothing selected.")
			return ExitOK
		}
	}

	// Interactive runs get the live screen; everything else keeps the running
	// log, which is what a script or a CI job wants to read back.
	if c.selectMode && !c.verbose {
		return downloadAllTUI(ctx, client, c, videos, stdout, stderr, logf)
	}
	return downloadAll(ctx, client, c, videos, stdout, stderr, logf)
}

// applySiteDefaults fills in settings the user did not pass explicitly from
// the site's config entry. An explicit flag always wins: the config records
// what a site usually needs, not what this particular run must do.
func applySiteDefaults(c *config, fs *flag.FlagSet, site appcfg.Site, logf func(string, ...any)) {
	if site.Dir != "" && !flagGiven(fs, "o", "output") {
		c.output = site.Dir
	}
	if site.Match != "" && !flagGiven(fs, "match") {
		c.match = site.Match
		logf("match from config: %s", c.match)
	}
	// --follow counts as having set the depth, so it is not overridden either.
	if site.Depth > 0 && !flagGiven(fs, "d", "depth", "f", "follow") {
		c.depth = site.Depth
		logf("depth from config: %d", c.depth)
	}
	if site.CookieFromBrowser && !flagGiven(fs, "cookie-from-browser") {
		c.cookieBrowse = true
	}
	if site.Remux && !flagGiven(fs, "remux") {
		c.remux = true
	}
	if site.TitleTrim != "" {
		// Already validated at config load, so a failure here cannot happen;
		// ignoring it keeps the caller from handling an impossible error.
		if re, err := regexp.Compile(site.TitleTrim); err == nil {
			c.titleTrim = re
			logf("title_trim from config: %s", site.TitleTrim)
		}
	}
}

// flagGiven reports whether the user set any of these flag names, as opposed
// to the value merely being the registered default. Aliases mean one setting
// has several names, so any of them counts.
func flagGiven(fs *flag.FlagSet, names ...string) bool {
	given := false
	fs.Visit(func(f *flag.Flag) {
		for _, n := range names {
			if f.Name == n {
				given = true
			}
		}
	})
	return given
}

// genericStems are URL basenames that name a delivery mechanism rather than a
// video. A file called "master" or "index" tells the user nothing, so a page
// title is worth preferring over any of these.
var genericStems = map[string]bool{
	"master": true, "index": true, "playlist": true, "stream": true,
	"manifest": true, "chunklist": true, "prog_index": true, "media": true,
	"video": true, "file": true, "download": true, "output": true,
}

// undownloadable reports why a discovered video cannot be fetched, or "" when
// it can. The picker and the downloader share this so the list you are offered
// is exactly the list that will be acted on.
func undownloadable(v scrape.Video) string {
	switch {
	case v.Kind == scrape.KindEmbed:
		return "a player page, with no direct media URL"
	case scrape.IsDASH(v.URL):
		return "a DASH manifest, which needs muxing"
	default:
		return ""
	}
}

// stemFor picks the filename stem for a video, with trim (may be nil) removed
// from the title first.
//
// A URL's own basename is normally the best name available — it is what the
// publisher called the file, and "promo.mp4" beats any title a page wrapped it
// in. The title only wins where the URL carries no real name: streaming
// manifests are addressed by delivery mechanism ("master.m3u8") or by an opaque
// CDN asset ID, neither of which a human would recognise on disk.
func stemFor(v scrape.Video, trim *regexp.Regexp) string {
	name := download.NameFromURL(v.URL)
	urlStem := strings.TrimSuffix(name, filepath.Ext(name))

	title := v.Title
	if trim != nil {
		title = trim.ReplaceAllString(title, "")
	}
	titleStem := download.NameFromTitle(title)

	uninformative := !hasAlphanumeric(urlStem) ||
		genericStems[strings.ToLower(urlStem)] ||
		scrape.IsHLS(v.URL) || scrape.IsDASH(v.URL)
	if uninformative && titleStem != "" {
		return titleStem
	}
	if hasAlphanumeric(urlStem) {
		return urlStem
	}
	return titleStem
}

// hasAlphanumeric reports whether s carries at least one letter or digit. A
// stem of "-" survives sanitising but names nothing, so it is treated the same
// as having no name at all.
func hasAlphanumeric(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// selectVideos runs the interactive picker, pre-marking videos the target
// directory already holds so a re-run defaults to fetching only what is new.
//
// Whether those rows are shown at all is remembered per site: the picker's
// starting state comes from the state file, falling back to the site's config
// and then to showing them, and wherever the user leaves the toggle is written
// back. Config is never rewritten, so a hand-annotated file stays as written.
func selectVideos(dir, host string, videos []scrape.Video, trim *regexp.Regexp, site appcfg.Site, stderr io.Writer, logf func(string, ...any)) ([]scrape.Video, error) {
	items := make([]tui.Item, 0, len(videos))
	var hidden int
	for _, v := range videos {
		// Offering a row that cannot be downloaded only invites the user to
		// tick it and then read a skip message. --list still reports these.
		if undownloadable(v) != "" {
			hidden++
			continue
		}
		stem := stemFor(v, trim)
		existing, _ := download.ExistingFile(dir, stem)
		items = append(items, tui.Item{
			Video:    v,
			Dest:     filepath.Join(dir, stem),
			Existing: existing,
		})
	}
	if hidden > 0 {
		fmt.Fprintf(stderr, "%d discovered entr%s hidden: nothing to download from %s.\n"+
			"Run with --list to see them.\n",
			hidden, plural(hidden, "y", "ies"), plural(hidden, "it", "them"))
	}
	show := true
	if site.ShowDownloaded != nil {
		show = *site.ShowDownloaded
	}
	st := state.Load()
	if remembered, known := st.ShowDownloaded(host); known {
		show = remembered
	}

	res, err := tui.SelectWith(items, tui.SelectOptions{ShowDownloaded: show})
	// Saved before the error is handled: quitting the picker is still a
	// statement about how the user wants it to look next time.
	if res.ShowDownloaded != show {
		st.SetShowDownloaded(host, res.ShowDownloaded)
		if saveErr := st.Save(); saveErr != nil {
			// Losing a preference is not worth failing a run over.
			logf("could not save interface state: %v", saveErr)
		}
	}
	if err != nil {
		return nil, err
	}
	out := make([]scrape.Video, 0, len(res.Chosen))
	for _, it := range res.Chosen {
		out = append(out, it.Video)
	}
	return out, nil
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

		if why := undownloadable(v); why != "" {
			fmt.Fprintf(stderr, "%s skipping %s: %s\n", label, why, v.URL)
			skipped++
			continue
		}

		stem := stemFor(v, c.titleTrim)

		// The downloaders match an exact filename; the library may hold the
		// same video under a different extension (an earlier HLS pull lands as
		// ".ts", a later one as ".mp4"). Checking here keeps --skip-existing
		// honest across that difference.
		if c.skipExisting {
			if p, ok := download.ExistingFile(c.output, stem); ok {
				fmt.Fprintf(stderr, "%s exists: %s\n", label, download.DisplayPath(p))
				continue
			}
		}

		fmt.Fprintf(stderr, "%s %s\n", label, v.URL)

		if scrape.IsHLS(v.URL) {
			res, err := streams.DownloadAs(ctx, v.URL, stem)
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

			path := res.Path
			if c.remux {
				out, err := remux.ToMP4(ctx, path, false)
				if err != nil {
					// The download itself succeeded, so this is a warning, not
					// a failure: the .ts is still on disk and still playable.
					fmt.Fprintf(stderr, "  warning: remux failed, keeping %s: %v\n",
						download.DisplayPath(path), err)
				} else if out != path {
					fmt.Fprintf(stderr, "  remuxed: %s\n", download.DisplayPath(out))
					path = out
				}
			}
			fmt.Fprintln(stdout, path)
			continue
		}

		res, err := direct.DownloadAs(ctx, v.URL, stem)
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

// plural picks between a singular and a plural form.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// downloadAllTUI is the interactive counterpart to downloadAll: same work,
// rendered as a live screen instead of a running log.
//
// Interactive use and piped use want opposite things. A person watching wants
// to know which file is going and how far in; a script wants the paths and
// nothing else. Signed CDN URLs run to hundreds of characters and belong in
// neither, so they appear only under --verbose.
func downloadAllTUI(ctx context.Context, client *fetch.Client, c config, videos []scrape.Video, stdout, stderr io.Writer, logf func(string, ...any)) int {
	items := make([]tui.DownloadItem, 0, len(videos))
	stems := make([]string, 0, len(videos))
	for _, v := range videos {
		stem := stemFor(v, c.titleTrim)
		title := v.Title
		if title == "" {
			title = stem
		}
		items = append(items, tui.DownloadItem{Title: title, Dest: filepath.Join(c.output, stem)})
		stems = append(stems, stem)
	}

	// current is written between files and read by the segment workers of the
	// file in flight, hence atomic.
	var (
		current atomic.Int64
		updates chan<- tui.Update
	)
	send := func(u tui.Update) {
		if updates != nil {
			u.Index = int(current.Load())
			updates <- u
		}
	}

	direct := download.New(client, download.Options{
		Dir:          c.output,
		SkipExisting: c.skipExisting,
		OnBytes: func(done, total int64) {
			send(tui.Update{Stage: tui.StageDownloading, Bytes: done, TotalBytes: total})
		},
	})
	streams := hls.New(client, hls.Options{
		Dir:          c.output,
		Quality:      c.quality,
		Concurrency:  c.concurrency,
		SkipExisting: c.skipExisting,
		Warnf:        func(f string, a ...any) { logf("  warning: "+f, a...) },
		Logf:         logf,
		OnSegment: func(done, total int) {
			send(tui.Update{Stage: tui.StageDownloading, Segments: done, TotalSegs: total})
		},
	})

	var (
		failures  int
		skipped   int
		completed []string
		failed    []error
	)

	err := tui.RunDownloads(stderr, download.DisplayPath(c.output), items, func(ch chan<- tui.Update) error {
		updates = ch
		for i, v := range videos {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			current.Store(int64(i))
			stem := stems[i]

			if c.skipExisting {
				if p, ok := download.ExistingFile(c.output, stem); ok {
					skipped++
					ch <- tui.Update{Index: i, Stage: tui.StageSkipped, Note: p}
					continue
				}
			}
			ch <- tui.Update{Index: i, Stage: tui.StageDownloading}

			path, bytes, segments, err := fetchOne(ctx, direct, streams, v, stem)
			if err != nil {
				failures++
				failed = append(failed, fmt.Errorf("%s: %w", items[i].Title, err))
				ch <- tui.Update{Index: i, Stage: tui.StageFailed, Err: err}
				continue
			}

			if c.remux && strings.EqualFold(filepath.Ext(path), ".ts") {
				ch <- tui.Update{Index: i, Stage: tui.StageRemuxing}
				if out, rerr := remux.ToMP4(ctx, path, false); rerr != nil {
					// The download succeeded, so this is a warning: the .ts is
					// still on disk and still plays.
					logf("remux failed for %s: %v", path, rerr)
				} else {
					path = out
				}
			}

			completed = append(completed, path)
			ch <- tui.Update{Index: i, Stage: tui.StageDone, Bytes: bytes, TotalSegs: segments}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, tui.ErrAborted) {
			fmt.Fprintln(stderr, "Aborted.")
			return ExitOK
		}
		fmt.Fprintf(stderr, "vid-scraper: %v\n", err)
		return ExitError
	}

	// Paths go to stdout so the interactive run is still pipeable, but only
	// when something is actually reading them: echoed to a terminal they are
	// just noise under the screen that already named every file.
	if !isTerminalWriter(stdout) {
		for _, p := range completed {
			fmt.Fprintln(stdout, p)
		}
	}

	fmt.Fprintf(stderr, "\nDone: %d downloaded", len(completed))
	if skipped > 0 {
		fmt.Fprintf(stderr, ", %d already present", skipped)
	}
	if failures > 0 {
		fmt.Fprintf(stderr, ", %d failed", failures)
	}
	fmt.Fprintf(stderr, " — %s\n", download.DisplayPath(c.output))
	for _, e := range failed {
		fmt.Fprintf(stderr, "  failed: %v\n", e)
	}
	if failures > 0 {
		return ExitError
	}
	return ExitOK
}

// fetchOne downloads one video with whichever downloader suits its kind.
func fetchOne(ctx context.Context, direct *download.Downloader, streams *hls.Downloader, v scrape.Video, stem string) (path string, bytes int64, segments int, err error) {
	if scrape.IsHLS(v.URL) {
		res, err := streams.DownloadAs(ctx, v.URL, stem)
		if err != nil {
			return "", 0, 0, err
		}
		return res.Path, res.Bytes, res.Segments, nil
	}
	res, err := direct.DownloadAs(ctx, v.URL, stem)
	if err != nil {
		return "", 0, 0, err
	}
	return res.Path, res.Bytes, 0, nil
}

// isTerminalWriter reports whether w is an interactive terminal.
func isTerminalWriter(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}
