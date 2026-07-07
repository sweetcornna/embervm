//go:build !linux

package template

import "archive/tar"

// Ownership and device nodes only matter for production builds, which run
// as root on Linux under the node agent; development hosts skip both.
func lchownAsRoot(string, int, int) error  { return nil }
func makeDevice(string, *tar.Header) error { return nil }
