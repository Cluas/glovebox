//go:build !linux

package main

import (
	"net"
	"syscall"
)

func listenTCP(addr string) (net.Listener, error) {
	return net.Listen("tcp4", addr)
}

func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
