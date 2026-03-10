package components

import (
	"fmt"

	"github.com/Faizan2005/DFS-Go/ipc"
	"github.com/charmbracelet/lipgloss"
)

var (
	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#FFFFFF")).
			Background(lipgloss.Color("#1a1a2e")).
			Padding(0, 1)

	headerKeyStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#7D56F4"))

	headerValStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#CCCCCC"))

	headerSep = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#555555")).
			SetString(" │ ")
)

// RenderHeader renders the global status bar.
func RenderHeader(status ipc.NodeStatusInfo, width int) string {
	addr := status.Addr
	if addr == "" {
		addr = "disconnected"
	}

	parts := []string{
		headerKeyStyle.Render("Node: ") + headerValStyle.Render(addr),
		headerKeyStyle.Render("Status: ") + headerValStyle.Render(status.Status),
		headerKeyStyle.Render("Peers: ") + headerValStyle.Render(fmt.Sprintf("%d", status.PeerCount)),
		headerKeyStyle.Render("Uptime: ") + headerValStyle.Render(status.Uptime),
	}

	content := parts[0]
	for i := 1; i < len(parts); i++ {
		content += headerSep.String() + parts[i]
	}

	return headerStyle.Width(width).Render(content)
}
