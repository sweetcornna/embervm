//go:build !linux

package memsnap

import (
	"errors"
	"os"
)

// Diff extraction needs page-granular SEEK_DATA/SEEK_HOLE, which only the
// Linux staging filesystems (ext4/tmpfs) are verified to provide. macOS/APFS
// reports materialized extents for sparse files (observed: a truncated file
// with two written chunks reports one whole-file data extent), which would
// silently corrupt partially-dirty chunks — so refuse instead.
func dataExtents(*os.File) ([]extent, error) {
	return nil, errors.New("memsnap: diff extraction requires SEEK_DATA with page-granular holes (linux)")
}
