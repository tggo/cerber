// Package tlscert generates a local CA and a leaf certificate so cerber can
// impersonate an upstream host (e.g. api.anthropic.com) for clients that trust
// the CA. This is only meant for isolated environments (Docker) — never trust the
// CA on your real machine, or you would hijack all traffic to that host.
package tlscert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// Files holds the paths produced by Generate.
type Files struct {
	CA   string // CA certificate (trust this: NODE_EXTRA_CA_CERTS)
	Cert string // leaf cert + CA chain (server uses this)
	Key  string // leaf private key (server uses this)
}

// Generate writes a CA and a leaf certificate (valid for hosts) into dir, using
// now as the validity start. Returns the file paths.
func Generate(dir string, hosts []string, now time.Time) (Files, error) {
	var f Files
	if len(hosts) == 0 {
		return f, fmt.Errorf("tlscert: at least one host required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return f, fmt.Errorf("tlscert: mkdir: %w", err)
	}

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return f, fmt.Errorf("tlscert: ca key: %w", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "cerber local CA", Organization: []string{"cerber"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return f, fmt.Errorf("tlscert: create ca: %w", err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return f, fmt.Errorf("tlscert: leaf key: %w", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return f, fmt.Errorf("tlscert: parse ca: %w", err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: hosts[0], Organization: []string{"cerber"}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     hosts,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return f, fmt.Errorf("tlscert: create leaf: %w", err)
	}

	f = Files{
		CA:   filepath.Join(dir, "ca.pem"),
		Cert: filepath.Join(dir, "cert.pem"),
		Key:  filepath.Join(dir, "key.pem"),
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	leafPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	if err := os.WriteFile(f.CA, caPEM, 0o644); err != nil {
		return f, fmt.Errorf("tlscert: write ca: %w", err)
	}
	// Server cert is the leaf followed by the CA (full chain).
	if err := os.WriteFile(f.Cert, append(leafPEM, caPEM...), 0o644); err != nil {
		return f, fmt.Errorf("tlscert: write cert: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		return f, fmt.Errorf("tlscert: marshal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(f.Key, keyPEM, 0o600); err != nil {
		return f, fmt.Errorf("tlscert: write key: %w", err)
	}
	return f, nil
}
