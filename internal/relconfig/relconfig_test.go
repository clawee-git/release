package relconfig

import (
	"testing"

	"github.com/clawee-git/core/channel"
)

func TestBinsClawee(t *testing.T) {
	got, err := Bins("clawee", "v0.1.90.2026.07.14.deadbeef", channel.Stable)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"clawee": "./cmd/clawee", "clawee-updater": "./cmd/clawee-updater"}
	if len(got) != len(want) {
		t.Fatalf("got %d bins, want %d", len(got), len(want))
	}
	for _, b := range got {
		if want[b.Name] != b.Package {
			t.Errorf("bin %s: package %q, want %q", b.Name, b.Package, want[b.Name])
		}
		// The channel ldflag is not optional even on stable: core/channel.Parse
		// refuses an empty string, so a binary built without it fails to start.
		if b.Ldflags != "-X main.version=v0.1.90.2026.07.14.deadbeef -X main.channel=stable" {
			t.Errorf("bin %s: ldflags %q", b.Name, b.Ldflags)
		}
	}
}

func TestBinsClaweed(t *testing.T) {
	got, err := Bins("claweed", "v0.1.34.2026.07.14.abc12345", channel.Stable)
	if err != nil {
		t.Fatal(err)
	}
	// TWO binaries: the setuid clawee-spawn helper is retired and its package is
	// gone from the daemon repo. The length check below is what makes this test
	// able to fail if it is ever added back.
	want := map[string]string{"claweed": "./cmd/claweed", "claweed-updater": "./cmd/claweed-updater"}
	if len(got) != len(want) {
		t.Fatalf("got %d bins, want %d", len(got), len(want))
	}
	for _, b := range got {
		if want[b.Name] != b.Package {
			t.Errorf("bin %s: package %q, want %q", b.Name, b.Package, want[b.Name])
		}
	}
}

func TestBinsUnknown(t *testing.T) {
	if _, err := Bins("nope", "v0", channel.Stable); err == nil {
		t.Fatal("expected error for unknown component")
	}
}

// An unrecognised channel yields core/channel's zero Names — a table of empty
// strings. Building binaries called "" is worse than refusing.
func TestBinsUnknownChannel(t *testing.T) {
	if _, err := Bins("clawee", "v0", channel.Channel("nightly")); err == nil {
		t.Fatal("expected error for unknown channel")
	}
}

// The twin is the same source under different names, with the channel burned in.
func TestBinsBetaNamesTheTwins(t *testing.T) {
	got, err := Bins("clawee", "v0.3.0.beta.2026.09.05.deadbeef", channel.Beta)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"claweeb": "./cmd/clawee", "claweeb-updater": "./cmd/clawee-updater"}
	if len(got) != len(want) {
		t.Fatalf("got %d bins, want %d", len(got), len(want))
	}
	for _, b := range got {
		if want[b.Name] != b.Package {
			t.Errorf("bin %s: package %q, want %q", b.Name, b.Package, want[b.Name])
		}
		if b.Ldflags != "-X main.version=v0.3.0.beta.2026.09.05.deadbeef -X main.channel=beta" {
			t.Errorf("bin %s: ldflags %q", b.Name, b.Ldflags)
		}
	}
}

func TestTargets(t *testing.T) {
	if len(Targets()) != 4 {
		t.Fatalf("want 4 targets, got %d", len(Targets()))
	}
}
