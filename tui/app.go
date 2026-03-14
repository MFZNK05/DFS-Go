package tui

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/Faizan2005/DFS-Go/ipc"
	"github.com/Faizan2005/DFS-Go/tui/components"
	"github.com/Faizan2005/DFS-Go/tui/tabs"
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// UploadFunc is the signature for the upload handler injected from cmd/.
type UploadFunc func(name, path, shareWith, shareWithKey, sockPath string, public bool) error

// UploadDirFunc is the signature for the directory upload handler.
type UploadDirFunc func(name, dirPath, shareWith, shareWithKey, sockPath string, public bool) error

// DownloadFunc is the signature for the download handler injected from cmd/.
type DownloadFunc func(name, outputPath, fromAlias, fromKey, sockPath string) error

// Handlers holds the injected upload/download functions to avoid import cycles.
type Handlers struct {
	UploadFile      UploadFunc
	UploadDirectory UploadDirFunc
	DownloadFile    DownloadFunc
}

const (
	tabNetwork     = 0
	tabTransfers   = 1
	tabVault       = 2
	tabDiagnostics = 3
)

var tabNames = []string{"Network", "Transfers", "Vault", "Diagnostics"}

// RootModel is the top-level Bubble Tea model.
type RootModel struct {
	client   *DaemonClient
	sockPath string
	handlers Handlers

	activeTab int
	prevTab   int
	width     int
	height    int

	status ipc.NodeStatusInfo
	err    string

	network     *tabs.NetworkTab
	transfers   *tabs.TransfersTab
	vault       *tabs.VaultTab
	diagnostics *tabs.DiagnosticsTab

	uploadModal    UploadModal
	downloadPrompt DownloadPrompt
	showHelp       bool

	pendingUpload   string // name of in-progress upload, "" if none
	pendingDownload string // name of in-progress download, "" if none

	lastTransfers []ipc.TransferInfo // cached for footer status line
}

// NewRootModel creates the root model.
func NewRootModel(sockPath string, handlers Handlers) RootModel {
	return RootModel{
		client:      NewDaemonClient(sockPath),
		sockPath:    sockPath,
		handlers:    handlers,
		activeTab:   tabNetwork,
		prevTab:     -1,
		network:     tabs.NewNetworkTab(),
		transfers:   tabs.NewTransfersTab(),
		vault:       tabs.NewVaultTab(),
		diagnostics: tabs.NewDiagnosticsTab(),
	}
}

// Run starts the TUI.
func Run(sockPath string, handlers Handlers) error {
	p := tea.NewProgram(
		NewRootModel(sockPath, handlers),
		tea.WithAltScreen(),
	)
	_, err := p.Run()
	return err
}

func (m RootModel) Init() tea.Cmd {
	return tea.Batch(
		fetchStatus(m.client),
		pollCmd(5*time.Second),
	)
}

