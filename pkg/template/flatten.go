package template

import (
	"context"
	"fmt"
	"io"
	"runtime"

	"github.com/google/go-containerregistry/pkg/crane"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
)

// flattenImage pulls ref from its registry (daemonless) and returns the
// flattened filesystem as a tar stream plus the image's runtime defaults.
// The caller must Close the reader. The platform follows the HOST arch:
// guests share the host CPU (KVM, no emulation), so an amd64 rootfs on an
// aarch64 host would boot to "exec format error".
func flattenImage(ctx context.Context, ref string) (io.ReadCloser, ImageConfig, error) {
	img, err := crane.Pull(ref,
		crane.WithContext(ctx),
		crane.WithPlatform(&v1.Platform{OS: "linux", Architecture: runtime.GOARCH}),
	)
	if err != nil {
		return nil, ImageConfig{}, fmt.Errorf("pull %s: %w", ref, err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		return nil, ImageConfig{}, fmt.Errorf("config of %s: %w", ref, err)
	}
	cfg := ImageConfig{
		Image:      ref,
		Env:        cf.Config.Env,
		Entrypoint: cf.Config.Entrypoint,
		Cmd:        cf.Config.Cmd,
		WorkingDir: cf.Config.WorkingDir,
		User:       cf.Config.User,
	}
	return mutate.Extract(img), cfg, nil
}
