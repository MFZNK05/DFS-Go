package cmd

import (
	"encoding/hex"
	"fmt"

	"github.com/Faizan2005/DFS-Go/Crypto/identity"
	"github.com/spf13/cobra"
)

var identityAlias string

var identityCmd = &cobra.Command{
	Use:   "identity",
	Short: "Manage node identity (keypairs for ECDH sharing)",
}

var identityInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Generate a new identity keypair",
	RunE: func(cmd *cobra.Command, args []string) error {
		if identityAlias == "" {
			return fmt.Errorf("--alias is required")
		}

		path := identity.DefaultPath()

		// Check if identity already exists.
		if existing, err := identity.Load(path); err == nil {
			return fmt.Errorf("identity already exists at %s (alias=%q, fingerprint=%s). Identity is permanent — all your files are tied to this fingerprint",
				path, existing.Alias, existing.Fingerprint())
		}

		id, err := identity.Generate(identityAlias)
		if err != nil {
			return fmt.Errorf("generate identity: %w", err)
		}
		if err := id.Save(path); err != nil {
			return fmt.Errorf("save identity: %w", err)
		}

		fmt.Printf("Identity created:\n")
		fmt.Printf("  Alias:       %s\n", id.Alias)
		fmt.Printf("  Fingerprint: %s\n", id.Fingerprint())
		fmt.Printf("  Path:        %s\n", path)
		return nil
	},
}

var identityShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Show current identity info",
	RunE: func(cmd *cobra.Command, args []string) error {
		path := identity.DefaultPath()
		id, err := identity.Load(path)
		if err != nil {
			return fmt.Errorf("no identity found. Run 'dfs identity init --alias <name>' first")
		}

		fmt.Printf("Alias:         %s\n", id.Alias)
		fmt.Printf("Fingerprint:   %s\n", id.Fingerprint())
		fmt.Printf("X25519 Pub:    %s\n", hex.EncodeToString(id.X25519Pub))
		fmt.Printf("Ed25519 Pub:   %s\n", hex.EncodeToString(id.Ed25519Pub))
		fmt.Printf("Path:          %s\n", path)
		return nil
	},
}

func init() {
	identityInitCmd.Flags().StringVar(&identityAlias, "alias", "", "Alias for this node (required)")
	identityInitCmd.MarkFlagRequired("alias")

	identityCmd.AddCommand(identityInitCmd)
	identityCmd.AddCommand(identityShowCmd)
	rootCmd.AddCommand(identityCmd)
}
