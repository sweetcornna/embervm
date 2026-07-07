package template

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeFakeGuestd(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "guestd")
	if err := os.WriteFile(path, []byte("fake-guestd-elf"), 0o755); err != nil {
		t.Fatalf("write fake guestd: %v", err)
	}
	return path
}

func TestInjectRuntime(t *testing.T) {
	root := t.TempDir()
	// Simulate a docker-export tree that already has /etc but lacks the
	// mount-point directories.
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := ImageConfig{
		Image:      "alpine:3.20",
		Env:        []string{"PATH=/usr/bin", "LANG=C.UTF-8"},
		Entrypoint: []string{"/bin/sh"},
		Cmd:        []string{"-l"},
		WorkingDir: "/root",
		User:       "0:0",
	}
	if err := injectRuntime(root, writeFakeGuestd(t), cfg); err != nil {
		t.Fatalf("injectRuntime: %v", err)
	}

	// guestd installed 0755 with content intact.
	bin := filepath.Join(root, "usr", "local", "bin", "guestd")
	data, err := os.ReadFile(bin)
	if err != nil {
		t.Fatalf("read injected guestd: %v", err)
	}
	if string(data) != "fake-guestd-elf" {
		t.Errorf("guestd content = %q", data)
	}
	fi, err := os.Stat(bin)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("guestd mode = %o, want 755", fi.Mode().Perm())
	}

	// image.json roundtrips the config.
	raw, err := os.ReadFile(filepath.Join(root, "etc", "embervm", "image.json"))
	if err != nil {
		t.Fatalf("read image.json: %v", err)
	}
	var got ImageConfig
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal image.json: %v", err)
	}
	if got.Image != cfg.Image || got.WorkingDir != cfg.WorkingDir ||
		len(got.Env) != 2 || got.Entrypoint[0] != "/bin/sh" || got.Cmd[0] != "-l" || got.User != "0:0" {
		t.Errorf("image.json roundtrip = %+v, want %+v", got, cfg)
	}

	// etc files.
	for path, want := range map[string]string{
		"etc/resolv.conf": "nameserver 8.8.8.8\n",
		"etc/hostname":    "ember\n",
	} {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			t.Errorf("read %s: %v", path, err)
			continue
		}
		if string(data) != want {
			t.Errorf("%s = %q, want %q", path, data, want)
		}
	}
	hosts, err := os.ReadFile(filepath.Join(root, "etc", "hosts"))
	if err != nil {
		t.Fatalf("read etc/hosts: %v", err)
	}
	for _, needle := range []string{"127.0.0.1", "localhost", "ember"} {
		if !containsLine(string(hosts), needle) {
			t.Errorf("etc/hosts missing %q; got %q", needle, hosts)
		}
	}

	// Mount-point dirs exist even though the tar lacked them.
	for _, dir := range []string{"proc", "sys", "dev", "tmp", "run"} {
		fi, err := os.Stat(filepath.Join(root, dir))
		if err != nil || !fi.IsDir() {
			t.Errorf("mount point %s missing (err=%v)", dir, err)
		}
	}
}

// containsLine reports whether any line of haystack has needle as a
// whitespace-separated field.
func containsLine(haystack, needle string) bool {
	for _, line := range strings.Split(haystack, "\n") {
		if slices.Contains(strings.Fields(line), needle) {
			return true
		}
	}
	return false
}
