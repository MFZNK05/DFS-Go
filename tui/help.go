package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

type helpSection struct {
	Title string
	Binds []helpBind
}

type helpBind struct {
	Key  string
	Desc string
}

var helpSections = []helpSection{
	{
		Title: "Global",
		Binds: []helpBind{
			{"1-4", "Switch tab"},
			{"Tab/Shift+Tab", "Next/prev tab"},
			{"Ctrl+U", "Upload file/dir"},
			{"?", "Toggle help"},
			{"q / Ctrl+C", "Quit"},
		},
	},
	{
		Title: "Navigation (all tables)",
		Binds: []helpBind{
			{"j/k or arrows", "Move up/down"},
			{"g / G", "Jump to top/bottom"},
			{"/", "Search/filter"},
			{"Esc", "Clear filter"},
		},
	},
	{
		Title: "Network Tab",
		Binds: []helpBind{
			{"Tab", "Switch pane (Peers / Results)"},
			{"/", "Search files across network"},
			{"Enter", "Browse selected peer"},
			{"a", "Add peer (host:port)"},
			{"x", "Disconnect peer"},
			{"d", "Download selected file"},
		},
	},
	{
		Title: "Transfers Tab",
		Binds: []helpBind{
			{"p", "Pause transfer"},
			{"r", "Resume transfer"},
			{"x", "Cancel transfer"},
		},
	},
	{
		Title: "Vault Tab",
		Binds: []helpBind{
			{"a / b / c", "Switch Shares/Downloads/Inbox"},
			{"f", "Cycle filter (All/Public/Private)"},
			{"d", "Delete file / Download inbox item"},
			{"s", "Send file to peer (by name or addr)"},
		},
	},
	{
		Title: "Upload Modal",
		Binds: []helpBind{
			{"Tab/Shift+Tab", "Next/prev field"},
			{"Space", "Toggle public flag"},
			{"Enter", "Submit"},
			{"Esc", "Cancel"},
		},
	},
}

func renderHelp(termWidth, termHeight int) string {
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7D56F4")).MarginBottom(1)
	sectionStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFAA00"))
	keyStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#FFFFFF")).Bold(true).Width(18)
	descStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#CCCCCC"))
	hintStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#555555")).Italic(true)

	var b strings.Builder
	b.WriteString(titleStyle.Render("Keyboard Shortcuts") + "\n")

	for i, sec := range helpSections {
		b.WriteString(sectionStyle.Render(sec.Title) + "\n")
		for _, bind := range sec.Binds {
			b.WriteString("  " + keyStyle.Render(bind.Key) + descStyle.Render(bind.Desc) + "\n")
		}
		if i < len(helpSections)-1 {
			b.WriteString("\n")
		}
	}

	b.WriteString("\n" + hintStyle.Render("Press ? or Esc to close"))

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#7D56F4")).
		Padding(1, 3).
		Width(50).
		Render(b.String())

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

	return lipgloss.NewStyle().MarginLeft(padLeft).MarginTop(padTop).Render(box)
}
