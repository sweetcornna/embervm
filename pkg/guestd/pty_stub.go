//go:build !linux

package guestd

import "errors"

// startShell exists off Linux only so the portable handler compiles for unit
// tests and lint; the guest is always Linux.
func startShell(int, int) (termProc, error) {
	return nil, errors.New("terminal requires linux")
}
