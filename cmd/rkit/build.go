package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/burrowee-git/release-kit/build"
	"github.com/burrowee-git/release-kit/checksum"
	"github.com/burrowee-git/release-kit/minisign"
	"github.com/burrowee-git/release-kit/sign"
	"github.com/burrowee-git/release-kit/vulncheck"

	"github.com/clawee-git/release/internal/relconfig"
)

type Options struct {
	Component, OutDir, RepoDir string
	// SrcDir is the COMPONENT source worktree (e.g. cli/code/cli) — distinct
	// from RepoDir, which is the release repo holding versions/, inner/, and
	// tools/. Defaults to RepoDir when empty, so the fixture-based
	// orchestrate tests (which double one dir as both) keep working
	// unchanged.
	SrcDir      string
	MinisignKey string
	// SkipGate bypasses the mandatory vulncheck.Gate. Set ONLY by the harness
	// (runHarness): release.sh --dry-run does not run the CVE gate, so gating on
	// one side of the payload diff would abort orchestrate before it builds and
	// make an apples-to-apples comparison impossible. The CVE gate is validated
	// separately (Task 3 fixture) and stays mandatory for real cuts — the zero
	// value (false) keeps `run` fail-closed.
	SkipGate bool
	// Apple selects the Developer-ID signer (selectSigner) for build.Compile and
	// gates darwin zips through Notarizer.Notarize after assembly. Zero value
	// (false) keeps the existing ad-hoc, non-notarized behavior.
	Apple bool
	// DryRun, when Apple is set, skips the real notarize submission (logs intent
	// instead) — a dry run's artifacts are throwaway and notarization is a real
	// Apple API call.
	DryRun bool
}

type Result struct {
	Stamp         string
	Zips          []string
	Sums, Minisig string
}

// buildOpts configures `rkit build` — the real release-cut entry point:
// output lands at <RepoDir>/dist/<stamp>/ (never an arbitrary --out), an
// optional version bump shells the proven tools/version.sh with a
// revert-on-failure/dry-run trap, and the CVE gate runs by DEFAULT (the
// opposite of harness's SkipGate, which exists only for release.sh parity).
type buildOpts struct {
	Component, RepoDir, SrcDir, SignKey string
	Apple, DryRun, NoVulncheck          bool
	// Bump is "", "patch", "minor", or "major" — the tools/version.sh
	// --bump-<kind> action to run before stamping. Empty means no bump.
	Bump string
}

func runBuild(args []string) error {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	var o buildOpts
	fs.StringVar(&o.Component, "component", "", "clawee|claweed")
	fs.StringVar(&o.RepoDir, "repo", ".", "release repo worktree")
	fs.StringVar(&o.SrcDir, "src", "", "component source worktree (default: resolved from CLAWEE_SRC_<COMP>)")
	fs.StringVar(&o.SignKey, "sign-key", "", "minisign secret key (required for a real cut; --dry-run defaults to the TEST key)")
	fs.BoolVar(&o.Apple, "apple", false, "notarize macOS binaries")
	fs.BoolVar(&o.DryRun, "dry-run", false, "build without bumping the version or requiring a real sign key")
	fs.BoolVar(&o.NoVulncheck, "no-vulncheck", false, "skip the CVE gate (default: the gate runs)")
	bumpPatch := fs.Bool("bump-patch", false, "bump the component's patch version before building")
	bumpMinor := fs.Bool("bump-minor", false, "bump the component's minor version before building (prompts unless CLAWEE_RELEASE_YES=1)")
	bumpMajor := fs.Bool("bump-major", false, "bump the component's major version before building (prompts unless CLAWEE_RELEASE_YES=1)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if (*bumpPatch && *bumpMinor) || (*bumpPatch && *bumpMajor) || (*bumpMinor && *bumpMajor) {
		return fmt.Errorf("only one of --bump-patch|--bump-minor|--bump-major may be set")
	}
	switch {
	case *bumpPatch:
		o.Bump = "patch"
	case *bumpMinor:
		o.Bump = "minor"
	case *bumpMajor:
		o.Bump = "major"
	}
	if o.SrcDir == "" {
		o.SrcDir = srcDirFor(o.Component)
	}
	return buildRun(o)
}

// srcDirFor resolves a component's source worktree exactly like
// tools/release.sh: SRC_CLAWEE=${CLAWEE_SRC_CLAWEE:-<default>},
// SRC_CLAWEED=${CLAWEE_SRC_CLAWEED:-<default>}. Shared with the harness
// (Task 6) so both build a real component with the same resolution rule.
func srcDirFor(comp string) string {
	env := func(key, def string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return def
	}
	switch comp {
	case "clawee":
		return env("CLAWEE_SRC_CLAWEE", "/Volumes/MacintoshED/Workstation/Coding/Clawee/cli/code/cli")
	case "claweed":
		return env("CLAWEE_SRC_CLAWEED", "/Volumes/MacintoshED/Workstation/Coding/Clawee/daemon/code/daemon")
	}
	return ""
}

