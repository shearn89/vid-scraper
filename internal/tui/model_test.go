package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/shearn89/vid-scraper/internal/scrape"
)

// item builds a row; a non-empty existing marks it as already downloaded.
func item(title, kind, existing string) Item {
	return Item{
		Video:    scrape.Video{URL: "https://example.com/" + title + ".mp4", Kind: kind, Title: title},
		Dest:     "/tmp/" + title + ".mp4",
		Existing: existing,
	}
}

func fixture() []Item {
	return []Item{
		item("alpha", scrape.KindDirect, ""),
		item("bravo", scrape.KindStream, "/library/bravo.mp4"),
		item("charlie", scrape.KindDirect, ""),
		item("delta", scrape.KindEmbed, "/library/delta.mp4"),
	}
}

func key(s string) tea.KeyMsg {
	switch s {
	case " ":
		return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// press feeds keys through Update, exactly as the program would. Any command
// Update returns is dropped: the only one it produces is tea.Quit, and the
// tests assert on model state rather than on the program shutting down.
func press(t *testing.T, m *model, keys ...string) {
	t.Helper()
	for _, k := range keys {
		m.Update(key(k))
	}
}

func selectedTitles(m *model) []string {
	var out []string
	for _, it := range m.items {
		if it.Selected {
			out = append(out, it.Video.Title)
		}
	}
	return out
}

func TestNewModelDefaultsSkipExisting(t *testing.T) {
	// A caller may hand us Selected already set; the picker overrides it.
	items := fixture()
	items[1].Selected = true

	m := newModel(items)

	got := strings.Join(selectedTitles(m), ",")
	if want := "alpha,charlie"; got != want {
		t.Errorf("initial selection = %q, want %q", got, want)
	}
	if selected, existing, total := m.counts(); selected != 2 || existing != 2 || total != 4 {
		t.Errorf("counts() = %d, %d, %d; want 2, 2, 4", selected, existing, total)
	}
	// The caller's slice must not be rewritten underneath them.
	if !items[1].Selected {
		t.Error("newModel mutated the caller's items")
	}
}

func TestSpaceToggles(t *testing.T) {
	m := newModel(fixture())

	press(t, m, " ") // untick alpha
	if m.items[0].Selected {
		t.Error("space did not deselect the row under the cursor")
	}
	press(t, m, " ") // and back
	if !m.items[0].Selected {
		t.Error("space did not reselect the row under the cursor")
	}

	press(t, m, "down", " ") // bravo, already downloaded
	if !m.items[1].Selected {
		t.Error("an already-downloaded row must still be selectable")
	}
}

func TestMoveCursorClampsAtEnds(t *testing.T) {
	m := newModel(fixture())

	press(t, m, "up")
	if m.cursor != 0 {
		t.Errorf("cursor = %d at top, want 0", m.cursor)
	}
	press(t, m, "j", "j", "j", "j", "j")
	if m.cursor != len(m.items)-1 {
		t.Errorf("cursor = %d at bottom, want %d", m.cursor, len(m.items)-1)
	}
	press(t, m, "k")
	if m.cursor != len(m.items)-2 {
		t.Errorf("cursor = %d after k, want %d", m.cursor, len(m.items)-2)
	}
}

func TestSelectAllNoneInvert(t *testing.T) {
	m := newModel(fixture())

	press(t, m, "a")
	if got, _, total := m.counts(); got != total {
		t.Errorf("after a: %d selected, want %d", got, total)
	}

	press(t, m, "n")
	if got, _, _ := m.counts(); got != 0 {
		t.Errorf("after n: %d selected, want 0", got)
	}

	press(t, m, "i")
	if got, _, total := m.counts(); got != total {
		t.Errorf("after i from empty: %d selected, want %d", got, total)
	}

	press(t, m, " ", "i") // untick alpha, then invert: only alpha survives
	if got := strings.Join(selectedTitles(m), ","); got != "alpha" {
		t.Errorf("after invert: selected = %q, want \"alpha\"", got)
	}
}

func TestHideExistingKeepsThemUntouched(t *testing.T) {
	m := newModel(fixture())

	press(t, m, "e")
	if m.showExisting {
		t.Fatal("e did not hide already-downloaded rows")
	}
	if got, want := len(m.visibleIndexes()), 2; got != want {
		t.Errorf("%d visible rows, want %d", got, want)
	}

	// Selecting "all" while they are hidden must not queue them.
	press(t, m, "a")
	if m.items[1].Selected || m.items[3].Selected {
		t.Error("a selected hidden already-downloaded rows")
	}

	// The cursor cannot come to rest on a hidden row.
	press(t, m, "j", "j", "j")
	if !m.visible(m.cursor) {
		t.Errorf("cursor %d landed on a hidden row", m.cursor)
	}

	press(t, m, "e")
	if !m.showExisting || len(m.visibleIndexes()) != 4 {
		t.Error("e did not restore the hidden rows")
	}
}

func TestToggleShowExistingMovesCursorOffHiddenRow(t *testing.T) {
	m := newModel(fixture())

	press(t, m, "j") // bravo, which is about to be hidden
	press(t, m, "e")
	if !m.visible(m.cursor) {
		t.Errorf("cursor %d left on a hidden row", m.cursor)
	}
}

func TestEnterConfirmsAndQuitAborts(t *testing.T) {
	m := newModel(fixture())
	press(t, m, "enter")
	if !m.confirmed {
		t.Error("enter did not confirm")
	}
	if got := strings.Join(itemTitles(m.chosen()), ","); got != "alpha,charlie" {
		t.Errorf("chosen() = %q, want \"alpha,charlie\"", got)
	}

	m = newModel(fixture())
	press(t, m, "q")
	if m.confirmed {
		t.Error("q confirmed the selection")
	}
}

func itemTitles(items []Item) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Video.Title)
	}
	return out
}

func TestSelectEmptyReturnsImmediately(t *testing.T) {
	// Guards the real hazard: Select(nil) must not start a Bubble Tea program,
	// which would block forever with no TTY. The timeout catches a regression
	// instead of hanging the whole test binary.
	done := make(chan struct{})
	var got []Item
	var err error
	go func() {
		defer close(done)
		got, err = Select(nil)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Select(nil) blocked; it must return without starting a program")
	}
	if err != nil {
		t.Errorf("Select(nil) error = %v, want nil", err)
	}
	if got != nil {
		t.Errorf("Select(nil) = %v, want nil", got)
	}

	got, err = Select([]Item{})
	if err != nil || got != nil {
		t.Errorf("Select(empty) = %v, %v; want nil, nil", got, err)
	}
}

func TestViewFitsTheWindow(t *testing.T) {
	m := newModel(fixture())
	m.items[0].Video.Title = strings.Repeat("very long title ", 40)

	if _, cmd := m.Update(tea.WindowSizeMsg{Width: 40, Height: 10}); cmd != nil {
		t.Errorf("resize returned a command: %v", cmd)
	}
	for _, line := range strings.Split(m.View(), "\n") {
		if w := len([]rune(stripANSI(line))); w > 40 {
			t.Errorf("line of width %d exceeds the 40-column window: %q", w, line)
		}
	}
}

// stripANSI removes styling so a rendered line can be measured in tests.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			i++
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
