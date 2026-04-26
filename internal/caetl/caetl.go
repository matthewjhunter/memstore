// Package caetl ("cert ETL") provides a small, opinionated wrapper around
// crypto/x509 for generating a self-signed CA and issuing server / client leaf
// certificates from it.
//
// Defaults are chosen for memstored's deployment shape: ECDSA P-256 keys,
// 128-bit random serials, TLS 1.3 friendly extensions, and CA-imposed
// invariants (BasicConstraintsValid, IsCA, KeyUsage) set at construction time
// so misuse is hard. Stdlib only — no third-party dependencies.
package caetl

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"strings"
	"time"
)

const (
	// CADefaultValidity is the lifetime applied when NewCA is called with zero.
	CADefaultValidity = 10 * 365 * 24 * time.Hour
	// LeafDefaultValidity is the lifetime applied to issued server/client certs
	// when validity is zero.
	LeafDefaultValidity = 365 * 24 * time.Hour

	pemTypeCert = "CERTIFICATE"
	pemTypeKey  = "EC PRIVATE KEY"
)

// CA bundles a CA certificate and its private key. Use NewCA or LoadCA.
type CA struct {
	Cert *x509.Certificate
	Key  *ecdsa.PrivateKey
	der  []byte
}

// Pair bundles a leaf certificate (server or client) and its private key.
type Pair struct {
	Cert *x509.Certificate
	Key  *ecdsa.PrivateKey
	der  []byte
}

// NewCA generates a self-signed CA with the given subject CN and validity.
// A zero validity uses CADefaultValidity (10y).
func NewCA(commonName string, validity time.Duration) (*CA, error) {
	if commonName == "" {
		return nil, errors.New("caetl: CA common name is required")
	}
	if validity == 0 {
		validity = CADefaultValidity
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("caetl: generate CA key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             now.Add(-time.Minute), // tolerate small clock skew on issuance
		NotAfter:              now.Add(validity),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		MaxPathLen:            0, // CA may sign leaves but not intermediate CAs
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("caetl: sign CA cert: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("caetl: parse CA cert: %w", err)
	}
	return &CA{Cert: cert, Key: key, der: der}, nil
}

// LoadCA reads a CA cert + key from disk.
func LoadCA(certPath, keyPath string) (*CA, error) {
	cert, der, err := readCertPEM(certPath)
	if err != nil {
		return nil, err
	}
	if !cert.IsCA {
		return nil, fmt.Errorf("caetl: %s is not a CA cert", certPath)
	}
	key, err := readECKeyPEM(keyPath)
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key, der: der}, nil
}

// Save writes the CA cert (0644) and private key (0600) to the given paths.
func (ca *CA) Save(certPath, keyPath string) error {
	if err := writePEM(certPath, pemTypeCert, ca.der, 0644); err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(ca.Key)
	if err != nil {
		return fmt.Errorf("caetl: marshal CA key: %w", err)
	}
	return writePEM(keyPath, pemTypeKey, keyDER, 0600)
}

// IssueServer mints a server cert signed by the CA, valid for the given hosts.
// Each host may be a DNS name or an IP literal; classification is automatic.
// At least one host is required.
func (ca *CA) IssueServer(commonName string, hosts []string, validity time.Duration) (*Pair, error) {
	if len(hosts) == 0 {
		return nil, errors.New("caetl: at least one server host is required (DNS name or IP)")
	}
	return ca.issueLeaf(commonName, hosts, validity, x509.ExtKeyUsageServerAuth)
}

// IssueClient mints a client cert signed by the CA. The CN is the client's
// identity; no SANs are required.
func (ca *CA) IssueClient(commonName string, validity time.Duration) (*Pair, error) {
	if commonName == "" {
		return nil, errors.New("caetl: client common name is required")
	}
	return ca.issueLeaf(commonName, nil, validity, x509.ExtKeyUsageClientAuth)
}

func (ca *CA) issueLeaf(commonName string, hosts []string, validity time.Duration, eku x509.ExtKeyUsage) (*Pair, error) {
	if validity == 0 {
		validity = LeafDefaultValidity
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("caetl: generate leaf key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{eku},
		BasicConstraintsValid: true,
		IsCA:                  false,
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
		return nil, fmt.Errorf("caetl: sign leaf cert: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("caetl: parse leaf cert: %w", err)
	}
	return &Pair{Cert: cert, Key: key, der: der}, nil
}

// Save writes the leaf cert (0644) and private key (0600) to disk.
func (p *Pair) Save(certPath, keyPath string) error {
	if err := writePEM(certPath, pemTypeCert, p.der, 0644); err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(p.Key)
	if err != nil {
		return fmt.Errorf("caetl: marshal leaf key: %w", err)
	}
	return writePEM(keyPath, pemTypeKey, keyDER, 0600)
}

// LoadPair reads a leaf cert + key from disk.
func LoadPair(certPath, keyPath string) (*Pair, error) {
	cert, der, err := readCertPEM(certPath)
	if err != nil {
		return nil, err
	}
	key, err := readECKeyPEM(keyPath)
	if err != nil {
		return nil, err
	}
	return &Pair{Cert: cert, Key: key, der: der}, nil
}

// Fingerprint returns the lowercase hex SHA-256 of a certificate's DER bytes,
// formatted with colon separators (the format used by `openssl x509 -fingerprint`).
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	hexStr := hex.EncodeToString(sum[:])
	var b strings.Builder
	b.Grow(len(hexStr) + len(hexStr)/2)
	for i := 0; i < len(hexStr); i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(hexStr[i : i+2])
	}
	return b.String()
}

// randomSerial returns a positive 128-bit serial, satisfying RFC 5280's
// "non-zero, ≤20 octets" requirement.
func randomSerial() (*big.Int, error) {
	max := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return nil, fmt.Errorf("caetl: random serial: %w", err)
	}
	if n.Sign() == 0 {
		n.SetInt64(1) // pathologically rare
	}
	return n, nil
}

func writePEM(path, blockType string, der []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("caetl: open %s: %w", path, err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: blockType, Bytes: der}); err != nil {
		return fmt.Errorf("caetl: encode %s: %w", path, err)
	}
	return nil
}

func readCertPEM(path string) (*x509.Certificate, []byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("caetl: read %s: %w", path, err)
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != pemTypeCert {
		return nil, nil, fmt.Errorf("caetl: %s does not contain a CERTIFICATE block", path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("caetl: parse %s: %w", path, err)
	}
	return cert, block.Bytes, nil
}

func readECKeyPEM(path string) (*ecdsa.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("caetl: read %s: %w", path, err)
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != pemTypeKey {
		return nil, fmt.Errorf("caetl: %s does not contain an EC PRIVATE KEY block", path)
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("caetl: parse %s: %w", path, err)
	}
	return key, nil
}