// buildRun is the testable seam behind runBuild. It resolves dirs, optionally
// bumps the component's version (registering a revert that fires on error or
// --dry-run), runs the CVE gate unless NoVulncheck, then reuses orchestrate to
// build+assemble+checksum+sign into <RepoDir>/dist/<stamp>/.
func buildRun(o buildOpts) (err error) {
	if o.SrcDir == "" {
		o.SrcDir = o.RepoDir
	}

	// Fail fast: a real cut requires a real sign key. Check this before the
	// bump + CVE gate so a doomed real cut doesn't waste either of them.
	// --dry-run defaults to the TEST key further down, where it's used.
	if !o.DryRun && o.SignKey == "" {
		return fmt.Errorf("--sign-key is required for a real build (only --dry-run defaults to the test key)")
	}
	if !o.DryRun {
		if _, err := os.Stat(o.SignKey); err != nil {
			return fmt.Errorf("--sign-key %s: %w", o.SignKey, err)
		}
	}

	// Revert the version bump if the build fails, or unconditionally on
	// --dry-run — a dry run must never leave a bumped versions/<comp> behind.
	// Registered BEFORE the bump step below so it also covers the bump
	// step's own failure (version.sh writes the file then `git add` fails).
	defer func() {
		if err != nil || o.DryRun {
			exec.Command("git", "-C", o.RepoDir, "restore", "--staged", "--worktree", "versions/"+o.Component).Run()
		}
	}()

	if !o.DryRun && o.Bump != "" {
		cmd := exec.Command("bash", filepath.Join(o.RepoDir, "tools", "version.sh"), o.Component, "--bump-"+o.Bump)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("version bump: %w", err)
		}
	}

	ctx := context.Background()
	stamp, err := relconfig.Stamp(ctx, filepath.Join(o.RepoDir, "versions", o.Component), o.SrcDir)
	if err != nil {
		return err
	}
	distDir := filepath.Join(o.RepoDir, "dist", stamp)

	// CVE gate — ON BY DEFAULT for a real build (unlike harness, which
	// SkipGates for release.sh --dry-run parity). --no-vulncheck bypasses it.
	if !o.NoVulncheck {
		if err = vulncheck.Gate(ctx, []vulncheck.Module{{Name: o.Component, Dir: o.SrcDir}},
			vulncheck.GateOpts{ReportDir: filepath.Join(distDir, "vulncheck")}); err != nil {
			return fmt.Errorf("cve gate: %w", err)
		}
	}

	key := o.SignKey
	if key == "" {
		// Reached only when o.DryRun (the real-cut case already returned above).
		key = filepath.Join(o.RepoDir, "tools", "testkeys", "test.key")
	}

	_, err = orchestrate(ctx, Options{
		Component: o.Component, OutDir: filepath.Join(o.RepoDir, "dist"),
		RepoDir: o.RepoDir, SrcDir: o.SrcDir,
		MinisignKey: key,
		SkipGate:    true, // the gate above already ran (or was explicitly bypassed)
		Apple:       o.Apple, DryRun: o.DryRun,
	})
	return err
}

// selectSigner picks build.Compile's Signer: a real Developer-ID signature via
// the product's modernech-sign helper when --apple is set, otherwise the
// existing ad-hoc codesign (macOS needs any signature to run, unsigned or
// ad-hoc, on non-apple/non-darwin cuts).
func selectSigner(apple bool) sign.Signer {
	if apple {
		return sign.AppleSigner{ToolPath: "modernech-sign"}
	}
	return sign.AdHocSigner{}
}

// notarizerFor returns the Notarizer to submit darwin zips to Apple when
// --apple is set, and whether notarization should run at all. Non-apple cuts
// never notarize.
func notarizerFor(apple bool) (sign.Notarizer, bool) {
	if apple {
		return sign.Notarizer{ToolPath: "modernech-sign"}, true
	}
	return sign.Notarizer{}, false
}

// renderInstall writes the component's install.sh into the stamp dir exactly as
// release.sh's render_inner does: clawee copies inner/clawee/install.sh verbatim;
// claweed sed-substitutes __CLAWEED_VERSION__ in the daemon's install.sh.in.
func renderInstall(comp, stamp, srcDir, repoDir, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	switch comp {
	case "clawee":
		data, err := os.ReadFile(filepath.Join(repoDir, "inner", "clawee", "install.sh"))
		if err != nil {
			return err
		}
		return os.WriteFile(dst, data, 0o755)
	case "claweed":
		in := filepath.Join(srcDir, "install", "install.sh.in")
		data, err := os.ReadFile(in)
		if err != nil {
			return fmt.Errorf("claweed installer template %s: %w", in, err)
		}
		out := strings.ReplaceAll(string(data), "__CLAWEED_VERSION__", stamp)
		return os.WriteFile(dst, []byte(out), 0o755)
	}
	return fmt.Errorf("renderInstall: unknown component %q", comp)
}

