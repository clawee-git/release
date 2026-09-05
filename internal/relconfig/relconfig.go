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
//
// Every binary is linked with -X main.channel=stable as well: the cli's
// cmd/clawee and cmd/clawee-updater parse that value at startup and REFUSE an
// empty one ("bad build: -X main.channel=\"\" is not a channel"), so a cut
// without it publishes a clawee that cannot start (this line found out on
// 2026-09-05, one cut after the cli adopted the check). main is the stable
// cut origin, so the value is the constant; the channel-aware rework on dev
// derives it from the source branch and supersedes this when it lands.
// A package without a main.channel symbol (claweed today) drops the flag
// silently, which is the harmless direction.
func Bins(comp, stamp string) ([]build.BinSpec, error) {
	v := "-X main.version=" + stamp + " -X main.channel=stable"
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
