package tabs

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Faizan2005/DFS-Go/ipc"
	"github.com/Faizan2005/DFS-Go/tui/components"
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// focus tracks which pane has keyboard focus.
type focus int

const (
	focusPeers   focus = 0
	focusResults focus = 1
)

var (
	paneStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#444444")).
			Padding(0, 1)

	paneFocusedStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("#7D56F4")).
				Padding(0, 1)

	paneTitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#7D56F4")).
			MarginBottom(0)
)

// NetworkTab implements the Network tab with peer roster and browse.
type NetworkTab struct {
	Width  int
	Height int

	focus focus

	peers       []ipc.PeerInfo
	peerTable   components.Table
	browseFiles []ipc.BrowseEntry
	resultTable components.Table

	browsingPeer string

	// Add peer state
	addingPeer   bool
	addPeerInput string

	// Confirm disconnect state
	confirmDisconnect bool
	disconnectAddr    string
	disconnectAlias   string
}

func NewNetworkTab() *NetworkTab {
	peerCols := []components.Column{
		{Title: "Alias", Width: 10},
		{Title: "Address", Width: 18},
		{Title: "State", Width: 6},
	}
	resultCols := []components.Column{
		{Title: "Name", Width: 28},
		{Title: "Size", Width: 10},
		{Title: "Type", Width: 5},
		{Title: "Label", Width: 8},
	}
	return &NetworkTab{
		peerTable:   components.NewTable(peerCols, "No peers connected"),
		resultTable: components.NewTable(resultCols, "Select a peer and press Enter to browse"),
	}
}

func (n *NetworkTab) SetSize(w, h int) {
	n.Width = w
	n.Height = h
	peerW := w*25/100 - 4
	resultW := w*75/100 - 4
	n.peerTable.SetSize(peerW, h-4)
	n.resultTable.SetSize(resultW, h-4)
}

// SetPeers updates the peer list.
func (n *NetworkTab) SetPeers(peers []ipc.PeerInfo) {
	n.peers = peers
	rows := make([][]string, len(peers))
	for i, p := range peers {
		rows[i] = []string{p.Alias, p.Addr, p.State}
	}
	n.peerTable.SetRows(rows)
}

// SetBrowseResults updates the results pane with browse results (latest first).
func (n *NetworkTab) SetBrowseResults(peerAddr string, files []ipc.BrowseEntry) {
	// Sort by timestamp descending (newest first).
	sort.SliceStable(files, func(i, j int) bool {
		return files[i].Timestamp > files[j].Timestamp
	})

	n.browseFiles = files
	rows := make([][]string, len(files))
	for i, f := range files {
		typ := "file"
		if f.IsDir {
			typ = "dir"
		}
		label := strings.ToUpper(f.Label)
		rows[i] = []string{f.Name, formatSize(f.Size), typ, label}
	}

	n.browsingPeer = peerAddr
	n.resultTable.SetRows(rows)
}

// Update handles input for the network tab. Returns a command if IPC action needed.
func (n *NetworkTab) Update(msg tea.KeyMsg) tea.Cmd {
	// Handle add-peer input mode
	if n.addingPeer {
		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("esc"))):
			n.addingPeer = false
			n.addPeerInput = ""
		case key.Matches(msg, key.NewBinding(key.WithKeys("enter"))):
			n.addingPeer = false
			addr := n.addPeerInput
			n.addPeerInput = ""
			if addr != "" {
				return func() tea.Msg {
					return NetworkConnectMsg{Addr: addr}
				}
			}
		case key.Matches(msg, key.NewBinding(key.WithKeys("backspace"))):
			if len(n.addPeerInput) > 0 {
				n.addPeerInput = n.addPeerInput[:len(n.addPeerInput)-1]
			}
		default:
			if msg.Paste && len(msg.Runes) > 0 {
				pasted := strings.ReplaceAll(string(msg.Runes), "\n", "")
				n.addPeerInput += pasted
			} else {
				r := msg.String()
				if len(r) == 1 {
					n.addPeerInput += r
				}
			}
		}
		return nil
	}

	// Handle disconnect confirmation
	if n.confirmDisconnect {
		switch {
		case key.Matches(msg, key.NewBinding(key.WithKeys("y", "enter"))):
			n.confirmDisconnect = false
			addr := n.disconnectAddr
			return func() tea.Msg {
				return NetworkDisconnectMsg{Addr: addr}
			}
		case key.Matches(msg, key.NewBinding(key.WithKeys("n", "esc"))):
			n.confirmDisconnect = false
		}
		return nil
	}

	// Tab between panes
	if key.Matches(msg, key.NewBinding(key.WithKeys("tab"))) {
		if n.focus == focusPeers {
			n.focus = focusResults
		} else {
			n.focus = focusPeers
		}
		return nil
	}

	// Download from results pane (browse results)
	if n.focus == focusResults && key.Matches(msg, key.NewBinding(key.WithKeys("d"))) {
		idx := n.resultTable.SelectedRow()
		if idx >= 0 && idx < len(n.browseFiles) {
			f := n.browseFiles[idx]
			if f.Key == "" {
				return nil // separator row
			}
			fromKey := ""
			if i := strings.Index(f.Key, "/"); i >= 0 {
				fromKey = f.Key[:i]
			}
			return func() tea.Msg {
				return NetworkDownloadMsg{Key: f.Key, Name: f.Name, FromKey: fromKey}
			}
		}
		return nil
	}

	// Add peer (peers pane)
	if n.focus == focusPeers && key.Matches(msg, key.NewBinding(key.WithKeys("a"))) {
		n.addingPeer = true
		n.addPeerInput = ""
		return nil
	}

	// Disconnect peer (peers pane) — show confirmation first
	if n.focus == focusPeers && key.Matches(msg, key.NewBinding(key.WithKeys("x"))) {
		idx := n.peerTable.SelectedRow()
		if idx >= 0 && idx < len(n.peers) {
			n.confirmDisconnect = true
			n.disconnectAddr = n.peers[idx].Addr
			n.disconnectAlias = n.peers[idx].Alias
		}
		return nil
	}

	// Enter on peer → browse
	if n.focus == focusPeers && key.Matches(msg, key.NewBinding(key.WithKeys("enter"))) {
		idx := n.peerTable.SelectedRow()
		if idx >= 0 && idx < len(n.peers) {
			addr := n.peers[idx].Addr
			return func() tea.Msg {
				return NetworkBrowseMsg{PeerAddr: addr}
			}
		}
		return nil
	}

	// Dispatch to focused table
	if n.focus == focusPeers {
		return n.peerTable.Update(msg)
	}
	return n.resultTable.Update(msg)
}