func (m RootModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		contentH := m.height - 4
		m.network.SetSize(m.width, contentH)
		m.transfers.SetSize(m.width, contentH)
		m.vault.SetSize(m.width, contentH)
		m.diagnostics.SetSize(m.width, contentH)
		return m, nil

	case tea.KeyMsg:
		// Help overlay captures all input when active
		if m.showHelp {
			if msg.String() == "?" || msg.String() == "esc" {
				m.showHelp = false
			}
			return m, nil
		}
		// Modals capture all input when active
		if m.uploadModal.Active {
			cmd := m.uploadModal.Update(msg)
			return m, cmd
		}
		if m.downloadPrompt.Active {
			cmd := m.downloadPrompt.Update(msg)
			return m, cmd
		}
		return m.handleKey(msg)

	// ── Status ──
	case StatusUpdatedMsg:
		if msg.Err != nil {
			m.err = fmt.Sprintf("status: %v", msg.Err)
			m.status = ipc.NodeStatusInfo{Status: "disconnected"}
		} else {
			m.err = ""
			m.status = msg.Status
			m.diagnostics.Status = msg.Status
		}
		return m, nil

	// ── Network ──
	case PeersUpdatedMsg:
		if msg.Err != nil {
			m.err = fmt.Sprintf("peers: %v", msg.Err)
		} else {
			m.network.SetPeers(msg.Peers)
		}
		return m, nil

	case LANScanMsg:
		if msg.Err == nil {
			m.network.SetDiscovered(msg.Peers)
		}
		return m, nil

	case BrowseResultsMsg:
		if msg.Err != nil {
			return m, m.setError("browse: %v", msg.Err)
		}
		m.err = ""
		m.network.SetBrowseResults(msg.PeerAddr, msg.Files)
		return m, nil

	case tabs.NetworkBrowseMsg:
		return m, browsePeer(m.client, msg.PeerAddr)

	case tabs.NetworkDownloadMsg:
		m.downloadPrompt.Open(msg.Key, msg.Name, msg.FromKey)
		return m, nil

	case tabs.NetworkConnectMsg:
		return m, connectPeer(m.client, msg.Addr)

	case tabs.NetworkDisconnectMsg:
		return m, disconnectPeer(m.client, msg.Addr)

	case PeerActionMsg:
		if msg.Err != nil {
			cmd := m.setError("%s peer: %v", msg.Action, msg.Err)
			return m, tea.Batch(cmd, fetchPeers(m.client))
		}
		m.err = ""
		return m, fetchPeers(m.client)

	// ── Transfers ──
	case TransfersUpdatedMsg:
		if msg.Err != nil {
			m.err = fmt.Sprintf("transfers: %v", msg.Err)
		} else {
			m.transfers.SetTransfers(msg.Transfers)
			m.lastTransfers = msg.Transfers
		}
		return m, nil

	case tabs.TransferPauseMsg:
		return m, pauseTransfer(m.client, msg.ID)

	case tabs.TransferResumeMsg:
		return m, resumeTransfer(m.client, msg.ID)

	case tabs.TransferCancelMsg:
		return m, cancelTransfer(m.client, msg.ID)

	case TransferActionMsg:
		if msg.Err != nil {
			cmd := m.setError("%s: %v", msg.Action, msg.Err)
			return m, tea.Batch(cmd, fetchTransfers(m.client))
		}
		m.err = ""
		return m, fetchTransfers(m.client)

	// ── Vault ──
	case UploadsUpdatedMsg:
		if msg.Err != nil {
			m.err = fmt.Sprintf("uploads: %v", msg.Err)
		} else {
			m.vault.SetUploads(msg.Uploads)
		}
		return m, nil

	case DownloadsUpdatedMsg:
		if msg.Err != nil {
			m.err = fmt.Sprintf("downloads: %v", msg.Err)
		} else {
			m.vault.SetDownloads(msg.Downloads)
		}
		return m, nil

	case InboxUpdatedMsg:
		// Inbox is now shown in Network browse, not Vault.
		return m, nil

	case tabs.VaultDeleteMsg:
		return m, removeFile(m.client, msg.Key)

	case tabs.VaultSendMsg:
		var cmds []tea.Cmd
		for _, addr := range msg.PeerAddrs {
			cmds = append(cmds, sendToPeer(m.client, msg.FileKey, addr))
		}
		return m, tea.Batch(cmds...)

	case RemoveFileMsg:
		if msg.Err != nil {
			cmd := m.setError("delete: %v", msg.Err)
			return m, tea.Batch(cmd, fetchUploads(m.client), fetchDownloads(m.client))
		}
		m.err = ""
		return m, tea.Batch(fetchUploads(m.client), fetchDownloads(m.client))

	// ── Upload / Download ──
	case UploadRequestMsg:
		m.pendingUpload = msg.Name
		return m, doUpload(msg, m.sockPath, m.handlers)

	case UploadCompleteMsg:
		m.pendingUpload = ""
		m.activeTab = tabTransfers
		if msg.Err != nil {
			cmd := m.setError("upload '%s': %v", msg.Name, msg.Err)
			return m, tea.Batch(cmd, fetchTransfers(m.client), fetchUploads(m.client))
		}
		m.err = ""
		return m, tea.Batch(fetchTransfers(m.client), fetchUploads(m.client))

	case DownloadRequestMsg:
		m.pendingDownload = msg.Name
		return m, doDownload(msg, m.sockPath, m.handlers)

	case DownloadCompleteMsg:
		m.pendingDownload = ""
		m.activeTab = tabTransfers
		if msg.Err != nil {
			cmd := m.setError("download '%s': %v", msg.Name, msg.Err)
			return m, tea.Batch(cmd, fetchTransfers(m.client), fetchDownloads(m.client))
		}
		m.err = ""
		return m, tea.Batch(fetchTransfers(m.client), fetchDownloads(m.client))

	// ── Tick ──
	case TickMsg:
		var cmds []tea.Cmd
		cmds = append(cmds, fetchStatus(m.client))
		// Always fetch transfers so the footer status line stays current.
		cmds = append(cmds, fetchTransfers(m.client))
		switch m.activeTab {
		case tabNetwork:
			cmds = append(cmds, fetchPeers(m.client), fetchLANPeers(m.client))
		case tabVault:
			cmds = append(cmds, fetchUploads(m.client), fetchDownloads(m.client))
		}
		// Poll faster (1s) when a transfer is active so progress feels responsive.
		interval := 3 * time.Second
		if m.pendingUpload != "" || m.pendingDownload != "" {
			interval = 1 * time.Second
		}
		cmds = append(cmds, pollCmd(interval))
		return m, tea.Batch(cmds...)

	case ErrorMsg:
		m.err = fmt.Sprintf("%s: %v", msg.Context, msg.Err)
		return m, clearErrorAfter(5 * time.Second)

	case ClearErrorMsg:
		m.err = ""
		return m, nil
	}

	return m, nil
}

