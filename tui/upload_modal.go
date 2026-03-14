package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Faizan2005/DFS-Go/tui/components"
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
)

const (
	fieldName      = 0
	fieldPath      = 1
	fieldPublic    = 2
	fieldShareWith = 3
	fieldCount     = 4
)

// UploadModal is the Ctrl+U upload overlay.
type UploadModal struct {
	Active     bool
	ActiveField int

	Name      string
	FilePath  string
	Public    bool
	ShareWith string

	ErrMsg string
}

// Update handles key events when the modal is active.
func (u *UploadModal) Update(msg tea.KeyMsg) tea.Cmd {
	// Escape closes modal
	if key.Matches(msg, key.NewBinding(key.WithKeys("esc"))) {
		u.Close()
		return nil
	}

	// Tab / Shift+Tab navigate fields
	if key.Matches(msg, key.NewBinding(key.WithKeys("tab"))) {
		u.ActiveField = (u.ActiveField + 1) % fieldCount
		return nil
	}
	if key.Matches(msg, key.NewBinding(key.WithKeys("shift+tab"))) {
		u.ActiveField = (u.ActiveField + fieldCount - 1) % fieldCount
		return nil
	}

	// Enter on toggle = toggle; Enter elsewhere = submit
	if key.Matches(msg, key.NewBinding(key.WithKeys("enter"))) {
		if u.ActiveField == fieldPublic {
			u.Public = !u.Public
			return nil
		}
		return u.submit()
	}

	// Space on toggle
	if u.ActiveField == fieldPublic && key.Matches(msg, key.NewBinding(key.WithKeys(" "))) {
		u.Public = !u.Public
		return nil
	}

	// Text input for text fields
	switch u.ActiveField {
	case fieldName:
		u.Name = handleTextInput(u.Name, msg)
	case fieldPath:
		u.FilePath = handleTextInput(u.FilePath, msg)
	case fieldShareWith:
		u.ShareWith = handleTextInput(u.ShareWith, msg)
	}

	return nil
}

func (u *UploadModal) submit() tea.Cmd {
	u.ErrMsg = ""

	name := strings.TrimSpace(sanitizeInput(u.Name))
	path := strings.TrimSpace(sanitizeInput(u.FilePath))

	if name == "" {
		u.ErrMsg = "Name is required"
		return nil
	}
	if path == "" {
		u.ErrMsg = "File path is required"
		return nil
	}

	path = expandHome(path)
	path = filepath.Clean(path)

	// Check if path exists
	info, err := os.Stat(path)
	if err != nil {
		u.ErrMsg = fmt.Sprintf("Path error: %v", err)
		return nil
	}

	isDir := info.IsDir()

	// Auto-append the source file's extension to the name if the user
	// didn't include one. This ensures cross-platform downloads retain the
	// correct file type (Windows/macOS rely on extensions).
	if !isDir && filepath.Ext(name) == "" {
		if ext := filepath.Ext(path); ext != "" {
			name += ext
		}
	}

	shareWith := strings.TrimSpace(u.ShareWith)
	public := u.Public

	// Close modal immediately
	u.Close()

	return func() tea.Msg {
		return UploadRequestMsg{
			Name:      name,
			FilePath:  path,
			IsDir:     isDir,
			Public:    public,
			ShareWith: shareWith,
		}
	}
}

// Close resets and hides the modal.
func (u *UploadModal) Close() {
	u.Active = false
	u.ActiveField = fieldName
	u.Name = ""
	u.FilePath = ""
	u.Public = false
	u.ShareWith = ""
	u.ErrMsg = ""
}

// Open shows the modal.
func (u *UploadModal) Open() {
	u.Active = true
	u.ActiveField = fieldName
}

// View renders the modal.
func (u *UploadModal) View(termWidth, termHeight int) string {
	var lines []string

	lines = append(lines, components.RenderField("Name:", u.Name, u.ActiveField == fieldName))
	lines = append(lines, components.RenderField("Path:", u.FilePath, u.ActiveField == fieldPath))
	lines = append(lines, components.RenderToggle("Public:", u.Public, u.ActiveField == fieldPublic))
	lines = append(lines, components.RenderField("Share with:", u.ShareWith, u.ActiveField == fieldShareWith))

	if u.ErrMsg != "" {
		lines = append(lines, "\n"+ErrorStyle.Render(u.ErrMsg))
	}

	lines = append(lines, components.RenderModalHint("Tab: next field  Space: toggle  Enter: submit  Esc: cancel"))

	content := strings.Join(lines, "\n")
	return components.RenderModal("Upload File", content, termWidth, termHeight)
}

// UploadRequestMsg is sent when the user submits the upload form.
type UploadRequestMsg struct {
	Name      string
	FilePath  string
	IsDir     bool
	Public    bool
	ShareWith string
}

// UploadCompleteMsg is sent when a background upload finishes.
type UploadCompleteMsg struct {
	Name string
	Err  error
}

// expandHome replaces a leading ~ with the user's home directory.
func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") || path == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return filepath.Join(home, path[1:])
	}
	return path
}

func handleTextInput(current string, msg tea.KeyMsg) string {
	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("backspace"))):
		if len(current) > 0 {
			return current[:len(current)-1]
		}
	default:
		// Handle pasted text (bracketed paste delivers all runes at once).
		if msg.Paste && len(msg.Runes) > 0 {
			pasted := sanitizeInput(string(msg.Runes))
			return current + pasted
		}
		r := msg.String()
		if len(r) == 1 {
			return current + r
		}
	}
	return current
}

// sanitizeInput strips control characters and invisible Unicode that cause
// os.Stat "invalid argument" errors (common with Windows clipboard paste).
func sanitizeInput(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\t' { // strip \r \n \x00 etc.
			return -1
		}
		if r == 0x7F { // DEL
			return -1
		}
		if r == 0xFEFF { // BOM
			return -1
		}
		return r
	}, s)
}
