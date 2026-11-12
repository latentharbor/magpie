package claudelogin

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/filelock"
)

// Authority never installs trust. Only the public CA goes into a client's
// trust store; the private bundle remains on this machine, mode 0600.
type Authority struct {
	CA          *x509.Certificate
	Certificate tls.Certificate
}

func EnsureAuthority(ctx context.Context, dir string) (*Authority, error) {
	return ensureAuthority(ctx, dir, Host)
}
func EnsureWebAuthority(ctx context.Context, dir string) (*Authority, error) {
	return ensureAuthority(ctx, dir, UsageHost)
}
func ensureAuthority(ctx context.Context, dir, host string) (*Authority, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	unlock, err := filelock.LockContext(ctx, filepath.Join(dir, "authority.lock"))
	if err != nil {
		return nil, err
	}
	defer unlock()
	path := filepath.Join(dir, "authority.pem")
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		b, err = newAuthorityForHost(host)
		if err == nil {
			err = privateWrite(path, b)
		}
	}
	if err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair(b, b)
	if err != nil {
		return nil, fmt.Errorf("Claude login authority is invalid; it was not overwritten: %w", err)
	}
	ca, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	if !ca.IsCA || !ca.PermittedDNSDomainsCritical || len(ca.PermittedDNSDomains) != 1 || ca.PermittedDNSDomains[0] != host || time.Now().Before(ca.NotBefore) || time.Until(ca.NotAfter) < 24*time.Hour {
		return nil, fmt.Errorf("Claude login authority is expired or not restricted to %s; it was not overwritten", host)
	}
	key, ok := pair.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("Claude login authority has an unsupported key")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, err
	}
	if err := privateWrite(filepath.Join(dir, "ca.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw})); err != nil {
		return nil, err
	}
	return issueCertificateForHost(ca, key, host)
}

func issueCertificate(ca *x509.Certificate, key *ecdsa.PrivateKey) (*Authority, error) {
	return issueCertificateForHost(ca, key, Host)
}
func issueCertificateForHost(ca *x509.Certificate, key *ecdsa.PrivateKey, host string) (*Authority, error) {
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	leaf := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: host}, DNSNames: []string{host}, NotBefore: now.Add(-time.Hour), NotAfter: minTime(now.Add(30*24*time.Hour), ca.NotAfter), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, key)
	if err != nil {
		return nil, err
	}
	return &Authority{CA: ca, Certificate: tls.Certificate{Certificate: [][]byte{der, ca.Raw}, PrivateKey: leafKey}}, nil
}

func newAuthority() ([]byte, error) { return newAuthorityForHost(Host) }
func newAuthorityForHost(host string) ([]byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "magpie Claude login local CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(5 * 365 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, PermittedDNSDomainsCritical: true, PermittedDNSDomains: []string{host}}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	priv, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	b := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return append(b, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: priv})...), nil
}

func privateWrite(path string, b []byte) error {
	// A private parent prevents a newly created file being exposed before chmod.
	if err := edit.WriteAtomic(path, b); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
