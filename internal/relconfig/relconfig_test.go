package relconfig

import "testing"

func TestBinsClawee(t *testing.T) {
	got, err := Bins("clawee", "v0.1.90.2026.07.14.deadbeef")
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
		if b.Ldflags != "-X main.version=v0.1.90.2026.07.14.deadbeef" {
			t.Errorf("bin %s: ldflags %q", b.Name, b.Ldflags)
		}
	}
}

func TestBinsClaweed(t *testing.T) {
	got, err := Bins("claweed", "v0.1.34.2026.07.14.abc12345")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"claweed": "./cmd/claweed", "clawee-spawn": "./cmd/clawee-spawn", "claweed-updater": "./cmd/claweed-updater"}
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
	if _, err := Bins("nope", "v0"); err == nil {
		t.Fatal("expected error for unknown component")
	}
}

func TestTargets(t *testing.T) {
	if len(Targets()) != 4 {
		t.Fatalf("want 4 targets, got %d", len(Targets()))
	}
}
