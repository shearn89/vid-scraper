package tui

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	vprogress "github.com/shearn89/vid-scraper/internal/progress"
)

// Stage is how far one download has got.
type Stage int

const (
	StagePending Stage = iota
	StageDownloading
	StageRemuxing
	StageDone
	StageSkipped
	StageFailed
)

// DownloadItem is one row of the progress screen.
type DownloadItem struct {
	Title string
	Dest  string
}

// Update reports a change to one item. Zero values mean "unchanged", so a
// caller can send a stage transition without restating the byte counts.
type Update struct {
	Index      int
	Stage      Stage
	Bytes      int64
	TotalBytes int64
	Segments   int
	TotalSegs  int
	Note       string
	Err        error
}

// RunDownloads shows a live progress screen while work runs, and returns
// whatever work returns.
//
// Downloads are the part of a run with the most to say and the least worth
// reading: signed CDN URLs hundreds of characters long, a line per file per
// stage. The screen keeps what a person actually wants — which file, how far
// in, how big — and drops the rest. Piped or without a terminal it degrades to
// one line per completed file, which is what a log wants instead.
func RunDownloads(stderr io.Writer, dir string, items []DownloadItem, work func(chan<- Update) error) error {
	if len(items) == 0 {
		return nil
	}
	if !isTerminal(stderr) {
		return runDownloadsPlain(stderr, dir, items, work)
	}
	return runDownloadsTUI(stderr, dir, items, work)
}

// runDownloadsPlain is the no-terminal path: a line when a file finishes, and
// nothing while it is in flight.
func runDownloadsPlain(stderr io.Writer, dir string, items []DownloadItem, work func(chan<- Update) error) error {
	fmt.Fprintf(stderr, "Downloading %d file%s to %s\n", len(items), plural(len(items)), dir)

	ch := make(chan Update, 64)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var done int
		for u := range ch {
			if u.Index < 0 || u.Index >= len(items) {
				continue
			}
			switch u.Stage {
			case StageDone:
				done++
				fmt.Fprintf(stderr, "  [%d/%d] %s — %s\n",
					done, len(items), items[u.Index].Title, vprogress.Bytes(u.Bytes))
			case StageFailed:
				done++
				fmt.Fprintf(stderr, "  [%d/%d] %s — failed: %v\n",
					done, len(items), items[u.Index].Title, u.Err)
			case StageSkipped:
				done++
				fmt.Fprintf(stderr, "  [%d/%d] %s — skipped\n", done, len(items), items[u.Index].Title)
			}
		}
	}()

	err := work(ch)
	close(ch)
	wg.Wait()
	return err
}

// rowState tracks one item's progress.
type rowState struct {
	stage      Stage
	bytes      int64
	totalBytes int64
	segments   int
	totalSegs  int
	note       string
	err        error
}

// fraction reports how far this row has got, and whether that is known at all.
// Segment counts are preferred: an HLS download knows its segment total up
// front, while its byte total only becomes clear as it goes.
func (r rowState) fraction() (float64, bool) {
	switch {
	case r.stage == StageDone || r.stage == StageSkipped:
		return 1, true
	case r.totalSegs > 0:
		return float64(r.segments) / float64(r.totalSegs), true
	case r.totalBytes > 0:
		return float64(r.bytes) / float64(r.totalBytes), true
	default:
		return 0, false
	}
}

type downloadModel struct {
	dir     string
	items   []DownloadItem
	rows    []rowState
	spin    spinner.Model
	bar     progress.Model
	width   int
	started time.Time
	done    bool
	err     error
	aborted bool
}

// downloadFinished ends the screen.
type downloadFinished struct{ err error }

func (m *downloadModel) Init() tea.Cmd { return m.spin.Tick }

