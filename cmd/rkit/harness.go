package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/clawee-git/release/internal/relconfig"
)

// reproducibilityGuard prints the active toolchain (go version + GOFLAGS) and
// hard-fails on an obviously inconsistent one. release.sh's build.sh
// invocations and orchestrate's build.Compile both assume module mode with
// GOWORK=off per binary; a leaked vendor GOFLAGS or ambient GOWORK could
// silently resolve a different module graph on one side of the diff,
// invalidating the whole comparison.
func reproducibilityGuard(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, "go", "version").Output()
	if err != nil {
		return fmt.Errorf("go version: %w", err)
	}
	goVersion := strings.TrimSpace(string(out))
	goflags := os.Getenv("GOFLAGS")
	fmt.Printf("→ toolchain: %s (GOFLAGS=%q)\n", goVersion, goflags)
	if strings.Contains(goflags, "-mod=vendor") {
		return fmt.Errorf("reproducibility guard: GOFLAGS=%q forces vendor mode — release.sh and orchestrate both assume module mode", goflags)
	}
	if gowork := os.Getenv("GOWORK"); gowork != "" && gowork != "off" {
		return fmt.Errorf("reproducibility guard: GOWORK=%q must be unset or \"off\" — a leaked workspace can resolve a different module graph than the pinned-tag build orchestrate performs", gowork)
	}
	return nil
}

// runHarness runs the live release.sh oracle and orchestrate for the same
// component against the same repo state, then diffs their assembled zips for
// payload equivalence. Exits (via the returned error) non-zero on any
// per-target FAIL.
func runHarness(args []string) error {
	fs := flag.NewFlagSet("harness", flag.ContinueOnError)
	var comp, out, repo string
	fs.StringVar(&comp, "component", "", "clawee|claweed")
	fs.StringVar(&out, "out", "", "scratch output dir (default: a temp dir)")
	fs.StringVar(&repo, "repo", ".", "release repo worktree")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if comp == "" {
		return fmt.Errorf("--component is required")
	}
	srcDir := srcDirFor(comp)
	if srcDir == "" {
		return fmt.Errorf("unknown component %q (harness covers clawee|claweed)", comp)
	}

	ctx := context.Background()
	if err := reproducibilityGuard(ctx); err != nil {
		return err
	}

	repoAbs, err := filepath.Abs(repo)
	if err != nil {
		return err
	}
	if out == "" {
		out, err = os.MkdirTemp("", "release-harness-*")
		if err != nil {
			return err
		}
	}

	// 1. Run the live oracle: tools/release.sh <comp> --dry-run. Inherits the
	// parent env so any CLAWEE_SRC_* overrides resolved above apply
	// identically to the oracle and to orchestrate below. Needs a TEST
	// minisign key on disk at tools/testkeys/test.key — Task 8 provisions it
	// and runs this path live; here it's exercised only by the
	// RELEASE_HARNESS_LIVE-gated test.
	oracleCmd := exec.CommandContext(ctx, "bash", filepath.Join(repoAbs, "tools", "release.sh"), comp, "--dry-run")
	oracleCmd.Dir = repoAbs
	oracleCmd.Env = os.Environ()
	oracleCmd.Stdout = os.Stdout
	oracleCmd.Stderr = os.Stderr
	if err := oracleCmd.Run(); err != nil {
		return fmt.Errorf("oracle: release.sh %s --dry-run: %w", comp, err)
	}

	// 2. Run the candidate: orchestrate() into its own scratch subdir.
	candOut := filepath.Join(out, "candidate")
	res, err := orchestrate(ctx, Options{
		Component: comp, OutDir: candOut, RepoDir: repoAbs,
		SrcDir: srcDir,
		// harness parity — release.sh --dry-run does not gate; the CVE gate is
		// validated separately (Task 3 fixture) and stays mandatory for real cuts.
		SkipGate: true,
	})
	if err != nil {
		return fmt.Errorf("orchestrate %s: %w", comp, err)
	}

	// 3. Locate both zip dirs (same stamp on both sides — same versions file
	// + same src worktree revision) and diff.
	oracleZipDir := filepath.Join(repoAbs, "dist", res.Stamp)
	candZipDir := filepath.Join(candOut, res.Stamp)
	report, err := comparePayloads(oracleZipDir, candZipDir)
	if err != nil {
		return fmt.Errorf("comparePayloads: %w", err)
	}

	// Guard against a vacuous PASS: comparePayloads only reports on targets it
	// actually found zips for on at least one side, so a broken build (e.g. an
	// empty dist dir) would otherwise compare zero targets and fall through to
	// printReport's default "no FAILs" PASS. Require the full expected target
	// set before trusting the report at all.
	wantTargets := len(relconfig.Targets())
	if len(report.Targets) == 0 {
		return fmt.Errorf("harness compared zero targets for %s (stamp %s) — refusing to report PASS", comp, res.Stamp)
	}
	if len(report.Targets) != wantTargets {
		return fmt.Errorf("harness compared %d targets, expected %d — refusing to report PASS on a partial comparison", len(report.Targets), wantTargets)
	}

	if printReport(report) {
		return fmt.Errorf("harness: %s payload diverges from the release.sh oracle (stamp %s)", comp, res.Stamp)
	}
	return nil
}

// printReport prints a per-target PASS/FAIL table and reports whether any
// target failed.
func printReport(report Report) (failed bool) {
	for _, tr := range report.Targets {
		status := "PASS"
		if !tr.OK {
			status = "FAIL"
			failed = true
		}
		fmt.Printf("%-4s  %-34s  bins=%v install.sh=%v files=%v\n",
			status, tr.Target, tr.BinMismatch, tr.InstallShEqual, tr.FileSetEqual)
	}
	return failed
}