// View renders the network tab.
func (n NetworkTab) View() string {
	peerW := n.Width*25/100 - 4
	resultW := n.Width*75/100 - 4

	// Left pane: peers
	leftStyle := paneStyle.Width(peerW)
	if n.focus == focusPeers {
		leftStyle = paneFocusedStyle.Width(peerW)
	}
	var addPeerLine string
	if n.addingPeer {
		addPeerLine = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFAA00")).
			Render("add peer: "+n.addPeerInput+"_") + "\n"
	}
	var confirmLine string
	if n.confirmDisconnect {
		label := n.disconnectAlias
		if label == "" {
			label = n.disconnectAddr
		}
		confirmLine = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#FF4444")).
			Padding(1, 2).
			Render(fmt.Sprintf("Disconnect peer \"%s\"?\n\n  [y] Yes   [n] No", label)) + "\n"
	}
	leftContent := paneTitleStyle.Render("Peers") + "\n" + addPeerLine + confirmLine + n.peerTable.View()
	left := leftStyle.Render(leftContent)

	// Right pane: browse results
	rightStyle := paneStyle.Width(resultW)
	if n.focus == focusResults {
		rightStyle = paneFocusedStyle.Width(resultW)
	}

	rightTitle := "Files"
	if n.browsingPeer != "" {
		rightTitle = fmt.Sprintf("Browse: %s", n.browsingPeer)
	}

	rightContent := paneTitleStyle.Render(rightTitle) + "\n" + n.resultTable.View()
	right := rightStyle.Render(rightContent)

	return lipgloss.JoinHorizontal(lipgloss.Top, left, right)
}

// IsInputMode returns true when the network tab is capturing text input.
func (n NetworkTab) IsInputMode() bool {
	return n.addingPeer || n.confirmDisconnect || n.peerTable.Filtering || n.resultTable.Filtering
}

func (n NetworkTab) FooterHints() []components.FooterHint {
	hints := []components.FooterHint{
		{Key: "Tab", Desc: "Switch pane"},
	}
	if n.focus == focusPeers {
		hints = append(hints,
			components.FooterHint{Key: "Enter", Desc: "Browse peer"},
			components.FooterHint{Key: "a", Desc: "Add peer"},
			components.FooterHint{Key: "x", Desc: "Disconnect"},
		)
	} else {
		hints = append(hints, components.FooterHint{Key: "d", Desc: "Download"})
	}
	hints = append(hints,
		components.FooterHint{Key: "j/k", Desc: "Navigate"},
		components.FooterHint{Key: "1-4", Desc: "Switch tab"},
		components.FooterHint{Key: "q", Desc: "Quit"},
	)
	return hints
}

// NetworkBrowseMsg triggers a peer browse from the network tab.
type NetworkBrowseMsg struct {
	PeerAddr string
}

// NetworkDownloadMsg triggers a download from a browse result.
type NetworkDownloadMsg struct {
	Key     string
	Name    string
	FromKey string // owner fingerprint for cross-user downloads
}

// NetworkConnectMsg triggers a peer connection.
type NetworkConnectMsg struct {
	Addr string
}

// NetworkDisconnectMsg triggers a peer disconnection.
type NetworkDisconnectMsg struct {
	Addr string
}

func formatSize(bytes int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)
	switch {
	case bytes >= GB:
		return fmt.Sprintf("%.1f GB", float64(bytes)/float64(GB))
	case bytes >= MB:
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(MB))
	case bytes >= KB:
		return fmt.Sprintf("%.1f KB", float64(bytes)/float64(KB))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}
