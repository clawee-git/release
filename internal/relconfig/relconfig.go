package relconfig

import (
	"fmt"

	"github.com/burrowee-git/release-kit/build"
)

// Components lists every releasable clawee component. clawee has no dispatcher
// and no console-gated component, so this is the whole set.
var Components = []string{"clawee", "claweed"}

func Targets() []build.Target {
	return []build.Target{
		{OS: "darwin", Arch: "arm64"}, {OS: "darwin", Arch: "amd64"},
		{OS: "linux", Arch: "arm64"}, {OS: "linux", Arch: "amd64"},
	}
}

// Bins returns the build.BinSpec list for comp, mirroring tools/build.sh's
// binary->package map. GoWork is left empty (release-kit build.Compile defaults
// it to "off" — module mode, pinned tags).
func Bins(comp, stamp string) ([]build.BinSpec, error) {
	v := "-X main.version=" + stamp
	switch comp {
	case "clawee":
		return []build.BinSpec{
			{Name: "clawee", Package: "./cmd/clawee", Ldflags: v},
			{Name: "clawee-updater", Package: "./cmd/clawee-updater", Ldflags: v},
		}, nil
	// claweed is TWO binaries, not three. It shipped a third — the setuid-root
	// clawee-spawn helper an unprivileged daemon execed to gain the privilege to
	// fork a tenant session. The daemon is root itself now and forks its own
	// per-user children, so the helper was retired in the daemon repo: the
	// ./cmd/clawee-spawn package is gone. Naming a package that no longer exists
	// fails the cut at compile with "directory not found", so nothing may be
	// added back here that the daemon does not build.
	case "claweed":
		return []build.BinSpec{
			{Name: "claweed", Package: "./cmd/claweed", Ldflags: v},
			{Name: "claweed-updater", Package: "./cmd/claweed-updater", Ldflags: v},
		}, nil
	}
	return nil, fmt.Errorf("unknown component %q", comp)
}
