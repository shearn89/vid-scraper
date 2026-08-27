package tui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/shearn89/vid-scraper/internal/config"
)

func launchable(host, name, dir, url string) config.Launchable {
	return config.Launchable{
		Host: host,
		Site: config.Site{Name: name, Dir: dir, URL: url},
	}
}

func siteFixture() []config.Launchable {
	return []config.Launchable{
		launchable("alpha.example", "Alpha", "/library/alpha", "https://alpha.example/videos"),
		launchable("bravo.example", "", "/library/bravo", "https://bravo.example/watch"),
		launchable("charlie.example", "Charlie", "/library/charlie", "https://charlie.example/all"),
	}
}

// siteKey builds the special keys the chooser handles that the shared helper
// does not; anything else goes through it unchanged.
func siteKey(s string) tea.KeyMsg {
	switch s {
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	}
	return key(s)
}

// pressSite feeds keys through Update, exactly as the program would. Commands
// are dropped: the only one Update produces is tea.Quit, and the tests assert
// on model state rather than on the program shutting down.
func pressSite(t *testing.T, m *siteModel, keys ...string) {
	t.Helper()
	for _, k := range keys {
		m.Update(siteKey(k))
	}
}

func TestSiteCursorMovesAndClamps(t *testing.T) {
	m := newSiteModel(siteFixture())

	pressSite(t, m, "down", "j")
	if m.cursor != 2 {
		t.Errorf("cursor after two downs = %d, want 2", m.cursor)
	}

	// Past the bottom stops at the last row rather than wrapping.
	pressSite(t, m, "down", "down")
	if m.cursor != 2 {
		t.Errorf("cursor at bottom = %d, want 2", m.cursor)
	}

	pressSite(t, m, "up", "k", "up", "up")
	if m.cursor != 0 {
		t.Errorf("cursor at top = %d, want 0", m.cursor)
	}
}

func TestSiteEnterChoosesRowUnderCursor(t *testing.T) {
	m := newSiteModel(siteFixture())

	pressSite(t, m, "down", "enter")

	if !m.confirmed {
		t.Fatal("enter did not confirm")
	}
	if got, want := m.selected().Host, "bravo.example"; got != want {
		t.Errorf("selected host = %q, want %q", got, want)
	}
}

func TestSiteQuitAborts(t *testing.T) {
	for _, k := range []string{"q", "esc", "ctrl+c"} {
		m := newSiteModel(siteFixture())
		m.Update(siteKey(k))
		if m.confirmed {
			t.Errorf("%q confirmed the selection, want abort", k)
		}
	}
}

func TestSiteNumberShortcutPicksRow(t *testing.T) {
	m := newSiteModel(siteFixture())

	pressSite(t, m, "3")

	if !m.confirmed {
		t.Fatal("number key did not confirm")
	}
	if got, want := m.selected().Host, "charlie.example"; got != want {
		t.Errorf("selected host = %q, want %q", got, want)
	}
}

func TestSiteNumberBeyondListIgnored(t *testing.T) {
	m := newSiteModel(siteFixture())

	pressSite(t, m, "7")

	if m.confirmed {
		t.Error("a number past the end of the list confirmed a selection")
	}
	// Nor may it move the cursor: clamping would offer up a site the user did
	// not name.
	if m.cursor != 0 {
		t.Errorf("cursor = %d after out-of-range number, want 0", m.cursor)
	}
}

func TestSelectSiteEmptyAbortsWithoutProgram(t *testing.T) {
	// No TTY here: if this ever starts a Bubble Tea program the test hangs.
	got, err := SelectSite(nil)
	if !errors.Is(err, ErrAborted) {
		t.Fatalf("SelectSite(nil) error = %v, want ErrAborted", err)
	}
	if got != (config.Launchable{}) {
		t.Errorf("SelectSite(nil) = %+v, want zero value", got)
	}
}

func TestSiteRowShowsLabelDirAndURL(t *testing.T) {
	m := newSiteModel(siteFixture())

	// The unnamed site falls back to its host, and both the directory and the
	// start URL must be visible before the user commits.
	lines := m.renderRow(1)
	if len(lines) != siteRowLines {
		t.Fatalf("renderRow gave %d lines, want %d", len(lines), siteRowLines)
	}
	if !strings.Contains(lines[0], "bravo.example") {
		t.Errorf("label line %q does not name the site", lines[0])
	}
	for _, want := range []string{"/library/bravo", "https://bravo.example/watch"} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("detail line %q does not contain %q", lines[1], want)
		}
	}
}

func TestSiteResizeKeepsCursorRowVisible(t *testing.T) {
	sites := make([]config.Launchable, 0, 12)
	for _, l := range siteFixture() {
		for i := 0; i < 4; i++ {
			sites = append(sites, l)
		}
	}
	m := newSiteModel(sites)

	// A window far shorter than the list: the last row must still be scrolled
	// into view, both lines of it.
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	for i := 0; i < len(sites); i++ {
		pressSite(t, m, "down")
	}

	top := m.cursor * siteRowLines
	bottom := top + siteRowLines - 1
	if top < m.viewport.YOffset || bottom >= m.viewport.YOffset+m.viewport.Height {
		t.Errorf("cursor rows %d-%d outside viewport offset %d height %d",
			top, bottom, m.viewport.YOffset, m.viewport.Height)
	}
}

func TestSiteSingleEntryIsNotAutoSelected(t *testing.T) {
	// One configured site is still presented: acting on it silently would hide
	// which host is about to be crawled.
	m := newSiteModel(siteFixture()[:1])
	if m.confirmed {
		t.Error("a single site was confirmed without a keypress")
	}
}

// TestSelectSiteWithoutTerminal pins the distinction the CLI relies on to give
// advice instead of a stack of internal errors: no sites is an abort, no
// terminal is its own condition.
func TestSelectSiteWithoutTerminal(t *testing.T) {
	// Tests never run attached to a TTY, so interactive() is false here.
	_, err := SelectSite([]config.Launchable{{Host: "example.com", Site: config.Site{URL: "https://example.com"}}})
	if !errors.Is(err, ErrNoTerminal) {
		t.Errorf("err = %v, want ErrNoTerminal", err)
	}
	if _, err := SelectSite(nil); !errors.Is(err, ErrAborted) {
		t.Errorf("empty list err = %v, want ErrAborted", err)
	}
}
