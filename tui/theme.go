package tui

import "github.com/charmbracelet/lipgloss"

// Colors used throughout the TUI.
var (
	ColorActive  = lipgloss.Color("#00FF00")
	ColorPaused  = lipgloss.Color("#FFAA00")
	ColorFailed  = lipgloss.Color("#FF4444")
	ColorPending = lipgloss.Color("#888888")
	ColorAccent  = lipgloss.Color("#7D56F4")
	ColorDim     = lipgloss.Color("#555555")
	ColorWhite   = lipgloss.Color("#FFFFFF")
	ColorBorder  = lipgloss.Color("#444444")
)

// Styles for layout components.
var (
	HeaderStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(ColorWhite).
			Background(lipgloss.Color("#1a1a2e")).
			Padding(0, 1)

	TabActiveStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(ColorAccent).
			Border(lipgloss.NormalBorder(), false, false, true, false).
			BorderForeground(ColorAccent).
			Padding(0, 1)

	TabInactiveStyle = lipgloss.NewStyle().
				Foreground(ColorDim).
				Padding(0, 1)

	FooterStyle = lipgloss.NewStyle().
			Foreground(ColorDim).
			Padding(0, 1)

	StatusCardStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(ColorAccent).
			Padding(1, 2)

	StatusKeyStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(ColorAccent).
			Width(14)

	StatusValStyle = lipgloss.NewStyle().
			Foreground(ColorWhite)

	TableHeaderStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(ColorAccent).
				Border(lipgloss.NormalBorder(), false, false, true, false).
				BorderForeground(ColorBorder)

	TableSelectedStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(ColorWhite).
				Background(lipgloss.Color("#333355"))

	TableRowStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#CCCCCC"))

	ModalStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(ColorAccent).
			Padding(1, 2).
			Width(60)

	ErrorStyle = lipgloss.NewStyle().
			Foreground(ColorFailed).
			Bold(true)

	PlaceholderStyle = lipgloss.NewStyle().
				Foreground(ColorDim).
				Italic(true).
				Align(lipgloss.Center)
)
