//go:build linux

package memsnap

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// dataExtents lists the data (non-hole) regions of f using SEEK_DATA /
// SEEK_HOLE. Diff extraction depends on page-granular (4 KiB) hole
// reporting, which ext4 and tmpfs provide; the node agent probes for it at
// startup. Coarser granularity (e.g. a ZFS dataset with recordsize=16k)
// would round dirty extents up and let materialized zeros clobber clean
// parent pages, so snapshot staging directories must not live there.
func dataExtents(f *os.File) ([]extent, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	if size == 0 {
		return nil, nil
	}
	fd := int(f.Fd())
	var exts []extent
	off := int64(0)
	for off < size {
		dataOff, err := unix.Seek(fd, off, unix.SEEK_DATA)
		if err != nil {
			if errors.Is(err, unix.ENXIO) {
				break // only holes remain
			}
			return nil, fmt.Errorf("SEEK_DATA at %d: %w", off, err)
		}
		if dataOff >= size {
			break
		}
		holeOff, err := unix.Seek(fd, dataOff, unix.SEEK_HOLE)
		if err != nil {
			return nil, fmt.Errorf("SEEK_HOLE at %d: %w", dataOff, err)
		}
		if holeOff > size {
			holeOff = size
		}
		exts = append(exts, extent{dataOff, holeOff - dataOff})
		off = holeOff
	}
	return exts, nil
}
