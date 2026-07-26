// memory-vault-tui: a standalone terminal browser for the memory-vault
// database, talking to Postgres directly through internal/store (no MCP
// client, no LLM in the loop except for embeddings on save/search).
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"memory-vault/internal/embed"
	"memory-vault/internal/store"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envOrInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

// embedderFromEnv mirrors main.go's EMBED_PROVIDER handling, so the TUI
// reads/writes memories with whichever embedding backend the MCP server
// is configured to use. Any value other than "ollama"/"openai" is a
// configuration error, not a silent fallback to Ollama.
func embedderFromEnv() (embed.Embedder, error) {
	switch provider := envOr("EMBED_PROVIDER", "ollama"); provider {
	case "ollama":
		return &embed.OllamaEmbedder{
			URL:   envOr("OLLAMA_URL", "http://localhost:11434"),
			Model: envOr("OLLAMA_EMBED_MODEL", "all-minilm"),
		}, nil
	case "openai":
		return &embed.OpenAIEmbedder{
			BaseURL: envOr("OPENAI_EMBED_BASE_URL", "https://api.openai.com/v1"),
			APIKey:  os.Getenv("OPENAI_EMBED_API_KEY"),
			Model:   envOr("OPENAI_EMBED_MODEL", "text-embedding-3-small"),
		}, nil
	default:
		return nil, fmt.Errorf("unrecognized EMBED_PROVIDER %q (want \"ollama\" or \"openai\")", provider)
	}
}

func storeConfig() (store.Config, error) {
	embedder, err := embedderFromEnv()
	if err != nil {
		return store.Config{}, err
	}
	return store.Config{
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		Embedder:        embedder,
		EmbedDim:        envOrInt("EMBED_DIM", store.DefaultEmbedDim),
		MaxOpenConns:    5,
		MaxIdleConns:    2,
		ConnMaxLifetime: 30 * time.Minute,
	}, nil
}

func editor() string { return envOr("EDITOR", "nvim") }

// --- styling: monochrome-friendly, borders/bold over color ---

var (
	borderStyle   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder())
	selectedStyle = lipgloss.NewStyle().Bold(true).Reverse(true)
	headerStyle   = lipgloss.NewStyle().Bold(true)
	dimStyle      = lipgloss.NewStyle().Faint(true)
)

// --- rows: a flattened, groups-with-headers view of the memory list ---

type row struct {
	isHeader bool
	space    string
	name     string
	label    string // display text; for search results, includes the score
}

func rowsFromAll(all []store.SpaceName) []row {
	var rows []row
	cur := ""
	for _, sn := range all {
		if sn.Space != cur {
			rows = append(rows, row{isHeader: true, space: sn.Space, label: sn.Space})
			cur = sn.Space
		}
		rows = append(rows, row{space: sn.Space, name: sn.Name, label: fmt.Sprintf("%s  (%s)", sn.Name, sn.Source)})
	}
	return rows
}

func rowsFromMatches(space string, matches []store.SearchMatch) []row {
	rows := make([]row, len(matches))
	for i, m := range matches {
		rows[i] = row{space: space, name: m.Name, label: fmt.Sprintf("%s (%.3f)", m.Name, m.Score)}
	}
	return rows
}

// --- messages ---

type rowsLoadedMsg struct {
	rows []row
	err  error
}
type contentLoadedMsg struct {
	content string
	source  string
	err     error
}
type searchDoneMsg struct {
	matches []store.SearchMatch
	err     error
}
type mutateDoneMsg struct {
	status string
	err    error
}
type editDoneMsg struct {
	space, name, content string
	err                  error
}

// --- model ---

type mode int

const (
	modeBrowse mode = iota
	modeSearchInput
	modeSearchResults
	modeConfirmDelete
	modeNewName
	modeNewSpace
	modeNewSource
	modeNewContent // waiting on $EDITOR
)

type model struct {
	st *store.Store

	rows         []row
	cursor       int
	currentSpace string // space of the last-selected item; also the search/new default

	content       string
	currentSource string
	viewport      viewport.Model

	mode      mode
	input     textinput.Model
	newName   string
	newSpace  string
	pending   row // for delete confirmation
	status    string
	statusErr bool

	width, height int
	ready         bool
}

func initialModel(st *store.Store) model {
	ti := textinput.New()
	ti.Prompt = "> "
	return model{st: st, currentSpace: store.DefaultSpace, input: ti}
}

func (m model) Init() tea.Cmd {
	return loadRowsCmd(m.st)
}

func loadRowsCmd(st *store.Store) tea.Cmd {
	return func() tea.Msg {
		all, err := st.ListAll("")
		return rowsLoadedMsg{rows: rowsFromAll(all), err: err}
	}
}

func loadContentCmd(st *store.Store, space, name string) tea.Cmd {
	return func() tea.Msg {
		meta, err := st.ReassembleMeta(space, name)
		return contentLoadedMsg{content: meta.Content, source: meta.Source, err: err}
	}
}

