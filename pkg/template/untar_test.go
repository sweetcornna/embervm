package template

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// tarEntry describes one entry for buildTar.
type tarEntry struct {
	name     string
	typeflag byte
	mode     int64
	body     string
	linkname string
}

func buildTar(t *testing.T, entries []tarEntry) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: e.typeflag,
			Mode:     e.mode,
			Linkname: e.linkname,
			Size:     int64(len(e.body)),
			Format:   tar.FormatPAX,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %q: %v", e.name, err)
		}
		if e.body != "" {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("write body %q: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	return &buf
}

func fixtureTar(t *testing.T) *bytes.Buffer {
	t.Helper()
	return buildTar(t, []tarEntry{
		{name: "bin/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "bin/busybox", typeflag: tar.TypeReg, mode: 0o755, body: "#!fake-binary"},
		{name: "bin/sh", typeflag: tar.TypeSymlink, mode: 0o777, linkname: "/bin/busybox"},
		{name: "bin/busybox2", typeflag: tar.TypeLink, mode: 0o755, linkname: "bin/busybox"},
		{name: "etc/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "etc/secret", typeflag: tar.TypeReg, mode: 0o640, body: "s3cr3t"},
	})
}

func TestUntarBasicTree(t *testing.T) {
	dst := t.TempDir()
	if err := Untar(dst, fixtureTar(t)); err != nil {
		t.Fatalf("Untar: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dst, "bin", "busybox"))
	if err != nil {
		t.Fatalf("read extracted file: %v", err)
	}
	if string(data) != "#!fake-binary" {
		t.Errorf("busybox content = %q", data)
	}

	fi, err := os.Stat(filepath.Join(dst, "bin", "busybox"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("busybox mode = %o, want 755", fi.Mode().Perm())
	}
	fi, err = os.Stat(filepath.Join(dst, "etc", "secret"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Errorf("secret mode = %o, want 640", fi.Mode().Perm())
	}

	// Symlink target is a guest-absolute path, preserved verbatim.
	link, err := os.Readlink(filepath.Join(dst, "bin", "sh"))
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if link != "/bin/busybox" {
		t.Errorf("symlink target = %q, want /bin/busybox", link)
	}

	// Hardlink shares the inode with its source.
	var st1, st2 syscall.Stat_t
	if err := syscall.Stat(filepath.Join(dst, "bin", "busybox"), &st1); err != nil {
		t.Fatalf("stat busybox: %v", err)
	}
	if err := syscall.Stat(filepath.Join(dst, "bin", "busybox2"), &st2); err != nil {
		t.Fatalf("stat busybox2: %v", err)
	}
	if st1.Ino != st2.Ino {
		t.Errorf("hardlink inode %d != source inode %d", st2.Ino, st1.Ino)
	}
}

func TestUntarRejectsEscapes(t *testing.T) {
	cases := []struct {
		name    string
		entries []tarEntry
	}{
		{"dotdot name", []tarEntry{
			{name: "../escape", typeflag: tar.TypeReg, mode: 0o644, body: "x"},
		}},
		{"absolute name", []tarEntry{
			{name: "/abs/escape", typeflag: tar.TypeReg, mode: 0o644, body: "x"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dst := t.TempDir()
			if err := Untar(dst, buildTar(t, tc.entries)); err == nil {
				t.Errorf("Untar accepted escaping entry, want error")
			}
		})
	}
}

// TestUntarSymlinkTraversalContained is the regression test for the
// tar-slip finding: a symlink whose target escapes dst, followed by a write
// "through" it, must land inside dst — never on the host path.
func TestUntarSymlinkTraversalContained(t *testing.T) {
	cases := []struct {
		name   string
		linkTo func(outside string) string
	}{
		{"absolute escaping symlink", func(outside string) string { return outside }},
		{"dotdot escaping symlink", func(string) string { return "../../../../../../../../tmp" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dst := t.TempDir()
			outside := t.TempDir() // a sibling that must stay untouched
			sentinel := filepath.Join(outside, "passwd")

			entries := []tarEntry{
				{name: "bin", typeflag: tar.TypeSymlink, mode: 0o777, linkname: tc.linkTo(outside)},
				{name: "bin/passwd", typeflag: tar.TypeReg, mode: 0o644, body: "pwned"},
			}
			// Must not error, and must not escape.
			if err := Untar(dst, buildTar(t, entries)); err != nil {
				t.Fatalf("Untar: %v", err)
			}
			if _, err := os.Stat(sentinel); err == nil {
				t.Fatalf("write escaped to %s", sentinel)
			}
			// The byte payload stayed under dst (clamped through the symlink).
			var found bool
			_ = filepath.WalkDir(dst, func(p string, d os.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					if b, _ := os.ReadFile(p); string(b) == "pwned" {
						found = true
					}
				}
				return nil
			})
			if !found {
				t.Errorf("payload not found anywhere under dst; extraction lost it")
			}
		})
	}
}

// TestUntarHardlinkTargetContained ensures a hardlink cannot point at a host
// file outside dst.
func TestUntarHardlinkTargetContained(t *testing.T) {
	dst := t.TempDir()
	entries := []tarEntry{
		{name: "f", typeflag: tar.TypeLink, mode: 0o644, linkname: "../../etc/passwd"},
	}
	// SecureJoin clamps the source to dst/etc/passwd, which does not exist,
	// so os.Link fails — the important part is no host file is linked.
	if err := Untar(dst, buildTar(t, entries)); err == nil {
		t.Errorf("Untar linked a nonexistent clamped source without error")
	}
}

func TestUntarLongPAXNames(t *testing.T) {
	long := strings.Repeat("d", 120) + "/" + strings.Repeat("f", 160)
	dst := t.TempDir()
	err := Untar(dst, buildTar(t, []tarEntry{
		{name: strings.Repeat("d", 120) + "/", typeflag: tar.TypeDir, mode: 0o755},
		{name: long, typeflag: tar.TypeReg, mode: 0o644, body: "deep"},
	}))
	if err != nil {
		t.Fatalf("Untar: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(long)))
	if err != nil {
		t.Fatalf("read long-named file: %v", err)
	}
	if string(data) != "deep" {
		t.Errorf("content = %q, want deep", data)
	}
}

func TestUntarIdempotentOverwrite(t *testing.T) {
	dst := t.TempDir()
	if err := Untar(dst, fixtureTar(t)); err != nil {
		t.Fatalf("first Untar: %v", err)
	}
	// Second extraction must replace symlinks rather than follow them and
	// overwrite regular files in place.
	if err := Untar(dst, fixtureTar(t)); err != nil {
		t.Fatalf("second Untar: %v", err)
	}
	link, err := os.Readlink(filepath.Join(dst, "bin", "sh"))
	if err != nil {
		t.Fatalf("readlink after overwrite: %v", err)
	}
	if link != "/bin/busybox" {
		t.Errorf("symlink target = %q, want /bin/busybox", link)
	}
}
