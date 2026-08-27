package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/shearn89/vid-scraper/internal/config"
)

// siteRowLines is how many screen lines one site occupies: its label plus the
// dimmer line naming where the files land.
const siteRowLines = 2

// maxShortcut is the highest row reachable by a number key. Ten would need a
// two-key sequence, and the shortcut only earns its place while it is one
// keypress.
const maxShortcut = 9

// SelectSite runs the site chooser and returns the site the user picked.
// It returns ErrAborted if the user quits without choosing.
func SelectSite(sites []config.Launchable) (config.Launchable, error) {
	// Nothing to choose from means nothing to show. Checked before the
	// terminal, because having no sites is the more specific answer and holds
	// whether or not a terminal exists.
	if len(sites) == 0 {
		return config.Launchable{}, ErrAborted
	}
	if !interactive() {
		return config.Launchable{}, ErrNoTerminal
	}

	// A single configured site is still shown rather than returned outright:
	// launching starts a crawl against a real host and writes to a real
	// directory, and auto-selecting would hide both from the user at the one
	// moment they can still say no.
	final, err := tea.NewProgram(newSiteModel(sites), tea.WithAltScreen()).Run()
	if err != nil {
		return config.Launchable{}, fmt.Errorf("running site picker: %w", err)
	}
	m, ok := final.(*siteModel)
	if !ok {
		return config.Launchable{}, fmt.Errorf("running site picker: unexpected model %T", final)
	}
	if !m.confirmed {
		return config.Launchable{}, ErrAborted
	}
	return m.selected(), nil
}

// siteModel holds the chooser state. As with the video picker, every mutation
// is a pure method so the Update loop stays a key-to-method mapping and the
// logic underneath is testable without a terminal.
type siteModel struct {
	sites  []config.Launchable
	cursor int // index into sites; always in range

	viewport viewport.Model
	width    int
	height   int

	confirmed bool
}

func newSiteModel(sites []config.Launchable) *siteModel {
	m := &siteModel{
		sites:  make([]config.Launchable, len(sites)),
		width:  defaultWidth,
		height: defaultHeight,
	}
	copy(m.sites, sites)
	m.viewport = viewport.New(m.width, m.bodyHeight())
	m.refresh()
	return m
}

// selected returns the site under the cursor. An empty list yields the zero
// value so callers need not guard; SelectSite refuses to start on one anyway.
func (m *siteModel) selected() config.Launchable {
	if m.cursor < 0 || m.cursor >= len(m.sites) {
		return config.Launchable{}
	}
	return m.sites[m.cursor]
}

// moveCursor steps delta rows, stopping at the ends rather than wrapping: the
// list is short enough that running off the end reads as a mistake.
func (m *siteModel) moveCursor(delta int) {
	if len(m.sites) == 0 {
		m.cursor = 0
		return
	}
	pos := m.cursor + delta
	if pos < 0 {
		pos = 0
	}
	if pos >= len(m.sites) {
		pos = len(m.sites) - 1
	}
	m.cursor = pos
}

// choose picks row i outright, as the number shortcuts do. It reports whether
// the index existed; a number past the end of the list is ignored rather than
// clamped, since clamping would launch a site the user did not name.
func (m *siteModel) choose(i int) bool {
	if i < 0 || i >= len(m.sites) {
		return false
	}
	m.cursor = i
	m.confirmed = true
	return true
}

// confirm accepts the row under the cursor.
func (m *siteModel) confirm() {
	if len(m.sites) == 0 {
		return
	}
	m.confirmed = true
}

func (m *siteModel) bodyHeight() int {
	h := m.height - headerLines - footerLines
	if h < 1 {
		h = 1
	}
	return h
}

// setSize re-lays the view after a terminal resize.
func (m *siteModel) setSize(width, height int) {
	m.width, m.height = width, height
	m.viewport.Width = width
	m.viewport.Height = m.bodyHeight()
}

