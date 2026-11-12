package claudelogin

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"os"
	"path/filepath"
	"time"
)

type CertificateStatus struct {
	Path    string `json:"path"`
	Exists  bool   `json:"exists"`
	Trusted bool   `json:"trusted"`
	SHA256  string `json:"sha256,omitempty"`
	Error   string `json:"error,omitempty"`
}

// ReadCertificate is read-only and discloses no private key. System trust
// must be granted by the user separately; merely generating a CA isn't trust.
func ReadCertificate() CertificateStatus    { return readCertificate(CertDir(), Host) }
func ReadWebCertificate() CertificateStatus { return readCertificate(WebCertDir(), UsageHost) }
func readCertificate(dir, host string) CertificateStatus {
	s := CertificateStatus{Path: filepath.Join(dir, "ca.pem")}
	b, err := os.ReadFile(s.Path)
	if os.IsNotExist(err) {
		return s
	}
	if err != nil {
		s.Error = err.Error()
		return s
	}
	s.Exists = true
	block, _ := pem.Decode(b)
	if block == nil {
		s.Error = "invalid Claude login CA"
		return s
	}
	ca, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		s.Error = err.Error()
		return s
	}
	private, err := os.ReadFile(filepath.Join(dir, "authority.pem"))
	if err != nil {
		s.Error = "Claude login authority is unavailable"
		return s
	}
	pair, err := tls.X509KeyPair(private, private)
	if err != nil || !bytes.Equal(pair.Certificate[0], ca.Raw) || !ca.IsCA || !ca.PermittedDNSDomainsCritical || len(ca.PermittedDNSDomains) != 1 || ca.PermittedDNSDomains[0] != host || time.Until(ca.NotAfter) < 24*time.Hour {
		s.Error = "Claude login public CA does not match a valid restricted authority"
		return s
	}
	sum := sha256.Sum256(ca.Raw)
	s.SHA256 = hex.EncodeToString(sum[:])
	key, ok := pair.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		s.Error = "unsupported Claude login authority key"
		return s
	}
	// Check the exact server's TLS policy, not basic trust of the root.
	// This is the system trust result, not proof that Chromium or a guest VM
	// accepts the same trust record. Chromium ignores hostname-scoped Keychain
	// trust entries; use SSL policy trust with the CA's own DNS constraint.
	a, err := issueCertificateForHost(ca, key, host)
	if err != nil {
		s.Error = err.Error()
		return s
	}
	leaf, err := x509.ParseCertificate(a.Certificate.Certificate[0])
	if err != nil {
		s.Error = err.Error()
		return s
	}
	intermediates := x509.NewCertPool()
	intermediates.AddCert(ca)
	_, err = leaf.Verify(x509.VerifyOptions{DNSName: host, Intermediates: intermediates})
	s.Trusted = err == nil
	return s
}
