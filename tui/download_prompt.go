package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// DownloadPrompt is a simple overlay asking for an output path.
type DownloadPrompt struct {
	Active    bool
	Key       string
	Name      string
	FromKey   string
	OutputDir string
	ErrMsg    string
}

func (d *DownloadPrompt) Open(fileKey, fileName, fromKey string) {
	d.Active = true
	d.Key = fileKey
	d.Name = fileName
	d.FromKey = fromKey
	d.OutputDir = ""
	d.ErrMsg = ""
}

func (d *DownloadPrompt) Close() {
	d.Active = false
	d.Key = ""
	d.Name = ""
	d.FromKey = ""
	d.OutputDir = ""
	d.ErrMsg = ""
}

func (d *DownloadPrompt) Update(msg tea.KeyMsg) tea.Cmd {
	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
		d.Close()
		return nil
	case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
		outDir := strings.TrimSpace(sanitizeInput(d.OutputDir))
		if outDir == "" {
			outDir = "."
		}
		outDir = expandHome(outDir)
		fileKey := d.Key
		fileName := d.Name
		fromKey := d.FromKey
		d.Close()
		return func() tea.Msg {
			return DownloadRequestMsg{
				Key:       fileKey,
				Name:      fileName,
				FromKey:   fromKey,
				OutputDir: outDir,
			}
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("backspace"))):
		if len(d.OutputDir) > 0 {
			d.OutputDir = d.OutputDir[:len(d.OutputDir)-1]
		}
	default:
		if msg.Paste && len(msg.Runes) > 0 {
			pasted := sanitizeInput(string(msg.Runes))
			d.OutputDir += pasted
		} else {
			r := msg.String()
			if len(r) == 1 {
				d.OutputDir += r
			}
		}
	}
	return nil
}

func (d *DownloadPrompt) View(termWidth, termHeight int) string {
	var b strings.Builder

	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7D56F4"))
	labelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#CCCCCC"))
	inputStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#FFFFFF")).Background(lipgloss.Color("#333355")).Padding(0, 1)
	hintStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#555555")).Italic(true)

	b.WriteString(titleStyle.Render("Download: "+d.Name) + "\n\n")
	b.WriteString(labelStyle.Render("Output dir: ") + inputStyle.Render(d.OutputDir+"_") + "\n")

	if d.ErrMsg != "" {
		b.WriteString("\n" + ErrorStyle.Render(d.ErrMsg) + "\n")
	}

	b.WriteString("\n" + hintStyle.Render("Enter: download  Esc: cancel  (empty = current dir)"))

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#7D56F4")).
		Padding(1, 2).
		Width(50).
		Render(b.String())

	// Center
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

// DownloadRequestMsg is sent when user confirms download.
type DownloadRequestMsg struct {
	Key       string
	Name      string
	FromKey   string
	OutputDir string
}

// DownloadCompleteMsg is sent when background download finishes.
type DownloadCompleteMsg struct {
	Name string
	Err  error
}
