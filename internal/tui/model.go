package tui

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Fallback geometry for the window between program start and the first
// tea.WindowSizeMsg, and for tests, which never receive one.
const (
	defaultWidth  = 80
	defaultHeight = 24

	headerLines = 2 // counts line + blank
	footerLines = 2 // blank + key hints
)

// Colours are declared as adaptive pairs rather than escape codes so lipgloss
// can drop them entirely on a terminal that reports no colour profile.
var (
	styleHeader = lipgloss.NewStyle().Bold(true)
	styleHint   = lipgloss.NewStyle().Faint(true)
	styleDim    = lipgloss.NewStyle().Faint(true)
	styleCursor = lipgloss.NewStyle().Bold(true).Foreground(
		lipgloss.AdaptiveColor{Light: "#3B5BDB", Dark: "#91A7FF"})
	styleCursorDim = lipgloss.NewStyle().Faint(true).Foreground(
		lipgloss.AdaptiveColor{Light: "#3B5BDB", Dark: "#91A7FF"})
)

// model holds the whole picker state. Every mutation goes through a method so
// that the Bubble Tea Update loop stays a key-to-method mapping and the logic
// underneath can be exercised without a terminal.
type model struct {
	items  []Item
	cursor int // index into items; always points at a visible row

	// showExisting keeps already-downloaded rows on screen (dimmed). Hiding
	// them is for the common case of a re-crawl where most of the page is
	// already in the library.
	showExisting bool

	viewport viewport.Model
	width    int
	height   int

	confirmed bool
}

// newModel applies the default selection: everything not already on disk. The
// caller's Selected values are ignored — the whole point of the picker is that
// it decides the opening state and the user amends it.
func newModel(items []Item) *model {
	m := &model{
		items:        make([]Item, len(items)),
		showExisting: true,
		width:        defaultWidth,
		height:       defaultHeight,
	}
	copy(m.items, items)
	for i := range m.items {
		m.items[i].Selected = m.items[i].Existing == ""
	}
	m.viewport = viewport.New(m.width, m.bodyHeight())
	m.refresh()
	return m
}

// visible reports whether row i is currently on screen.
func (m *model) visible(i int) bool {
	return m.showExisting || m.items[i].Existing == ""
}

// visibleIndexes lists the rows on screen, in order.
func (m *model) visibleIndexes() []int {
	out := make([]int, 0, len(m.items))
	for i := range m.items {
		if m.visible(i) {
			out = append(out, i)
		}
	}
	return out
}

// counts summarises the list for the header.
func (m *model) counts() (selected, existing, total int) {
	for _, it := range m.items {
		if it.Selected {
			selected++
		}
		if it.Existing != "" {
			existing++
		}
	}
	return selected, existing, len(m.items)
}

