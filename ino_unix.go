//go:build unix

package main

import (
	"os"
	"syscall"
)

// fileIno identifies a file across renames, so log shipping can finish a rotated log
func fileIno(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}
