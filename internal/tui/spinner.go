package tui

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/term"
)

// Status is how work running behind a spinner reports what it is doing. It is
// safe to call from any goroutine, and safe to call when no spinner is running.
type Status func(format string, args ...any)

// WithSpinner runs work while showing a spinner labelled title, and returns
// whatever work returns.
//
// Crawling a site can take minutes, and until now that time looked identical to
// a hang. The spinner exists to distinguish the two.
//
// Without a terminal on stderr — a pipe, a cron run, CI — no spinner is drawn
// and status updates are written as plain lines instead, so the output stays
// readable in a log.
func WithSpinner(stderr io.Writer, title string, work func(Status) error) error {
	if !isTerminal(stderr) {
		return runPlain(stderr, title, work)
	}
	return runSpinner(stderr, title, work)
}

// isTerminal reports whether w is an interactive terminal.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// runPlain is the no-terminal path: one line per distinct status, so a log
// records progress without control characters or repetition.
func runPlain(stderr io.Writer, title string, work func(Status) error) error {
	fmt.Fprintf(stderr, "%s\n", title)
	var (
		mu   sync.Mutex
		last string
	)
	return work(func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		mu.Lock()
		defer mu.Unlock()
		if msg == last {
			return
		}
		last = msg
		fmt.Fprintf(stderr, "  %s\n", msg)
	})
}

// spinnerDone carries the result of the background work back to the model.
type spinnerDone struct{ err error }

// spinnerStatus is a status update from the background work.
type spinnerStatus string

// spinnerModel draws the spinner and the latest status line.
type spinnerModel struct {
	spin    spinner.Model
	title   string
	status  string
	started time.Time
	err     error
	done    bool
}

func (m *spinnerModel) Init() tea.Cmd { return m.spin.Tick }

func (m *spinnerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case spinnerStatus:
		m.status = string(msg)
		return m, nil
	case spinnerDone:
		m.err = msg.err
		m.done = true
		return m, tea.Quit
	case tea.KeyMsg:
		// Ctrl+C must still interrupt: the work runs in its own goroutine and
		// keeps going, but the process is expected to exit shortly after.
		if msg.Type == tea.KeyCtrlC {
			m.err = ErrAborted
			m.done = true
			return m, tea.Quit
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.spin, cmd = m.spin.Update(msg)
	return m, cmd
}

func (m *spinnerModel) View() string {
	if m.done {
		// Leave nothing behind: the caller prints its own summary, and a
		// stale spinner frame above it reads as though work is still running.
		return ""
	}
	line := m.spin.View() + " " + m.title
	if m.status != "" {
		line += " — " + m.status
	}
	if d := time.Since(m.started).Truncate(time.Second); d > 0 {
		line += fmt.Sprintf(" (%s)", d)
	}
	return line + "\n"
}

// runSpinner is the interactive path.
func runSpinner(stderr io.Writer, title string, work func(Status) error) error {
	s := spinner.New()
	s.Spinner = spinner.Dot
	m := &spinnerModel{spin: s, title: title, started: time.Now()}

	// Rendered to stderr so that piping stdout — the documented way to feed
	// discovered URLs into another tool — is unaffected by the animation.
	p := tea.NewProgram(m, tea.WithOutput(stderr))

	// Status arrives from the crawl's worker goroutines; tea.Program.Send is
	// safe for concurrent use, so no extra locking is needed here.
	status := Status(func(format string, args ...any) {
		p.Send(spinnerStatus(fmt.Sprintf(format, args...)))
	})

	var workErr error
	go func() {
		workErr = work(status)
		p.Send(spinnerDone{err: workErr})
	}()

	if _, err := p.Run(); err != nil {
		return fmt.Errorf("running spinner: %w", err)
	}
	if m.err != nil {
		return m.err
	}
	return workErr
}