// setError sets an error message and returns a command to auto-dismiss it.
func (m *RootModel) setError(format string, args ...interface{}) tea.Cmd {
	m.err = fmt.Sprintf(format, args...)
	return clearErrorAfter(5 * time.Second)
}

func (m RootModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Global keys — but let network tab handle "/" for search
	if !m.isTabCapturingKey(msg) {
		switch {
		case key.Matches(msg, GlobalKeyMap.Quit):
			return m, tea.Quit
		case key.Matches(msg, GlobalKeyMap.Help):
			m.showHelp = true
			return m, nil
		case key.Matches(msg, GlobalKeyMap.Upload):
			m.uploadModal.Open()
			return m, nil
		case key.Matches(msg, GlobalKeyMap.Tab1):
			return m.switchTab(tabNetwork), m.onTabActivated(tabNetwork)
		case key.Matches(msg, GlobalKeyMap.Tab2):
			return m.switchTab(tabTransfers), m.onTabActivated(tabTransfers)
		case key.Matches(msg, GlobalKeyMap.Tab3):
			return m.switchTab(tabVault), m.onTabActivated(tabVault)
		case key.Matches(msg, GlobalKeyMap.Tab4):
			return m.switchTab(tabDiagnostics), nil
		case key.Matches(msg, GlobalKeyMap.NextTab):
			newTab := (m.activeTab + 1) % 4
			return m.switchTab(newTab), m.onTabActivated(newTab)
		case key.Matches(msg, GlobalKeyMap.PrevTab):
			newTab := (m.activeTab + 3) % 4
			return m.switchTab(newTab), m.onTabActivated(newTab)
		}
	}

	// Dispatch to active tab
	switch m.activeTab {
	case tabNetwork:
		return m, m.network.Update(msg)
	case tabTransfers:
		return m, m.transfers.Update(msg)
	case tabVault:
		return m, m.vault.Update(msg)
	}

	return m, nil
}

func (m RootModel) isTabCapturingKey(msg tea.KeyMsg) bool {
	k := msg.String()
	switch m.activeTab {
	case tabNetwork:
		if m.network.IsInputMode() {
			return true // suppress all global keys during text input
		}
		return k == "/" || k == "tab"
	case tabTransfers:
		if m.transfers.IsInputMode() {
			return true
		}
		return k == "/"
	case tabVault:
		if m.vault.IsInputMode() {
			return true
		}
		return k == "/"
	}
	return false
}

func (m RootModel) switchTab(tab int) RootModel {
	m.prevTab = m.activeTab
	m.activeTab = tab
	return m
}

func (m RootModel) onTabActivated(tab int) tea.Cmd {
	switch tab {
	case tabNetwork:
		return tea.Batch(fetchPeers(m.client), fetchLANPeers(m.client))
	case tabTransfers:
		return fetchTransfers(m.client)
	case tabVault:
		return tea.Batch(fetchUploads(m.client), fetchDownloads(m.client))
	}
	return nil
}

