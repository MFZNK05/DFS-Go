package tabs

import (
	"fmt"
	"strings"
	"time"

	"github.com/Faizan2005/DFS-Go/ipc"
	"github.com/Faizan2005/DFS-Go/tui/components"
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	subViewShares    = 0
	subViewDownloads = 1
)

const (
	filterAll     = 0
	filterPublic  = 1
	filterPrivate = 2
)

var filterLabels = []string{"All", "Public", "Private"}

var (
	vaultTitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#7D56F4")).
			MarginBottom(0)

	vaultSubTabActive = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("#FFFFFF")).
				Background(lipgloss.Color("#7D56F4")).
				Padding(0, 1)

	vaultSubTabInactive = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#888888")).
				Padding(0, 1)

	vaultFilterStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#FFAA00")).
				MarginLeft(2)
)

// VaultTab shows uploads (shares) and downloads history.
type VaultTab struct {
	Width  int
	Height int

	subView int
	filter  int

	uploads   []ipc.UploadEntry
	downloads []ipc.DownloadEntry

	sharesTable    components.Table
	downloadsTable components.Table

	// Confirm delete state
	confirmDelete bool
	deleteKey     string
	deleteName    string

	// Send to peer state
	sendingFile   bool
	sendFileKey   string
	sendFileName  string
	sendPeerInput string
}

func NewVaultTab() *VaultTab {
	shareCols := []components.Column{
		{Title: "Name", Width: 22},
		{Title: "Size", Width: 10},
		{Title: "Type", Width: 6},
		{Title: "Enc", Width: 5},
		{Title: "Pub", Width: 5},
		{Title: "Uploaded", Width: 20},
	}
	dlCols := []components.Column{
		{Title: "Name", Width: 22},
		{Title: "Size", Width: 10},
		{Title: "Type", Width: 6},
		{Title: "Enc", Width: 5},
		{Title: "Downloaded", Width: 20},
		{Title: "Output", Width: 24},
	}
	return &VaultTab{
		sharesTable:    components.NewTable(shareCols, "No uploads yet"),
		downloadsTable: components.NewTable(dlCols, "No downloads yet"),
	}
}

func (v *VaultTab) SetSize(w, h int) {
	v.Width = w
	v.Height = h
	v.sharesTable.SetSize(w-4, h-6)
	v.downloadsTable.SetSize(w-4, h-6)
}

// SetUploads updates the shares data.
func (v *VaultTab) SetUploads(uploads []ipc.UploadEntry) {
	v.uploads = uploads
	v.rebuildSharesTable()
}

// SetDownloads updates the downloads data.
func (v *VaultTab) SetDownloads(downloads []ipc.DownloadEntry) {
	v.downloads = downloads
	v.rebuildDownloadsTable()
}

func (v *VaultTab) rebuildSharesTable() {
	var rows [][]string
	for _, u := range v.uploads {
		if v.filter == filterPublic && !u.Public {
			continue
		}
		if v.filter == filterPrivate && u.Public {
			continue
		}
		typ := "file"
		if u.IsDir {
			typ = "dir"
		}
		enc := "no"
		if u.Encrypted {
			enc = "yes"
		}
		pub := "no"
		if u.Public {
			pub = "yes"
		}
		ts := time.Unix(0, u.Timestamp).Format("2006-01-02 15:04")
		rows = append(rows, []string{u.Name, formatSize(u.Size), typ, enc, pub, ts})
	}
	v.sharesTable.SetRows(rows)
}

func (v *VaultTab) rebuildDownloadsTable() {
	rows := make([][]string, len(v.downloads))
	for i, d := range v.downloads {
		typ := "file"
		if d.IsDir {
			typ = "dir"
		}
		enc := "no"
		if d.Encrypted {
			enc = "yes"
		}
		ts := time.Unix(0, d.Timestamp).Format("2006-01-02 15:04")
		rows[i] = []string{d.Name, formatSize(d.Size), typ, enc, ts, d.OutputPath}
	}
	v.downloadsTable.SetRows(rows)
}