func searchCmd(st *store.Store, space, query string) tea.Cmd {
	return func() tea.Msg {
		matches, err := st.SearchMemories(space, query, 10, store.SearchWeights{Semantic: 1.0, HalfLifeDays: 30}, "")
		return searchDoneMsg{matches: matches, err: err}
	}
}

func deleteCmd(st *store.Store, space, name string) tea.Cmd {
	return func() tea.Msg {
		_, err := st.DeleteMemory(space, name)
		return mutateDoneMsg{status: fmt.Sprintf("deleted %q from space %q", name, space), err: err}
	}
}

// editCmd suspends the TUI, opens content in $EDITOR against a temp file,
// and on return reads it back and saves it via the store. source is
// preserved as-is for an edit of an existing memory, or whatever the user
// entered when creating a new one.
func editCmd(st *store.Store, space, name, content, source string) tea.Cmd {
	f, err := os.CreateTemp("", "memory-vault-*.md")
	if err != nil {
		return func() tea.Msg { return editDoneMsg{err: err} }
	}
	path := f.Name()
	f.WriteString(content)
	f.Close()

	c := exec.Command(editor(), path)
	return tea.ExecProcess(c, func(err error) tea.Msg {
		defer os.Remove(path)
		if err != nil {
			return editDoneMsg{err: err}
		}
		edited, err := os.ReadFile(path)
		if err != nil {
			return editDoneMsg{err: err}
		}
		text := strings.TrimSpace(string(edited))
		if text == "" {
			return editDoneMsg{err: fmt.Errorf("empty content, not saved")}
		}
		if _, err := st.SaveMemory(space, name, text, source); err != nil {
			return editDoneMsg{err: err}
		}
		return editDoneMsg{space: space, name: name, content: text}
	})
}

// selected returns the currently highlighted item row, if any.
func (m model) selected() (row, bool) {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return row{}, false
	}
	r := m.rows[m.cursor]
	if r.isHeader {
		return row{}, false
	}
	return r, true
}

// moveCursor moves the selection, skipping header rows, and returns a
// command to load the newly selected item's content (browse mode only).
func (m *model) moveCursor(delta int) tea.Cmd {
	if len(m.rows) == 0 {
		return nil
	}
	next := m.cursor
	for i := 0; i < len(m.rows); i++ {
		next += delta
		if next < 0 || next >= len(m.rows) {
			return nil
		}
		if !m.rows[next].isHeader {
			m.cursor = next
			m.currentSpace = m.rows[next].space
			if m.mode == modeSearchResults {
				return nil // search rows already carry content; loaded on search
			}
			return loadContentCmd(m.st, m.rows[next].space, m.rows[next].name)
		}
	}
	return nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		listW := m.width / 3
		m.viewport.Width = m.width - listW - 4
		m.viewport.Height = m.height - 4
		m.ready = true
		return m, nil

	case rowsLoadedMsg:
		if msg.err != nil {
			m.status, m.statusErr = msg.err.Error(), true
			return m, nil
		}
		m.rows = msg.rows
		if m.cursor >= len(m.rows) {
			m.cursor = 0
		}
		return m, m.moveCursor(0)

	case contentLoadedMsg:
		if msg.err != nil {
			m.status, m.statusErr = msg.err.Error(), true
			return m, nil
		}
		m.content = msg.content
		m.currentSource = msg.source
		m.viewport.SetContent(msg.content)
		m.viewport.GotoTop()
		return m, nil

	case searchDoneMsg:
		if msg.err != nil {
			m.status, m.statusErr = msg.err.Error(), true
			m.mode = modeBrowse
			return m, nil
		}
		m.rows = rowsFromMatches(m.currentSpace, msg.matches)
		m.cursor = 0
		m.mode = modeSearchResults
		m.status = fmt.Sprintf("%d result(s) in space %q (esc to go back)", len(msg.matches), m.currentSpace)
		m.statusErr = false
		if len(m.rows) > 0 {
			m.content = msg.matches[0].Content
			m.viewport.SetContent(m.content)
			m.viewport.GotoTop()
		}
		return m, nil

	case mutateDoneMsg:
		m.statusErr = msg.err != nil
		if msg.err != nil {
			m.status = msg.err.Error()
			return m, nil
		}
		m.status = msg.status
		m.mode = modeBrowse
		return m, loadRowsCmd(m.st)

	case editDoneMsg:
		m.statusErr = msg.err != nil
		if msg.err != nil {
			m.status = "edit: " + msg.err.Error()
			m.mode = modeBrowse
			return m, nil
		}
		m.status = fmt.Sprintf("saved %q in space %q", msg.name, msg.space)
		m.mode = modeBrowse
		return m, loadRowsCmd(m.st)

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	return m, cmd
}

