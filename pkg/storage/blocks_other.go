//go:build !unix

package storage

import "io/fs"

// blocks512 cannot be determined without unix stat; report -1 so callers
// treat sparseness as unknown rather than failing.
func blocks512(fs.FileInfo) int64 { return -1 }
