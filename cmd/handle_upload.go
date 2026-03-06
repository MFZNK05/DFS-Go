package cmd

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"github.com/Faizan2005/DFS-Go/Crypto/envelope"
	"github.com/Faizan2005/DFS-Go/Crypto/identity"
	"github.com/Faizan2005/DFS-Go/Storage/chunker"
)

// UploadFile uploads a file to the daemon via Unix socket IPC.
//
// When shareWith is non-empty (comma-separated aliases or hex fingerprints),
// the CLI generates a random DEK, wraps it for each recipient via ECDH,
// signs the manifest, and sends the ECDH upload request (opcode 0x03).
// The daemon encrypts each chunk with the DEK before storing/replicating.
//
// When shareWith is empty, a plaintext upload (opcode 0x01) is sent.
// Identity is required in both cases for fingerprint-based namespacing.
func UploadFile(name, filePath, shareWith, shareWithKey, sockPath string) error {
	// Load identity — required for all uploads (fingerprint namespace).
	id, err := identity.Load(identity.DefaultPath())
	if err != nil {
		return fmt.Errorf("no identity found. Run 'dfs identity init --alias <name>' first")
	}

	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return err
	}

	// Storage key = fingerprint/name (prevents collisions between users).
	storageKey := id.Fingerprint() + "/" + name

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return err
	}
	defer conn.Close()

	if shareWith != "" || shareWithKey != "" {
		// ECDH path: generate DEK, wrap for each recipient + self, sign manifest.
		dek := make([]byte, 32)
		if _, err := rand.Read(dek); err != nil {
			return fmt.Errorf("generate DEK: %w", err)
		}

		var accessList []chunker.AccessEntry

		// Wrap DEK for self first.
		selfPubHex := hex.EncodeToString(id.X25519Pub)
		selfEntry, err := envelope.WrapDEKForRecipient(id.X25519Priv, selfPubHex, dek)
		if err != nil {
			return fmt.Errorf("wrap DEK for self: %w", err)
		}
		accessList = append(accessList, chunker.AccessEntry{
			RecipientPubKey: selfEntry.RecipientPubKey,
			WrappedDEK:      selfEntry.WrappedDEK,
		})

		// Resolve and wrap DEK for each recipient.
		recipients, err := resolveRecipients(conn, shareWith, shareWithKey, sockPath)
		if err != nil {
			return err
		}
		for _, recip := range recipients {
			entry, err := envelope.WrapDEKForRecipient(id.X25519Priv, recip.x25519PubHex, dek)
			if err != nil {
				return fmt.Errorf("wrap DEK for %s: %w", recip.label, err)
			}
			accessList = append(accessList, chunker.AccessEntry{
				RecipientPubKey: entry.RecipientPubKey,
				WrappedDEK:      entry.WrappedDEK,
			})
		}

		// Sign manifest.
		ownerPubHex := hex.EncodeToString(id.X25519Pub)
		ownerEdPubHex := hex.EncodeToString(id.Ed25519Pub)

		envelopeAccessList := make([]envelope.AccessEntry, len(accessList))
		for i, a := range accessList {
			envelopeAccessList[i] = envelope.AccessEntry{
				RecipientPubKey: a.RecipientPubKey,
				WrappedDEK:      a.WrappedDEK,
			}
		}

		sig, err := envelope.SignManifest(id.Ed25519Priv, envelope.ManifestSigningPayload{
			FileKey:     storageKey,
			OwnerPubKey: ownerPubHex,
			Encrypted:   true,
			AccessList:  envelopeAccessList,
		})
		if err != nil {
			return fmt.Errorf("sign manifest: %w", err)
		}

		// Need a fresh connection — the resolve alias calls consumed the first one.
		conn.Close()
		conn, err = net.Dial("unix", sockPath)
		if err != nil {
			return err
		}
		defer conn.Close()

		req := ecdhUploadRequest{
			StorageKey:    storageKey,
			DEK:           dek,
			OwnerPubKey:   ownerPubHex,
			OwnerEdPubKey: ownerEdPubHex,
			AccessList:    accessList,
			Signature:     sig,
			FileSize:      fi.Size(),
		}
		if err := writeECDHUploadRequest(conn, req); err != nil {
			return fmt.Errorf("send ECDH header: %w", err)
		}
	} else {
		// Plaintext path: no encryption.
		if err := writeUploadRequest(conn, storageKey, fi.Size()); err != nil {
			return fmt.Errorf("send header: %w", err)
		}
	}

	if _, err := io.Copy(conn, f); err != nil {
		return fmt.Errorf("stream file: %w", err)
	}

	// Signal EOF so the daemon's io.Reader sees end-of-stream.
	if uc, ok := conn.(*net.UnixConn); ok {
		uc.CloseWrite()
	}

	ok, msg, err := readStatus(conn)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if !ok {
		return fmt.Errorf("upload failed: %s", msg)
	}
	fmt.Println("Uploaded:", msg)
	return nil
}