// refresh re-renders the rows into the viewport and scrolls the cursor back
// into view. Each site is two lines, so scrolling works in row units and then
// converts, otherwise a cursor row can end up half off the bottom.
func (m *siteModel) refresh() {
	lines := make([]string, 0, len(m.sites)*siteRowLines)
	for i := range m.sites {
		lines = append(lines, m.renderRow(i)...)
	}
	if len(lines) == 0 {
		lines = append(lines, styleDim.Render("(no sites with a url configured)"))
	}
	m.viewport.SetContent(strings.Join(lines, "\n"))

	top := m.cursor * siteRowLines
	bottom := top + siteRowLines - 1
	switch h := m.viewport.Height; {
	case top < m.viewport.YOffset:
		m.viewport.SetYOffset(top)
	case bottom >= m.viewport.YOffset+h:
		m.viewport.SetYOffset(bottom - h + 1)
	}
}

// renderRow lays out one site as a label line and a dimmer line giving the
// download directory and start URL, so the user can see where files will land
// before committing.
func (m *siteModel) renderRow(i int) []string {
	site := m.sites[i]

	cursor := "  "
	if i == m.cursor {
		cursor = "> "
	}
	// Numbering the rows is what makes the number shortcut discoverable; rows
	// past it line up with the rest but advertise nothing.
	num := "  "
	if i < maxShortcut {
		num = fmt.Sprintf("%d ", i+1)
	}

	dir := site.Site.Dir
	if dir == "" {
		dir = "(default directory)"
	}
	detail := dir + " · " + site.Site.URL

	label := cursor + num + m.fit(site.Label(), lipgloss.Width(cursor+num))
	// The detail line indents under the label rather than under the cursor, so
	// the two lines read as one entry.
	const indent = "      "
	detail = indent + m.fit(detail, lipgloss.Width(indent))

	// Each line takes a single style: nesting lipgloss styles inside one
	// another produces stray resets on terminals with a limited profile.
	if i == m.cursor {
		return []string{styleCursor.Render(label), styleCursorDim.Render(detail)}
	}
	return []string{label, styleDim.Render(detail)}
}

// fit truncates s to what is left of the row after a prefix of the given width.
func (m *siteModel) fit(s string, prefix int) string {
	avail := m.width - prefix
	if avail < 1 {
		avail = 1
	}
	return ansi.Truncate(s, avail, "…")
}

func (m *siteModel) header() string {
	s := fmt.Sprintf("choose a site · %d configured", len(m.sites))
	return styleHeader.Render(ansi.Truncate(s, max(m.width, 1), "…"))
}

func (m *siteModel) footer() string {
	const hints = "↑/↓ move · 1-9 pick · enter choose · q abort"
	return styleHint.Render(ansi.Truncate(hints, max(m.width, 1), "…"))
}

func (m *siteModel) Init() tea.Cmd { return nil }

func (m *siteModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.setSize(msg.Width, msg.Height)

	case tea.KeyMsg:
		s := msg.String()
		switch s {
		case "ctrl+c", "q", "esc":
			return m, tea.Quit
		case "enter":
			m.confirm()
			return m, tea.Quit
		case "up", "k":
			m.moveCursor(-1)
		case "down", "j":
			m.moveCursor(1)
		case "pgup":
			m.moveCursor(-m.rowsPerScreen())
		case "pgdown":
			m.moveCursor(m.rowsPerScreen())
		default:
			if len(s) == 1 && s[0] >= '1' && s[0] <= '9' {
				if m.choose(int(s[0] - '1')) {
					return m, tea.Quit
				}
			}
		}
	}

	m.refresh()
	return m, nil
}

// rowsPerScreen converts the viewport height into whole sites, so a page step
// never lands between the two lines of an entry.
func (m *siteModel) rowsPerScreen() int {
	n := m.viewport.Height / siteRowLines
	if n < 1 {
		n = 1
	}
	return n
}

func (m *siteModel) View() string {
	return strings.Join([]string{
		m.header(),
		"",
		m.viewport.View(),
		"",
		m.footer(),
	}, "\n")
}
