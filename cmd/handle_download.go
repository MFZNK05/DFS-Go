package cmd

import (
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"github.com/Faizan2005/DFS-Go/Crypto/envelope"
	"github.com/Faizan2005/DFS-Go/Crypto/identity"
)

// DownloadFile downloads a file from the daemon via Unix socket IPC.
//
// When --from is specified, the CLI resolves the alias to a fingerprint,
// constructs the storage key as <fingerprint>/name, then fetches the manifest.
// If the file is encrypted, the CLI finds its own AccessEntry, unwraps the DEK
// via ECDH, and sends it with the download request (opcode 0x04).
//
// When --from is empty, the CLI uses its own fingerprint (downloading own file).
func DownloadFile(name, outputPath, fromAlias, fromKey, sockPath string) error {
	// Load identity — required for all downloads (fingerprint namespace).
	id, err := identity.Load(identity.DefaultPath())
	if err != nil {
		return fmt.Errorf("no identity found. Run 'dfs identity init --alias <name>' first")
	}

	// Determine the owner's fingerprint.
	var ownerFingerprint string
	if fromKey != "" {
		// Direct fingerprint provided.
		ownerFingerprint = fromKey
	} else if fromAlias != "" {
		// Resolve alias to fingerprint.
		resolveConn, err := net.Dial("unix", sockPath)
		if err != nil {
			return err
		}
		if err := writeResolveAliasRequest(resolveConn, fromAlias); err != nil {
			resolveConn.Close()
			return fmt.Errorf("resolve alias %q: %w", fromAlias, err)
		}
		results, err := readResolveAliasResponse(resolveConn)
		resolveConn.Close()
		if err != nil {
			return fmt.Errorf("resolve alias %q: %w", fromAlias, err)
		}
		if len(results) == 0 {
			return fmt.Errorf("alias %q not found in cluster. Ensure that node is running with identity", fromAlias)
		}
		if len(results) > 1 {
			var fps []string
			for _, r := range results {
				fps = append(fps, fmt.Sprintf("%s (node %s)", r.Fingerprint, r.NodeAddr))
			}
			return fmt.Errorf("multiple nodes with alias %q: %s. Use --from-key <fingerprint> instead",
				fromAlias, strings.Join(fps, ", "))
		}
		ownerFingerprint = results[0].Fingerprint
	} else {
		// No --from specified: download own file.
		ownerFingerprint = id.Fingerprint()
	}

	storageKey := ownerFingerprint + "/" + name

	// Fetch manifest to check if file is encrypted.
	manifestConn, err := net.Dial("unix", sockPath)
	if err != nil {
		return err
	}
	if err := writeGetManifestRequest(manifestConn, storageKey); err != nil {
		manifestConn.Close()
		return fmt.Errorf("send manifest request: %w", err)
	}
	manifestResp, err := readManifestInfoResponse(manifestConn)
	manifestConn.Close()
	if err != nil {
		return fmt.Errorf("read manifest info: %w", err)
	}

	// Open the download connection.
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return err
	}
	defer conn.Close()

	if manifestResp.Encrypted {
		// ECDH path: find our AccessEntry, unwrap DEK, send with download.
		myPubHex := hex.EncodeToString(id.X25519Pub)

		var wrappedDEKHex string
		found := false
		for _, entry := range manifestResp.AccessList {
			if entry.RecipientPubKey == myPubHex {
				wrappedDEKHex = entry.WrappedDEK
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("you are not in the access list for this file. Ask the owner to share with your alias")
		}

		// Unwrap DEK using ECDH.
		dek, err := envelope.UnwrapDEK(id.X25519Priv, manifestResp.OwnerPub, wrappedDEKHex)
		if err != nil {
			return fmt.Errorf("unwrap DEK: %w", err)
		}

		if err := writeECDHDownloadRequest(conn, storageKey, dek); err != nil {
			return fmt.Errorf("send ECDH download request: %w", err)
		}
	} else {
		// Plaintext path.
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

	var out *os.File
	if outputPath == "" {
		out = os.Stdout
	} else {
		out, err = os.Create(outputPath)
		if err != nil {
			return err
		}
		defer out.Close()
	}

	var src io.Reader
	if contentLen > 0 {
		src = io.LimitReader(conn, contentLen)
	} else {
		src = conn
	}

	n, err := io.Copy(out, src)
	if err != nil {
		return fmt.Errorf("stream to disk: %w", err)
	}
	if outputPath != "" {
		fmt.Printf("Downloaded %d bytes → %s\n", n, outputPath)
	}
	return nil
}
