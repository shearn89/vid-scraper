// Package tui presents discovered videos as an interactive checklist so the
// user picks what to download.
//
// Discovery is deliberately greedy: a page yields more URLs than anyone wants
// on disk, and only a human can tell a lecture from a trailer. The picker sits
// between the two, and it opens with a defensible guess rather than a blank
// slate — anything already in the library starts unticked, so confirming
// straight away downloads exactly the new material.
//
// All selection logic lives on the model in pure methods, so it is testable
// without a terminal; the Bubble Tea layer only translates keys into calls.
package tui

import (
	"errors"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/shearn89/vid-scraper/internal/scrape"
)

// ErrAborted is returned when the user quits the picker without confirming.
var ErrAborted = errors.New("selection aborted")

// ErrNoTerminal reports that an interactive screen was asked for where there is
// no terminal to draw it on — a pipe, a cron run, a CI job. Callers turn this
// into advice, because the fix is a different flag rather than a bug report.
var ErrNoTerminal = errors.New("no terminal available for interactive selection")

// interactive reports whether there is a terminal to read keystrokes from.
// Checked on standard input specifically: output can be redirected while the
// screen still works, but with no input there is no way to choose anything.
func interactive() bool {
	return isTerminal(os.Stdin)
}

// Item is one row: a discovered video plus what we know about its download
// target.
type Item struct {
	Video    scrape.Video
	Dest     string // absolute target path we would write
	Existing string // path of a file already on disk, "" if none
	Selected bool
}

// Select runs the interactive picker and returns the items the user chose.
// It returns ErrAborted if the user quits without confirming.
func Select(items []Item) ([]Item, error) {
	res, err := SelectWith(items, SelectOptions{ShowDownloaded: true})
	return res.Chosen, err
}

// SelectOptions carries the picker's starting state.
type SelectOptions struct {
	// ShowDownloaded lists already-downloaded videos, dimmed, rather than
	// hiding them.
	ShowDownloaded bool
}

// SelectResult is what the picker returns: the chosen items, and the interface
// state worth remembering for next time.
type SelectResult struct {
	Chosen []Item
	// ShowDownloaded is where the user left the toggle. Returned rather than
	// written here so that persisting it stays the caller's decision, and this
	// package keeps no opinion about where state lives.
	ShowDownloaded bool
}

// SelectWith runs the interactive picker with an explicit starting state.
func SelectWith(items []Item, opts SelectOptions) (SelectResult, error) {
	res := SelectResult{ShowDownloaded: opts.ShowDownloaded}

	// Nothing to pick means nothing to show. Starting a program here would
	// block forever wherever there is no TTY — a test, a pipe, a cron run.
	if len(items) == 0 {
		return res, nil
	}
	if !interactive() {
		return res, ErrNoTerminal
	}

	m := newModel(items)
	m.showExisting = opts.ShowDownloaded
	m.clampCursor()

	final, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	if err != nil {
		return res, fmt.Errorf("running video picker: %w", err)
	}
	got, ok := final.(*model)
	if !ok {
		return res, fmt.Errorf("running video picker: unexpected model %T", final)
	}
	// Reported whether or not the user confirmed: they expressed a preference
	// about the display either way, and abandoning a run should not discard it.
	res.ShowDownloaded = got.showExisting
	if !got.confirmed {
		return res, ErrAborted
	}
	res.Chosen = got.chosen()
	return res, nil
}
