package cmd

import (
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/Faizan2005/DFS-Go/Crypto/envelope"
	"github.com/Faizan2005/DFS-Go/Crypto/identity"
)

// DownloadFile downloads a file from the daemon via Unix socket IPC.
//
// Two download paths:
//
//	Direct-to-disk (--output specified): CLI sends the absolute file path to the
//	daemon (opcode 0x07/0x08). The daemon writes directly to disk using
//	random-access I/O — zero data flows through the IPC socket. The CLI
//	receives only tiny progress updates until completion.
//
//	Streaming pipe (stdout): CLI uses the legacy opcode 0x02/0x04. The daemon
//	streams bytes sequentially over the socket using a sliding-window algorithm.
func DownloadFile(name, outputPath, fromAlias, fromKey, sockPath string) error {
	// Load identity — required for all downloads (fingerprint namespace).
	id, err := identity.Load(identity.DefaultPath())
	if err != nil {
		return fmt.Errorf("no identity found. Run 'dfs identity init --alias <name>' first")
	}

	// Determine the owner's fingerprint.
	ownerFingerprint, err := resolveOwner(id, fromAlias, fromKey, sockPath)
	if err != nil {
		return err
	}

	storageKey := ownerFingerprint + "/" + name

	// Fetch manifest to check if file is encrypted.
	manifestResp, err := fetchManifestInfo(storageKey, sockPath)
	if err != nil {
		return err
	}

	// Unwrap DEK if encrypted.
	var dek []byte
	if manifestResp.Encrypted {
		dek, err = unwrapDEK(id, manifestResp)
		if err != nil {
			return err
		}
	}

	// Choose download path based on whether output is a file or stdout.
	if outputPath != "" {
		return downloadToFile(storageKey, outputPath, dek, sockPath)
	}
	return downloadToStream(storageKey, dek, sockPath)
}

// downloadToFile uses the direct-to-disk path (opcode 0x07/0x08).
// The daemon writes directly to the file — no data flows through IPC.
func downloadToFile(storageKey, outputPath string, dek []byte, sockPath string) error {
	// Convert to absolute path so the daemon can open it.
	absPath, err := filepath.Abs(outputPath)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Send the download-to-file request.
	if dek != nil {
		if err := writeECDHDownloadToFileRequest(conn, storageKey, absPath, dek); err != nil {
			return fmt.Errorf("send ECDH download-to-file request: %w", err)
		}
	} else {
		if err := writeDownloadToFileRequest(conn, storageKey, absPath); err != nil {
			return fmt.Errorf("send download-to-file request: %w", err)
		}
	}

	// Read progress updates until final status.
	for {
		completed, total, isProgress, finalOK, msg, err := readProgressOrStatus(conn)
		if err != nil {
			return fmt.Errorf("read progress: %w", err)
		}
		if isProgress {
			fmt.Printf("\rDownloading: %d/%d chunks", completed, total)
			continue
		}
		// Final status.
		fmt.Println() // newline after progress
		if !finalOK {
			return fmt.Errorf("download failed: %s", msg)
		}
		fmt.Printf("Downloaded → %s\n", outputPath)
		return nil
	}
}

// downloadToStream uses the legacy streaming path (opcode 0x02/0x04).
// Bytes flow sequentially over the IPC socket to stdout.
func downloadToStream(storageKey string, dek []byte, sockPath string) error {
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return err
	}
	defer conn.Close()

	if dek != nil {
		if err := writeECDHDownloadRequest(conn, storageKey, dek); err != nil {
			return fmt.Errorf("send ECDH download request: %w", err)
		}
	} else {
		if err := writeDownloadRequest(conn, storageKey); err != nil {
			return fmt.Errorf("send request: %w", err)
		}
	}

	ok, contentLen, err := readDownloadResponseHeader(conn)
	if err != nil {
		return fmt.Errorf("read response header: %w", err)
	}
	if !ok {
		errMsg := make([]byte, contentLen)
		io.ReadFull(conn, errMsg) //nolint:errcheck
		return fmt.Errorf("download failed: %s", errMsg)
	}

	var src io.Reader
	if contentLen > 0 {
		src = io.LimitReader(conn, contentLen)
	} else {
		src = conn
	}

	_, err = io.Copy(os.Stdout, src)
	return err
}

// resolveOwner determines the file owner's fingerprint from flags.
func resolveOwner(id *identity.Identity, fromAlias, fromKey, sockPath string) (string, error) {
	if fromKey != "" {
		return fromKey, nil
	}
	if fromAlias != "" {
		resolveConn, err := net.Dial("unix", sockPath)
		if err != nil {
			return "", err
		}
		if err := writeResolveAliasRequest(resolveConn, fromAlias); err != nil {
			resolveConn.Close()
			return "", fmt.Errorf("resolve alias %q: %w", fromAlias, err)
		}
		results, err := readResolveAliasResponse(resolveConn)
		resolveConn.Close()
		if err != nil {
			return "", fmt.Errorf("resolve alias %q: %w", fromAlias, err)
		}
		if len(results) == 0 {
			return "", fmt.Errorf("alias %q not found in cluster. Ensure that node is running with identity", fromAlias)
		}
		if len(results) > 1 {
			var fps []string
			for _, r := range results {
				fps = append(fps, fmt.Sprintf("%s (node %s)", r.Fingerprint, r.NodeAddr))
			}
			return "", fmt.Errorf("multiple nodes with alias %q: %s. Use --from-key <fingerprint> instead",
				fromAlias, strings.Join(fps, ", "))
		}
		return results[0].Fingerprint, nil
	}
	return id.Fingerprint(), nil
}

// fetchManifestInfo retrieves the manifest metadata for the given key.
func fetchManifestInfo(storageKey, sockPath string) (*manifestInfoResponse, error) {
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return nil, err
	}
	if err := writeGetManifestRequest(conn, storageKey); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send manifest request: %w", err)
	}
	resp, err := readManifestInfoResponse(conn)
	conn.Close()
	if err != nil {
		return nil, fmt.Errorf("read manifest info: %w", err)
	}
	return resp, nil
}

// unwrapDEK finds our AccessEntry and unwraps the DEK via ECDH.
func unwrapDEK(id *identity.Identity, resp *manifestInfoResponse) ([]byte, error) {
	myPubHex := hex.EncodeToString(id.X25519Pub)

	var wrappedDEKHex string
	found := false
	for _, entry := range resp.AccessList {
		if entry.RecipientPubKey == myPubHex {
			wrappedDEKHex = entry.WrappedDEK
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("you are not in the access list for this file. Ask the owner to share with your alias")
	}

	dek, err := envelope.UnwrapDEK(id.X25519Priv, resp.OwnerPub, wrappedDEKHex)
	if err != nil {
		return nil, fmt.Errorf("unwrap DEK: %w", err)
	}
	return dek, nil
}