func (m RootModel) View() string {
	if m.width == 0 {
		return "Loading..."
	}

	// Full-screen overlays
	if m.showHelp {
		return renderHelp(m.width, m.height)
	}
	if m.uploadModal.Active {
		return m.uploadModal.View(m.width, m.height)
	}
	if m.downloadPrompt.Active {
		return m.downloadPrompt.View(m.width, m.height)
	}

	header := components.RenderHeader(m.status, m.width)
	tabBar := m.renderTabBar()

	var content string
	var hints []components.FooterHint
	switch m.activeTab {
	case tabNetwork:
		content = m.network.View()
		hints = m.network.FooterHints()
	case tabTransfers:
		content = m.transfers.View()
		hints = m.transfers.FooterHints()
	case tabVault:
		content = m.vault.View()
		hints = m.vault.FooterHints()
	case tabDiagnostics:
		content = m.diagnostics.View()
		hints = m.diagnostics.FooterHints()
	}

	footer := components.RenderFooter(hints, m.width)

	// Show pending operations above error/footer with live progress
	var statusLines []string
	if m.pendingUpload != "" {
		statusLines = append(statusLines, m.transferStatusLine("upload", m.pendingUpload))
	}
	if m.pendingDownload != "" {
		statusLines = append(statusLines, m.transferStatusLine("download", m.pendingDownload))
	}
	if m.err != "" {
		statusLines = append(statusLines, lipgloss.NewStyle().Foreground(lipgloss.Color("#FF4444")).Bold(true).Render("  "+m.err))
	}
	if len(statusLines) > 0 {
		footer = strings.Join(statusLines, "\n") + "\n" + footer
	}

	contentHeight := m.height - lipgloss.Height(header) - lipgloss.Height(tabBar) - lipgloss.Height(footer)
	if contentHeight < 0 {
		contentHeight = 0
	}
	paddedContent := lipgloss.NewStyle().Height(contentHeight).Render(content)

	return strings.Join([]string{header, tabBar, paddedContent, footer}, "\n")
}

