package tui

import (
	"encoding/json"
	"fmt"
	"net"

	"github.com/Faizan2005/DFS-Go/ipc"
)

// DaemonClient communicates with the daemon over IPC
// (Unix socket on Linux/macOS, named pipe on Windows).
type DaemonClient struct {
	SockPath string
}

// NewDaemonClient creates a client for the given IPC address.
func NewDaemonClient(sockPath string) *DaemonClient {
	return &DaemonClient{SockPath: sockPath}
}

// dial opens a new connection to the daemon.
func (c *DaemonClient) dial() (net.Conn, error) {
	return ipc.Dial(c.SockPath)
}

// NodeStatus queries the daemon for node status info.
func (c *DaemonClient) NodeStatus() (ipc.NodeStatusInfo, error) {
	conn, err := c.dial()
	if err != nil {
		return ipc.NodeStatusInfo{}, err
	}
	defer conn.Close()

	if _, err := conn.Write([]byte{ipc.OpNodeStatus}); err != nil {
		return ipc.NodeStatusInfo{}, err
	}

	data, err := ipc.ReadJSONResponse(conn)
	if err != nil {
		return ipc.NodeStatusInfo{}, err
	}

	var status ipc.NodeStatusInfo
	if err := json.Unmarshal(data, &status); err != nil {
		return ipc.NodeStatusInfo{}, err
	}
	return status, nil
}

// ListPeers queries the daemon for the peer list.
func (c *DaemonClient) ListPeers() ([]ipc.PeerInfo, error) {
	conn, err := c.dial()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if _, err := conn.Write([]byte{ipc.OpListPeers}); err != nil {
		return nil, err
	}

	data, err := ipc.ReadJSONResponse(conn)
	if err != nil {
		return nil, err
	}

	var peers []ipc.PeerInfo
	if err := json.Unmarshal(data, &peers); err != nil {
		return nil, err
	}
	return peers, nil
}

// ListTransfers queries the daemon for active transfers.
func (c *DaemonClient) ListTransfers() ([]ipc.TransferInfo, error) {
	conn, err := c.dial()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if _, err := conn.Write([]byte{ipc.OpListTransfers}); err != nil {
		return nil, err
	}

	data, err := ipc.ReadJSONResponse(conn)
	if err != nil {
		return nil, err
	}

	var transfers []ipc.TransferInfo
	if err := json.Unmarshal(data, &transfers); err != nil {
		return nil, err
	}
	return transfers, nil
}

// ListUploads queries the daemon for upload history.
func (c *DaemonClient) ListUploads() ([]ipc.UploadEntry, error) {
	conn, err := c.dial()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if _, err := conn.Write([]byte{ipc.OpListUploads}); err != nil {
		return nil, err
	}

	data, err := ipc.ReadJSONResponse(conn)
	if err != nil {
		return nil, err
	}

	var uploads []ipc.UploadEntry
	if err := json.Unmarshal(data, &uploads); err != nil {
		return nil, err
	}
	return uploads, nil
}

// ListDownloads queries the daemon for download history.
func (c *DaemonClient) ListDownloads() ([]ipc.DownloadEntry, error) {
	conn, err := c.dial()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if _, err := conn.Write([]byte{ipc.OpListDownloads}); err != nil {
		return nil, err
	}

	data, err := ipc.ReadJSONResponse(conn)
	if err != nil {
		return nil, err
	}

	var downloads []ipc.DownloadEntry
	if err := json.Unmarshal(data, &downloads); err != nil {
		return nil, err
	}
	return downloads, nil
}

// BrowsePeer queries the daemon for a peer's public files.
func (c *DaemonClient) BrowsePeer(peerAddr string) ([]ipc.PublicFileEntry, error) {
	conn, err := c.dial()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if _, err := conn.Write([]byte{ipc.OpBrowsePeer}); err != nil {
		return nil, err
	}
	if err := ipc.WriteString16(conn, peerAddr); err != nil {
		return nil, err
	}

	data, err := ipc.ReadJSONResponse(conn)
	if err != nil {
		return nil, err
	}

	var files []ipc.PublicFileEntry
	if err := json.Unmarshal(data, &files); err != nil {
		return nil, err
	}
	return files, nil
}

