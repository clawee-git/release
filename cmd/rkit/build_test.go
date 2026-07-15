package main

import (
	"archive/zip"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/burrowee-git/release-kit/sign"
	"github.com/clawee-git/release/internal/relconfig"
)

func TestOrchestrateBuildsMatrixIntoScratch(t *testing.T) {
	// Minimal module fixture: one main package printing a version var.
	repo := t.TempDir()
	writeFixtureModule(t, repo) // helper below: go.mod + cmd/clawee/main.go + cmd/clawee-updater/main.go + versions/clawee
	out := t.TempDir()
	ctx := context.Background()
	res, err := orchestrate(ctx, Options{
		Component: "clawee", OutDir: out, RepoDir: repo,
		MinisignKey: testMinisignKey(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stamp == "" {
		t.Fatal("empty stamp")
	}
	// binaries for every target must exist.
	for _, tgt := range relconfig.Targets() {
		for _, b := range []string{"clawee", "clawee-updater"} {
			p := filepath.Join(out, res.Stamp, tgt.OS+"-"+tgt.Arch, b)
			if _, err := os.Stat(p); err != nil {
				t.Errorf("missing %s: %v", p, err)
			}
		}
	}
	// one assembled zip per target, containing exactly the component bins and
	// install.sh — nothing else (clawee has no dispatcher).
	if len(res.Zips) != len(relconfig.Targets()) {
		t.Fatalf("got %d zips, want %d: %v", len(res.Zips), len(relconfig.Targets()), res.Zips)
	}
	wantEntries := []string{"clawee", "clawee-updater", "install.sh"}
	sort.Strings(wantEntries)
	wantInstallSh, err := os.ReadFile(filepath.Join(repo, "inner", "clawee", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, zp := range res.Zips {
		r, err := zip.OpenReader(zp)
		if err != nil {
			t.Fatalf("open %s: %v", zp, err)
		}
		var got []string
		for _, f := range r.File {
			got = append(got, f.Name)
			if f.Name == "install.sh" {
				rc, err := f.Open()
				if err != nil {
					t.Fatalf("open install.sh in %s: %v", zp, err)
				}
				data, err := io.ReadAll(rc)
				rc.Close()
				if err != nil {
					t.Fatalf("read install.sh in %s: %v", zp, err)
				}
				if string(data) != string(wantInstallSh) {
					t.Errorf("%s install.sh not byte-identical to inner/clawee/install.sh", zp)
				}
			}
		}
		r.Close()
		sort.Strings(got)
		if !reflect.DeepEqual(got, wantEntries) {
			t.Errorf("%s entries = %v, want %v", zp, got, wantEntries)
		}
	}
	if _, err := os.Stat(res.Sums); err != nil {
		t.Errorf("missing sums file %s: %v", res.Sums, err)
	}
	if res.Minisig != "" {
		if _, err := os.Stat(res.Minisig); err != nil {
			t.Errorf("Result.Minisig=%s but file missing: %v", res.Minisig, err)
		}
	}
}

// writeFixtureModule creates a self-contained module (no external deps) with a
// trivial main package per binary (a stampable `var version string`), a
// versions/clawee semver file, a verbatim inner/clawee/install.sh, and the
// real tools/verify-no-env.sh + tools/version.sh (both location-relative, so a
// copy works unmodified inside the fixture repo), then commits it so
// relconfig.Stamp's `git rev-parse` has a HEAD to read.
func writeFixtureModule(t *testing.T, repo string) {
	t.Helper()
	write := func(rel, content string) {
		full := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module fixture\n\ngo 1.25.0\n")
	mainSrc := "package main\n\nimport \"fmt\"\n\nvar version string\n\nfunc main() { fmt.Println(version) }\n"
	write("cmd/clawee/main.go", mainSrc)
	write("cmd/clawee-updater/main.go", mainSrc)
	write("versions/clawee", "0.1.0\n")
	write("inner/clawee/install.sh", "#!/bin/sh\necho fixture-install\n")

	// tools/verify-no-env.sh and tools/version.sh both resolve REPO_ROOT from
	// their own script location (dirname $0), so a verbatim copy works inside
	// the fixture repo unmodified.
	copyReal := func(rel string) {
		repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(repoRoot, rel))
		if err != nil {
			t.Fatal(err)
		}
		full := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, data, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	copyReal("tools/verify-no-env.sh")
	copyReal("tools/version.sh")

	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", repo}, args...)...)
		c.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("add", "-A")
	git("commit", "-q", "-m", "fixture")
}

// testMinisignKey generates a password-less minisign secret key in a temp dir
// and returns its path. If minisign isn't installed, it returns "" — the
// os.Stat guard in orchestrate() then skips signing rather than failing the
// whole test on a missing binary.
func testMinisignKey(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("minisign"); err != nil {
		t.Log("minisign not installed — signing portion will be skipped")
		return ""
	}
	dir := t.TempDir()
	pub := filepath.Join(dir, "key.pub")
	sec := filepath.Join(dir, "key.sec")
	if out, err := exec.Command("minisign", "-G", "-W", "-p", pub, "-s", sec).CombinedOutput(); err != nil {
		t.Fatalf("minisign keygen: %v\n%s", err, out)
	}
	return sec
}

// TestOrchestrateSkipGateBypassesCVEGate proves Options.SkipGate short-circuits
// the mandatory vulncheck.Gate. To make a real gate call deterministically fail
// regardless of host state, GOPATH is repointed at an empty dir: govulncheck is
// installed under the real GOPATH/bin (and is NOT on PATH), so
// resolveGovulncheck can't find it and Gate returns "govulncheck not found".
// The fixture module is stdlib-only, so the empty GOPATH doesn't affect the
// build itself.
func TestOrchestrateSkipGateBypassesCVEGate(t *testing.T) {
	t.Setenv("GOPATH", t.TempDir())
	repo := t.TempDir()
	writeFixtureModule(t, repo)
	ctx := context.Background()
	key := testMinisignKey(t)

	// Gate ON (SkipGate zero value): orchestrate must abort at the CVE gate.
	if _, err := orchestrate(ctx, Options{
		Component: "clawee", OutDir: t.TempDir(), RepoDir: repo, MinisignKey: key,
	}); err == nil || !strings.Contains(err.Error(), "cve gate") {
		t.Fatalf("gate ON: want a cve gate error, got %v", err)
	}

	// Gate SKIPPED: the same build now succeeds past the gate.
	if _, err := orchestrate(ctx, Options{
		Component: "clawee", OutDir: t.TempDir(), RepoDir: repo, MinisignKey: key, SkipGate: true,
	}); err != nil {
		t.Fatalf("gate SKIPPED: orchestrate should bypass the gate and succeed, got %v", err)
	}
}

// TestOrchestrateFailsClosedOnMissingExplicitMinisignKey proves that when a
// NON-EMPTY Options.MinisignKey is supplied but doesn't resolve to a real
// file, orchestrate returns an error instead of silently skipping the sign
// step (leaving Result.Minisig empty and reporting success). The
// o.MinisignKey=="" fixture fallback (testMinisignKey unavailable) must
// still tolerate a missing key — see TestOrchestrateBuildsMatrixIntoScratch.
func TestOrchestrateFailsClosedOnMissingExplicitMinisignKey(t *testing.T) {
	repo := t.TempDir()
	writeFixtureModule(t, repo)
	ctx := context.Background()
	_, err := orchestrate(ctx, Options{
		Component: "clawee", OutDir: t.TempDir(), RepoDir: repo,
		MinisignKey: filepath.Join(t.TempDir(), "nonexistent.key"), SkipGate: true,
	})
	if err == nil {
		t.Fatal("expected error for missing explicit MinisignKey, got nil")
	}
	if !strings.Contains(err.Error(), "minisign") {
		t.Fatalf("error = %q, want it to mention the minisign key", err.Error())
	}
}

func TestBuildWritesToDistStamp(t *testing.T) {
	repo := t.TempDir()
	writeFixtureModule(t, repo)
	// build with default gate SKIPPED for the fixture host: pass --no-vulncheck.
	err := buildRun(buildOpts{Component: "clawee", RepoDir: repo, SrcDir: repo,
		NoVulncheck: true, SignKey: testMinisignKey(t)})
	if err != nil {
		t.Fatal(err)
	}
	// artifacts land under repo/dist/<stamp>/, NOT an arbitrary --out.
	stamp := mustStamp(t, repo, "clawee")
	if _, err := os.Stat(filepath.Join(repo, "dist", stamp, "clawee-clawee-linux-amd64.zip")); err != nil {
		t.Errorf("missing zip under dist/<stamp>: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "dist", stamp, "SHA256SUMS.txt")); err != nil {
		t.Errorf("missing SHA256SUMS: %v", err)
	}
}

// TestBuildRunFailsFastOnMissingSignKeyFile proves a real cut (DryRun:false)
// with a --sign-key path that doesn't exist on disk fails IMMEDIATELY, before
// any build work runs — never silently skipping the sign step and reporting
// success.
func TestBuildRunFailsFastOnMissingSignKeyFile(t *testing.T) {
	repo := t.TempDir()
	writeFixtureModule(t, repo)
	stamp := mustStamp(t, repo, "clawee")

	err := buildRun(buildOpts{
		Component: "clawee", RepoDir: repo, SrcDir: repo,
		DryRun: false, SignKey: "/nonexistent/path/key.key", NoVulncheck: true,
	})
	if err == nil {
		t.Fatal("expected error for missing --sign-key file, got nil")
	}
	if !strings.Contains(err.Error(), "sign-key") || !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("error = %q, want it to mention sign-key and no such file", err.Error())
	}
	// Fail-fast means no build work happened: no dist/<stamp> zips.
	if _, statErr := os.Stat(filepath.Join(repo, "dist", stamp)); !os.IsNotExist(statErr) {
		t.Fatalf("dist/%s should not exist (fail-fast before build), stat err = %v", stamp, statErr)
	}
}

// TestBuildRunBumpDryRunReverts exercises the bump+revert path end-to-end: a
// --bump-patch --dry-run build must run the revert (registered before the
// bump block) and leave versions/clawee exactly as it was committed, with no
// staged or worktree diff left behind.
func TestBuildRunBumpDryRunReverts(t *testing.T) {
	repo := t.TempDir()
	writeFixtureModule(t, repo) // versions/clawee = "0.1.0\n", committed
	if err := buildRun(buildOpts{
		Component: "clawee", RepoDir: repo, SrcDir: repo,
		Bump: "patch", DryRun: true, NoVulncheck: true, SignKey: testMinisignKey(t),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(repo, "versions", "clawee"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "0.1.0\n" {
		t.Fatalf("versions/clawee = %q, want unchanged %q (revert did not fire)", got, "0.1.0\n")
	}
	out, err := exec.Command("git", "-C", repo, "status", "--porcelain", "versions/clawee").CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("git status --porcelain versions/clawee = %q, want clean", out)
	}
}

func TestBuildGateOnByDefaultCanBeSkipped(t *testing.T) {
	// With NoVulncheck=false and no govulncheck resolvable, the gate must RUN
	// (and here fail) — proving default-on. Then NoVulncheck=true bypasses.
	repo := t.TempDir()
	writeFixtureModule(t, repo)
	t.Setenv("GOPATH", t.TempDir()) // make govulncheck unresolvable → gate errors
	if err := buildRun(buildOpts{Component: "clawee", RepoDir: repo, SrcDir: repo,
		NoVulncheck: false, SignKey: testMinisignKey(t)}); err == nil {
		t.Fatal("expected default-on gate to run and fail")
	}
	if err := buildRun(buildOpts{Component: "clawee", RepoDir: repo, SrcDir: repo,
		NoVulncheck: true, SignKey: testMinisignKey(t)}); err != nil {
		t.Fatalf("--no-vulncheck should bypass: %v", err)
	}
}

// mustStamp computes the expected dist/<stamp> directory name the same way
// buildRun/orchestrate do, so tests can assert on artifact paths without
// duplicating the stamp scheme.
func mustStamp(t *testing.T, repo, comp string) string {
	t.Helper()
	stamp, err := relconfig.Stamp(context.Background(), filepath.Join(repo, "versions", comp), repo)
	if err != nil {
		t.Fatal(err)
	}
	return stamp
}

func TestBuildAppleSelectsDevIDSignerAndNotarizes(t *testing.T) {
	// Unit-level: assert selectSigner(apple=true) returns AppleSigner{ToolPath:"modernech-sign"}
	// and notarizerFor(apple=true) returns Notarizer{ToolPath:"modernech-sign"};
	// apple=false returns AdHocSigner and a nil/skip notarizer.
	s := selectSigner(true)
	if _, ok := s.(sign.AppleSigner); !ok {
		t.Fatalf("apple signer type = %T", s)
	}
	if got := s.(sign.AppleSigner).ToolPath; got != "modernech-sign" {
		t.Fatalf("toolpath %q", got)
	}
	if selectSigner(false) == nil {
		t.Fatal("adhoc signer nil")
	}
	if _, ok := selectSigner(false).(sign.AdHocSigner); !ok {
		t.Fatal("non-apple must be adhoc")
	}
	n, do := notarizerFor(true)
	if !do || n.ToolPath != "modernech-sign" {
		t.Fatalf("notarizer %+v do=%v", n, do)
	}
	if _, do2 := notarizerFor(false); do2 {
		t.Fatal("non-apple must not notarize")
	}
}

// TestSrcDirFor proves srcDirFor resolves CLAWEE_SRC_CLAWEE/CLAWEE_SRC_CLAWEED
// when set, and otherwise falls back to the documented default source
// worktree paths. An unknown component resolves to "".
func TestSrcDirFor(t *testing.T) {
	t.Setenv("CLAWEE_SRC_CLAWEE", "/env/clawee/src")
	if got := srcDirFor("clawee"); got != "/env/clawee/src" {
		t.Fatalf("srcDirFor(clawee) = %q, want env override", got)
	}

	t.Setenv("CLAWEE_SRC_CLAWEED", "")
	const wantDefault = "/Volumes/MacintoshED/Workstation/Coding/Clawee/daemon/code/daemon"
	if got := srcDirFor("claweed"); got != wantDefault {
		t.Fatalf("srcDirFor(claweed) default = %q, want %q", got, wantDefault)
	}

	if got := srcDirFor("unknown"); got != "" {
		t.Fatalf("srcDirFor(unknown) = %q, want empty", got)
	}
}

// TestRenderInstall covers both components' install.sh rendering directly
// (no compile): clawee is a byte-verbatim copy of inner/clawee/install.sh;
// claweed sed-substitutes __CLAWEED_VERSION__ in the daemon source's
// install/install.sh.in — `strings.ReplaceAll` on a fixed literal token
// produces byte-identical output to `sed s/…/…/g`.
func TestRenderInstall(t *testing.T) {
	t.Run("clawee verbatim copy", func(t *testing.T) {
		repoDir := t.TempDir()
		mustWriteFile(t, filepath.Join(repoDir, "inner", "clawee", "install.sh"), "#!/bin/sh\necho hi\n")
		dst := filepath.Join(t.TempDir(), "out", "install.sh")
		if err := renderInstall("clawee", "v0.1.0.x", "/unused/src", repoDir, dst); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(dst)
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(filepath.Join(repoDir, "inner", "clawee", "install.sh"))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf("clawee install.sh = %q, want verbatim %q", got, want)
		}
		fi, err := os.Stat(dst)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o755 {
			t.Fatalf("mode = %v, want 0755", fi.Mode().Perm())
		}
	})

	t.Run("claweed sed substitution", func(t *testing.T) {
		srcDir := t.TempDir()
		mustWriteFile(t, filepath.Join(srcDir, "install", "install.sh.in"),
			"VERSION=\"__CLAWEED_VERSION__\"\necho done\n")
		dst := filepath.Join(t.TempDir(), "out", "install.sh")
		stamp := "v0.1.34.2026.07.14.deadbeef"
		if err := renderInstall("claweed", stamp, srcDir, "/unused/repo", dst); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(dst)
		if err != nil {
			t.Fatal(err)
		}
		want := "VERSION=\"" + stamp + "\"\necho done\n"
		if string(got) != want {
			t.Fatalf("claweed install.sh = %q, want %q", got, want)
		}
		fi, err := os.Stat(dst)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o755 {
			t.Fatalf("mode = %v, want 0755", fi.Mode().Perm())
		}
	})

	t.Run("unknown component", func(t *testing.T) {
		dst := filepath.Join(t.TempDir(), "install.sh")
		if err := renderInstall("nope", "v0", "/x", "/y", dst); err == nil {
			t.Fatal("expected error for unknown component")
		}
	})
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestOrchestrateAbortsOnForbiddenEnvLiteral proves the env-literal guard
// (parity with release.sh's step 2 verify-no-env.sh call) actually aborts
// the cut when a built binary embeds a forbidden config-env literal — the
// happy path (a clean fixture build) never exercises the abort branch.
// Exercised THROUGH orchestrate (preferred over a standalone unit test on
// the guard script) so the assertion covers the real wiring: guard runs
// after compile, before install.sh/assemble/sign, and its failure surfaces
// as an orchestrate error mentioning "verify-no-env".
func TestOrchestrateAbortsOnForbiddenEnvLiteral(t *testing.T) {
	repo := t.TempDir()
	writeFixtureModule(t, repo)

	// Inject one of tools/verify-no-env.sh's forbidden literals
	// (CLAWEE_DATA_DIR/CLAWEE_SOCKET/CLAWEE_SPAWN_HELPER/mustEnv) into the
	// clawee main package's source so the compiled binary embeds it as a
	// string constant `strings` can find.
	forbidden := "package main\n\nimport \"fmt\"\n\nvar version string\n\nfunc main() { fmt.Println(version, \"CLAWEE_DATA_DIR\") }\n"
	mainGo := filepath.Join(repo, "cmd", "clawee", "main.go")
	if err := os.WriteFile(mainGo, []byte(forbidden), 0o644); err != nil {
		t.Fatal(err)
	}
	commit := exec.Command("git", "-C", repo, "commit", "-aqm", "inject forbidden literal")
	commit.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}

	ctx := context.Background()
	_, err := orchestrate(ctx, Options{
		Component: "clawee", OutDir: t.TempDir(), RepoDir: repo,
		MinisignKey: testMinisignKey(t), SkipGate: true,
	})
	if err == nil {
		t.Fatal("expected orchestrate to abort on a forbidden env literal, got nil error")
	}
	if !strings.Contains(err.Error(), "verify-no-env") {
		t.Fatalf("error = %q, want it to mention verify-no-env", err.Error())
	}
}