// Update handles key events for the vault tab.
func (v *VaultTab) Update(msg tea.KeyMsg) tea.Cmd {
	// Send-to-peer input mode
	if v.sendingFile {
		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
			v.sendingFile = false
			v.sendPeerInput = ""
		case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
			v.sendingFile = false
			raw := v.sendPeerInput
			fileKey := v.sendFileKey
			fileName := v.sendFileName
			v.sendPeerInput = ""
			var addrs []string
			for _, part := range strings.Split(raw, ",") {
				a := strings.TrimSpace(part)
				if a != "" {
					addrs = append(addrs, a)
				}
			}
			if len(addrs) > 0 {
				return func() tea.Msg {
					return VaultSendMsg{FileKey: fileKey, FileName: fileName, PeerAddrs: addrs}
				}
			}
		case key.Matches(msg, key.NewBinding(key.WithKeys("backspace"))):
			if len(v.sendPeerInput) > 0 {
				v.sendPeerInput = v.sendPeerInput[:len(v.sendPeerInput)-1]
			}
		default:
			if msg.Paste && len(msg.Runes) > 0 {
				pasted := strings.ReplaceAll(string(msg.Runes), "\n", "")
				v.sendPeerInput += pasted
			} else {
				r := msg.String()
				if len(r) == 1 {
					v.sendPeerInput += r
				}
			}
		}
		return nil
	}

	// Confirm delete modal
	if v.confirmDelete {
		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("y", "enter"))):
			v.confirmDelete = false
			deleteKey := v.deleteKey
			return func() tea.Msg {
				return VaultDeleteMsg{Key: deleteKey}
			}
		case key.Matches(msg, key.NewBinding(key.WithKeys("n", "esc"))):
			v.confirmDelete = false
		}
		return nil
	}

	// Sub-view switching
	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("a"))):
		v.subView = subViewShares
		return nil
	case key.Matches(msg, key.NewBinding(key.WithKeys("b"))):
		v.subView = subViewDownloads
		return nil
	}

	switch v.subView {
	case subViewShares:
		// Filter toggle
		if key.Matches(msg, key.NewBinding(key.WithKeys("f"))) {
			v.filter = (v.filter + 1) % 3
			v.rebuildSharesTable()
			return nil
		}
		// Delete
		if key.Matches(msg, key.NewBinding(key.WithKeys("d"))) {
			idx := v.sharesTable.SelectedRow()
			if idx >= 0 && idx < len(v.uploads) {
				v.confirmDelete = true
				v.deleteKey = v.uploads[idx].Key
				v.deleteName = v.uploads[idx].Name
			}
			return nil
		}
		// Send to peer
		if key.Matches(msg, key.NewBinding(key.WithKeys("s"))) {
			idx := v.sharesTable.SelectedRow()
			if idx >= 0 && idx < len(v.uploads) {
				v.sendingFile = true
				v.sendFileKey = v.uploads[idx].Key
				v.sendFileName = v.uploads[idx].Name
				v.sendPeerInput = ""
			}
			return nil
		}
		return v.sharesTable.Update(msg)
	default:
		return v.downloadsTable.Update(msg)
	}
}

// View renders the vault tab.
func (v *VaultTab) View() string {
	// Sub-tab bar
	sharesLabel := vaultSubTabInactive.Render("[a] My Shares")
	dlLabel := vaultSubTabInactive.Render("[b] My Downloads")
	switch v.subView {
	case subViewShares:
		sharesLabel = vaultSubTabActive.Render("[a] My Shares")
	case subViewDownloads:
		dlLabel = vaultSubTabActive.Render("[b] My Downloads")
	}
	subBar := lipgloss.JoinHorizontal(lipgloss.Top, sharesLabel, "  ", dlLabel)

	if v.subView == subViewShares {
		filterInfo := vaultFilterStyle.Render(fmt.Sprintf("Filter: %s", filterLabels[v.filter]))
		subBar += filterInfo
	}

	var tableView string
	switch v.subView {
	case subViewShares:
		tableView = v.sharesTable.View()
	default:
		tableView = v.downloadsTable.View()
	}

	// Send to peer overlay
	if v.sendingFile {
		sendPrompt := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#7D56F4")).
			Padding(1, 2).
			Render(fmt.Sprintf("Send \"%s\" to peer(s)\n\n  Address: %s_\n  (comma-separated for multiple)\n\n  Enter: send   Esc: cancel", v.sendFileName, v.sendPeerInput))
		tableView = sendPrompt + "\n\n" + tableView
	}

	// Confirm delete overlay
	if v.confirmDelete {
		confirm := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#FF4444")).
			Padding(1, 2).
			Render(fmt.Sprintf("Delete \"%s\"?\n\n  [y] Yes   [n] No", v.deleteName))
		tableView = confirm + "\n\n" + tableView
	}

	return subBar + "\n" + tableView
}

func (v *VaultTab) FooterHints() []components.FooterHint {
	hints := []components.FooterHint{
		{Key: "a/b", Desc: "Switch view"},
	}
	if v.subView == subViewShares {
		hints = append(hints,
			components.FooterHint{Key: "f", Desc: "Filter"},
			components.FooterHint{Key: "d", Desc: "Delete"},
			components.FooterHint{Key: "s", Desc: "Send to peer"},
		)
	}
	hints = append(hints,
		components.FooterHint{Key: "j/k", Desc: "Navigate"},
		components.FooterHint{Key: "/", Desc: "Search"},
		components.FooterHint{Key: "G/g", Desc: "End/Start"},
		components.FooterHint{Key: "1-4", Desc: "Switch tab"},
		components.FooterHint{Key: "q", Desc: "Quit"},
	)
	return hints
}

// IsInputMode returns true when the vault tab is capturing text input.
func (v VaultTab) IsInputMode() bool {
	return v.sendingFile || v.sharesTable.Filtering || v.downloadsTable.Filtering
}

// VaultDeleteMsg is sent when user confirms deletion.
type VaultDeleteMsg struct {
	Key string
}

// VaultSendMsg is sent when the user confirms sending a file to peer(s).
type VaultSendMsg struct {
	FileKey   string
	FileName  string
	PeerAddrs []string
}
