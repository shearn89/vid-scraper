// Package crawl walks from a starting page to a bounded depth, running
// discovery on each page it reaches.
package crawl

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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
	// Progress, when set, is called as the walk advances with the running
	// totals of pages fetched and videos found. It is called from the fetch
	// workers as well as the main loop, so it must be safe for concurrent
	// use. It exists so a caller can show that a long crawl is alive.
	Progress func(pages, videos int)
}

// Crawler walks pages using a Fetcher.
type Crawler struct {
	fetcher fetch.Fetcher
	opts    Options
	// pages and videos are running totals for Options.Progress. They are
	// touched from the fetch workers, hence atomic.
	pages  atomic.Int64
	videos atomic.Int64
}

// report pushes the running totals to Options.Progress, if one is set.
func (c *Crawler) report() {
	if c.opts.Progress != nil {
		c.opts.Progress(int(c.pages.Load()), int(c.videos.Load()))
	}
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

// task is one page to visit, with its place in the discovery tree.
type task struct {
	url   *url.URL
	depth int
	// path locates this page among its siblings, all the way up to the start
	// page: {2, 0, 1} is the second child of the first child of the third link
	// on the start page. It is fixed by the shape of the site rather than by
	// when a worker happened to get to it, which is what lets results be
	// sorted back into a stable order after a concurrent walk.
	path []int
}

// found is a discovered video plus enough context to sort it deterministically.
type found struct {
	video scrape.Video
	path  []int
	index int // position within its own page
}

// taskQueue orders pending pages deepest-first.
//
// Depth order is what makes the walk pipeline. Media typically sits at the far
// end of a chain — catalogue, then programme page, then player — so finishing
// one chain yields a result, while spreading the same effort across every
// chain's first hop yields none. Taking the deepest available page next means
// a worker that has just fetched a programme page goes straight on to that
// page's frame instead of queueing behind its forty-five siblings.
//
// Ties break on tree position so the walk is reproducible rather than dependent
// on which worker happened to finish first.
//
// The slice is kept ordered worst-first so that pop takes the last element.
// Insertion is linear rather than a heap's logarithmic: container/heap works
// through `any`, and the unchecked type assertions that forces are a poor
// trade for a queue holding hundreds of entries whose real cost is the network.
type taskQueue []task

func (q taskQueue) Len() int { return len(q) }

// better reports whether a should be visited before b.
func better(a, b task) bool {
	if a.depth != b.depth {
		return a.depth > b.depth
	}
	return comparePaths(a.path, b.path) < 0
}

func (q *taskQueue) push(t task) {
	i := sort.Search(len(*q), func(i int) bool { return better((*q)[i], t) })
	*q = slices.Insert(*q, i, t)
}

func (q *taskQueue) pop() task {
	old := *q
	t := old[len(old)-1]
	*q = old[:len(old)-1]
	return t
}

// Run walks from start and returns every video discovered, along with any
// non-fatal errors encountered along the way.
//
// Pages are visited by a pool of workers that follow each page's links as soon
// as that page is parsed, rather than waiting for every page at the current
// depth to finish first. That matters when the media only appears at the far
// end of a chain — a catalogue page leading to a programme page leading to a
// player — because a barrier at every level means nothing is found until the
// last one, and the whole walk looks stalled until it suddenly completes.
// Pipelining lets the first chain finish in the time it takes to fetch its own
// three pages, so results arrive steadily.
//
// Results are sorted by tree position before returning, so the order does not
// depend on which worker won a race, and repeated runs agree.
func (c *Crawler) Run(ctx context.Context, start *url.URL) ([]scrape.Video, []error) {
	var (
		mu        sync.Mutex
		hits      []found
		errs      []error
		seenVideo = make(map[string]bool)
		visited   = map[string]bool{start.String(): true}
	)

	// A fixed pool draws from a shared queue. Fixed rather than a goroutine
	// per page so that a site with tens of thousands of links cannot spawn a
	// goroutine for each one.
	var (
		queue  = &taskQueue{}
		cond   = sync.NewCond(&mu)
		active int
	)
	queue.push(task{url: start})

	worker := func() {
		for {
			mu.Lock()
			for queue.Len() == 0 && active > 0 && ctx.Err() == nil {
				cond.Wait()
			}
			// Nothing queued and nobody still working means the walk is done;
			// wake the other workers so they reach the same conclusion.
			if queue.Len() == 0 || ctx.Err() != nil {
				cond.Broadcast()
				mu.Unlock()
				return
			}
			t := queue.pop()
			active++
			mu.Unlock()

			c.logf("  fetch %s", t.url)
			result, err := c.discover(ctx, t.url)
			c.pages.Add(1)
			c.report()

			if c.opts.Delay > 0 {
				select {
				case <-time.After(c.opts.Delay):
				case <-ctx.Done():
				}
			}

			mu.Lock()
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", t.url, err))
			} else {
				for i, v := range result.Videos {
					if seenVideo[v.URL] {
						continue
					}
					seenVideo[v.URL] = true
					hits = append(hits, found{video: v, path: t.path, index: i})
					c.videos.Add(1)
					c.logf("  found [%s/%s] %s", v.Strategy, v.Kind, v.URL)
				}
				if t.depth < c.opts.Depth {
					var n int
					for _, l := range result.Links {
						if visited[l.URL] || !c.shouldFollow(l, start) {
							continue
						}
						u, parseErr := url.Parse(l.URL)
						if parseErr != nil {
							continue
						}
						visited[l.URL] = true
						queue.push(task{
							url:   u,
							depth: t.depth + 1,
							path:  append(append([]int{}, t.path...), n),
						})
						n++
					}
				}
			}
			active--
			cond.Broadcast()
			mu.Unlock()
			c.report()
		}
	}

	// Cancellation has to wake anyone parked on the condition variable, which
	// no context plumbing does on its own.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			mu.Lock()
			cond.Broadcast()
			mu.Unlock()
		case <-stop:
		}
	}()

	var wg sync.WaitGroup
	for range c.opts.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			worker()
		}()
	}
	wg.Wait()

	sort.SliceStable(hits, func(i, j int) bool {
		if cmp := comparePaths(hits[i].path, hits[j].path); cmp != 0 {
			return cmp < 0
		}
		return hits[i].index < hits[j].index
	})
	videos := make([]scrape.Video, 0, len(hits))
	for _, h := range hits {
		videos = append(videos, h.video)
	}
	return videos, errs
}

