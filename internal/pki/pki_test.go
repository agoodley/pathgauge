package pki

import (
	"crypto/tls"
	"crypto/x509"
	"strings"
	"testing"
)

func TestJoinTokenRoundTrip(t *testing.T) {
	ca, err := LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tok, secret := ca.NewJoinToken()
	prefix, got, err := ParseJoinToken(tok)
	if err != nil || got != secret || !strings.HasPrefix(ca.Fingerprint(), prefix) {
		t.Fatalf("parse %q: %v %q %q", tok, err, prefix, got)
	}
	for _, bad := range []string{"", "pgjoin_", "pgjoin_short_x", "other_0123456789abcdef_x", "pgjoin_0123456789abcdef_"} {
		if _, _, err := ParseJoinToken(bad); err == nil {
			t.Errorf("%q should not parse", bad)
		}
	}
	h1, h2 := HashSecret("a"), HashSecret("a")
	if HashSecret("a") == HashSecret("b") || h1 != h2 {
		t.Error("hash")
	}
}

func TestCAReloadAndCerts(t *testing.T) {
	dir := t.TempDir()
	ca1, _ := LoadOrCreateCA(dir)
	ca2, err := LoadOrCreateCA(dir)
	if err != nil || ca1.Fingerprint() != ca2.Fingerprint() {
		t.Fatal("CA must persist across loads")
	}
	cert, err := ca1.ServerCert(dir, []string{"server.example", "192.0.2.10", "localhost"})
	if err != nil || len(cert.Certificate) != 2 {
		t.Fatalf("server cert: %v (chain %d)", err, len(cert.Certificate))
	}
	leaf, _ := x509.ParseCertificate(cert.Certificate[0])
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: ca1.Pool(), DNSName: "server.example"}); err != nil {
		t.Errorf("server cert does not verify: %v", err)
	}
	keyPEM, csrPEM, err := NewKeyAndCSR("agent-x")
	if err != nil || len(keyPEM) == 0 {
		t.Fatal(err)
	}
	certPEM, serial, err := ca1.SignCSR(csrPEM, "agt_123")
	if err != nil || serial == "" {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	al, _ := x509.ParseCertificate(pair.Certificate[0])
	if al.Subject.CommonName != "agt_123" {
		t.Error("agent CN must be the server-assigned ID, not the requested name")
	}
	if _, err := al.Verify(x509.VerifyOptions{Roots: ca1.Pool(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Errorf("agent cert: %v", err)
	}
	if _, _, err := ca1.SignCSR([]byte("garbage"), "x"); err == nil {
		t.Error("garbage CSR accepted")
	}
}

func TestPinnedAndChainVerifiers(t *testing.T) {
	dir := t.TempDir()
	ca, _ := LoadOrCreateCA(dir)
	cert, _ := ca.ServerCert(dir, []string{"localhost"})
	var chain []*x509.Certificate
	for _, raw := range cert.Certificate {
		c, _ := x509.ParseCertificate(raw)
		chain = append(chain, c)
	}
	cs := tls.ConnectionState{PeerCertificates: chain}
	if err := PinnedVerifier(ca.Fingerprint()[:16])(cs); err != nil {
		t.Errorf("correct pin refused: %v", err)
	}
	if err := PinnedVerifier("0000000000000000")(cs); err == nil {
		t.Error("wrong pin accepted")
	}
	if err := PinnedVerifier(ca.Fingerprint()[:16])(tls.ConnectionState{PeerCertificates: chain[:1]}); err == nil {
		t.Error("a chain without the CA must be refused")
	}
	if err := ChainVerifier(ca.Pool())(cs); err != nil {
		t.Errorf("chain verifier: %v", err)
	}
	other, _ := LoadOrCreateCA(t.TempDir())
	if err := ChainVerifier(other.Pool())(cs); err == nil {
		t.Error("a certificate from another CA must be refused")
	}
}
