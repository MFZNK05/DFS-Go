package cmd

import (
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"strings"

	"github.com/Faizan2005/DFS-Go/Crypto/envelope"
	"github.com/Faizan2005/DFS-Go/Crypto/identity"
	server "github.com/Faizan2005/DFS-Go/Server"
	"github.com/Faizan2005/DFS-Go/Storage/chunker"
	"github.com/Faizan2005/DFS-Go/ipc"
)

// resolveAddr resolves an input string to a peer address.
// If it looks like host:port, returns it unchanged.
// If it's a bare IP address, appends the default port :3000.
// Otherwise, tries alias lookup via cluster gossip metadata.
func resolveAddr(input string, s *server.Server) (string, error) {
	// Already has a port — use as-is.
	if _, _, err := net.SplitHostPort(input); err == nil {
		return input, nil
	}
	// Bare IP address without port — append default :3000.
	if ip := net.ParseIP(input); ip != nil {
		return net.JoinHostPort(input, "3000"), nil
	}
	// Treat as alias — lookup via gossip.
	results := s.LookupAlias(input)
	if len(results) == 0 {
		return "", fmt.Errorf("unknown peer %q (no alias match)", input)
	}
	return results[0].NodeAddr, nil
}

// handleConnectPeer connects to a new peer by address or alias (opcode 0x19).
func handleConnectPeer(conn net.Conn, s *server.Server) {
	input, err := readString16(conn)
	if err != nil {
		writeStatus(conn, statusError, "bad request: "+err.Error())
		return
	}
	addr, err := resolveAddr(input, s)
	if err != nil {
		writeStatus(conn, statusError, err.Error())
		return
	}
	if err := s.ConnectPeer(addr); err != nil {
		writeStatus(conn, statusError, err.Error())
		return
	}
	writeStatus(conn, statusOK, fmt.Sprintf("connected to %s", addr))
}

// handleDisconnectPeer disconnects from a peer by address or alias (opcode 0x1A).
func handleDisconnectPeer(conn net.Conn, s *server.Server) {
	input, err := readString16(conn)
	if err != nil {
		writeStatus(conn, statusError, "bad request: "+err.Error())
		return
	}
	addr, err := resolveAddr(input, s)
	if err != nil {
		writeStatus(conn, statusError, err.Error())
		return
	}
	if err := s.DisconnectAndIgnorePeer(addr); err != nil {
		writeStatus(conn, statusError, err.Error())
		return
	}
	writeStatus(conn, statusOK, fmt.Sprintf("disconnected from %s (blocklisted)", addr))
}

// handleUnblockPeer removes a peer from the blocklist (opcode 0x1E).
func handleUnblockPeer(conn net.Conn, s *server.Server) {
	input, err := readString16(conn)
	if err != nil {
		writeStatus(conn, statusError, "bad request: "+err.Error())
		return
	}
	addr, err := resolveAddr(input, s)
	if err != nil {
		writeStatus(conn, statusError, err.Error())
		return
	}
	if err := s.UnblockPeer(addr); err != nil {
		writeStatus(conn, statusError, err.Error())
		return
	}
	writeStatus(conn, statusOK, fmt.Sprintf("unblocked %s", addr))
}

// handleFullBrowse returns a combined view of a peer's files with labels (opcode 0x1B).
// Currently returns public files labeled "public". Inbox entries will be added in Sprint J.
func handleFullBrowse(conn net.Conn, s *server.Server) {
	input, err := readString16(conn)
	if err != nil {
		writeStatus(conn, statusError, "bad request: "+err.Error())
		return
	}
	peerAddr, err := resolveAddr(input, s)
	if err != nil {
		writeStatus(conn, statusError, err.Error())
		return
	}

	// Fetch public catalog from peer.
	catalog, err := s.GetPublicCatalog(peerAddr)
	if err != nil {
		writeStatus(conn, statusError, err.Error())
		return
	}

	// Convert to BrowseEntry with "public" label.
	entries := make([]ipc.BrowseEntry, len(catalog))
	for i, f := range catalog {
		entries[i] = ipc.BrowseEntry{
			Key:       f.Key,
			Name:      f.Name,
			Size:      f.Size,
			IsDir:     f.IsDir,
			Label:     "public",
			Timestamp: f.Timestamp,
		}
	}

	// Append inbox entries sent by the browsed peer.
	if s.StateDB != nil {
		// Resolve the browsed peer's fingerprint for filtering.
		var peerFP string
		if node, ok := s.Cluster.GetNode(peerAddr); ok && node.Metadata != nil {
			peerFP = node.Metadata["fingerprint"]
		}

		inbox, err := s.StateDB.ListInbox()
		if err == nil {
			for _, f := range inbox {
				// Only show inbox entries from the peer we're browsing.
				if peerFP != "" && f.SenderFP != peerFP {
					continue
				}
				entries = append(entries, ipc.BrowseEntry{
					Key:        f.Key,
					Name:       f.Name,
					Size:       f.Size,
					IsDir:      f.IsDir,
					OwnerAlias: f.SenderAlias,
					OwnerFP:    f.SenderFP,
					Label:      "shared",
					Timestamp:  f.ReceivedAt,
				})
			}
		}
	}

	writeJSONResponse(conn, entries)
}