// FullBrowse queries the daemon for a combined view of a peer's files with labels.
func (c *DaemonClient) FullBrowse(peerAddr string) ([]ipc.BrowseEntry, error) {
	conn, err := c.dial()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if _, err := conn.Write([]byte{ipc.OpFullBrowse}); err != nil {
		return nil, err
	}
	if err := ipc.WriteString16(conn, peerAddr); err != nil {
		return nil, err
	}

	data, err := ipc.ReadJSONResponse(conn)
	if err != nil {
		return nil, err
	}

	var entries []ipc.BrowseEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// PauseTransfer sends a pause request for a transfer.
func (c *DaemonClient) PauseTransfer(id string) (string, error) {
	return c.transferAction(ipc.OpPauseTransfer, id)
}

// ResumeTransfer sends a resume request for a transfer.
func (c *DaemonClient) ResumeTransfer(id string) (string, error) {
	return c.transferAction(ipc.OpResumeTransfer, id)
}

// CancelTransfer sends a cancel request for a transfer.
func (c *DaemonClient) CancelTransfer(id string) (string, error) {
	return c.transferAction(ipc.OpCancelTransfer, id)
}

// RemoveFile sends a remove request for a file.
func (c *DaemonClient) RemoveFile(key string) (string, error) {
	conn, err := c.dial()
	if err != nil {
		return "", err
	}
	defer conn.Close()

	if _, err := conn.Write([]byte{ipc.OpRemoveFile}); err != nil {
		return "", err
	}
	if err := ipc.WriteString16(conn, key); err != nil {
		return "", err
	}

	ok, msg, err := ipc.ReadStatus(conn)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("%s", msg)
	}
	return msg, nil
}

// ConnectPeer asks the daemon to dial a new peer.
func (c *DaemonClient) ConnectPeer(addr string) (string, error) {
	return c.peerAction(ipc.OpConnectPeer, addr)
}

// DisconnectPeer asks the daemon to disconnect from a peer.
func (c *DaemonClient) DisconnectPeer(addr string) (string, error) {
	return c.peerAction(ipc.OpDisconnectPeer, addr)
}

// ListInbox returns the local inbox entries (files sent to us by peers).
func (c *DaemonClient) ListInbox() ([]ipc.InboxEntry, error) {
	conn, err := c.dial()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if _, err := conn.Write([]byte{ipc.OpListInbox}); err != nil {
		return nil, err
	}

	data, err := ipc.ReadJSONResponse(conn)
	if err != nil {
		return nil, err
	}
	var entries []ipc.InboxEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// SendToPeer sends a direct-share notification for the given file to a peer.
func (c *DaemonClient) SendToPeer(fileKey, peerAddr string) (string, error) {
	conn, err := c.dial()
	if err != nil {
		return "", err
	}
	defer conn.Close()

	if _, err := conn.Write([]byte{ipc.OpSendToPeer}); err != nil {
		return "", err
	}
	if err := ipc.WriteString16(conn, fileKey); err != nil {
		return "", err
	}
	if err := ipc.WriteString16(conn, peerAddr); err != nil {
		return "", err
	}

	ok, msg, err := ipc.ReadStatus(conn)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("%s", msg)
	}
	return msg, nil
}

// ScanLAN triggers an mDNS scan for Hermond peers on the local network.
func (c *DaemonClient) ScanLAN() ([]ipc.DiscoveredPeer, error) {
	conn, err := c.dial()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if _, err := conn.Write([]byte{ipc.OpScanLAN}); err != nil {
		return nil, err
	}

	data, err := ipc.ReadJSONResponse(conn)
	if err != nil {
		return nil, err
	}

	var peers []ipc.DiscoveredPeer
	if err := json.Unmarshal(data, &peers); err != nil {
		return nil, err
	}
	return peers, nil
}

func (c *DaemonClient) peerAction(op byte, addr string) (string, error) {
	conn, err := c.dial()
	if err != nil {
		return "", err
	}
	defer conn.Close()

	if _, err := conn.Write([]byte{op}); err != nil {
		return "", err
	}
	if err := ipc.WriteString16(conn, addr); err != nil {
		return "", err
	}

	ok, msg, err := ipc.ReadStatus(conn)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("%s", msg)
	}
	return msg, nil
}

func (c *DaemonClient) transferAction(op byte, id string) (string, error) {
	conn, err := c.dial()
	if err != nil {
		return "", err
	}
	defer conn.Close()

	if _, err := conn.Write([]byte{op}); err != nil {
		return "", err
	}
	if err := ipc.WriteString16(conn, id); err != nil {
		return "", err
	}

	ok, msg, err := ipc.ReadStatus(conn)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("%s", msg)
	}
	return msg, nil
}
