package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// certManager provides certificates for the HTTPS listener.
//
// If tls_cert / tls_key are set they are used as is. Otherwise Glovebox acts as its own CA: on first
// start it generates a local root whose name constraints only allow the configured hostnames and
// listen IPs, so even a leaked key cannot sign for other sites. Server certificates live in memory,
// are issued at startup and re-issued on the next handshake if the device clock jumps (without a
// SIM the clock boots at the factory date). Trust the root once on a phone and
// https://glovebox.lan becomes a secure context that can be installed as a PWA.
type certManager struct {
	dir   string
	hosts []string
	ips   []net.IP

	mu     sync.Mutex
	custom *tls.Certificate
	ca     *x509.Certificate
	caKey  *ecdsa.PrivateKey
	leaf   *tls.Certificate
}

// leafLifetime stays well below Apple's 825-day limit for TLS server certificates
const leafLifetime = 397 * 24 * time.Hour

func newCertManager(dir string, c Config) (*certManager, error) {
	m := &certManager{dir: dir, hosts: c.Hostnames}
	if c.TLSCert != "" || c.TLSKey != "" {
		crt, err := tls.LoadX509KeyPair(c.TLSCert, c.TLSKey)
		if err != nil {
			return nil, fmt.Errorf("load certificate: %w", err)
		}
		m.custom = &crt
		return m, nil
	}
	if len(m.hosts) == 0 {
		return nil, errors.New("no hostnames configured, cannot issue certificates")
	}
	seen := map[string]bool{}
	addIP := func(s string) {
		ip, err := netip.ParseAddr(s)
		if err != nil || ip.IsUnspecified() || seen[ip.String()] {
			return
		}
		seen[ip.String()] = true
		m.ips = append(m.ips, net.IP(ip.AsSlice()))
	}
	for _, a := range c.TLSListen {
		if h, _, err := net.SplitHostPort(a); err == nil {
			addIP(h)
		}
	}
	if a, _, ok := strings.Cut(c.AliasAddr, "/"); ok || a != "" {
		addIP(a)
	}
	if err := m.loadCA(); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("local CA unusable (%v), generating a new one; clients must trust the new root", err)
		}
		if err := m.createCA(); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func (m *certManager) caPath() (string, string) {
	return filepath.Join(m.dir, "ca.pem"), filepath.Join(m.dir, "ca.key")
}

func (m *certManager) loadCA() error {
	cp, kp := m.caPath()
	cb, err := os.ReadFile(cp)
	if err != nil {
		return err
	}
	kb, err := os.ReadFile(kp)
	if err != nil {
		return err
	}
	cblk, _ := pem.Decode(cb)
	kblk, _ := pem.Decode(kb)
	if cblk == nil || kblk == nil {
		return errors.New("invalid PEM")
	}
	ca, err := x509.ParseCertificate(cblk.Bytes)
	if err != nil {
		return err
	}
	key, err := x509.ParseECPrivateKey(kblk.Bytes)
	if err != nil {
		return err
	}
	if time.Until(ca.NotAfter) < 365*24*time.Hour {
		return errors.New("root certificate is about to expire")
	}
	// after changing hostnames or listen addresses the old root's name constraints may not cover them
	for _, h := range m.hosts {
		if !permitsDNS(ca.PermittedDNSDomains, h) {
			return fmt.Errorf("root certificate does not permit %s", h)
		}
	}
	for _, ip := range m.ips {
		if !permitsIP(ca.PermittedIPRanges, ip) {
			return fmt.Errorf("root certificate does not permit %s", ip)
		}
	}
	m.ca, m.caKey = ca, key
	return nil
}

func permitsDNS(domains []string, host string) bool {
	host = strings.ToLower(host)
	for _, d := range domains {
		d = strings.ToLower(strings.TrimPrefix(d, "."))
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

func permitsIP(ranges []*net.IPNet, ip net.IP) bool {
	for _, r := range ranges {
		if r.Contains(ip) {
			return true
		}
	}
	return false
}

func randSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}

func (m *certManager) createCA() error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := randSerial()
	if err != nil {
		return err
	}
	id := make([]byte, 3)
	rand.Read(id)
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "Glovebox Local CA " + strings.ToUpper(hex.EncodeToString(id)),
			Organization: []string{"Glovebox"},
		},
		// the device clock may lag (factory date), so backdate by a year
		NotBefore:             now.AddDate(-1, 0, 0),
		NotAfter:              now.AddDate(20, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		// name constraints: only the console's own hostnames and IPs
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         m.hosts,
	}
	for _, ip := range m.ips {
		bits := 32
		if ip.To4() == nil {
			bits = 128
		}
		tmpl.PermittedIPRanges = append(tmpl.PermittedIPRanges, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
	}
	if len(m.ips) == 0 {
		// without fixed IPs, exclude all IPs so only hostnames are valid
		tmpl.ExcludedIPRanges = []*net.IPNet{
			{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)},
			{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)},
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		return err
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return err
	}
	cp, kp := m.caPath()
	if err := writeFileAtomic(kp, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600); err != nil {
		return err
	}
	if err := writeFileAtomic(cp, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return err
	}
	m.ca, m.caKey, m.leaf = ca, key, nil
	log.Printf("generated local root %s (SHA-256 %s)", ca.Subject.CommonName, fingerprint(der))
	return nil
}

func writeFileAtomic(path string, b []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (m *certManager) issueLeaf(now time.Time) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := randSerial()
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: m.hosts[0]},
		DNSNames:     m.hosts,
		IPAddresses:  m.ips,
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(leafLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, m.ca, &key.PublicKey, m.caKey)
	if err != nil {
		return err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return err
	}
	m.leaf = &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
	return nil
}

func (m *certManager) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.custom != nil {
		return m.custom, nil
	}
	now := time.Now()
	if l := m.leaf; l == nil || now.Before(l.Leaf.NotBefore) || now.After(l.Leaf.NotAfter.Add(-30*24*time.Hour)) {
		if err := m.issueLeaf(now); err != nil {
			return nil, err
		}
	}
	return m.leaf, nil
}

// caPEM returns the local root certificate, or nil when a custom certificate is used
func (m *certManager) caPEM() []byte {
	if m.ca == nil {
		return nil
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: m.ca.Raw})
}

func fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	var parts []string
	for i := 0; i < len(h); i += 2 {
		parts = append(parts, h[i:i+2])
	}
	return strings.Join(parts, ":")
}

type httpsInfo struct {
	URL        string `json:"url"`
	Custom     bool   `json:"custom"`
	CAName     string `json:"ca_name,omitempty"`
	CASHA256   string `json:"ca_sha256,omitempty"`
	CANotAfter string `json:"ca_not_after,omitempty"`
}

func (m *certManager) info(c Config) httpsInfo {
	out := httpsInfo{Custom: m.custom != nil}
	host := "localhost"
	if len(c.Hostnames) > 0 {
		host = c.Hostnames[0]
	}
	if len(c.TLSListen) > 0 {
		if _, port, err := net.SplitHostPort(c.TLSListen[0]); err == nil && port != "443" {
			host = net.JoinHostPort(host, port)
		}
	}
	out.URL = "https://" + host + "/"
	if m.ca != nil {
		out.CAName = m.ca.Subject.CommonName
		out.CASHA256 = fingerprint(m.ca.Raw)
		out.CANotAfter = m.ca.NotAfter.Format("2006-01-02")
	}
	return out
}
