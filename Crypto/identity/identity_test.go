package identity

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGenerate(t *testing.T) {
	id, err := Generate("alice")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if id.Alias != "alice" {
		t.Errorf("alias = %q, want alice", id.Alias)
	}
	if len(id.Ed25519Priv) != 64 {
		t.Errorf("ed25519 priv len = %d, want 64", len(id.Ed25519Priv))
	}
	if len(id.Ed25519Pub) != 32 {
		t.Errorf("ed25519 pub len = %d, want 32", len(id.Ed25519Pub))
	}
	if len(id.X25519Priv) != 32 {
		t.Errorf("x25519 priv len = %d, want 32", len(id.X25519Priv))
	}
	if len(id.X25519Pub) != 32 {
		t.Errorf("x25519 pub len = %d, want 32", len(id.X25519Pub))
	}
}

func TestGenerateEmptyAlias(t *testing.T) {
	_, err := Generate("")
	if err == nil {
		t.Error("expected error for empty alias")
	}
}

func TestSaveLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "identity.json")

	id, err := Generate("bob")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if err := id.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Check file permissions.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("perms = %o, want 0600", perm)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Alias != id.Alias {
		t.Errorf("alias mismatch: %q vs %q", loaded.Alias, id.Alias)
	}
	if string(loaded.Ed25519Priv) != string(id.Ed25519Priv) {
		t.Error("ed25519 priv mismatch")
	}
	if string(loaded.X25519Pub) != string(id.X25519Pub) {
		t.Error("x25519 pub mismatch")
	}
}

func TestLoadNotFound(t *testing.T) {
	_, err := Load("/nonexistent/path/identity.json")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestFingerprint(t *testing.T) {
	id, err := Generate("charlie")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	fp := id.Fingerprint()
	if len(fp) != 16 {
		t.Errorf("fingerprint len = %d, want 16", len(fp))
	}

	// Deterministic: same identity → same fingerprint.
	if fp2 := id.Fingerprint(); fp != fp2 {
		t.Errorf("fingerprint not deterministic: %s vs %s", fp, fp2)
	}
}

func TestFingerprintUnique(t *testing.T) {
	id1, _ := Generate("alice")
	id2, _ := Generate("bob")
	if id1.Fingerprint() == id2.Fingerprint() {
		t.Error("two different identities should have different fingerprints")
	}
}

func TestGossipMetadata(t *testing.T) {
	id, err := Generate("dave")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	meta := id.GossipMetadata()
	if meta["alias"] != "dave" {
		t.Errorf("alias = %q, want dave", meta["alias"])
	}
	for _, key := range []string{"x25519_pub", "ed25519_pub", "fingerprint"} {
		if meta[key] == "" {
			t.Errorf("missing metadata key %q", key)
		}
	}
	if meta["fingerprint"] != id.Fingerprint() {
		t.Error("fingerprint mismatch in metadata")
	}
}