// transferStatusLine finds the matching active transfer and renders a status
// line with chunk progress, percentage, and speed.
func (m RootModel) transferStatusLine(dir, name string) string {
	// Search the cached transfers for a matching active entry.
	for _, tr := range m.lastTransfers {
		if tr.Name == name && tr.Total > 0 {
			pct := float64(tr.Completed) / float64(tr.Total) * 100
			speed := ""
			if tr.Speed > 0 {
				const (
					KB = 1024.0
					MB = KB * 1024
				)
				switch {
				case tr.Speed >= MB:
					speed = fmt.Sprintf(" %.1f MB/s", tr.Speed/MB)
				case tr.Speed >= KB:
					speed = fmt.Sprintf(" %.0f KB/s", tr.Speed/KB)
				default:
					speed = fmt.Sprintf(" %.0f B/s", tr.Speed)
				}
			}
			eta := ""
			if tr.Speed > 0 && tr.BytesDone < tr.Size {
				remaining := float64(tr.Size-tr.BytesDone) / tr.Speed
				d := time.Duration(remaining * float64(time.Second))
				if d < time.Minute {
					eta = fmt.Sprintf(" ETA %ds", int(d.Seconds()))
				} else if d < time.Hour {
					eta = fmt.Sprintf(" ETA %dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
				} else {
					eta = fmt.Sprintf(" ETA %dh%02dm", int(d.Hours()), int(d.Minutes())%60)
				}
			}
			label := "Uploading"
			if dir == "download" {
				label = "Downloading"
			}
			byteInfo := humanBytes(tr.BytesDone, tr.Size)
			return lipgloss.NewStyle().Foreground(ColorPaused).Render(
				fmt.Sprintf("  %s: %s | %s (%.0f%%)%s%s", label, name, byteInfo, pct, speed, eta))
		}
	}
	// Fallback if transfer not found in cache yet.
	label := "Uploading"
	if dir == "download" {
		label = "Downloading"
	}
	return lipgloss.NewStyle().Foreground(ColorPaused).Render("  " + label + ": " + name + "...")
}

func humanBytes(done, total int64) string {
	const (
		KB = 1024.0
		MB = KB * 1024
		GB = MB * 1024
	)
	format := func(b int64) string {
		v := float64(b)
		switch {
		case v >= GB:
			return fmt.Sprintf("%.1f GB", v/GB)
		case v >= MB:
			return fmt.Sprintf("%.1f MB", v/MB)
		case v >= KB:
			return fmt.Sprintf("%.1f KB", v/KB)
		default:
			return fmt.Sprintf("%d B", b)
		}
	}
	return format(done) + "/" + format(total)
}

func (m RootModel) renderTabBar() string {
	var renderedTabs []string
	for i, name := range tabNames {
		label := fmt.Sprintf(" %d:%s ", i+1, name)
		if i == m.activeTab {
			renderedTabs = append(renderedTabs, TabActiveStyle.Render(label))
		} else {
			renderedTabs = append(renderedTabs, TabInactiveStyle.Render(label))
		}
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, renderedTabs...)
}

// ── Tea commands ──

func fetchStatus(c *DaemonClient) tea.Cmd {
	return func() tea.Msg {
		status, err := c.NodeStatus()
		return StatusUpdatedMsg{Status: status, Err: err}
	}
}

func fetchPeers(c *DaemonClient) tea.Cmd {
	return func() tea.Msg {
		peers, err := c.ListPeers()
		return PeersUpdatedMsg{Peers: peers, Err: err}
	}
}

func fetchTransfers(c *DaemonClient) tea.Cmd {
	return func() tea.Msg {
		transfers, err := c.ListTransfers()
		return TransfersUpdatedMsg{Transfers: transfers, Err: err}
	}
}

func fetchUploads(c *DaemonClient) tea.Cmd {
	return func() tea.Msg {
		uploads, err := c.ListUploads()
		return UploadsUpdatedMsg{Uploads: uploads, Err: err}
	}
}

func fetchDownloads(c *DaemonClient) tea.Cmd {
	return func() tea.Msg {
		downloads, err := c.ListDownloads()
		return DownloadsUpdatedMsg{Downloads: downloads, Err: err}
	}
}

func fetchInbox(c *DaemonClient) tea.Cmd {
	return func() tea.Msg {
		inbox, err := c.ListInbox()
		return InboxUpdatedMsg{Inbox: inbox, Err: err}
	}
}

func fetchLANPeers(c *DaemonClient) tea.Cmd {
	return func() tea.Msg {
		peers, err := c.ScanLAN()
		return LANScanMsg{Peers: peers, Err: err}
	}
}

func browsePeer(c *DaemonClient, addr string) tea.Cmd {
	return func() tea.Msg {
		files, err := c.FullBrowse(addr)
		return BrowseResultsMsg{PeerAddr: addr, Files: files, Err: err}
	}
}

func pauseTransfer(c *DaemonClient, id string) tea.Cmd {
	return func() tea.Msg {
		msg, err := c.PauseTransfer(id)
		return TransferActionMsg{Action: "pause", ID: id, Msg: msg, Err: err}
	}
}

func resumeTransfer(c *DaemonClient, id string) tea.Cmd {
	return func() tea.Msg {
		msg, err := c.ResumeTransfer(id)
		return TransferActionMsg{Action: "resume", ID: id, Msg: msg, Err: err}
	}
}

func cancelTransfer(c *DaemonClient, id string) tea.Cmd {
	return func() tea.Msg {
		msg, err := c.CancelTransfer(id)
		return TransferActionMsg{Action: "cancel", ID: id, Msg: msg, Err: err}
	}
}

func connectPeer(c *DaemonClient, addr string) tea.Cmd {
	return func() tea.Msg {
		msg, err := c.ConnectPeer(addr)
		return PeerActionMsg{Action: "connect", Addr: addr, Msg: msg, Err: err}
	}
}

func disconnectPeer(c *DaemonClient, addr string) tea.Cmd {
	return func() tea.Msg {
		msg, err := c.DisconnectPeer(addr)
		return PeerActionMsg{Action: "disconnect", Addr: addr, Msg: msg, Err: err}
	}
}

func sendToPeer(c *DaemonClient, fileKey, peerAddr string) tea.Cmd {
	return func() tea.Msg {
		msg, err := c.SendToPeer(fileKey, peerAddr)
		return PeerActionMsg{Action: "send", Addr: peerAddr, Msg: msg, Err: err}
	}
}

func removeFile(c *DaemonClient, key string) tea.Cmd {
	return func() tea.Msg {
		msg, err := c.RemoveFile(key)
		return RemoveFileMsg{Key: key, Msg: msg, Err: err}
	}
}

// doUpload runs the upload in a background goroutine.
func doUpload(req UploadRequestMsg, sockPath string, h Handlers) tea.Cmd {
	return func() tea.Msg {
		var err error
		if req.IsDir {
			err = h.UploadDirectory(req.Name, req.FilePath, req.ShareWith, "", sockPath, req.Public)
		} else {
			err = h.UploadFile(req.Name, req.FilePath, req.ShareWith, "", sockPath, req.Public)
		}
		return UploadCompleteMsg{Name: req.Name, Err: err}
	}
}

// doDownload runs the download in a background goroutine.
func doDownload(req DownloadRequestMsg, sockPath string, h Handlers) tea.Cmd {
	return func() tea.Msg {
		name := req.Name
		outputPath := filepath.Join(req.OutputDir, name)
		err := h.DownloadFile(name, outputPath, "", req.FromKey, sockPath)
		return DownloadCompleteMsg{Name: name, Err: err}
	}
}
