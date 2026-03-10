package components

import (
	"github.com/charmbracelet/lipgloss"
)

var (
	modalOverlayStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("#7D56F4")).
				Padding(1, 2).
				Width(60)

	modalTitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#7D56F4")).
			MarginBottom(1)

	modalLabelStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#CCCCCC")).
			Width(12)

	modalActiveFieldStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#FFFFFF")).
				Background(lipgloss.Color("#333355")).
				Padding(0, 1)

	modalFieldStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#888888")).
			Padding(0, 1)

	modalHintStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#555555")).
			Italic(true).
			MarginTop(1)
)

// RenderModal renders a centered modal overlay with the given content.
func RenderModal(title, content string, termWidth, termHeight int) string {
	inner := modalTitleStyle.Render(title) + "\n" + content
	box := modalOverlayStyle.Render(inner)

	// Center vertically and horizontally
	boxW := lipgloss.Width(box)
	boxH := lipgloss.Height(box)

	padLeft := 0
	if termWidth > boxW {
		padLeft = (termWidth - boxW) / 2
	}
	padTop := 0
	if termHeight > boxH {
		padTop = (termHeight - boxH) / 3
	}

	return lipgloss.NewStyle().
		MarginLeft(padLeft).
		MarginTop(padTop).
		Render(box)
}

// RenderField renders a single form field.
func RenderField(label, value string, active bool) string {
	l := modalLabelStyle.Render(label)
	v := value
	if v == "" {
		v = "(empty)"
	}
	if active {
		return l + modalActiveFieldStyle.Render(v+"_")
	}
	return l + modalFieldStyle.Render(v)
}

// RenderToggle renders a toggle field.
func RenderToggle(label string, on bool, active bool) string {
	l := modalLabelStyle.Render(label)
	val := "[ ] No"
	if on {
		val = "[✓] Yes"
	}
	if active {
		return l + modalActiveFieldStyle.Render(val)
	}
	return l + modalFieldStyle.Render(val)
}

// RenderModalHint renders the hint line at the bottom.
func RenderModalHint(hint string) string {
	return modalHintStyle.Render(hint)
}
