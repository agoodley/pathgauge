// Package pki runs PathGauge's internal certificate authority: the server's
// TLS certificate, agent client certificates (mTLS), and enrollment tokens
// that pin the CA so a joining agent can verify the server before trusting it.
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base32"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// CA is the internal certificate authority.
type CA struct {
	Cert    *x509.Certificate
	Key     *ecdsa.PrivateKey
	CertPEM []byte
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}

// LoadOrCreateCA loads ca.crt/ca.key from dir, creating them if missing.
func LoadOrCreateCA(dir string) (*CA, error) {
	certPath, keyPath := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")
	if cb, err := os.ReadFile(certPath); err == nil {
		kb, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, err
		}
		return parseCA(cb, kb)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "PathGauge CA " + host, Organization: []string{"PathGauge"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(20, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return nil, err
	}
	return parseCA(certPEM, keyPEM)
}

func parseCA(certPEM, keyPEM []byte) (*CA, error) {
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return nil, errors.New("invalid CA files")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key, CertPEM: certPEM}, nil
}

// Fingerprint is the SHA-256 of the CA certificate (hex).
func (ca *CA) Fingerprint() string {
	sum := sha256.Sum256(ca.Cert.Raw)
	return hex.EncodeToString(sum[:])
}

// Pool returns a cert pool containing the CA.
func (ca *CA) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.Cert)
	return p
}

// ServerCert loads or issues the server's TLS certificate for hosts. It
// is reissued when hosts change or it is within 30 days of expiry.
func (ca *CA) ServerCert(dir string, hosts []string) (tls.Certificate, error) {
	certPath, keyPath := filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key")
	slices.Sort(hosts)
	hosts = slices.Compact(hosts)
	if c, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil && len(c.Certificate) > 0 {
		if leaf, err := x509.ParseCertificate(c.Certificate[0]); err == nil && time.Until(leaf.NotAfter) > 30*24*time.Hour && sameHosts(leaf, hosts) {
			c.Certificate = append(c.Certificate[:1], ca.Cert.Raw)
			return c, nil
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: hosts[0], Organization: []string{"PathGauge"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(2, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		return tls.Certificate{}, err
	}
	kder, _ := x509.MarshalECPrivateKey(key)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return tls.Certificate{}, err
	}
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, err
	}
	c.Certificate = append(c.Certificate, ca.Cert.Raw)
	return c, nil
}

func sameHosts(leaf *x509.Certificate, hosts []string) bool {
	var have []string
	have = append(have, leaf.DNSNames...)
	for _, ip := range leaf.IPAddresses {
		have = append(have, ip.String())
	}
	slices.Sort(have)
	norm := make([]string, 0, len(hosts))
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			norm = append(norm, ip.String())
		} else {
			norm = append(norm, h)
		}
	}
	slices.Sort(norm)
	return slices.Equal(have, norm)
}

// SignCSR issues an agent client certificate with the given common name.
func (ca *CA) SignCSR(csrPEM []byte, cn string) (certPEM []byte, serialHex string, err error) {
	b, _ := pem.Decode(csrPEM)
	if b == nil {
		return nil, "", errors.New("invalid CSR")
	}
	csr, err := x509.ParseCertificateRequest(b.Bytes)
	if err != nil {
		return nil, "", err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, "", fmt.Errorf("CSR signature: %w", err)
	}
	sn := serial()
	tmpl := &x509.Certificate{
		SerialNumber: sn,
		Subject:      pkix.Name{CommonName: cn, Organization: []string{"PathGauge agent"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(5, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, csr.PublicKey, ca.Key)
	if err != nil {
		return nil, "", err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), sn.Text(16), nil
}

// NewKeyAndCSR creates an agent key pair and certificate request.
func NewKeyAndCSR(cn string) (keyPEM, csrPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: cn}}, key)
	if err != nil {
		return nil, nil, err
	}
	kder, _ := x509.MarshalECPrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}

// Join tokens look like pgjoin_<16 hex of CA fingerprint>_<secret>.
const tokenPrefix = "pgjoin_"

// NewJoinToken returns a token for this CA and the secret part.
func (ca *CA) NewJoinToken() (token, secret string) {
	b := make([]byte, 20)
	_, _ = rand.Read(b)
	secret = strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
	return tokenPrefix + ca.Fingerprint()[:16] + "_" + secret, secret
}

// ParseJoinToken splits a token into the CA fingerprint prefix and secret.
func ParseJoinToken(t string) (caPrefix, secret string, err error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(t), tokenPrefix)
	if !ok {
		return "", "", errors.New("not a PathGauge join token (expected pgjoin_...)")
	}
	caPrefix, secret, ok = strings.Cut(rest, "_")
	if !ok || len(caPrefix) != 16 || secret == "" {
		return "", "", errors.New("malformed join token")
	}
	return caPrefix, secret, nil
}

// HashSecret hashes a token or credential for storage.
func HashSecret(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// PinnedVerifier returns a tls.Config.VerifyConnection function that
// accepts a server whose chain includes a CA with the given fingerprint
// prefix and whose leaf is signed by it. Used once, at enrollment, before
// the CA is stored. VerifyConnection (unlike VerifyPeerCertificate) also
// runs on resumed sessions.
func PinnedVerifier(caPrefix string) func(tls.ConnectionState) error {
	return func(cs tls.ConnectionState) error {
		raw := cs.PeerCertificates
		if len(raw) < 2 {
			return errors.New("server did not present its CA certificate")
		}
		leaf := raw[0]
		for _, ca := range raw[1:] {
			sum := sha256.Sum256(ca.Raw)
			if !strings.HasPrefix(hex.EncodeToString(sum[:]), caPrefix) {
				continue
			}
			if err := leaf.CheckSignatureFrom(ca); err != nil {
				return fmt.Errorf("server certificate not signed by the pinned CA: %w", err)
			}
			return nil
		}
		return errors.New("server CA does not match the join token (wrong server, or interception)")
	}
}

// ChainVerifier returns a VerifyConnection function that checks the
// server's leaf against a private CA pool, without host-name checks (the
// CA is pinned, and servers are often reached by IP).
func ChainVerifier(pool *x509.CertPool) func(tls.ConnectionState) error {
	return func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return errors.New("no server certificate")
		}
		inter := x509.NewCertPool()
		for _, c := range cs.PeerCertificates[1:] {
			inter.AddCert(c)
		}
		_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{Roots: pool, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
		return err
	}
}
