//go:build !linux

package nodeagent

import (
	"errors"

	"github.com/embervm/embervm/pkg/nodeapi"
)

// New returns an error off Linux: the node agent drives Firecracker, netns,
// and cgroups, all of which are Linux-only. `embervm dev` and cmd/nodeagent
// still compile on other hosts so the rest of the toolchain (lint, unit
// tests) runs there.
func New(Config) (nodeapi.Agent, error) {
	return nil, errors.New("nodeagent is linux-only")
}
