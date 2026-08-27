package tui

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// A bytes.Buffer is not a terminal, so WithSpinner must take the plain path.
// This is also what keeps the test from starting a Bubble Tea program, which
// would block without a TTY.
func TestWithSpinnerPlainPath(t *testing.T) {
	var buf bytes.Buffer
	err := WithSpinner(&buf, "Searching example.com", func(status Status) error {
		status("%d page searched", 1)
		status("%d pages searched", 2)
		return nil
	})
	if err != nil {
		t.Fatalf("WithSpinner: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"Searching example.com", "1 page searched", "2 pages searched"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// No terminal means no animation: a log must not be peppered with escapes.
	if strings.ContainsRune(out, '\x1b') {
		t.Errorf("plain output contains an escape sequence:\n%q", out)
	}
}

// A status repeated verbatim would otherwise fill a log with identical lines,
// since progress fires once per fetched page.
func TestWithSpinnerPlainDedupesRepeats(t *testing.T) {
	var buf bytes.Buffer
	err := WithSpinner(&buf, "t", func(status Status) error {
		for range 5 {
			status("3 pages searched")
		}
		status("4 pages searched")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(buf.String(), "3 pages searched"); got != 1 {
		t.Errorf("repeated status written %d times, want 1", got)
	}
}

func TestWithSpinnerReturnsWorkError(t *testing.T) {
	sentinel := errors.New("crawl exploded")
	var buf bytes.Buffer
	if err := WithSpinner(&buf, "t", func(Status) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Errorf("got %v, want %v", err, sentinel)
	}
}

// Status is documented as safe from any goroutine, and the crawl calls it from
// its fetch workers. Run with -race, this pins that.
func TestWithSpinnerStatusIsConcurrencySafe(t *testing.T) {
	var buf bytes.Buffer
	err := WithSpinner(&buf, "t", func(status Status) error {
		var wg sync.WaitGroup
		for i := range 20 {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				status("page %d", i)
			}(i)
		}
		wg.Wait()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSpinnerModelUpdate(t *testing.T) {
	m := &spinnerModel{title: "Searching example.com"}

	if _, _ = m.Update(spinnerStatus("7 pages searched")); m.status != "7 pages searched" {
		t.Errorf("status = %q", m.status)
	}
	if view := m.View(); !strings.Contains(view, "7 pages searched") {
		t.Errorf("view omits the status: %q", view)
	}

	// Ctrl+C reports the shared abort error, so callers treat it the same way
	// they treat quitting either picker.
	m2 := &spinnerModel{}
	if _, _ = m2.Update(tea.KeyMsg{Type: tea.KeyCtrlC}); !errors.Is(m2.err, ErrAborted) {
		t.Errorf("ctrl+c err = %v, want ErrAborted", m2.err)
	}
	if !m2.done {
		t.Error("ctrl+c did not mark the model done")
	}

	// A finished spinner renders nothing: the caller prints its own summary,
	// and a leftover frame above it reads as work still running.
	m3 := &spinnerModel{}
	if _, _ = m3.Update(spinnerDone{}); m3.View() != "" {
		t.Errorf("finished view = %q, want empty", m3.View())
	}
}

func TestSpinnerModelCarriesWorkError(t *testing.T) {
	sentinel := errors.New("boom")
	m := &spinnerModel{}
	if _, _ = m.Update(spinnerDone{err: sentinel}); !errors.Is(m.err, sentinel) {
		t.Errorf("err = %v, want %v", m.err, sentinel)
	}
}
