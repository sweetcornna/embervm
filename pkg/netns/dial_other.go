//go:build !linux

package netns

import (
	"context"
	"errors"
	"net"
)

// DialContext is linux-only (it relies on setns); development hosts return
// an error so the pool bookkeeping still compiles and tests elsewhere.
func (l Lease) DialContext(_ context.Context, _, _ string) (net.Conn, error) {
	return nil, errors.New("netns dialing is linux-only")
}
