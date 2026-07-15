package main

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestReproducibilityGuard covers reproducibilityGuard's positive and negative
// paths. srcDirFor's own env-override/default coverage lives in build_test.go's
// TestSrcDirFor (Task 5) — not duplicated here.
func TestReproducibilityGuard(t *testing.T) {
	ctx := context.Background()

	t.Run("GOWORK=off ok", func(t *testing.T) {
		t.Setenv("GOFLAGS", "")
		t.Setenv("GOWORK", "off")
		if err := reproducibilityGuard(ctx); err != nil {
			t.Fatalf("GOWORK=off should pass: %v", err)
		}
	})

	t.Run("GOWORK unset ok", func(t *testing.T) {
		t.Setenv("GOFLAGS", "")
		os.Unsetenv("GOWORK")
		if err := reproducibilityGuard(ctx); err != nil {
			t.Fatalf("GOWORK unset should pass: %v", err)
		}
	})

	t.Run("GOWORK=/tmp/x errors", func(t *testing.T) {
		t.Setenv("GOFLAGS", "")
		t.Setenv("GOWORK", "/tmp/x")
		err := reproducibilityGuard(ctx)
		if err == nil || !strings.Contains(err.Error(), "GOWORK") {
			t.Fatalf("GOWORK=/tmp/x should error mentioning GOWORK, got %v", err)
		}
	})

	t.Run("GOFLAGS=-mod=vendor errors", func(t *testing.T) {
		t.Setenv("GOFLAGS", "-mod=vendor")
		os.Unsetenv("GOWORK")
		err := reproducibilityGuard(ctx)
		if err == nil || !strings.Contains(err.Error(), "vendor") {
			t.Fatalf("GOFLAGS=-mod=vendor should error mentioning vendor, got %v", err)
		}
	})
}

// TestRunHarnessLiveClawee runs the full oracle-vs-candidate harness for the
// clawee component: it shells out to the real tools/release.sh --dry-run,
// needs a TEST minisign key on disk, and reads the real component source
// worktrees (or their CLAWEE_SRC_*/CLAWEE_SRC_CLAWEED overrides). It never
// runs by default — Task 8 is where this goes live; the comparePayloads unit
// tests in diff_test.go are this task's real coverage. Set
// RELEASE_HARNESS_LIVE=1 and RELEASE_REPO_DIR to opt in.
func TestRunHarnessLiveClawee(t *testing.T) {
	if os.Getenv("RELEASE_HARNESS_LIVE") != "1" {
		t.Skip("set RELEASE_HARNESS_LIVE=1 (and RELEASE_REPO_DIR) to run the live oracle-vs-candidate harness — see Task 8")
	}
	repo := os.Getenv("RELEASE_REPO_DIR")
	if repo == "" {
		t.Fatal("RELEASE_HARNESS_LIVE=1 requires RELEASE_REPO_DIR to point at a clawee release worktree")
	}
	if err := runHarness([]string{"--component", "clawee", "--repo", repo}); err != nil {
		t.Fatal(err)
	}
}
