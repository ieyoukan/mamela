// Package cp is the Control Plane's server for mamelet: enrollment with one-time tokens, mutual
// TLS with certificates from the Control Plane's own CA, signed revisions, and reports.
package cp

import (
	"bytes"
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
	"io/fs"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ieyoukan/mamela/internal/noderev"
)

// serverRenewBefore is how long before it ends the server certificate is replaced, at start and
// while the Control Plane runs.
const serverRenewBefore = 30 * 24 * time.Hour

// PKI holds the Control Plane's keys: a CA for TLS (server and agent certificates) and a separate
// Ed25519 key that signs revisions. They live in one directory readable only by the owner.
type PKI struct {
	CA          *x509.Certificate
	caKey       *ecdsa.PrivateKey
	Server      tls.Certificate // the one loaded at start; ServerCertificate gives the current one
	RevisionKey ed25519.PrivateKey

	// AgentValidity is how long the certificate of a mamelet lasts (zero: one year).
	AgentValidity time.Duration

	dir   string   // where the keys live (the server certificate is reissued there)
	hosts []string // what the server certificate must cover

	mu      sync.Mutex
	current *tls.Certificate // the server certificate in use (nil: Server)
}

// keyFiles are the files without which the Control Plane's identity is gone: the CA (what every
// mamelet is pinned to) and the key that signs revisions (the key mamelet verifies them with). The
// server certificate is not among them: it is issued again from the CA when it is missing.
var keyFiles = []string{"ca.pem", "ca-key.pem", "revision-key.pem"}

// Files are the files the Control Plane keeps in its key directory: its identity (keyFiles) and the
// server certificate, which a backup keeps too so that a restart need not issue another.
var Files = []string{"ca.pem", "ca-key.pem", "revision-key.pem", "server.pem", "server-key.pem"}

// KeysMissingError is the refusal to make a new CA or revision key for a database that has
// mamelets registered: they are pinned to the ones that were lost.
type KeysMissingError struct {
	Dir     string
	Missing []string
	Agents  int
}

func (e *KeysMissingError) Error() string {
	return fmt.Sprintf("MAM-PKI-0201: this database has %d registered mamelet(s), but %s lacks %s, the Control Plane's identity.\n"+
		"  Making new ones would leave every node pinned to a CA and a revision key that no longer exist: none could connect, and none could be told to.\n"+
		"  hint: put back the keys that belong to this database (mamela restore --in <backup>, or copy the files of that directory from the backup). "+
		"If they are really lost, mamela controlplane new-keys --confirm makes new ones and keeps this database; every node must then be enrolled again (docs/recovery.md)",
		e.Agents, e.Dir, strings.Join(e.Missing, ", "))
}

// CheckKeys refuses to go on without the Control Plane's identity when agents are registered: the
// keys are then created by nobody but the first start. With no agents registered there is nothing
// to lose, and the keys are created as before.
func CheckKeys(dir string, agents int) error {
	if agents == 0 {
		return nil
	}
	if missing := MissingKeys(dir); len(missing) > 0 {
		return &KeysMissingError{Dir: dir, Missing: missing, Agents: agents}
	}
	return nil
}

// MissingKeys names the files of dir that hold the Control Plane's identity and are not there.
func MissingKeys(dir string) []string {
	var missing []string
	for _, f := range keyFiles {
		if _, err := os.Stat(filepath.Join(dir, f)); errors.Is(err, fs.ErrNotExist) {
			missing = append(missing, f)
		}
	}
	return missing
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
	p := &PKI{dir: dir, hosts: slices.Clone(hosts)}
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

// ServerCertificate is the certificate the server presents at now. A Control Plane that runs for
// more than a year must not go on presenting the one it loaded at start: when it ends within 30
// days (or has ended), a new one is issued from the CA, saved where a restart would load it, and
// reissued is true. If it cannot be issued, the one in use is returned with the error.
func (p *PKI) ServerCertificate(now time.Time) (c *tls.Certificate, reissued bool, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current == nil {
		cur := p.Server
		p.current = &cur
	}
	leaf, perr := p.current.Leaf, error(nil)
	if leaf == nil {
		leaf, perr = x509.ParseCertificate(p.current.Certificate[0])
	}
	if perr == nil && now.Before(leaf.NotAfter.Add(-serverRenewBefore)) {
		return p.current, false, nil
	}
	if p.dir == "" {
		return p.current, false, errors.New("the server certificate is ending and this PKI has no directory to issue another in")
	}
	next, err := loadOrCreateServer(p.dir, p, p.hosts, now)
	if err != nil {
		return p.current, false, fmt.Errorf("issue a new server certificate: %w", err)
	}
	reissued = !bytes.Equal(next.Certificate[0], p.current.Certificate[0])
	p.current = &next
	return p.current, reissued, nil
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

// writePEM writes the file next to path and renames it over, so that a Control Plane that reads it
// at the same moment (the server certificate is replaced while it runs) never sees half of it.
func writePEM(path, typ string, der []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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

// SignAgent certifies an agent's key for node, at enrollment. The CSR must be signed by the key
// and name the node; the certificate is good only for client authentication.
func (p *PKI) SignAgent(csrPEM string, node string, now time.Time) (certPEM []byte, serialHex string, err error) {
	return p.signAgent(csrPEM, node, "", now)
}

// SignRenewal certifies an agent's key again before its certificate ends. registration is the
// serial of the certificate issued at enrollment (see Registration): the new certificate carries
// it, so that it stands for the same registration as the one it renews.
func (p *PKI) SignRenewal(csrPEM string, node, registration string, now time.Time) (certPEM []byte, serialHex string, err error) {
	if registration == "" {
		return nil, "", errors.New("a renewal needs the registration it renews")
	}
	return p.signAgent(csrPEM, node, registration, now)
}

// Registration is the identity a mamelet's certificate stands for: the serial of the certificate
// issued at its enrollment, which the database records (agents.cert_serial). That certificate is its
// own registration; a renewed one names it in its subject's serialNumber, set by the CA, never taken
// from a request. Plans, approvals and explicit operations are bound to this identity, so renewing
// a certificate does not invalidate them; enrolling again does, as before.
func Registration(leaf *x509.Certificate) string {
	if r := leaf.Subject.SerialNumber; r != "" {
		return r
	}
	return hex.EncodeToString(leaf.SerialNumber.Bytes())
}

func (p *PKI) signAgent(csrPEM string, node, registration string, now time.Time) (certPEM []byte, serialHex string, err error) {
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
	notAfter := now.AddDate(1, 0, 0)
	if p.AgentValidity > 0 {
		notAfter = now.Add(p.AgentValidity)
	}
	tmpl := &x509.Certificate{
		SerialNumber: sn, Subject: pkix.Name{CommonName: node, SerialNumber: registration},
		NotBefore: now.Add(-time.Hour), NotAfter: notAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.CA, csr.PublicKey, p.caKey)
	if err != nil {
		return nil, "", err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), hex.EncodeToString(sn.Bytes()), nil
}
