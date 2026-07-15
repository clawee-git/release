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
	case "claweed":
		return []build.BinSpec{
			{Name: "claweed", Package: "./cmd/claweed", Ldflags: v},
			{Name: "clawee-spawn", Package: "./cmd/clawee-spawn", Ldflags: v},
			{Name: "claweed-updater", Package: "./cmd/claweed-updater", Ldflags: v},
		}, nil
	}
	return nil, fmt.Errorf("unknown component %q", comp)
}
