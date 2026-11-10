package pki

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A new CA orphans every enrolled agent, so one is created only when no CA
// exists. Anything else that stops the certificate being read is an error,
// and the key on disk is never overwritten.
func TestCAIsNeverRecreatedOverAnExistingKey(t *testing.T) {
	dir := t.TempDir()
	ca, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")
	key, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := os.ReadFile(certPath)
	keyUnchanged := func(when string) {
		t.Helper()
		now, err := os.ReadFile(keyPath)
		if err != nil || !bytes.Equal(now, key) {
			t.Fatalf("%s: the CA key was replaced or lost (%v)", when, err)
		}
	}

	// The certificate was deleted but the key is still there.
	if err := os.Remove(certPath); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateCA(dir); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("a key without its certificate must be an error, got %v", err)
	}
	keyUnchanged("certificate missing")

	// The certificate is there but can't be read (here: it is a directory,
	// which fails the same way an I/O error would: not "does not exist").
	if err := os.Mkdir(certPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateCA(dir); err == nil {
		t.Fatal("an unreadable certificate must be an error")
	}
	keyUnchanged("certificate unreadable")
	if err := os.Remove(certPath); err != nil {
		t.Fatal(err)
	}

	// Putting the certificate back restores the same CA.
	if err := os.WriteFile(certPath, cert, 0o644); err != nil {
		t.Fatal(err)
	}
	again, err := LoadOrCreateCA(dir)
	if err != nil || again.Fingerprint() != ca.Fingerprint() {
		t.Fatalf("the restored CA should load unchanged: %v", err)
	}

	// The certificate without its key is an error too, not a fresh start.
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateCA(dir); err == nil {
		t.Fatal("a certificate without its key must be an error")
	}
	if now, _ := os.ReadFile(certPath); !bytes.Equal(now, cert) {
		t.Error("the certificate was replaced")
	}
}
