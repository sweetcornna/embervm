package template

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ImageConfig is the subset of the OCI image config guestd may need at
// runtime. It is written to /etc/embervm/image.json inside the rootfs; M1 v0
// only records it (guestd exec defaults consume it in a later iteration).
type ImageConfig struct {
	Image      string   `json:"image"` // original reference
	Env        []string `json:"env,omitempty"`
	Entrypoint []string `json:"entrypoint,omitempty"`
	Cmd        []string `json:"cmd,omitempty"`
	WorkingDir string   `json:"working_dir,omitempty"`
	User       string   `json:"user,omitempty"`
}

// injectRuntime makes an extracted image tree bootable as an EmberVM guest:
// installs guestd (the PID 1, see cmd/guestd), records the image's runtime
// defaults, writes the static network identity files, and ensures the
// mount-point directories guestd expects (docker exports usually omit
// /proc, /sys, ...).
func injectRuntime(root, guestdPath string, cfg ImageConfig) error {
	for _, dir := range []string{"proc", "sys", "dev", "tmp", "run", "etc", "usr/local/bin", "etc/embervm"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(dir)), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}

	if err := copyFile(filepath.Join(root, "usr", "local", "bin", "guestd"), guestdPath, 0o755); err != nil {
		return fmt.Errorf("install guestd: %w", err)
	}

	manifest, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal image config: %w", err)
	}
	files := map[string][]byte{
		"etc/embervm/image.json": append(manifest, '\n'),
		// The guest address is per-sandbox-netns static (docs/zh/02 §4);
		// resolv.conf matches scripts/build-rootfs.sh's bench rootfs.
		"etc/resolv.conf": []byte("nameserver 8.8.8.8\n"),
		"etc/hostname":    []byte("ember\n"),
		"etc/hosts":       []byte("127.0.0.1 localhost\n172.16.0.2 ember\n"),
	}
	for rel, data := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", rel, err)
		}
	}
	return nil
}

func copyFile(dst, src string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}