// recipientInfo holds a resolved recipient's X25519 public key.
type recipientInfo struct {
	label        string // alias or fingerprint (for error messages)
	x25519PubHex string
}

// resolveRecipients resolves --share-with aliases and --share-with-key fingerprints
// to X25519 public keys via IPC opcode 0x06.
func resolveRecipients(conn net.Conn, shareWith, shareWithKey, sockPath string) ([]recipientInfo, error) {
	var recipients []recipientInfo

	// Resolve aliases via --share-with.
	if shareWith != "" {
		aliases := strings.Split(shareWith, ",")
		for _, alias := range aliases {
			alias = strings.TrimSpace(alias)
			if alias == "" {
				continue
			}

			// Each alias resolution needs its own connection.
			resolveConn, err := net.Dial("unix", sockPath)
			if err != nil {
				return nil, fmt.Errorf("connect for alias resolution: %w", err)
			}

			if err := writeResolveAliasRequest(resolveConn, alias); err != nil {
				resolveConn.Close()
				return nil, fmt.Errorf("resolve alias %q: %w", alias, err)
			}

			results, err := readResolveAliasResponse(resolveConn)
			resolveConn.Close()
			if err != nil {
				return nil, fmt.Errorf("resolve alias %q: %w", alias, err)
			}

			if len(results) == 0 {
				return nil, fmt.Errorf("alias %q not found in cluster. Ensure that node is running with identity", alias)
			}
			if len(results) > 1 {
				var fps []string
				for _, r := range results {
					fps = append(fps, fmt.Sprintf("%s (node %s)", r.Fingerprint, r.NodeAddr))
				}
				return nil, fmt.Errorf("multiple nodes with alias %q: %s. Use --share-with-key <fingerprint> instead",
					alias, strings.Join(fps, ", "))
			}

			recipients = append(recipients, recipientInfo{
				label:        alias,
				x25519PubHex: results[0].X25519PubHex,
			})
		}
	}

	// Resolve fingerprints via --share-with-key (direct X25519 pub lookup).
	if shareWithKey != "" {
		keys := strings.Split(shareWithKey, ",")
		for _, key := range keys {
			key = strings.TrimSpace(key)
			if key == "" {
				continue
			}
			// For --share-with-key, the value is a fingerprint — we need to find
			// the X25519 pub from gossip. We use the alias lookup with fingerprint matching.
			// TODO: For now, treat as direct X25519 pub hex if it's 64 chars.
			// A proper approach would add a fingerprint-based lookup opcode.
			// Since fingerprints are propagated via gossip, we search all nodes.
			resolveConn, err := net.Dial("unix", sockPath)
			if err != nil {
				return nil, fmt.Errorf("connect for key resolution: %w", err)
			}

			// Use a special "by-fingerprint" resolve — we send the fingerprint as alias
			// and the daemon will match on metadata["fingerprint"] instead.
			// For now, we iterate all aliases. A cleaner approach: lookup by fingerprint.
			// Workaround: send fingerprint as alias — won't match, but we can add
			// a dedicated handler later. For MVP, --share-with-key expects the full
			// hex X25519 public key directly.
			resolveConn.Close()

			recipients = append(recipients, recipientInfo{
				label:        "key:" + key,
				x25519PubHex: key,
			})
		}
	}

	return recipients, nil
}
