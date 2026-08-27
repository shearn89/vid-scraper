// Package progress renders transfer progress to stderr.
package progress

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// interval throttles redraws; without it a fast transfer spends more time
// formatting output than moving bytes.
const interval = 150 * time.Millisecond

// Bar reports progress for one transfer. A Bar with reporting disabled is a
// working io.Writer that simply counts, so callers need no conditionals.
type Bar struct {
	label   string
	total   int64
	enabled bool

	mu      sync.Mutex
	current int64
	last    time.Time
	drawn   bool
}

// New builds a Bar. Output is suppressed unless enabled and stderr is a
// terminal, keeping piped and redirected output clean.
func New(label string, total int64, enabled bool) *Bar {
	return &Bar{label: label, total: total, enabled: enabled && IsTerminal()}
}

// IsTerminal reports whether stderr is a character device.
func IsTerminal() bool {
	fi, err := os.Stderr.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// Write implements io.Writer so a Bar can sit in an io.MultiWriter.
func (b *Bar) Write(p []byte) (int, error) {
	b.Add(int64(len(p)))
	return len(p), nil
}

// Add records transferred bytes and redraws if due.
func (b *Bar) Add(n int64) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.current += n
	if !b.enabled || time.Since(b.last) < interval {
		return
	}
	b.last = time.Now()
	b.draw()
}

// Set replaces the current count, for transfers measured in units other than
// bytes (HLS counts segments).
func (b *Bar) Set(n int64) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.current = n
	if !b.enabled || time.Since(b.last) < interval {
		return
	}
	b.last = time.Now()
	b.draw()
}

// draw renders one line. The caller holds the lock.
func (b *Bar) draw() {
	b.drawn = true
	if b.total > 0 {
		pct := float64(b.current) / float64(b.total) * 100
		fmt.Fprintf(os.Stderr, "\r  %-40s %6.1f%%  %s / %s\033[K",
			truncate(b.label, 40), pct, Bytes(b.current), Bytes(b.total))
		return
	}
	fmt.Fprintf(os.Stderr, "\r  %-40s %s\033[K", truncate(b.label, 40), Bytes(b.current))
}

// Done clears the progress line.
func (b *Bar) Done() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.enabled && b.drawn {
		fmt.Fprint(os.Stderr, "\r\033[K")
	}
}

// Bytes formats a byte count for humans.
func Bytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 3; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}
