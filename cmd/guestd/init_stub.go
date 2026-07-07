//go:build !linux

package main

import "errors"

// runInit only exists so non-Linux development builds compile; guestd's
// PID 1 mode is inherently Linux-only.
func runInit() error {
	return errors.New("guestd init mode is linux-only")
}