// discover fetches one page and runs the discovery appropriate to what came
// back.
func (c *Crawler) discover(ctx context.Context, u *url.URL) (*scrape.Result, error) {
	page, err := c.fetcher.Fetch(ctx, u)
	switch {
	case err != nil:
		return nil, err
	case scrape.IsSitemap(page.ContentType, page.Body):
		// Checked before IsHTML, which accepts XML: a sitemap is the site's
		// own list of its pages, and reading it is far cheaper and more
		// complete than inferring the same list by following links through a
		// catalogue.
		return scrape.DiscoverSitemap(page.Body, page.URL)
	case !page.IsHTML():
		// Not markup; nothing to discover. Not an error: a link to a PDF or
		// image is a normal thing to encounter while crawling.
		return &scrape.Result{}, nil
	default:
		return scrape.Discover(page.Body, page.URL, c.opts.Scrape)
	}
}

// comparePaths orders two tree positions, shallower first on a shared prefix.
func comparePaths(a, b []int) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return len(a) - len(b)
}

// destructivePath matches link targets that act on the session or the account
// rather than serving a page. A crawl carrying real cookies is an authenticated
// user agent, and following every link it finds would eventually sign that user
// out, cancel something, or delete something. Discovery gains nothing from
// these endpoints, so they are never followed.
var destructivePath = regexp.MustCompile(`(?i)(^|/)(sign[_-]?out|log[_-]?out|logoff|destroy|delete|remove|cancel|unsubscribe|revoke|deactivate|close[_-]account)(/|$|\?)`)

// shouldFollow decides whether a discovered link is in scope for the walk.
func (c *Crawler) shouldFollow(l scrape.Link, start *url.URL) bool {
	u, err := url.Parse(l.URL)
	if err != nil {
		return false
	}
	// Embedded documents — iframes and turbo-frames — are followed even
	// cross-host: players and lazy-loaded frames routinely live on a separate
	// domain, and that hop is the whole point of following them.
	if scrape.IsEmbedded(l.Kind) {
		return true
	}
	if destructivePath.MatchString(u.Path) {
		c.logf("  skipping %s (looks like it acts on the account, not a page)", l.URL)
		return false
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
