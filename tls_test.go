package main

import (
	"crypto/x509"
	"net"
	"testing"
	"time"
)

func TestCertManagerNameConstraints(t *testing.T) {
	c := defaultConfig()
	dir := t.TempDir()
	m, err := newCertManager(dir, c)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := m.getCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(m.ca)
	for _, name := range []string{"glovebox.lan", "192.168.0.254"} {
		if _, err := leaf.Leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: name}); err != nil {
			t.Errorf("leaf not valid for %s: %v", name, err)
		}
	}

	// a certificate for any other name must not chain to the local root
	other := &x509.Certificate{
		SerialNumber: leaf.Leaf.SerialNumber,
		DNSNames:     []string{"example.com"},
		IPAddresses:  []net.IP{net.ParseIP("10.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(nil, other, m.ca, leaf.Leaf.PublicKey, m.caKey)
	if err != nil {
		t.Fatal(err)
	}
	forged, _ := x509.ParseCertificate(der)
	if _, err := forged.Verify(x509.VerifyOptions{Roots: roots, DNSName: "example.com"}); err == nil {
		t.Fatal("root signed a certificate outside its name constraints")
	}

	// the CA is persisted and reused
	m2, err := newCertManager(dir, c)
	if err != nil {
		t.Fatal(err)
	}
	if !m2.ca.Equal(m.ca) {
		t.Fatal("CA regenerated although it still covers the configuration")
	}
	// a new hostname outside the old constraints forces a new CA
	c.Hostnames = []string{"router.home.arpa"}
	m3, err := newCertManager(dir, c)
	if err != nil {
		t.Fatal(err)
	}
	if m3.ca.Equal(m.ca) {
		t.Fatal("CA reused although it does not permit the new hostname")
	}
}
