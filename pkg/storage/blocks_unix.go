//go:build unix

package storage

import (
	"io/fs"
	"syscall"
)

// blocks512 returns the number of 512-byte blocks actually allocated to the
// file, exposing sparseness (a hole-only file reports ~0 despite a large
// apparent size).
func blocks512(fi fs.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int64(st.Blocks)
	}
	return -1
}
