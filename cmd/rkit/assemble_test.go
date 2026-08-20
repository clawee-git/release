package main

import (
	"archive/zip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/burrowee-git/release-kit/build"
)

func TestAssembleFlatZip(t *testing.T) {
	root := t.TempDir()
	// fake per-target artifacts (2 bins x 1 target for brevity).
	mk := func(p, data string) string {
		full := filepath.Join(root, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(data), 0o755)
		return full
	}
	arts := []build.Artifact{
		{OS: "linux", Arch: "arm64", Path: mk("bin/clawee", "A")},
		{OS: "linux", Arch: "arm64", Path: mk("bin/clawee-updater", "B")},
	}
	installSh := mk("install.sh", "#!/bin/sh\n")

	zips, err := assemble("clawee", "v0.1.90.x", root, "/unused/src", installSh, arts)
	if err != nil {
		t.Fatal(err)
	}
	if len(zips) != 1 {
		t.Fatalf("want 1 zip, got %d", len(zips))
	}
	if base := filepath.Base(zips[0]); base != "clawee-clawee-linux-arm64.zip" {
		t.Fatalf("zip name = %s", base)
	}
	// zip contains clawee, clawee-updater, install.sh (flat).
	r, err := zip.OpenReader(zips[0])
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var names []string
	for _, f := range r.File {
		names = append(names, f.Name)
	}
	sort.Strings(names)
	want := []string{"clawee", "clawee-updater", "install.sh"}
	if len(names) != len(want) {
		t.Fatalf("entries = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("entries = %v, want %v", names, want)
		}
	}
}

// --- migration-ladder staging -----------------------------------------------
//
// stage_migrations.sh itself lives in the DAEMON repo (this repo does not own
// it — see stageMigrations's doc comment), so these tests write a small
// stand-in shell script that implements the same `stage_migrations SRC DST`
// interface, to exercise this package's Go wiring (shelling out, reading the
// result back, the fail-closed assert) rather than the daemon's own
// file-selection rule, which is the daemon's tests' job.

// writeFixtureStageRule writes a strict fixture of install/stage_migrations.sh
// under srcDir/install: it copies *.sh from SRC to DST, skips *_test.sh,
// chmods lib.sh 0644 and everything else 0755, and — like the real rule —
// refuses when SRC has no run.sh or when nothing was staged.
func writeFixtureStageRule(t *testing.T, srcDir string) {
	t.Helper()
	mustWriteFile(t, filepath.Join(srcDir, "install", "stage_migrations.sh"), `#!/bin/sh
stage_migrations() {
    _sm_src="$1"; _sm_dst="$2"
    [ -f "$_sm_src/run.sh" ] || { echo "fixture: no run.sh in $_sm_src" >&2; return 1; }
    mkdir -p "$_sm_dst" || return 1
    _sm_n=0
    for _sm_f in "$_sm_src"/*.sh; do
        [ -f "$_sm_f" ] || continue
        case "$_sm_f" in
            *_test.sh) continue ;;
        esac
        case "${_sm_f##*/}" in
            lib.sh) _sm_mode=0644 ;;
            *) _sm_mode=0755 ;;
        esac
        cp "$_sm_f" "$_sm_dst/${_sm_f##*/}" || return 1
        chmod "$_sm_mode" "$_sm_dst/${_sm_f##*/}" || return 1
        _sm_n=$((_sm_n + 1))
    done
    [ "$_sm_n" -gt 0 ] || { echo "fixture: nothing staged" >&2; return 1; }
    printf '%s\n' "$_sm_n"
}
`)
}

// writeFixtureLaxStageRule writes a fixture rule that stages *.sh files
// WITHOUT the real rule's completeness checks (no run.sh requirement, no
// executable bit on run.sh) — used to prove stageMigrations' own post-hoc
// assert catches an incomplete ladder even when the sourced rule reports
// success.
func writeFixtureLaxStageRule(t *testing.T, srcDir string) {
	t.Helper()
	mustWriteFile(t, filepath.Join(srcDir, "install", "stage_migrations.sh"), `#!/bin/sh
stage_migrations() {
    _sm_src="$1"; _sm_dst="$2"
    mkdir -p "$_sm_dst" || return 1
    _sm_n=0
    for _sm_f in "$_sm_src"/*.sh; do
        [ -f "$_sm_f" ] || continue
        cp "$_sm_f" "$_sm_dst/${_sm_f##*/}" || return 1
        chmod 0644 "$_sm_dst/${_sm_f##*/}" || return 1
        _sm_n=$((_sm_n + 1))
    done
    printf '%s\n' "$_sm_n"
}
`)
}

