//go:build !unix

package main

import "os"

// fileIno is unknown here: a rotated log is then only noticed when it gets shorter
func fileIno(os.FileInfo) uint64 { return 0 }
