// Package cp is the Control Plane's server for mamelet: enrollment with one-time tokens, mutual
// TLS with certificates from the Control Plane's own CA, signed revisions, and reports.
package cp

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/ieyoukan/mamela/internal/noderev"
)

// PKI holds the Control Plane's keys: a CA for TLS (server and agent certificates) and a separate
// Ed25519 key that signs revisions. They live in one directory readable only by the owner.
type PKI struct {
	CA          *x509.Certificate
	caKey       *ecdsa.PrivateKey
	Server      tls.Certificate
	RevisionKey ed25519.PrivateKey
}

// LoadPKI loads the keys from dir, creating any that are missing. hosts are the names and
// addresses mamelet uses to reach the server (they go into the server certificate).
func LoadPKI(dir string, hosts []string, now time.Time) (*PKI, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if fi, err := os.Stat(dir); err != nil {
		return nil, err
	} else if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is readable by others (%v); it holds private keys: chmod 700 it", dir, fi.Mode().Perm())
	}
	p := &PKI{}
	var err error
	if p.CA, p.caKey, err = loadOrCreateCA(dir, now); err != nil {
		return nil, err
	}
	if p.Server, err = loadOrCreateServer(dir, p, hosts, now); err != nil {
		return nil, err
	}
	if p.RevisionKey, err = loadOrCreateRevisionKey(dir); err != nil {
		return nil, err
	}
	return p, nil
}

// CAPEM is the CA certificate, for mamelet's configuration.
func (p *PKI) CAPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.CA.Raw})
}

// RevisionPublicKey is the key mamelet verifies revisions with.
func (p *PKI) RevisionPublicKey() ed25519.PublicKey {
	return p.RevisionKey.Public().(ed25519.PublicKey)
}

func serial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}

func readPEM(path, typ string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != typ {
		return nil, fmt.Errorf("%s: not a %s", path, typ)
	}
	return block.Bytes, nil
}

func writePEM(path, typ string, der []byte) error {
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600)
}

func loadOrCreateCA(dir string, now time.Time) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certPath, keyPath := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca-key.pem")
	if der, err := readPEM(certPath, "CERTIFICATE"); err == nil {
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, nil, err
		}
		kder, err := readPEM(keyPath, "PRIVATE KEY")
		if err != nil {
			return nil, nil, err
		}
		key, err := x509.ParsePKCS8PrivateKey(kder)
		if err != nil {
			return nil, nil, err
		}
		ek, ok := key.(*ecdsa.PrivateKey)
		if !ok {
			return nil, nil, fmt.Errorf("%s: not an ECDSA key", keyPath)
		}
		return cert, ek, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	sn, err := serial()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: sn, Subject: pkix.Name{CommonName: "Mamela Control Plane CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(10, 0, 0),
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, BasicConstraintsValid: true, IsCA: true, MaxPathLenZero: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	kder, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	if err := writePEM(keyPath, "PRIVATE KEY", kder); err != nil {
		return nil, nil, err
	}
	if err := writePEM(certPath, "CERTIFICATE", der); err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	return cert, key, err
}

func loadOrCreateServer(dir string, p *PKI, hosts []string, now time.Time) (tls.Certificate, error) {
	certPath, keyPath := filepath.Join(dir, "server.pem"), filepath.Join(dir, "server-key.pem")
	if c, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		leaf, err := x509.ParseCertificate(c.Certificate[0])
		if err == nil && coversHosts(leaf, hosts) && now.Before(leaf.NotAfter.Add(-30*24*time.Hour)) {
			return c, nil
		}
		// Otherwise (other hosts, or expiring within 30 days) issue a new one below.
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	sn, err := serial()
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: sn, Subject: pkix.Name{CommonName: "Mamela Control Plane"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(1, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.CA, &key.PublicKey, p.caKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	kder, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := writePEM(keyPath, "PRIVATE KEY", kder); err != nil {
		return tls.Certificate{}, err
	}
	if err := writePEM(certPath, "CERTIFICATE", der); err != nil {
		return tls.Certificate{}, err
	}
	return tls.LoadX509KeyPair(certPath, keyPath)
}

func coversHosts(c *x509.Certificate, hosts []string) bool {
	for _, h := range hosts {
		if c.VerifyHostname(h) != nil {
			return false
		}
	}
	return true
}

func loadOrCreateRevisionKey(dir string) (ed25519.PrivateKey, error) {
	path := filepath.Join(dir, "revision-key.pem")
	if der, err := readPEM(path, "PRIVATE KEY"); err == nil {
		key, err := x509.ParsePKCS8PrivateKey(der)
		if err != nil {
			return nil, err
		}
		ek, ok := key.(ed25519.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("%s: not an Ed25519 key", path)
		}
		return ek, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return key, writePEM(path, "PRIVATE KEY", der)
}

// SignAgent certifies an agent's key for node. The CSR must be signed by the key and name the
// node; the certificate is good only for client authentication.
func (p *PKI) SignAgent(csrPEM string, node string, now time.Time) (certPEM []byte, serialHex string, err error) {
	block, _ := pem.Decode([]byte(csrPEM))
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, "", errors.New("the certificate request is not PEM")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, "", err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, "", fmt.Errorf("certificate request: %w", err)
	}
	if !noderev.ValidNode(node) || csr.Subject.CommonName != node {
		return nil, "", fmt.Errorf("the certificate request names %q, not node %q", csr.Subject.CommonName, node)
	}
	if k, ok := csr.PublicKey.(*ecdsa.PublicKey); !ok || k.Curve != elliptic.P256() {
		return nil, "", errors.New("the agent key must be ECDSA P-256")
	}
	sn, err := serial()
	if err != nil {
		return nil, "", err
	}
	tmpl := &x509.Certificate{
		SerialNumber: sn, Subject: pkix.Name{CommonName: node},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(1, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.CA, csr.PublicKey, p.caKey)
	if err != nil {
		return nil, "", err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), hex.EncodeToString(sn.Bytes()), nil
}
