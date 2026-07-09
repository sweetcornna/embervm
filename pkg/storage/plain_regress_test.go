package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestPlainEnsureTemplateNeverRewritesInPlace: re-importing must not
// truncate-and-rewrite the live template file a concurrent CloneSandbox may
// be reading — an existing template short-circuits, a first import stages
// and renames.
func TestPlainEnsureTemplateNeverRewritesInPlace(t *testing.T) {
	ctx := context.Background()
	b := NewPlainBackend(t.TempDir())

	src1 := filepath.Join(t.TempDir(), "v1.ext4")
	if err := os.WriteFile(src1, []byte("template-v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := b.EnsureTemplate(ctx, "tpl1", src1); err != nil {
		t.Fatalf("first EnsureTemplate: %v", err)
	}

	// Second import with different bytes: the live template must win.
	src2 := filepath.Join(t.TempDir(), "v2.ext4")
	if err := os.WriteFile(src2, []byte("template-v2-DIFFERENT"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := b.EnsureTemplate(ctx, "tpl1", src2); err != nil {
		t.Fatalf("second EnsureTemplate: %v", err)
	}
	got, err := os.ReadFile(b.templateRootfs("tpl1"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "template-v1" {
		t.Fatalf("live template rewritten in place: %q", got)
	}

	// No staging litter left behind.
	entries, err := os.ReadDir(filepath.Dir(b.templateRootfs("tpl1")))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "rootfs.ext4" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("template dir litter: %v", names)
	}
}
