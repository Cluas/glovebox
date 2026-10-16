package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"testing"
	"time"
)

func freeAddr(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// The same console must serve plain HTTP and HTTPS (HTTP/2 and HTTP/1.1) side by side.
func TestServeHTTPAndHTTPS(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	s, err := newDemoServer(ctx, dir, freeAddr(t))
	if err != nil {
		t.Fatal(err)
	}
	httpAddr, tlsAddr := freeAddr(t), freeAddr(t)
	if err := s.cfg.update(func(c *Config) {
		c.Listen = []string{httpAddr}
		c.TLSListen = []string{tlsAddr}
		c.Hostnames = []string{"glovebox.test"}
	}); err != nil {
		t.Fatal(err)
	}
	if s.certs, err = newCertManager(dir, s.cfg.get()); err != nil {
		t.Fatal(err)
	}
	go s.run(ctx)

	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(s.certs.caPEM())
	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, tlsAddr)
	}
	for _, h2 := range []bool{true, false} {
		tr := &http.Transport{
			TLSClientConfig:   &tls.Config{RootCAs: roots, ServerName: "glovebox.test"},
			DialContext:       dial,
			ForceAttemptHTTP2: h2,
		}
		if !h2 {
			tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
		}
		get(t, &http.Client{Transport: tr}, "https://glovebox.test/", h2)
	}
	get(t, http.DefaultClient, "http://"+httpAddr+"/", false)
}

func get(t *testing.T, c *http.Client, url string, h2 bool) {
	t.Helper()
	var res *http.Response
	var err error
	for range 50 {
		if res, err = c.Get(url); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("GET %s (h2=%v): %v", url, h2, err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK || (h2 && res.ProtoMajor != 2) {
		t.Fatalf("GET %s: %s %s", url, res.Proto, res.Status)
	}
}
