package main

import (
	"context"
	"net"
	"os"
	"strings"
	"syscall"
)

// Android keeps its CA roots where Go only looks when built with GOOS=android; this build is
// GOOS=linux, so point Go at them unless the user chose a CA bundle.
var androidCertDirs = []string{
	"/apex/com.android.conscrypt/cacerts", // updatable roots (Android 14+)
	"/system/etc/security/cacerts",
	"/data/misc/keychain/certs-added", // user-trusted CAs
}

func init() {
	if d := certDirsFor(androidCertDirs); d != "" && os.Getenv("SSL_CERT_FILE") == "" && os.Getenv("SSL_CERT_DIR") == "" {
		os.Setenv("SSL_CERT_DIR", d)
	}
}

// certDirsFor returns the existing directories as an SSL_CERT_DIR value
func certDirsFor(dirs []string) string {
	var ok []string
	for _, d := range dirs {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			ok = append(ok, d)
		}
	}
	return strings.Join(ok, ":")
}

// IP_FREEBIND lets us bind before the alias address exists on br0; connections start once it is added
func listenTCP(addr string) (net.Listener, error) {
	lc := net.ListenConfig{Control: func(network, address string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(int(fd), syscall.SOL_IP, syscall.IP_FREEBIND, 1)
		})
		if err != nil {
			return err
		}
		return serr
	}}
	return lc.Listen(context.Background(), "tcp4", addr)
}

func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
