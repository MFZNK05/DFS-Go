package components

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var (
	tblHeaderStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#7D56F4")).
			Border(lipgloss.NormalBorder(), false, false, true, false).
			BorderForeground(lipgloss.Color("#444444"))

	tblSelectedStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("#FFFFFF")).
				Background(lipgloss.Color("#333355"))

	tblRowStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#CCCCCC"))

	tblEmptyStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#555555")).
			Italic(true).
			Padding(1, 2)
)

// Column defines a table column.
type Column struct {
	Title string
	Width int
}

// Table is a reusable filterable table with j/k navigation.
type Table struct {
	Columns    []Column
	baseWidths []int // original column widths for proportional scaling
	Rows       [][]string
	Cursor     int
	Filter     string
	Filtering  bool
	EmptyText  string
	Width      int
	Height     int
	filtered   []int // indices into Rows matching filter
}

// NewTable creates a new table.
func NewTable(columns []Column, emptyText string) Table {
	base := make([]int, len(columns))
	for i, c := range columns {
		base[i] = c.Width
	}
	return Table{
		Columns:    columns,
		baseWidths: base,
		EmptyText:  emptyText,
	}
}

// SetSize updates the table dimensions and scales column widths proportionally.
func (t *Table) SetSize(w, h int) {
	t.Width = w
	t.Height = h

	// Scale column widths proportionally from base widths
	totalBase := 0
	for _, bw := range t.baseWidths {
		totalBase += bw
	}
	if totalBase > 0 && w > 0 {
		for i, bw := range t.baseWidths {
			t.Columns[i].Width = bw * w / totalBase
			if t.Columns[i].Width < 4 {
				t.Columns[i].Width = 4
			}
		}
	}
}

// SetRows replaces the row data and recomputes the filter.
func (t *Table) SetRows(rows [][]string) {
	t.Rows = rows
	t.applyFilter()
	if t.Cursor >= len(t.filtered) {
		t.Cursor = max(0, len(t.filtered)-1)
	}
}

// SelectedRow returns the original index of the selected row, or -1.
func (t *Table) SelectedRow() int {
	if t.Cursor < 0 || t.Cursor >= len(t.filtered) {
		return -1
	}
	return t.filtered[t.Cursor]
}

// Update handles key events for navigation and filtering.
func (t *Table) Update(msg tea.KeyMsg) tea.Cmd {
	if t.Filtering {
		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
			t.Filtering = false
			t.Filter = ""
			t.applyFilter()
		case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
			t.Filtering = false
		case key.Matches(msg, key.NewBinding(key.WithKeys("backspace"))):
			if len(t.Filter) > 0 {
				t.Filter = t.Filter[:len(t.Filter)-1]
				t.applyFilter()
			}
		default:
			r := msg.String()
			if len(r) == 1 {
				t.Filter += r
				t.applyFilter()
				t.Cursor = 0
			}
		}
		return nil
	}

	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("j", "down"))):
		if t.Cursor < len(t.filtered)-1 {
			t.Cursor++
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("k", "up"))):
		if t.Cursor > 0 {
			t.Cursor--
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("g"))):
		t.Cursor = 0
	case key.Matches(msg, key.NewBinding(key.WithKeys("G"))):
		if len(t.filtered) > 0 {
			t.Cursor = len(t.filtered) - 1
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("/"))):
		t.Filtering = true
	case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
		t.Filter = ""
		t.applyFilter()
	}
	return nil
}

// View renders the table.
func (t *Table) View() string {
	if len(t.Rows) == 0 && t.EmptyText != "" {
		return tblEmptyStyle.Render(t.EmptyText)
	}

	var b strings.Builder

	// Header
	headerCells := make([]string, len(t.Columns))
	for i, col := range t.Columns {
		headerCells[i] = tblHeaderStyle.Width(col.Width).PaddingRight(1).Render(col.Title)
	}
	b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, headerCells...))
	b.WriteString("\n")

	// Filter indicator
	if t.Filter != "" {
		filterLine := lipgloss.NewStyle().Foreground(lipgloss.Color("#FFAA00")).Render("filter: " + t.Filter)
		if t.Filtering {
			filterLine += lipgloss.NewStyle().Foreground(lipgloss.Color("#FFAA00")).Blink(true).Render("_")
		}
		b.WriteString(filterLine + "\n")
	} else if t.Filtering {
		b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("#FFAA00")).Render("filter: _") + "\n")
	}

	// Rows
	visibleRows := t.Height - 3 // header + possible filter + padding
	if visibleRows < 1 {
		visibleRows = 1
	}

	// Scroll window
	start := 0
	if t.Cursor >= visibleRows {
		start = t.Cursor - visibleRows + 1
	}
	end := start + visibleRows
	if end > len(t.filtered) {
		end = len(t.filtered)
	}

	for vi := start; vi < end; vi++ {
		rowIdx := t.filtered[vi]
		row := t.Rows[rowIdx]
		cells := make([]string, len(t.Columns))
		for ci, col := range t.Columns {
			val := ""
			if ci < len(row) {
				val = row[ci]
			}
			if vi == t.Cursor {
				cells[ci] = tblSelectedStyle.Width(col.Width).PaddingRight(1).Render(val)
			} else {
				cells[ci] = tblRowStyle.Width(col.Width).PaddingRight(1).Render(val)
			}
		}
		b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, cells...))
		b.WriteString("\n")
	}

	if len(t.filtered) == 0 && len(t.Rows) > 0 {
		b.WriteString(tblEmptyStyle.Render("No matches"))
	}

	// Scroll indicator when more rows exist below/above
	if end < len(t.filtered) {
		more := len(t.filtered) - end
		b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("#555555")).Italic(true).
			Render(fmt.Sprintf("  ↓ %d more (j/G to scroll)", more)))
		b.WriteString("\n")
	}
	if start > 0 {
		b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("#555555")).Italic(true).
			Render(fmt.Sprintf("  ↑ %d above (k/g to scroll)", start)))
		b.WriteString("\n")
	}

	return b.String()
}

func (t *Table) applyFilter() {
	t.filtered = t.filtered[:0]
	lower := strings.ToLower(t.Filter)
	for i, row := range t.Rows {
		if t.Filter == "" {
			t.filtered = append(t.filtered, i)
			continue
		}
		for _, cell := range row {
			if strings.Contains(strings.ToLower(cell), lower) {
				t.filtered = append(t.filtered, i)
				break
			}
		}
	}
}