// chosen returns the selected items, in list order.
func (m *model) chosen() []Item {
	out := make([]Item, 0, len(m.items))
	for _, it := range m.items {
		if it.Selected {
			out = append(out, it)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// toggle flips one row. Out-of-range indexes are ignored so callers need not
// guard an empty or filtered list.
func (m *model) toggle(i int) {
	if i < 0 || i >= len(m.items) {
		return
	}
	m.items[i].Selected = !m.items[i].Selected
}

// setAllVisible ticks or unticks every row on screen. Hidden rows are left
// alone: changing what the user cannot see would make the header counts move
// for no visible reason.
func (m *model) setAllVisible(selected bool) {
	for _, i := range m.visibleIndexes() {
		m.items[i].Selected = selected
	}
}

// invertVisible flips every row on screen.
func (m *model) invertVisible() {
	for _, i := range m.visibleIndexes() {
		m.items[i].Selected = !m.items[i].Selected
	}
}

// toggleShowExisting shows or hides the already-downloaded rows, keeping the
// cursor on a row that still exists.
func (m *model) toggleShowExisting() {
	m.showExisting = !m.showExisting
	m.clampCursor()
}

// moveCursor steps delta visible rows, stopping at the ends rather than
// wrapping: wrapping in a long list loses the user's place.
func (m *model) moveCursor(delta int) {
	vis := m.visibleIndexes()
	if len(vis) == 0 {
		return
	}
	pos := 0
	for p, i := range vis {
		if i == m.cursor {
			pos = p
			break
		}
	}
	pos += delta
	if pos < 0 {
		pos = 0
	}
	if pos >= len(vis) {
		pos = len(vis) - 1
	}
	m.cursor = vis[pos]
}

// clampCursor parks the cursor on the nearest visible row.
func (m *model) clampCursor() {
	vis := m.visibleIndexes()
	if len(vis) == 0 {
		m.cursor = 0
		return
	}
	for _, i := range vis {
		if i >= m.cursor {
			m.cursor = i
			return
		}
	}
	m.cursor = vis[len(vis)-1]
}

func (m *model) bodyHeight() int {
	h := m.height - headerLines - footerLines
	if h < 1 {
		h = 1
	}
	return h
}

// setSize re-lays the view after a terminal resize.
func (m *model) setSize(width, height int) {
	m.width, m.height = width, height
	m.viewport.Width = width
	m.viewport.Height = m.bodyHeight()
}

// refresh re-renders the rows into the viewport and scrolls the cursor back
// into view. Rows are cheap to build, so there is no incremental update.
func (m *model) refresh() {
	vis := m.visibleIndexes()
	lines := make([]string, 0, len(vis))
	cursorLine := 0
	for n, i := range vis {
		if i == m.cursor {
			cursorLine = n
		}
		lines = append(lines, m.renderRow(i))
	}
	if len(lines) == 0 {
		lines = append(lines, styleDim.Render("(all rows hidden — press e to show already-downloaded)"))
	}
	m.viewport.SetContent(strings.Join(lines, "\n"))

	switch h := m.viewport.Height; {
	case cursorLine < m.viewport.YOffset:
		m.viewport.SetYOffset(cursorLine)
	case cursorLine >= m.viewport.YOffset+h:
		m.viewport.SetYOffset(cursorLine - h + 1)
	}
}

// renderRow lays out one row as "> [x] title      [kind]", truncating the
// title so the row never wraps.
func (m *model) renderRow(i int) string {
	it := m.items[i]

	cursor := "  "
	if i == m.cursor {
		cursor = "> "
	}
	box := "[ ] "
	if it.Selected {
		box = "[x] "
	}
	tag := "[" + it.Video.Kind + "]"

	label := strings.TrimSpace(it.Video.Title)
	if label == "" {
		label = it.Video.URL
	}
	if it.Existing != "" {
		label += "  (have " + filepath.Base(it.Existing) + ")"
	}

	// One space keeps the tag off the title even when the title fills the row.
	avail := m.width - lipgloss.Width(cursor+box+tag) - 1
	if avail < 1 {
		avail = 1
	}
	label = ansi.Truncate(label, avail, "…")
	pad := avail - lipgloss.Width(label)
	if pad < 0 {
		pad = 0
	}

	line := cursor + box + label + strings.Repeat(" ", pad) + " " + tag

	// The whole row takes a single style: nesting lipgloss styles inside one
	// another produces stray resets on terminals with a limited profile.
	switch {
	case i == m.cursor && it.Existing != "":
		return styleCursorDim.Render(line)
	case i == m.cursor:
		return styleCursor.Render(line)
	case it.Existing != "":
		return styleDim.Render(line)
	}
	return line
}

func (m *model) header() string {
	selected, existing, total := m.counts()
	s := fmt.Sprintf("%d selected · %d already downloaded · %d total", selected, existing, total)
	if !m.showExisting && existing > 0 {
		s += " · downloaded hidden"
	}
	return styleHeader.Render(ansi.Truncate(s, max(m.width, 1), "…"))
}

func (m *model) footer() string {
	const hints = "↑/↓ move · space toggle · a all · n none · i invert · e show/hide downloaded · enter confirm · q abort"
	return styleHint.Render(ansi.Truncate(hints, max(m.width, 1), "…"))
}

func (m *model) Init() tea.Cmd { return nil }

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.setSize(msg.Width, msg.Height)

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			return m, tea.Quit
		case "enter":
			m.confirmed = true
			return m, tea.Quit
		case "up", "k":
			m.moveCursor(-1)
		case "down", "j":
			m.moveCursor(1)
		case "pgup":
			m.moveCursor(-m.viewport.Height)
		case "pgdown":
			m.moveCursor(m.viewport.Height)
		case " ":
			m.toggle(m.cursor)
		case "a":
			m.setAllVisible(true)
		case "n":
			m.setAllVisible(false)
		case "i":
			m.invertVisible()
		case "e":
			m.toggleShowExisting()
		}
	}

	m.refresh()
	return m, nil
}

func (m *model) View() string {
	return strings.Join([]string{
		m.header(),
		"",
		m.viewport.View(),
		"",
		m.footer(),
	}, "\n")
}