func orchestrate(ctx context.Context, o Options) (*Result, error) {
	if o.SrcDir == "" {
		o.SrcDir = o.RepoDir
	}
	// 1. CVE gate (fail-closed) — scan the component module. Skipped only under
	//    harness parity (o.SkipGate): release.sh --dry-run does not gate, so the
	//    CVE gate is validated separately and stays mandatory for real cuts.
	if !o.SkipGate {
		if err := vulncheck.Gate(ctx, []vulncheck.Module{{Name: o.Component, Dir: o.SrcDir}},
			vulncheck.GateOpts{ReportDir: filepath.Join(o.OutDir, "vulncheck")}); err != nil {
			return nil, fmt.Errorf("cve gate: %w", err)
		}
	}
	// 2. Stamp (read-only, no bump).
	stamp, err := relconfig.Stamp(ctx, filepath.Join(o.RepoDir, "versions", o.Component), o.SrcDir)
	if err != nil {
		return nil, err
	}
	// 3. Build component matrix.
	bins, err := relconfig.Bins(o.Component, stamp)
	if err != nil {
		return nil, err
	}
	arts, err := build.Compile(ctx, build.Spec{
		SrcDir: o.SrcDir, OutDir: filepath.Join(o.OutDir, stamp),
		Targets: relconfig.Targets(), Bins: bins, Signer: selectSigner(o.Apple),
	})
	if err != nil {
		return nil, fmt.Errorf("compile %s: %w", o.Component, err)
	}
	res := &Result{Stamp: stamp}

	// env-literal guard: parity with release.sh — abort the cut if any built bin
	// embeds a forbidden config-env literal (see tools/verify-no-env.sh).
	guardArgs := make([]string, 0, len(arts))
	for _, a := range arts {
		guardArgs = append(guardArgs, a.Path)
	}
	guard := exec.CommandContext(ctx, "bash", filepath.Join(o.RepoDir, "tools", "verify-no-env.sh"))
	guard.Args = append(guard.Args, guardArgs...)
	guard.Stdout, guard.Stderr = os.Stderr, os.Stderr
	if err := guard.Run(); err != nil {
		return nil, fmt.Errorf("verify-no-env: %w", err)
	}

	// 4. install.sh — rendered per-component (see renderInstall): clawee is a
	//    verbatim copy of inner/clawee/install.sh; claweed sed-substitutes the
	//    version stamp into the daemon source's install/install.sh.in.
	installSh := filepath.Join(o.OutDir, stamp, "install.sh")
	if err := renderInstall(o.Component, stamp, o.SrcDir, o.RepoDir, installSh); err != nil {
		return nil, fmt.Errorf("install.sh: %w", err)
	}

	// 5. Assemble one flat zip per target: component bins + install.sh.
	zips, err := assemble(o.Component, stamp, o.OutDir, installSh, arts)
	if err != nil {
		return nil, fmt.Errorf("assemble: %w", err)
	}
	res.Zips = zips

	// 5b. Notarize darwin zips when --apple is set. Notarization submits the
	//     zip to Apple for review; it does NOT alter zip bytes (bare-binary
	//     zips aren't stapled — the ticket lives in Apple's online DB, checked
	//     at gatekeeper-assess time). --dry-run skips the real submission
	//     (logs intent) since dry-run artifacts are throwaway and notarizing
	//     is a real, rate-limited Apple API call.
	if n, do := notarizerFor(o.Apple); do {
		for _, zp := range res.Zips {
			if !strings.Contains(filepath.Base(zp), "-darwin-") {
				continue
			}
			if o.DryRun {
				fmt.Fprintf(os.Stderr, "dry-run: skipping notarize of %s\n", zp)
				continue
			}
			if err := n.Notarize(ctx, zp); err != nil {
				return nil, fmt.Errorf("notarize %s: %w", zp, err)
			}
		}
	}

	// 6. Checksum + sign the assembled zips. Per-target zip names are unique
	//    (unlike the raw artifacts, where every target ships the same bin
	//    basenames), so WriteSums's duplicate-basename guard never trips here.
	sums := filepath.Join(o.OutDir, stamp, "SHA256SUMS.txt")
	if err := checksum.WriteSums(res.Zips, sums); err != nil {
		return nil, fmt.Errorf("checksum: %w", err)
	}
	key := o.MinisignKey
	if key == "" {
		key = filepath.Join(o.RepoDir, "tools", "testkeys", "test.key")
	}
	if _, statErr := os.Stat(key); statErr == nil {
		if err := minisign.Sign(ctx, sums, key); err != nil {
			return nil, fmt.Errorf("minisign: %w", err)
		}
		res.Minisig = sums + ".minisig"
	}
	res.Sums = sums
	return res, nil
}
