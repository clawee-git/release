// Command channel-names prints one channel's names from core/channel as
// KEY=value lines, for the release kit's shell scripts to read.
//
// It exists so that the kit's channel-bound spellings — the twin binary names
// build.sh emits, and the placeholders release.sh substitutes into the daemon's
// inner installer — have exactly ONE source, the same table the binaries
// themselves derive from. The obvious alternative, a `case "$CHANNEL"` table in
// build.sh, is a second copy of core/channel written in shell: it drifts the
// first time either side adds a name, and the way it announces itself is a beta
// kit installing over a stable host.
//
// WHY NOT the daemon repo's tools/channelnames. That one resolves RUN_DIR
// through the daemon's internal/paths, which applies runtime.GOOS — correct for
// build-local.sh, which builds for the host. This kit cross-compiles four
// targets and renders an inner installer for each, so the OS is an ARGUMENT
// here, not the host's. Reading the daemon's helper would silently stamp the
// build host's run dir into a linux kit.
//
// Usage:
//
//	go run ./cmd/channel-names <stable|beta> [goos]
//
// goos defaults to the host's. Output (values single-quoted, sh-sourceable):
//
//	CHANNEL='beta'
//	CLIENT='claweeb'
//	...
package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/clawee-git/core/channel"
)

func main() {
	if len(os.Args) < 2 || len(os.Args) > 3 {
		fmt.Fprintln(os.Stderr, "usage: channel-names <stable|beta> [goos]")
		os.Exit(2)
	}
	c, err := channel.Parse(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "channel-names:", err)
		os.Exit(2)
	}
	goos := runtime.GOOS
	if len(os.Args) == 3 {
		goos = os.Args[2]
	}
	os.Stdout.WriteString(render(c, goos))
}

// render is the whole output, as one string, so a test can assert on it
// without running a process.
func render(c channel.Channel, goos string) string {
	n := channel.For(c)
	rows := [][2]string{
		{"CHANNEL", string(c)},
		{"CLIENT", n.Client},
		{"CLIENT_UPDATER", n.ClientUpdater},
		{"DAEMON", n.Daemon},
		{"DAEMON_UPDATER", n.DaemonUpdater},
		{"SERVICE", n.Service},
		{"CLIENT_CONFIG_FILE", n.ClientConfigFile},
		{"USER_DIR", n.UserDir},
		{"USER_DATA_DIR", n.UserDataDir},
		{"SYSTEM_ROOT", n.SystemRoot},
		{"SYSTEM_ETC", n.SystemEtc()},
		{"SYSTEM_BIN", n.SystemBin()},
		{"LABEL", n.LaunchdLabel},
		{"SYSTEMD_UNIT", n.SystemdUnit},
		{"RUN_DIR", n.RunDir(goos)},
	}
	var b strings.Builder
	for _, kv := range rows {
		fmt.Fprintf(&b, "%s='%s'\n", kv[0], strings.ReplaceAll(kv[1], "'", `'\''`))
	}
	return b.String()
}
