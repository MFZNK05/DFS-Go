package components

import (
	"github.com/charmbracelet/lipgloss"
)

var (
	footerStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#555555")).
			Padding(0, 1)

	footerKeyStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#7D56F4"))

	footerDescStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#888888"))
)

// FooterHint is a key-description pair for the footer.
type FooterHint struct {
	Key  string
	Desc string
}

// globalHints are always shown at the end of the footer.
var globalHints = []FooterHint{
	{Key: "^U", Desc: "Upload"},
	{Key: "?", Desc: "Help"},
}

// RenderFooter renders contextual keybind hints plus global hints.
func RenderFooter(hints []FooterHint, width int) string {
	all := append(hints, globalHints...)
	content := ""
	for i, h := range all {
		if i > 0 {
			content += "  "
		}
		content += footerKeyStyle.Render(h.Key) + " " + footerDescStyle.Render(h.Desc)
	}
	return footerStyle.Width(width).Render(content)
}
