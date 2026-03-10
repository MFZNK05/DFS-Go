package tabs

import (
	"fmt"
	"strings"

	"github.com/Faizan2005/DFS-Go/ipc"
	"github.com/Faizan2005/DFS-Go/tui/components"
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Transfer status constants (mirror Server/transfer package).
const (
	transferStatusQueued    = 0
	transferStatusActive    = 1
	transferStatusPaused    = 2
	transferStatusCompleted = 3
	transferStatusFailed    = 4
	transferStatusCancelled = 5
)

// Transfer direction constants.
const (
	directionUpload   = 0
	directionDownload = 1
)

var (
	progressFull  = lipgloss.NewStyle().Foreground(lipgloss.Color("#00FF00"))
	progressEmpty = lipgloss.NewStyle().Foreground(lipgloss.Color("#333333"))
)

// TransfersTab shows active and recent transfers with progress.
type TransfersTab struct {
	Width  int
	Height int

	transfers []ipc.TransferInfo
	table     components.Table

	// Confirm cancel state
	confirmCancel bool
	cancelID      string
	cancelName    string
}

func NewTransfersTab() *TransfersTab {
	cols := []components.Column{
		{Title: "Dir", Width: 4},
		{Title: "Name", Width: 20},
		{Title: "Status", Width: 10},
		{Title: "Progress", Width: 14},
		{Title: "Speed", Width: 10},
		{Title: "Size", Width: 10},
	}
	return &TransfersTab{
		table: components.NewTable(cols, "No active transfers"),
	}
}

func (t *TransfersTab) SetSize(w, h int) {
	t.Width = w
	t.Height = h
	t.table.SetSize(w-4, h-4)
}

// SetTransfers updates the transfers data.
func (t *TransfersTab) SetTransfers(transfers []ipc.TransferInfo) {
	t.transfers = transfers
	rows := make([][]string, len(transfers))
	for i, tr := range transfers {
		dir := "↑"
		if tr.Direction == directionDownload {
			dir = "↓"
		}
		rows[i] = []string{
			dir,
			tr.Name,
			statusLabel(tr.Status),
			progressBar(tr.Completed, tr.Total, 10),
			formatSpeed(tr.Speed),
			formatSize(tr.Size),
		}
	}
	t.table.SetRows(rows)
}

// Update handles key events for the transfers tab.
func (t *TransfersTab) Update(msg tea.KeyMsg) tea.Cmd {
	// Confirm cancel modal
	if t.confirmCancel {
		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("y", "enter"))):
			t.confirmCancel = false
			id := t.cancelID
			return func() tea.Msg {
				return TransferCancelMsg{ID: id}
			}
		case key.Matches(msg, key.NewBinding(key.WithKeys("n", "esc"))):
			t.confirmCancel = false
		}
		return nil
	}

	idx := t.table.SelectedRow()

	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("p"))):
		if idx >= 0 && idx < len(t.transfers) {
			id := t.transfers[idx].ID
			return func() tea.Msg {
				return TransferPauseMsg{ID: id}
			}
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("r"))):
		if idx >= 0 && idx < len(t.transfers) {
			id := t.transfers[idx].ID
			return func() tea.Msg {
				return TransferResumeMsg{ID: id}
			}
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("x"))):
		if idx >= 0 && idx < len(t.transfers) {
			t.confirmCancel = true
			t.cancelID = t.transfers[idx].ID
			t.cancelName = t.transfers[idx].Name
			return nil
		}
	}

	return t.table.Update(msg)
}

// View renders the transfers tab.
func (t *TransfersTab) View() string {
	tableView := t.table.View()

	if t.confirmCancel {
		confirm := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#FF4444")).
			Padding(1, 2).
			Render(fmt.Sprintf("Cancel transfer \"%s\"?\n\n  [y] Yes   [n] No", t.cancelName))
		tableView = confirm + "\n\n" + tableView
	}

	return tableView
}

func (t *TransfersTab) FooterHints() []components.FooterHint {
	return []components.FooterHint{
		{Key: "p", Desc: "Pause"},
		{Key: "r", Desc: "Resume"},
		{Key: "x", Desc: "Cancel"},
		{Key: "j/k", Desc: "Navigate"},
		{Key: "1-4", Desc: "Switch tab"},
		{Key: "q", Desc: "Quit"},
	}
}

// IsInputMode returns true when the transfers tab is capturing text input.
func (t TransfersTab) IsInputMode() bool {
	return t.table.Filtering
}

// Messages sent from the transfers tab.
type TransferPauseMsg struct{ ID string }
type TransferResumeMsg struct{ ID string }
type TransferCancelMsg struct{ ID string }

func statusLabel(status int) string {
	switch status {
	case transferStatusQueued:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#888888")).Render("queued")
	case transferStatusActive:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#00FF00")).Render("active")
	case transferStatusPaused:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#FFAA00")).Render("paused")
	case transferStatusCompleted:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#00FF00")).Render("done")
	case transferStatusFailed:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#FF4444")).Render("failed")
	case transferStatusCancelled:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#FF4444")).Render("cancelled")
	default:
		return "unknown"
	}
}

func progressBar(completed, total, width int) string {
	if total <= 0 {
		return strings.Repeat("░", width)
	}
	filled := completed * width / total
	if filled > width {
		filled = width
	}
	return progressFull.Render(strings.Repeat("█", filled)) +
		progressEmpty.Render(strings.Repeat("░", width-filled))
}

func formatSpeed(bytesPerSec float64) string {
	const (
		KB = 1024.0
		MB = KB * 1024
	)
	switch {
	case bytesPerSec >= MB:
		return fmt.Sprintf("%.1f MB/s", bytesPerSec/MB)
	case bytesPerSec >= KB:
		return fmt.Sprintf("%.1f KB/s", bytesPerSec/KB)
	case bytesPerSec > 0:
		return fmt.Sprintf("%.0f B/s", bytesPerSec)
	default:
		return "-"
	}
}
