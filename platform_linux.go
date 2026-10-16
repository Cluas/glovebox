package main

import (
	"context"
	"net"
	"syscall"
)

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
