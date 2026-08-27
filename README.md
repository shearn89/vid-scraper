# vid-scraper

Simple CLI to find every video linked from a webpage — including videos that
live one hop away on a detail or player page — and download them.

Single static Go binary. No `yt-dlp`, no `ffmpeg`, no runtime dependencies.

```
vid-scraper --list https://example.com/gallery
vid-scraper --follow --match '/videos/' -o ./downloads https://example.com/gallery
```

## Install

```sh
go install github.com/shearn89/vid-scraper@latest
```

Or build from a checkout:

```sh
go build -o vid-scraper .
```

## Usage

```
vid-scraper [flags] <url>
```

Flags may appear before or after the URL.

### Discovery

| Flag | Description |
|---|---|
| `-d, --depth N` | Follow links this many hops beyond the page (default `0`) |
| `-f, --follow` | Shorthand for `--depth 1` |
| `--match REGEX` | Only follow links whose URL matches |
| `--allow-host H` | Also follow links to this host (repeatable) |
| `--no-script-scan` | Disable the inline-`<script>` URL sweep |

### Output

| Flag | Description |
|---|---|
| `-o, --output DIR` | Download directory (default `.`) |
| `-l, --list` | List discovered videos instead of downloading |
| `--json` | List as JSON: url, source, kind, strategy, title |
| `-v, --verbose` | Report each fetch and which strategy found each hit |

### Downloading

| Flag | Description |
|---|---|
| `-c, --concurrency N` | Parallel fetches and segment downloads (default `4`) |
| `--skip-existing` | Don't re-download files already present |
| `--quality S` | HLS variant: `best` (default), `worst`, or a height like `720` |

### Requests

| Flag | Description |
|---|---|
| `--user-agent S` | Override the User-Agent |
| `--header K:V` | Extra request header (repeatable) |
| `--cookie S` | Cookie header value, for pages behind a login |
| `--timeout D` | Per-request timeout (default `30s`) |
| `--render` | Render JavaScript before scraping — **not yet implemented** |

Exit codes: `0` success, `1` an error or a failed download, `2` no videos found.

## How it finds videos

Six strategies run over each page. Every result records which one found it, so
when a site yields nothing useful, `--verbose` tells you which heuristic to fix.

| Strategy | What it reads |
|---|---|
| `media-tag` | `<video>`, `<source>`, `<embed>`, `<object>`, plus `data-src`-style lazy-load attributes |
| `anchor` | `<a href>` ending in a video extension |
| `meta` | `og:video`, `og:video:secure_url`, `twitter:player:stream` |
| `json-ld` | schema.org `VideoObject` `contentUrl` / `embedUrl` |
| `script` | Media URLs inside inline `<script>`, including JSON-escaped forms |
| `iframe` | Embedded player documents, followed when depth allows |

### Videos on another page

Two shapes are handled by `--depth`:

- **Detail pages.** A gallery links to a page per video. `--follow` fetches
  each link and runs discovery there. Use `--match` to skip nav and footer
  links: `--follow --match '/videos/'`.
- **Iframe players.** The video lives in an embedded document. Iframes are
  always followed when depth allows, even to a different host and even when
  `--match` is set, because that hop is the point.

Crawling stays on the starting host unless you pass `--allow-host`. Each page is
fetched once.

## Streaming formats

**HLS (`.m3u8`) is downloaded natively.** Variant selection by bandwidth or
height, concurrent segment fetching assembled in strict playlist order,
`AES-128` decryption, fMP4 init segments, and resume at segment granularity.
Output is `.ts` for transport streams and `.mp4` for fMP4.

**DASH (`.mpd`) is reported but not downloaded.** DASH ships audio and video as
separate representations that must be *muxed*, and muxing needs a real muxer.
The URL is printed so you can hand it to another tool.

Also unsupported, deliberately:

- **Muxing of any kind.** Concatenation is not muxing. When an HLS master
  declares audio as a separate rendition, the downloaded file would be silent —
  vid-scraper detects this and **warns before downloading**, naming the audio
  playlist URL, rather than handing you a mute file that looks fine.
- **Live streams** (no `#EXT-X-ENDLIST`) — no defined end to download to.
- **`SAMPLE-AES` and DRM** (Widevine, FairPlay) — refused, not attempted.
- **Site-specific extraction** for YouTube, Vimeo and similar. Those embeds are
  reported as `kind: "embed"` with their page URL.

## JavaScript-rendered pages

vid-scraper reads the HTML the server returns. That covers most sites, including
ones that hide the URL in inline `<script>` JSON. If a page injects its video
purely at runtime, nothing will be found — the `--render` flag is reserved for a
headless-browser backend, and the fetcher already sits behind an interface for
it, but it is not implemented yet.

## Examples

```sh
# See what's there before downloading anything
vid-scraper --list --verbose https://example.com/gallery

# Follow to per-video pages, ignoring nav links
vid-scraper --follow --match '/watch/' -o ./out https://example.com/gallery

# Pipe URLs into another tool
vid-scraper --list --json https://example.com/page | jq -r '.[].url'

# A page behind a login
vid-scraper --cookie "session=abc123" --follow https://example.com/members

# Grab a 720p HLS rendition rather than the largest
vid-scraper --quality 720 https://example.com/watch/42
```

## Development

```sh
go build ./...
go test ./...
go test -race ./...
```

Discovery fixtures live in `testdata/`. Adding a heuristic for a new site means
saving that page's HTML there, writing a test asserting the URLs that should
come out, then adding or adjusting a strategy in `internal/scrape/`. The fixture
stays as a regression test.

## Licence

MIT — see [LICENSE](LICENSE).
