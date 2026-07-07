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
	"path/filepath"
)

// Untar extracts a flattened filesystem tar into dst. It handles dirs,
// regular files, symlinks and hardlinks; preserves modes; applies uid/gid
// and device nodes only when running as root (unprivileged extraction skips
// them). Entry names and hardlink targets must stay inside dst; symlink
// TARGETS may be absolute or dot-dotted — they are guest paths, resolved at
// guest runtime, never during extraction.
func Untar(dst string, r io.Reader) error {
	tr := tar.NewReader(r)
	// Directory modes are applied after extraction: a read-only dir created
	// eagerly would block the files that follow it in the stream.
	type dirMode struct {
		path string
		mode fs.FileMode
	}
	var dirModes []dirMode

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}

		name := filepath.FromSlash(hdr.Name)
		if !filepath.IsLocal(name) {
			return fmt.Errorf("tar entry escapes root: %q", hdr.Name)
		}
		target := filepath.Join(dst, name)
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
			linkSrc := filepath.FromSlash(hdr.Linkname)
			if !filepath.IsLocal(linkSrc) {
				return fmt.Errorf("hardlink target escapes root: %q -> %q", hdr.Name, hdr.Linkname)
			}
			if err := removeIfExists(target); err != nil {
				return fmt.Errorf("replace %q: %w", hdr.Name, err)
			}
			if err := os.Link(filepath.Join(dst, linkSrc), target); err != nil {
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