// handleSendToPeer sends a direct-share notification to a peer (opcode 0x1C).
// Wire format: [0x1C][2B len][fileKey][2B len][peerAddr]
func handleSendToPeer(conn net.Conn, s *server.Server) {
	fileKey, err := readString16(conn)
	if err != nil {
		writeStatus(conn, statusError, "bad request: "+err.Error())
		return
	}
	peerInput, err := readString16(conn)
	if err != nil {
		writeStatus(conn, statusError, "bad request: "+err.Error())
		return
	}
	peerAddr, err := resolveAddr(peerInput, s)
	if err != nil {
		writeStatus(conn, statusError, err.Error())
		return
	}

	if s.StateDB == nil {
		writeStatus(conn, statusError, "state database not available")
		return
	}

	// Look up file metadata from uploads.
	uploads, err := s.StateDB.ListUploads()
	if err != nil {
		writeStatus(conn, statusError, "failed to list uploads: "+err.Error())
		return
	}
	var found bool
	var name string
	var size int64
	var isDir bool
	for _, u := range uploads {
		if u.Key == fileKey {
			found = true
			name = u.Name
			size = u.Size
			isDir = u.IsDir
			break
		}
	}
	if !found {
		writeStatus(conn, statusError, "file not found in uploads: "+fileKey)
		return
	}

	// Resolve recipient identity info for outbox keying and access grants.
	var recipientFP string
	var recipientX25519Pub string
	if s.Cluster != nil {
		if node, ok := s.Cluster.GetNode(peerAddr); ok && node.Metadata != nil {
			recipientFP = node.Metadata["fingerprint"]
			recipientX25519Pub = node.Metadata["x25519_pub"]
		}
	}
	// Also try alias lookup if input was an alias (fingerprint available there).
	if recipientFP == "" && !strings.Contains(peerInput, ":") {
		results := s.LookupAlias(peerInput)
		if len(results) > 0 {
			recipientFP = results[0].Fingerprint
			recipientX25519Pub = results[0].X25519PubHex
		}
	}

	// Grant access: if the file is encrypted, add recipient to access list
	// so they can actually decrypt it after receiving the share notification.
	if recipientX25519Pub != "" {
		if grantErr := grantAccessToRecipient(s, fileKey, isDir, recipientX25519Pub); grantErr != nil {
			log.Printf("[send-to-peer] grant access for %q to %s: %v", name, peerAddr, grantErr)
			writeStatus(conn, statusError, "grant access failed: "+grantErr.Error())
			return
		}
	}

	queued, err := s.SendOrQueueDirectShare(peerAddr, recipientFP, fileKey, name, size, isDir)
	if err != nil {
		writeStatus(conn, statusError, err.Error())
		return
	}
	if queued {
		writeStatus(conn, statusOK, fmt.Sprintf("queued %q for %s (offline)", name, peerAddr))
	} else {
		writeStatus(conn, statusOK, fmt.Sprintf("sent %q to %s", name, peerAddr))
	}
}