func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.mode {
	case modeSearchInput, modeNewName, modeNewSpace, modeNewSource:
		switch msg.String() {
		case "esc":
			m.mode = modeBrowse
			m.input.Blur()
			return m, nil
		case "enter":
			value := strings.TrimSpace(m.input.Value())
			m.input.Blur()
			switch m.mode {
			case modeSearchInput:
				if value == "" {
					m.mode = modeBrowse
					return m, nil
				}
				m.status = "searching..."
				return m, searchCmd(m.st, m.currentSpace, value)
			case modeNewName:
				if value == "" {
					m.mode = modeBrowse
					return m, nil
				}
				m.newName = value
				m.mode = modeNewSpace
				m.input.SetValue(m.currentSpace)
				m.input.Focus()
				return m, nil
			case modeNewSpace:
				m.newSpace = value
				if m.newSpace == "" {
					m.newSpace = store.DefaultSpace
				}
				m.mode = modeNewSource
				m.input.SetValue("")
				m.input.Placeholder = store.DefaultSource
				m.input.Focus()
				return m, nil
			case modeNewSource:
				source := value
				if source == "" {
					source = store.DefaultSource
				}
				m.mode = modeNewContent
				return m, editCmd(m.st, m.newSpace, m.newName, "", source)
			}
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd

	case modeConfirmDelete:
		switch msg.String() {
		case "y":
			m.mode = modeBrowse
			return m, deleteCmd(m.st, m.pending.space, m.pending.name)
		default:
			m.mode = modeBrowse
			m.status = "delete cancelled"
			m.statusErr = false
			return m, nil
		}

	case modeSearchResults:
		switch msg.String() {
		case "esc":
			m.mode = modeBrowse
			return m, loadRowsCmd(m.st)
		case "q", "ctrl+c":
			return m, tea.Quit
		case "up", "k":
			return m, m.moveCursorSearch(-1)
		case "down", "j":
			return m, m.moveCursorSearch(1)
		}
		return m, nil

	default: // modeBrowse
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "up", "k":
			return m, m.moveCursor(-1)
		case "down", "j":
			return m, m.moveCursor(1)
		case "/":
			m.mode = modeSearchInput
			m.input.SetValue("")
			m.input.Placeholder = "search query"
			m.input.Focus()
			return m, nil
		case "n":
			m.mode = modeNewName
			m.input.SetValue("")
			m.input.Placeholder = "memory name"
			m.input.Focus()
			return m, nil
		case "e":
			if r, ok := m.selected(); ok {
				return m, editCmd(m.st, r.space, r.name, m.content, m.currentSource)
			}
			return m, nil
		case "d":
			if r, ok := m.selected(); ok {
				m.pending = r
				m.mode = modeConfirmDelete
			}
			return m, nil
		}
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd
	}
}

// moveCursorSearch moves the selection within search results (no header
// rows to skip, content already loaded per result).
func (m *model) moveCursorSearch(delta int) tea.Cmd {
	if len(m.rows) == 0 {
		return nil
	}
	next := m.cursor + delta
	if next < 0 || next >= len(m.rows) {
		return nil
	}
	m.cursor = next
	return loadContentCmd(m.st, m.rows[next].space, m.rows[next].name)
}

func (m model) View() string {
	if !m.ready {
		return "loading..."
	}

	var list strings.Builder
	for i, r := range m.rows {
		switch {
		case r.isHeader:
			list.WriteString(headerStyle.Render("["+r.label+"]") + "\n")
		case i == m.cursor:
			list.WriteString(selectedStyle.Render(r.label) + "\n")
		default:
			list.WriteString("  " + r.label + "\n")
		}
	}
	listPane := borderStyle.Width(m.width/3 - 2).Height(m.height - 4).Render(list.String())
	contentPane := borderStyle.Width(m.viewport.Width).Height(m.viewport.Height).Render(m.viewport.View())

	body := lipgloss.JoinHorizontal(lipgloss.Top, listPane, contentPane)

	var bottom string
	switch m.mode {
	case modeSearchInput:
		bottom = "search: " + m.input.View()
	case modeNewName:
		bottom = "new memory name: " + m.input.View()
	case modeNewSpace:
		bottom = "space (" + store.DefaultSpace + "): " + m.input.View()
	case modeNewSource:
		bottom = "source (" + store.DefaultSource + "): " + m.input.View()
	case modeConfirmDelete:
		bottom = fmt.Sprintf("delete %q from space %q? (y/N)", m.pending.name, m.pending.space)
	default:
		status := m.status
		if m.statusErr {
			status = "error: " + status
		}
		keys := dimStyle.Render("/ search   e edit   n new   d delete   q quit")
		if status != "" {
			bottom = status + "   " + keys
		} else {
			bottom = keys
		}
	}

	return body + "\n" + bottom
}

func main() {
	cfg, err := storeConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		os.Exit(1)
	}
	st, err := store.Open(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "store.Open:", err)
		os.Exit(1)
	}
	defer st.Close()

	p := tea.NewProgram(initialModel(st), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "tui:", err)
		os.Exit(1)
	}
}
