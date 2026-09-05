package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestKeyBase pins the two layouts the two callers depend on: the flat public
// one promote writes, and the channel-prefixed staging one the cut writes. A
// silently-wrong prefix here puts a staged build where the public prune lists
// it, or a promoted one where no installer looks.
func TestKeyBase(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prefix string
		want   string
	}{
		{"no prefix is the flat public layout", "", "clawee/v0.2.0.2026.09.04.deadbeef"},
		{"channel prefix", "beta", "clawee/beta/v0.2.0.2026.09.04.deadbeef"},
		{"stable is a real prefix, not an absence", "stable", "clawee/stable/v0.2.0.2026.09.04.deadbeef"},
		{"surrounding slashes never double a separator", "/beta/", "clawee/beta/v0.2.0.2026.09.04.deadbeef"},
		{"whitespace-only prefix is no prefix", "   ", "clawee/v0.2.0.2026.09.04.deadbeef"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config{comp: "clawee", stamp: "v0.2.0.2026.09.04.deadbeef", prefix: tc.prefix}
			if got := cfg.keyBase(); got != tc.want {
				t.Fatalf("keyBase() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestBuildManifestUsesKeyBase guards the manifest's internal paths against
// drifting from the keys actually uploaded — a manifest that names
// <comp>/<stamp> while the bytes live under <comp>/<channel>/<stamp> is a
// 404 for every installer that follows it.
func TestBuildManifestUsesKeyBase(t *testing.T) {
	cfg := config{comp: "claweed", stamp: "v1.0.0.2026.09.04.abcd1234", version: "1.0.0", prefix: "beta"}
	m := buildManifest(cfg, []string{"clawee-claweed-darwin-arm64.zip"})
	want := "claweed/beta/v1.0.0.2026.09.04.abcd1234"
	if m.Path != want {
		t.Fatalf("Path = %q, want %q", m.Path, want)
	}
	if m.SHA256Sums != want+"/"+sumsName {
		t.Fatalf("SHA256Sums = %q, want %q", m.SHA256Sums, want+"/"+sumsName)
	}
	if m.Minisig != want+"/"+minisigName {
		t.Fatalf("Minisig = %q, want %q", m.Minisig, want+"/"+minisigName)
	}
}

// TestDryRunNoManifest asserts the cut's posture end to end through run():
// the planned keys carry the channel prefix and latest.json is explicitly NOT
// among them. --no-manifest is the flag that keeps a cut private, so a
// regression that quietly wrote the manifest anyway would publish a staged
// build to every installer — this test is the pin on that.
func TestDryRunNoManifest(t *testing.T) {
	stage := stageDirWithArtifacts(t)
	out := runDryRun(t, config{
		comp: "clawee", version: "0.2.0", stamp: "v0.2.0.2026.09.04.deadbeef",
		stageDir: stage, bucket: "staging", prefix: "stable", noManifest: true, dryRun: true,
	})
	if !strings.Contains(out, "clawee/stable/v0.2.0.2026.09.04.deadbeef/SHA256SUMS.txt") {
		t.Fatalf("dry-run did not print the prefixed sums key:\n%s", out)
	}
	if strings.Contains(out, "clawee/latest.json  (") {
		t.Fatalf("--no-manifest still planned a latest.json upload:\n%s", out)
	}
	if !strings.Contains(out, "no manifest") {
		t.Fatalf("dry-run did not say the manifest is skipped:\n%s", out)
	}
	if !strings.Contains(out, "would upload 3 objects") {
		t.Fatalf("object count did not drop the manifest:\n%s", out)
	}
}

// TestDryRunWithManifest is the other half: without --no-manifest the public
// posture is unchanged (flat keys, latest.json planned) — the promote path
// must keep behaving exactly as it did before the seams existed.
func TestDryRunWithManifest(t *testing.T) {
	stage := stageDirWithArtifacts(t)
	out := runDryRun(t, config{
		comp: "clawee", version: "0.2.0", stamp: "v0.2.0.2026.09.04.deadbeef",
		stageDir: stage, bucket: "public", dryRun: true,
	})
	if !strings.Contains(out, "clawee/v0.2.0.2026.09.04.deadbeef/SHA256SUMS.txt") {
		t.Fatalf("flat public layout lost:\n%s", out)
	}
	if !strings.Contains(out, "clawee/latest.json") {
		t.Fatalf("manifest no longer planned on the public path:\n%s", out)
	}
	if !strings.Contains(out, "would upload 4 objects") {
		t.Fatalf("object count changed on the public path:\n%s", out)
	}
}

// stageDirWithArtifacts builds the minimum dist/<stamp>/ collectArtifacts
// accepts: one zip plus the sums and the signature over them.
func stageDirWithArtifacts(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"clawee-clawee-darwin-arm64.zip", sumsName, minisigName} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

func runDryRun(t *testing.T, cfg config) string {
	t.Helper()
	var buf bytes.Buffer
	if err := execute(cfg, &buf); err != nil {
		t.Fatalf("execute: %v", err)
	}
	return buf.String()
}
