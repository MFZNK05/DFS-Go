package tui

import (
	"time"

	"github.com/Faizan2005/DFS-Go/ipc"
	"github.com/charmbracelet/bubbletea"
)

// ── Poll result messages ─────────────────────────────────────────────

type StatusUpdatedMsg struct {
	Status ipc.NodeStatusInfo
	Err    error
}

type PeersUpdatedMsg struct {
	Peers []ipc.PeerInfo
	Err   error
}

type TransfersUpdatedMsg struct {
	Transfers []ipc.TransferInfo
	Err       error
}

type UploadsUpdatedMsg struct {
	Uploads []ipc.UploadEntry
	Err     error
}

type DownloadsUpdatedMsg struct {
	Downloads []ipc.DownloadEntry
	Err       error
}

type InboxUpdatedMsg struct {
	Inbox []ipc.InboxEntry
	Err   error
}

type BrowseResultsMsg struct {
	PeerAddr string
	Files    []ipc.BrowseEntry
	Err      error
}

type TransferActionMsg struct {
	Action string
	ID     string
	Msg    string
	Err    error
}

type RemoveFileMsg struct {
	Key string
	Msg string
	Err error
}

type PeerActionMsg struct {
	Action string
	Addr   string
	Msg    string
	Err    error
}

type ErrorMsg struct {
	Err     error
	Context string
}

// ClearErrorMsg is sent after a timeout to auto-dismiss errors.
type ClearErrorMsg struct{}

// clearErrorAfter returns a command that sends ClearErrorMsg after the given duration.
func clearErrorAfter(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg {
		return ClearErrorMsg{}
	})
}

// TickMsg is used for periodic polling.
type TickMsg struct {
	Time time.Time
}

// tabActivatedMsg is sent when a tab becomes active to trigger data refresh.
type tabActivatedMsg struct {
	tab int
}

// pollCmd creates a polling tick command.
func pollCmd(interval time.Duration) tea.Cmd {
	return tea.Tick(interval, func(t time.Time) tea.Msg {
		return TickMsg{Time: t}
	})
}