func TestStageMigrationsClaweeNoOp(t *testing.T) {
	// clawee has no ladder — stageMigrations must short-circuit before ever
	// looking at srcDir, so a nonexistent srcDir is deliberately used.
	contents, err := stageMigrations("clawee", "/does/not/exist", filepath.Join(t.TempDir(), "migrations"))
	if err != nil {
		t.Fatalf("clawee: unexpected error: %v", err)
	}
	if contents != nil {
		t.Fatalf("clawee: want nil contents, got %v", contents)
	}
}

func TestStageMigrationsClaweedStagesLadder(t *testing.T) {
	srcDir := t.TempDir()
	writeFixtureStageRule(t, srcDir)
	mig := filepath.Join(srcDir, "install", "migrations")
	mustWriteFile(t, filepath.Join(mig, "run.sh"), "#!/bin/sh\necho run\n")
	mustWriteFile(t, filepath.Join(mig, "lib.sh"), "#!/bin/sh\n# sourced\n")
	mustWriteFile(t, filepath.Join(mig, "001_create_table.sh"), "#!/bin/sh\necho rung1\n")
	mustWriteFile(t, filepath.Join(mig, "001_create_table_test.sh"), "#!/bin/sh\necho harness\n")

	dst := filepath.Join(t.TempDir(), "migrations")
	contents, err := stageMigrations("claweed", srcDir, dst)
	if err != nil {
		t.Fatalf("stageMigrations: %v", err)
	}

	var names []string
	for _, c := range contents {
		names = append(names, c.Name)
	}
	sort.Strings(names)
	want := []string{"migrations/001_create_table.sh", "migrations/lib.sh", "migrations/run.sh"}
	if len(names) != len(want) {
		t.Fatalf("staged entries = %v, want %v (the *_test.sh harness file must be excluded)", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("staged entries = %v, want %v", names, want)
		}
	}

	// run.sh must be executable in the staged output.
	fi, err := os.Stat(filepath.Join(dst, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&0o111 == 0 {
		t.Fatalf("run.sh mode = %v, want executable", fi.Mode())
	}
	// lib.sh is sourced, never exec'd — mirrors the real rule's 0644.
	fi, err = os.Stat(filepath.Join(dst, "lib.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("lib.sh mode = %v, want 0644", fi.Mode().Perm())
	}
}

func TestStageMigrationsRuleMissing(t *testing.T) {
	srcDir := t.TempDir() // no install/stage_migrations.sh at all
	_, err := stageMigrations("claweed", srcDir, filepath.Join(t.TempDir(), "migrations"))
	if err == nil {
		t.Fatal("expected an error for a missing stage_migrations.sh")
	}
	if !strings.Contains(err.Error(), "stage_migrations.sh") {
		t.Fatalf("error = %v, want it to name the missing rule file", err)
	}
}

func TestStageMigrationsFailsClosedWhenLadderIncomplete(t *testing.T) {
	t.Run("run.sh absent from the source ladder", func(t *testing.T) {
		srcDir := t.TempDir()
		writeFixtureLaxStageRule(t, srcDir)
		mig := filepath.Join(srcDir, "install", "migrations")
		mustWriteFile(t, filepath.Join(mig, "lib.sh"), "#!/bin/sh\n")

		_, err := stageMigrations("claweed", srcDir, filepath.Join(t.TempDir(), "migrations"))
		if err == nil {
			t.Fatal("expected an error when the staged output has no run.sh")
		}
		if !strings.Contains(err.Error(), "run.sh") {
			t.Fatalf("error = %v, want it to mention run.sh", err)
		}
	})

	t.Run("run.sh present but not executable", func(t *testing.T) {
		srcDir := t.TempDir()
		writeFixtureLaxStageRule(t, srcDir) // always chmods 0644, never 0755
		mig := filepath.Join(srcDir, "install", "migrations")
		mustWriteFile(t, filepath.Join(mig, "run.sh"), "#!/bin/sh\necho run\n")

		_, err := stageMigrations("claweed", srcDir, filepath.Join(t.TempDir(), "migrations"))
		if err == nil {
			t.Fatal("expected an error when run.sh is staged but not executable")
		}
		if !strings.Contains(err.Error(), "run.sh") {
			t.Fatalf("error = %v, want it to mention run.sh", err)
		}
	})
}

// TestAssembleStagesMigrationsForClaweed proves assemble() wires
// stageMigrations into the real per-target zip: claweed's zip must contain
// migrations/run.sh (and its siblings) beside install.sh, while clawee's zip
// (TestAssembleFlatZip) stays untouched by any of this.
func TestAssembleStagesMigrationsForClaweed(t *testing.T) {
	root := t.TempDir()
	mk := func(p, data string) string {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(data), 0o755); err != nil {
			t.Fatal(err)
		}
		return full
	}
	arts := []build.Artifact{
		{OS: "linux", Arch: "arm64", Path: mk("bin/claweed", "A")},
		{OS: "linux", Arch: "arm64", Path: mk("bin/claweed-updater", "B")},
	}
	installSh := mk("install.sh", "#!/bin/sh\n")

	srcDir := t.TempDir()
	writeFixtureStageRule(t, srcDir)
	mig := filepath.Join(srcDir, "install", "migrations")
	mustWriteFile(t, filepath.Join(mig, "run.sh"), "#!/bin/sh\necho run\n")
	mustWriteFile(t, filepath.Join(mig, "lib.sh"), "#!/bin/sh\n")
	mustWriteFile(t, filepath.Join(mig, "001_create_table.sh"), "#!/bin/sh\n")

	zips, err := assemble("claweed", "v0.2.0.x", root, srcDir, installSh, arts)
	if err != nil {
		t.Fatal(err)
	}
	if len(zips) != 1 {
		t.Fatalf("want 1 zip, got %d", len(zips))
	}

	r, err := zip.OpenReader(zips[0])
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var names []string
	for _, f := range r.File {
		names = append(names, f.Name)
	}
	sort.Strings(names)
	want := []string{
		"claweed", "claweed-updater", "install.sh",
		"migrations/001_create_table.sh", "migrations/lib.sh", "migrations/run.sh",
	}
	if len(names) != len(want) {
		t.Fatalf("entries = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("entries = %v, want %v", names, want)
		}
	}
}

// TestAssembleFailsClosedWhenClaweedMigrationsIncomplete proves assemble()
// itself aborts (no zip written) when the staged ladder is missing run.sh —
// the build-time completeness gate, mirroring release.sh's unzip -l assert
// (release.sh:678-683) one layer earlier, before a zip exists at all.
func TestAssembleFailsClosedWhenClaweedMigrationsIncomplete(t *testing.T) {
	root := t.TempDir()
	mk := func(p, data string) string {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(data), 0o755); err != nil {
			t.Fatal(err)
		}
		return full
	}
	arts := []build.Artifact{
		{OS: "linux", Arch: "arm64", Path: mk("bin/claweed", "A")},
	}
	installSh := mk("install.sh", "#!/bin/sh\n")

	srcDir := t.TempDir()
	writeFixtureLaxStageRule(t, srcDir)
	mig := filepath.Join(srcDir, "install", "migrations")
	mustWriteFile(t, filepath.Join(mig, "lib.sh"), "#!/bin/sh\n") // no run.sh

	if _, err := assemble("claweed", "v0.2.0.x", root, srcDir, installSh, arts); err == nil {
		t.Fatal("expected assemble to fail closed when migrations/run.sh is missing")
	}
	if entries, _ := os.ReadDir(filepath.Join(root, "v0.2.0.x")); true {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".zip") {
				t.Fatalf("assemble must not leave a zip behind on failure, found %s", e.Name())
			}
		}
	}
}
