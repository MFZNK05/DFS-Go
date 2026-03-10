package tabs

import (
	"fmt"
	"strings"

	"github.com/Faizan2005/DFS-Go/ipc"
	"github.com/Faizan2005/DFS-Go/tui/components"
	"github.com/charmbracelet/lipgloss"
)

var (
	cardStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#7D56F4")).
			Padding(1, 2)

	keyStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#7D56F4")).
			Width(14)

	valStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FFFFFF"))

	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#7D56F4")).
			MarginBottom(1)
)

// DiagnosticsTab holds the diagnostics tab state.
type DiagnosticsTab struct {
	Status ipc.NodeStatusInfo
	Width  int
	Height int
}

// NewDiagnosticsTab creates a new diagnostics tab.
func NewDiagnosticsTab() *DiagnosticsTab {
	return &DiagnosticsTab{}
}

// SetSize updates the tab dimensions.
func (d *DiagnosticsTab) SetSize(w, h int) {
	d.Width = w
	d.Height = h
}

// View renders the diagnostics tab.
func (d DiagnosticsTab) View() string {
	rows := []string{
		keyStyle.Render("Address") + valStyle.Render(d.Status.Addr),
		keyStyle.Render("Status") + valStyle.Render(d.Status.Status),
		keyStyle.Render("Uptime") + valStyle.Render(d.Status.Uptime),
		keyStyle.Render("Peers") + valStyle.Render(fmt.Sprintf("%d", d.Status.PeerCount)),
		keyStyle.Render("Uploads") + valStyle.Render(fmt.Sprintf("%d", d.Status.Uploads)),
		keyStyle.Render("Downloads") + valStyle.Render(fmt.Sprintf("%d", d.Status.Downloads)),
	}

	card := cardStyle.Width(d.Width - 4).Render(
		titleStyle.Render("Node Status") + "\n" + strings.Join(rows, "\n"),
	)

	return card
}

// FooterHints returns the contextual footer hints for this tab.
func (d DiagnosticsTab) FooterHints() []components.FooterHint {
	return []components.FooterHint{
		{Key: "1-4", Desc: "Switch tab"},
		{Key: "q", Desc: "Quit"},
	}
}
