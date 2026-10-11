package serve

import (
	"debug/elf"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wstein/workharbor/internal/config"
	"github.com/wstein/workharbor/internal/runtime"
	"github.com/wstein/workharbor/internal/runtime/apple"
	"github.com/wstein/workharbor/internal/runtime/docker"
)

// DockerBuild is disposable build/cache storage outside the workspace. Outputs
// intended to survive are written to the workspace by the repository's tools.
const DockerBuild = "/var/tmp/whr/build"

func configuredRuntime(c *config.Config, bin string) (runtime.Adapter, error) {
	switch c.Runtime.Backend {
	case "", "apple":
		return apple.New(Owner, apple.WithShim(bin+"/whr-shim"))
	case "docker":
		if c.Environment.Image == "" {
			return nil, errors.New("runtime docker requires environment.image naming an existing stock language image; automatic image builds are unsupported")
		}
		if !filepath.IsAbs(c.Runtime.Toolbox) {
			return nil, errors.New("runtime docker requires an absolute existing BundleDock toolbox directory")
		}
		info, err := os.Stat(c.Runtime.Toolbox)
		if err != nil || !info.IsDir() {
			return nil, errors.New("runtime docker requires an existing BundleDock toolbox directory")
		}
		shim := filepath.Join(c.Roots.ToolStore, strings.TrimPrefix(bin, ToolsMount+"/"), "whr-shim")
		binary, err := elf.Open(shim)
		if err != nil {
			return nil, errors.New("runtime docker requires a pre-existing Linux ELF whr-shim in the selected read-only agent tool profile")
		}
		defer func() { _ = binary.Close() }()
		if binary.Class != elf.ELFCLASS64 || (binary.Machine != elf.EM_AARCH64 && binary.Machine != elf.EM_X86_64) {
			return nil, errors.New("runtime docker requires a supported 64-bit Linux guest shim; host binaries are not guest prerequisites")
		}
		return docker.New(Owner, c.Runtime.Endpoint, bin+"/whr-shim")
	default:
		return nil, fmt.Errorf("unsupported runtime backend %q", c.Runtime.Backend)
	}
}

func backendBuildDir(c *config.Config) string {
	if c.Runtime.IsDocker() {
		return DockerBuild
	}
	return GuestBuild
}

// prepareRuntimeSpec is the trusted factory boundary for backend policy choices.
// Repository specs cannot select D62's Docker outbound exception themselves.
func prepareRuntimeSpec(c *config.Config, opts runtime.PrepareOptions, s runtime.Spec) (runtime.PreparedSpec, error) {
	if c.Runtime.IsDocker() {
		opts.DockerOutbound = &runtime.DockerOutbound{Name: s.Network.Name, Owner: Owner}
	}
	return runtime.Prepare(opts, s)
}