func (m *downloadModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case Update:
		m.apply(msg)
		return m, nil
	case downloadFinished:
		m.err = msg.err
		m.done = true
		return m, tea.Quit
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil
	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC {
			m.aborted = true
			m.done = true
			return m, tea.Quit
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.spin, cmd = m.spin.Update(msg)
	return m, cmd
}

// apply folds one update into the model. Zero-valued counters are treated as
// "unchanged" so a stage transition need not restate progress already sent.
func (m *downloadModel) apply(u Update) {
	if u.Index < 0 || u.Index >= len(m.rows) {
		return
	}
	r := &m.rows[u.Index]
	r.stage = u.Stage
	if u.Bytes > 0 {
		r.bytes = u.Bytes
	}
	if u.TotalBytes > 0 {
		r.totalBytes = u.TotalBytes
	}
	if u.Segments > 0 {
		r.segments = u.Segments
	}
	if u.TotalSegs > 0 {
		r.totalSegs = u.TotalSegs
	}
	if u.Note != "" {
		r.note = u.Note
	}
	if u.Err != nil {
		r.err = u.Err
	}
}

// counts summarises the run for the footer.
func (m *downloadModel) counts() (complete int, failed int, bytes int64) {
	for _, r := range m.rows {
		switch r.stage {
		case StageDone, StageSkipped:
			complete++
		case StageFailed:
			failed++
		}
		bytes += r.bytes
	}
	return complete, failed, bytes
}

// styleDim and styleHeader come from model.go, so the two screens share one
// look.
var (
	styleOK   = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleFail = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
)

// titleWidth is the column the titles are padded to, wide enough for the
// programme names this is usually pointed at without pushing the bar off a
// narrow terminal.
const titleWidth = 28

func (m *downloadModel) View() string {
	if m.done {
		// The caller prints its own summary; a stale frame above it would read
		// as though downloads were still running.
		return ""
	}

	var b strings.Builder
	complete, failed, bytes := m.counts()

	fmt.Fprintf(&b, "%s %s\n\n",
		styleHeader.Render("Downloading to"), styleDim.Render(m.dir))

	for i, item := range m.items {
		b.WriteString(m.renderRow(i, item))
		b.WriteByte('\n')
	}

	footer := fmt.Sprintf("%d/%d complete", complete, len(m.items))
	if failed > 0 {
		footer += fmt.Sprintf(" · %s", styleFail.Render(fmt.Sprintf("%d failed", failed)))
	}
	if bytes > 0 {
		footer += " · " + vprogress.Bytes(bytes)
	}
	// started is zero when a caller builds the model directly, as tests do;
	// elapsed time from the zero instant is a meaningless six-figure number.
	if !m.started.IsZero() {
		if d := time.Since(m.started).Truncate(time.Second); d > 0 {
			footer += " · " + d.String()
		}
	}
	fmt.Fprintf(&b, "\n%s\n", styleDim.Render(footer))
	return b.String()
}

// tick is the spinner frame without its trailing space, so spinner rows line
// up with the tick and cross that mark finished ones.
func (m *downloadModel) tick() string {
	return strings.TrimSpace(m.spin.View())
}

func (m *downloadModel) renderRow(i int, item DownloadItem) string {
	r := m.rows[i]
	title := truncate(item.Title, titleWidth)

	switch r.stage {
	case StagePending:
		return "  " + styleDim.Render(fmt.Sprintf("  %-*s waiting", titleWidth, title))
	case StageSkipped:
		return "  " + styleDim.Render(fmt.Sprintf("· %-*s already downloaded", titleWidth, title))
	case StageDone:
		detail := vprogress.Bytes(r.bytes)
		if r.totalSegs > 0 {
			detail += fmt.Sprintf(", %d segments", r.totalSegs)
		}
		return fmt.Sprintf("  %s %-*s %s",
			styleOK.Render("✓"), titleWidth, title, styleDim.Render(detail))
	case StageFailed:
		return fmt.Sprintf("  %s %-*s %s",
			styleFail.Render("✗"), titleWidth, title, styleFail.Render(shortErr(r.err)))
	case StageRemuxing:
		return fmt.Sprintf("  %s %-*s %s",
			m.tick(), titleWidth, title, styleDim.Render("converting to mp4…"))
	default:
		frac, known := r.fraction()
		if !known {
			return fmt.Sprintf("  %s %-*s %s",
				m.tick(), titleWidth, title, styleDim.Render("starting…"))
		}
		detail := fmt.Sprintf("%3.0f%%", frac*100)
		switch {
		case r.totalSegs > 0:
			detail += fmt.Sprintf("  %d/%d segments", r.segments, r.totalSegs)
		case r.totalBytes > 0:
			detail += fmt.Sprintf("  %s / %s", vprogress.Bytes(r.bytes), vprogress.Bytes(r.totalBytes))
		}
		return fmt.Sprintf("  %s %-*s %s %s",
			m.tick(), titleWidth, title, m.bar.ViewAs(frac), styleDim.Render(detail))
	}
}

// shortErr keeps a failure on one line; the full text is printed by the caller
// once the screen has gone.
func shortErr(err error) string {
	if err == nil {
		return "failed"
	}
	return truncate(err.Error(), 44)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func runDownloadsTUI(stderr io.Writer, dir string, items []DownloadItem, work func(chan<- Update) error) error {
	s := spinner.New()
	s.Spinner = spinner.Dot

	m := &downloadModel{
		dir:     dir,
		items:   items,
		rows:    make([]rowState, len(items)),
		spin:    s,
		bar:     progress.New(progress.WithDefaultGradient(), progress.WithoutPercentage(), progress.WithWidth(18)),
		started: time.Now(),
	}

	p := tea.NewProgram(m, tea.WithOutput(stderr))

	ch := make(chan Update, 256)
	go func() {
		for u := range ch {
			p.Send(u)
		}
	}()

	var workErr error
	go func() {
		workErr = work(ch)
		close(ch)
		p.Send(downloadFinished{err: workErr})
	}()

	if _, err := p.Run(); err != nil {
		return fmt.Errorf("running download screen: %w", err)
	}
	if m.aborted {
		return ErrAborted
	}
	return workErr
}
