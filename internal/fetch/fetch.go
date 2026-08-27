// Package fetch retrieves pages and media over HTTP.
//
// Fetcher is the seam that keeps JavaScript rendering a future drop-in: the
// crawler depends on the interface, not on the HTTP implementation, so a
// browser-backed fetcher can be substituted without touching callers.
package fetch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultUserAgent identifies the tool to servers that care.
const DefaultUserAgent = "vid-scraper/0.1 (+https://github.com/shearn89/vid-scraper)"

// maxPageBytes caps how much of a document we will read into memory when
// parsing HTML. Media downloads stream and are not subject to this.
const maxPageBytes = 32 << 20

// Page is a fetched document.
type Page struct {
	// URL is the final URL after redirects. Relative links must be resolved
	// against this, not against the URL originally requested.
	URL         *url.URL
	Body        []byte
	ContentType string
	Status      int
}

// IsHTML reports whether the page looks like markup worth parsing.
func (p *Page) IsHTML() bool {
	ct := strings.ToLower(p.ContentType)
	return strings.Contains(ct, "html") || strings.Contains(ct, "xml") ||
		ct == "" || strings.HasPrefix(ct, "text/plain")
}

// Fetcher retrieves a document. Implemented by Client, and in future by a
// headless-browser fetcher behind the --render flag.
type Fetcher interface {
	Fetch(ctx context.Context, u *url.URL) (*Page, error)
}

// Options configures outbound requests.
type Options struct {
	UserAgent    string
	Headers      []string // "Key: Value"
	Cookie       string
	Timeout      time.Duration
	MaxRedirects int
}

// Client is the HTTP implementation of Fetcher. It is also the shared request
// builder for the download and hls packages, so custom headers and cookies
// apply consistently to pages, media files and playlist segments alike.
type Client struct {
	HTTP *http.Client
	opts Options
}

// New builds a Client from opts, applying defaults for unset fields.
func New(opts Options) *Client {
	if opts.UserAgent == "" {
		opts.UserAgent = DefaultUserAgent
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}
	if opts.MaxRedirects <= 0 {
		opts.MaxRedirects = 10
	}
	return &Client{
		HTTP: &http.Client{
			// Timeout is deliberately not set on the client: it would abort
			// long media downloads mid-stream. Per-request deadlines come from
			// the context for page fetches; transport timeouts below bound the
			// parts that can actually hang.
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				TLSHandshakeTimeout:   opts.Timeout,
				ResponseHeaderTimeout: opts.Timeout,
				IdleConnTimeout:       90 * time.Second,
				MaxIdleConnsPerHost:   8,
			},
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= opts.MaxRedirects {
					return fmt.Errorf("stopped after %d redirects", opts.MaxRedirects)
				}
				return nil
			},
		},
		opts: opts,
	}
}

// NewRequest builds a request carrying the configured User-Agent, cookie and
// any extra headers.
func (c *Client) NewRequest(ctx context.Context, method, rawURL string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.opts.UserAgent)
	req.Header.Set("Accept", "*/*")
	if c.opts.Cookie != "" {
		req.Header.Set("Cookie", c.opts.Cookie)
	}
	for _, h := range c.opts.Headers {
		k, v, ok := strings.Cut(h, ":")
		if !ok {
			continue
		}
		req.Header.Set(strings.TrimSpace(k), strings.TrimSpace(v))
	}
	return req, nil
}

// Do issues a prepared request.
func (c *Client) Do(req *http.Request) (*http.Response, error) { return c.HTTP.Do(req) }

// Get issues a GET and returns the live response. The caller closes the body.
// Used by the download and hls packages, which stream rather than buffer.
func (c *Client) Get(ctx context.Context, rawURL string) (*http.Response, error) {
	req, err := c.NewRequest(ctx, http.MethodGet, rawURL)
	if err != nil {
		return nil, err
	}
	return c.Do(req)
}

// GetBytes fetches a small resource wholly into memory, such as a playlist or
// an encryption key.
func (c *Client) GetBytes(ctx context.Context, rawURL string) ([]byte, *url.URL, error) {
	ctx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()
	resp, err := c.Get(ctx, rawURL)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("http %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPageBytes))
	if err != nil {
		return nil, nil, err
	}
	return body, resp.Request.URL, nil
}

// Fetch retrieves a document, implementing Fetcher.
func (c *Client) Fetch(ctx context.Context, u *url.URL) (*Page, error) {
	ctx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()

	req, err := c.NewRequest(ctx, http.MethodGet, u.String())
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("http %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPageBytes))
	if err != nil {
		return nil, fmt.Errorf("reading body: %w", err)
	}
	// resp.Request.URL reflects the last hop, which is what relative links on
	// the returned document resolve against.
	final := resp.Request.URL
	if final == nil {
		final = u
	}
	return &Page{
		URL:         final,
		Body:        body,
		ContentType: resp.Header.Get("Content-Type"),
		Status:      resp.StatusCode,
	}, nil
}

var _ Fetcher = (*Client)(nil)
