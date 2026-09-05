package relconfig

import (
	"fmt"

	"github.com/burrowee-git/release-kit/build"
	"github.com/clawee-git/core/channel"
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

// Bins returns the build.BinSpec list for comp on ch, mirroring tools/build.sh's
// binary->package map. GoWork is left empty (release-kit build.Compile defaults
// it to "off" — module mode, pinned tags).
//
// Both channel-bound halves come from core/channel, exactly as build.sh takes
// them from ./cmd/channel-names: the NAME on disk (clawee/claweeb) and the
// -X main.channel value the binary parses at startup. The PACKAGE is never
// channel-bound — a twin is one source built twice, and ./cmd/claweeb does not
// exist.
//
// The -X main.channel flag is not optional even for stable: core/channel.Parse
// refuses an empty string, so a binary built without it fails to start. This
// path shipped without it, which would have grounded every rkit-built binary
// the moment the cli and daemon adopted Parse.
func Bins(comp, stamp string, ch channel.Channel) ([]build.BinSpec, error) {
	n := channel.For(ch)
	if n.Client == "" {
		return nil, fmt.Errorf("unknown channel %q", ch)
	}
	v := "-X main.version=" + stamp + " -X main.channel=" + string(ch)
	switch comp {
	case "clawee":
		return []build.BinSpec{
			{Name: n.Client, Package: "./cmd/clawee", Ldflags: v},
			{Name: n.ClientUpdater, Package: "./cmd/clawee-updater", Ldflags: v},
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
			{Name: n.Daemon, Package: "./cmd/claweed", Ldflags: v},
			{Name: n.DaemonUpdater, Package: "./cmd/claweed-updater", Ldflags: v},
		}, nil
	}
	return nil, fmt.Errorf("unknown component %q", comp)
}