// grantAccessToRecipient adds the recipient's X25519 public key to the file's
// access list so they can decrypt it. This loads the sender's identity to
// unwrap the DEK and re-wrap it for the recipient.
// No-op for plaintext files or if the recipient already has access.
func grantAccessToRecipient(s *server.Server, fileKey string, isDir bool, recipientX25519PubHex string) error {
	id, err := identity.Load(identity.DefaultPath())
	if err != nil {
		return nil // no identity = plaintext mode, nothing to do
	}

	// Check if we're the owner by seeing if our fingerprint matches the key prefix.
	myFP := id.Fingerprint()
	if !strings.HasPrefix(fileKey, myFP+"/") {
		return nil // not our file, can't grant access
	}

	// Unwrap DEK from the manifest using our own key, then re-wrap for recipient.
	myPubHex := hex.EncodeToString(id.X25519Pub)

	// Determine the owner pub key and access list by inspecting the manifest.
	var ownerPubKey string
	var accessList []chunker.AccessEntry

	if isDir {
		dm, dmErr := s.GetDirectoryManifest(fileKey)
		if dmErr != nil {
			return fmt.Errorf("get dir manifest: %w", dmErr)
		}
		if !dm.Encrypted {
			return nil // plaintext
		}
		ownerPubKey = dm.OwnerPubKey
		accessList = dm.AccessList
	} else {
		m, ok := s.InspectManifest(fileKey)
		if !ok || m == nil {
			return fmt.Errorf("manifest not found for %q", fileKey)
		}
		if !m.Encrypted {
			return nil // plaintext
		}
		ownerPubKey = m.OwnerPubKey
		accessList = m.AccessList
	}

	// Check if recipient already has access.
	for _, ae := range accessList {
		if ae.RecipientPubKey == recipientX25519PubHex {
			return nil // already in access list
		}
	}

	// Find our wrapped DEK in the access list and unwrap it.
	var myWrappedDEK string
	for _, ae := range accessList {
		if ae.RecipientPubKey == myPubHex {
			myWrappedDEK = ae.WrappedDEK
			break
		}
	}
	if myWrappedDEK == "" {
		return fmt.Errorf("owner key not found in access list")
	}

	dek, err := envelope.UnwrapDEK(id.X25519Priv, ownerPubKey, myWrappedDEK)
	if err != nil {
		return fmt.Errorf("unwrap DEK: %w", err)
	}

	// Wrap DEK for the new recipient.
	newEntry, err := envelope.WrapDEKForRecipient(id.X25519Priv, recipientX25519PubHex, dek)
	if err != nil {
		return fmt.Errorf("wrap DEK for recipient: %w", err)
	}

	// Re-sign the manifest with updated access list.
	updatedAccessList := append(accessList, chunker.AccessEntry{
		RecipientPubKey: newEntry.RecipientPubKey,
		WrappedDEK:      newEntry.WrappedDEK,
	})
	envelopeAL := make([]envelope.AccessEntry, len(updatedAccessList))
	for i, ae := range updatedAccessList {
		envelopeAL[i] = envelope.AccessEntry{
			RecipientPubKey: ae.RecipientPubKey,
			WrappedDEK:      ae.WrappedDEK,
		}
	}
	sig, err := envelope.SignManifest(id.Ed25519Priv, envelope.ManifestSigningPayload{
		FileKey:     fileKey,
		OwnerPubKey: ownerPubKey,
		Encrypted:   true,
		AccessList:  envelopeAL,
	})
	if err != nil {
		return fmt.Errorf("sign manifest: %w", err)
	}

	// Update manifest(s) and replicate.
	_, err = s.AddAccessEntry(fileKey, isDir, chunker.AccessEntry{
		RecipientPubKey: newEntry.RecipientPubKey,
		WrappedDEK:      newEntry.WrappedDEK,
	}, sig)
	if err != nil {
		return fmt.Errorf("add access entry: %w", err)
	}

	log.Printf("[grant-access] granted %s access to %q", recipientX25519PubHex[:16]+"...", fileKey)
	return nil
}

// handleListInbox returns the local inbox entries (opcode 0x1D).
func handleListInbox(conn net.Conn, s *server.Server) {
	if s.StateDB == nil {
		writeJSONResponse(conn, []struct{}{})
		return
	}
	entries, err := s.StateDB.ListInbox()
	if err != nil {
		writeJSONResponse(conn, []struct{}{})
		return
	}
	writeJSONResponse(conn, entries)
}
