// Package template turns Docker/OCI images into bootable EmberVM rootfs
// images: pull+flatten (crane) → safe tar extraction → guestd injection →
// mkfs.ext4. Everything after the registry pull works offline so unit tests
// never touch the network (see the phase 2 plan under docs/superpowers).
package template

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	securejoin "github.com/cyphar/filepath-securejoin"
)

// maxUntarBytes caps the cumulative extracted size of one image filesystem
// (generous: real rootfs images are single-digit GiB). A var so tests can
// exercise the cap without a 16 GiB fixture.
var maxUntarBytes int64 = 16 << 30

// safeTarget resolves the on-disk path for a tar entry name, scoped to dst.
// The PARENT directory chain is resolved with SecureJoin (following existing
// symlinks but clamping any escape back inside dst); the final component is
// kept literal so an entry replaces whatever sits at that exact path instead
// of being written through a pre-existing symlink there. A malicious
// "bin -> /etc" parent is thus clamped to dst/etc, while re-extracting a
// symlink entry replaces it in place rather than following it.
func safeTarget(dst, name string) (string, error) {
	clean := path.Clean("/" + name) // collapses .. and leading slashes, always rooted
	dir, base := path.Split(clean)  // dir keeps a trailing slash; base is the literal leaf
	parent, err := securejoin.SecureJoin(dst, dir)
	if err != nil {
		return "", err
	}
	if base == "" { // directory entry like "bin/" → clean "/bin", base "bin"; only "/" hits this
		return parent, nil
	}
	return filepath.Join(parent, base), nil
}

// Untar extracts a flattened filesystem tar into dst. It handles dirs,
// regular files, symlinks and hardlinks; preserves modes; applies uid/gid
// and device nodes only when running as root (unprivileged extraction skips
// them).
//
// Extraction is hardened against tar-slip: every on-disk path is resolved
// with securejoin.SecureJoin, which follows symlinks already present in the
// tree but clamps any component that would escape dst back inside it. A
// malicious "bin -> /etc" symlink followed by a "bin/passwd" file therefore
// writes dst/etc/passwd, never the host's /etc/passwd. Symlink and hardlink
// TARGET strings are written verbatim (they are guest paths); only the
// resolution used to place bytes on the host is constrained.
func Untar(dst string, r io.Reader) error {
	tr := tar.NewReader(r)
	// Directory modes are applied after extraction: a read-only dir created
	// eagerly would block the files that follow it in the stream.
	type dirMode struct {
		path string
		mode fs.FileMode
	}
	var dirModes []dirMode
	var extracted int64

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		// Cumulative size cap: image content is attacker-supplied and a tiny
		// compressed layer can expand enormously, filling the host work dir.
		// hdr.Size is authoritative — tar.Reader hands out at most Size
		// bytes per entry.
		if extracted += hdr.Size; extracted > maxUntarBytes {
			return fmt.Errorf("tar expands past %d bytes: refusing (decompression bomb?)", maxUntarBytes)
		}

		// Reject obviously-hostile entry names early for a clear error;
		// SecureJoin below is the actual containment guarantee.
		if name := filepath.FromSlash(hdr.Name); name != "" && !filepath.IsLocal(name) {
			return fmt.Errorf("tar entry escapes root: %q", hdr.Name)
		}
		target, err := safeTarget(dst, hdr.Name)
		if err != nil {
			return fmt.Errorf("resolve %q: %w", hdr.Name, err)
		}
		mode := hdr.FileInfo().Mode()

		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("mkdir parents for %q: %w", hdr.Name, err)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("mkdir %q: %w", hdr.Name, err)
			}
			dirModes = append(dirModes, dirMode{target, mode.Perm()})

		case tar.TypeReg:
			if err := writeRegular(target, tr, mode.Perm()); err != nil {
				return fmt.Errorf("extract %q: %w", hdr.Name, err)
			}

		case tar.TypeSymlink:
			// Remove any existing entry so we never follow a stale symlink.
			if err := removeIfExists(target); err != nil {
				return fmt.Errorf("replace %q: %w", hdr.Name, err)
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return fmt.Errorf("symlink %q: %w", hdr.Name, err)
			}

		case tar.TypeLink:
			// The hardlink source is resolved scoped to dst as well, so it
			// can never point at a host file outside the extraction root.
			linkSrc, err := safeTarget(dst, hdr.Linkname)
			if err != nil {
				return fmt.Errorf("resolve hardlink target %q: %w", hdr.Linkname, err)
			}
			if err := removeIfExists(target); err != nil {
				return fmt.Errorf("replace %q: %w", hdr.Name, err)
			}
			if err := os.Link(linkSrc, target); err != nil {
				return fmt.Errorf("hardlink %q: %w", hdr.Name, err)
			}

		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			if err := makeDevice(target, hdr); err != nil {
				return fmt.Errorf("mknod %q: %w", hdr.Name, err)
			}

		default:
			// XGlobalHeader and friends: nothing to materialize.
			continue
		}

		if err := lchownAsRoot(target, hdr.Uid, hdr.Gid); err != nil {
			return fmt.Errorf("chown %q: %w", hdr.Name, err)
		}
	}

	// Deepest-first so a parent going read-only cannot block its children.
	for i := len(dirModes) - 1; i >= 0; i-- {
		if err := os.Chmod(dirModes[i].path, dirModes[i].mode); err != nil {
			return fmt.Errorf("chmod dir: %w", err)
		}
	}
	return nil
}

func writeRegular(target string, r io.Reader, perm fs.FileMode) error {
	if err := removeIfExists(target); err != nil {
		return err
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// The umask may have masked bits off at creation.
	return os.Chmod(target, perm)
}

func removeIfExists(target string) error {
	err := os.Remove(target)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
