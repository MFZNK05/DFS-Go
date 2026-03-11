// Package ipc defines the shared binary IPC protocol between the DFS daemon
// and its clients (CLI, TUI). All opcodes, status codes, and wire format
// helpers live here so both cmd/ and tui/ use a single source of truth.
package ipc

// Opcodes for daemon IPC over Unix socket.
const (
	OpUpload             byte = 0x01
	OpDownload           byte = 0x02
	OpECDHUpload         byte = 0x03
	OpECDHDownload       byte = 0x04
	OpGetManifest        byte = 0x05
	OpResolveAlias       byte = 0x06
	OpDownloadToFile     byte = 0x07
	OpECDHDownloadToFile byte = 0x08
	OpDirUpload          byte = 0x09
	OpDirDownload        byte = 0x0A
	OpECDHDirUpload      byte = 0x0B
	OpECDHDirDownload    byte = 0x0C
	OpListUploads        byte = 0x0D
	OpListDownloads      byte = 0x0E
	OpBrowsePeer         byte = 0x0F
	OpListPeers          byte = 0x10
	OpNodeStatus         byte = 0x11
	OpListTransfers      byte = 0x12
	OpCancelTransfer     byte = 0x13
	OpPauseTransfer      byte = 0x14
	OpResumeTransfer     byte = 0x15
	OpRemoveFile         byte = 0x17
	OpSearch             byte = 0x18
	OpConnectPeer        byte = 0x19
	OpDisconnectPeer     byte = 0x1A
	OpFullBrowse         byte = 0x1B
	OpSendToPeer         byte = 0x1C
	OpListInbox          byte = 0x1D
	OpUnblockPeer        byte = 0x1E
	OpScanLAN            byte = 0x1F
)

// Status codes for IPC responses.
const (
	StatusOK       byte = 0x00
	StatusError    byte = 0x01
	StatusProgress byte = 0x02
)
