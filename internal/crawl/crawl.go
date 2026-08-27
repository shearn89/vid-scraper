// Package crawl walks from a starting page to a bounded depth, running
// discovery on each page it reaches.
package crawl

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/shearn89/vid-scraper/internal/fetch"
	"github.com/shearn89/vid-scraper/internal/scrape"
)

// Options configures a walk.
type Options struct {
	// Depth is how many hops beyond the starting page to follow. 0 visits only
	// the page given.
	Depth int
	// AllowHosts widens same-host-only following to these extra hosts.
	AllowHosts []string
	// Match, when set, restricts which anchor links are followed. It is not
	// applied to iframes: an iframe is the embedded player itself, which is
	// exactly the hop the user wants, and requiring it to match a page-URL
	// pattern would defeat that.
	Match *regexp.Regexp
	// Concurrency bounds simultaneous page fetches.
	Concurrency int
	// Delay is a courtesy pause between a worker's requests.
	Delay time.Duration
	// Scrape configures which discovery strategies run.
	Scrape scrape.Options
	// Logf, when set, receives verbose progress.
	Logf func(format string, args ...any)
}

// Crawler walks pages using a Fetcher.
type Crawler struct {
	fetcher fetch.Fetcher
	opts    Options
}

// New builds a Crawler.
func New(f fetch.Fetcher, opts Options) *Crawler {
	if opts.Concurrency <= 0 {
		opts.Concurrency = 4
	}
	return &Crawler{fetcher: f, opts: opts}
}

func (c *Crawler) logf(format string, args ...any) {
	if c.opts.Logf != nil {
		c.opts.Logf(format, args...)
	}
}

// Run walks from start and returns every video discovered, in the order found,
// along with any non-fatal errors encountered along the way.
//
// The walk proceeds one depth level at a time. Breadth-first by level keeps
// results in a predictable order and makes the depth bound obvious, which
// matters more here than squeezing out the last of the available parallelism.
func (c *Crawler) Run(ctx context.Context, start *url.URL) ([]scrape.Video, []error) {
	var (
		videos    []scrape.Video
		errs      []error
		seenVideo = make(map[string]bool)
		visited   = map[string]bool{start.String(): true}
		frontier  = []*url.URL{start}
	)

	for depth := 0; depth <= c.opts.Depth && len(frontier) > 0; depth++ {
		c.logf("depth %d: %d page(s)", depth, len(frontier))
		pages := c.fetchLevel(ctx, frontier)

		var next []*url.URL
		for _, p := range pages {
			if p.err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", p.url, p.err))
				continue
			}
			for _, v := range p.result.Videos {
				if seenVideo[v.URL] {
					continue
				}
				seenVideo[v.URL] = true
				videos = append(videos, v)
				c.logf("  found [%s/%s] %s", v.Strategy, v.Kind, v.URL)
			}
			if depth == c.opts.Depth {
				continue
			}
			for _, l := range p.result.Links {
				if visited[l.URL] || !c.shouldFollow(l, start) {
					continue
				}
				u, err := url.Parse(l.URL)
				if err != nil {
					continue
				}
				visited[l.URL] = true
				next = append(next, u)
			}
		}
		frontier = next
	}
	return videos, errs
}

// pageResult carries one page's outcome back from a worker.
type pageResult struct {
	url    string
	result *scrape.Result
	err    error
}

// fetchLevel fetches and discovers over one depth level concurrently, then
// restores the frontier's original order so output stays deterministic.
func (c *Crawler) fetchLevel(ctx context.Context, frontier []*url.URL) []pageResult {
	results := make([]pageResult, len(frontier))
	sem := make(chan struct{}, c.opts.Concurrency)
	var wg sync.WaitGroup

	for i, u := range frontier {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(i int, u *url.URL) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			res := pageResult{url: u.String()}
			c.logf("  fetch %s", u)
			page, err := c.fetcher.Fetch(ctx, u)
			if err != nil {
				res.err = err
			} else if !page.IsHTML() {
				// Not markup; nothing to discover. Not an error: a link to a
				// PDF or image is a normal thing to encounter while crawling.
				res.result = &scrape.Result{}
			} else {
				res.result, res.err = scrape.Discover(page.Body, page.URL, c.opts.Scrape)
			}
			results[i] = res
			if c.opts.Delay > 0 {
				select {
				case <-time.After(c.opts.Delay):
				case <-ctx.Done():
				}
			}
		}(i, u)
	}
	wg.Wait()

	// results is already indexed by frontier position, so filtering in place
	// preserves order; entries left zeroed belong to workers that never ran
	// because the context was cancelled.
	out := results[:0]
	for _, r := range results {
		if r.url != "" {
			out = append(out, r)
		}
	}
	return out
}

// shouldFollow decides whether a discovered link is in scope for the walk.
func (c *Crawler) shouldFollow(l scrape.Link, start *url.URL) bool {
	u, err := url.Parse(l.URL)
	if err != nil {
		return false
	}
	// Iframes are followed even cross-host: embedded players routinely live on
	// a separate domain, and that hop is the whole point of following them.
	if l.Kind == scrape.LinkIframe {
		return true
	}
	if !c.hostAllowed(u.Host, start.Host) {
		return false
	}
	if c.opts.Match != nil && !c.opts.Match.MatchString(l.URL) {
		return false
	}
	return true
}

func (c *Crawler) hostAllowed(host, startHost string) bool {
	if strings.EqualFold(host, startHost) {
		return true
	}
	for _, h := range c.opts.AllowHosts {
		if strings.EqualFold(h, host) {
			return true
		}
	}
	return false
}
