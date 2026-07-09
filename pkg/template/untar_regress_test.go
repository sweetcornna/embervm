package template

import (
	"archive/tar"
	"strings"
	"testing"
)

// TestUntarRejectsOversizedContent pins the cumulative extraction cap:
// image content is attacker-supplied, and an expansion past the cap must
// abort instead of filling the host work dir.
func TestUntarRejectsOversizedContent(t *testing.T) {
	old := maxUntarBytes
	maxUntarBytes = 32 // tiny cap for the test
	defer func() { maxUntarBytes = old }()

	buf := buildTar(t, []tarEntry{
		{name: "a", typeflag: tar.TypeReg, mode: 0o644, body: strings.Repeat("x", 20)},
		{name: "b", typeflag: tar.TypeReg, mode: 0o644, body: strings.Repeat("y", 20)},
	})
	err := Untar(t.TempDir(), buf)
	if err == nil || !strings.Contains(err.Error(), "expands past") {
		t.Fatalf("Untar over the cap = %v, want size-cap refusal", err)
	}

	// Under the cap the same shape extracts fine.
	buf = buildTar(t, []tarEntry{
		{name: "a", typeflag: tar.TypeReg, mode: 0o644, body: strings.Repeat("x", 10)},
	})
	if err := Untar(t.TempDir(), buf); err != nil {
		t.Fatalf("Untar under the cap: %v", err)
	}
}
