package tlscert

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"testing"
	"time"
)

func TestGenerate(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	f, err := Generate(dir, []string{"api.anthropic.com"}, now)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Server can load the leaf cert + key.
	if _, err := tls.LoadX509KeyPair(f.Cert, f.Key); err != nil {
		t.Fatalf("LoadX509KeyPair: %v", err)
	}

	caPEM, _ := os.ReadFile(f.CA)
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("CA not appended")
	}

	certPEM, _ := os.ReadFile(f.Cert)
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("no PEM block in cert")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "api.anthropic.com", Roots: pool, CurrentTime: now}); err != nil {
		t.Fatalf("leaf does not verify against CA: %v", err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "evil.com", Roots: pool, CurrentTime: now}); err == nil {
		t.Fatal("expected verification failure for wrong host")
	}
}

func TestGenerate_NoHosts(t *testing.T) {
	if _, err := Generate(t.TempDir(), nil, time.Now()); err == nil {
		t.Fatal("expected error with no hosts")
	}
}
