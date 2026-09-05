package main

import (
	"strings"
	"testing"

	"github.com/clawee-git/core/channel"
)

// parse turns the rendered output back into a map, the way a shell `eval`
// would read it — so a test asserts on what a caller actually gets, not on a
// Go value the renderer happened to hold.
func parse(t *testing.T, out string) map[string]string {
	t.Helper()
	m := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("line %q is not KEY=value", line)
		}
		if !strings.HasPrefix(v, "'") || !strings.HasSuffix(v, "'") {
			t.Fatalf("value for %s is not single-quoted: %q", k, v)
		}
		m[k] = strings.Trim(v, "'")
	}
	return m
}

func TestStableNames(t *testing.T) {
	got := parse(t, render(channel.Stable, "darwin"))
	want := map[string]string{
		"CHANNEL": "stable", "CLIENT": "clawee", "CLIENT_UPDATER": "clawee-updater",
		"DAEMON": "claweed", "DAEMON_UPDATER": "claweed-updater",
		"SYSTEM_BIN": "/usr/local/clawee/bin", "SYSTEM_ETC": "/usr/local/clawee/etc",
		"LABEL": "org.clawee.claweed", "SYSTEMD_UNIT": "claweed.service",
		"RUN_DIR": "/var/run/claweed",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestBetaNames(t *testing.T) {
	got := parse(t, render(channel.Beta, "linux"))
	want := map[string]string{
		"CHANNEL": "beta", "CLIENT": "claweeb", "CLIENT_UPDATER": "claweeb-updater",
		"DAEMON": "claweedb", "DAEMON_UPDATER": "claweedb-updater",
		"SYSTEM_BIN": "/usr/local/clawee/beta/bin", "SYSTEM_ETC": "/usr/local/clawee/beta/etc",
		"LABEL": "org.clawee.beta.claweed", "SYSTEMD_UNIT": "claweed-beta.service",
		"RUN_DIR": "/run/claweed/beta",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

// The OS is an argument, not the host's: the kit renders an inner installer per
// cross-compiled target, and a linux kit stamped with a darwin run dir names a
// directory the daemon will never bind under.
func TestRunDirFollowsTheArgumentOS(t *testing.T) {
	for _, c := range []channel.Channel{channel.Stable, channel.Beta} {
		d := parse(t, render(c, "darwin"))["RUN_DIR"]
		l := parse(t, render(c, "linux"))["RUN_DIR"]
		if d == l {
			t.Errorf("%s: darwin and linux both render RUN_DIR=%q", c, d)
		}
		if !strings.HasPrefix(d, "/var/run/") || !strings.HasPrefix(l, "/run/") {
			t.Errorf("%s: darwin=%q linux=%q", c, d, l)
		}
	}
}

// Every value the kit substitutes must differ between channels except the ones
// core/channel deliberately shares (UserDir), or the twin quietly writes into
// stable's tree.
func TestNoChannelBoundValueIsShared(t *testing.T) {
	s := parse(t, render(channel.Stable, "linux"))
	b := parse(t, render(channel.Beta, "linux"))
	shared := map[string]bool{"USER_DIR": true}
	for k, sv := range s {
		if shared[k] {
			continue
		}
		if b[k] == sv {
			t.Errorf("%s is %q on both channels", k, sv)
		}
	}
}

// An empty or unknown channel must not render: core/channel's For returns the
// zero Names for one, and printing a table of empty strings would substitute
// empty paths into an installer.
func TestParseIsTheOnlyEntry(t *testing.T) {
	for _, in := range []string{"", "Beta", "nightly", "beta "} {
		if _, err := channel.Parse(in); err == nil {
			t.Errorf("Parse(%q) accepted an invalid channel", in)
		}
	}
}
