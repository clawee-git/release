package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/burrowee-git/release-kit/build"
	"github.com/burrowee-git/release-kit/pack"
)

// assemble builds one flat zip per target: component bins + install.sh (+
// claweed's migration ladder under migrations/). Zips land at
// outRoot/stamp/clawee-<comp>-<os>-<arch>.zip in sorted-target order. Clawee
// has no dispatcher and no extra payload — the updaters are regular bins
// already in compArts.
func assemble(comp, stamp, outRoot, srcDir, installSh string, compArts []build.Artifact) ([]string, error) {
	byTarget := map[string][]pack.Content{}
	for _, a := range compArts {
		k := a.OS + "-" + a.Arch
		byTarget[k] = append(byTarget[k], pack.Content{Src: a.Path})
	}

	targets := make([]string, 0, len(byTarget))
	for k := range byTarget {
		targets = append(targets, k)
	}
	sort.Strings(targets)

	zipDir := filepath.Join(outRoot, stamp)
	if err := os.MkdirAll(zipDir, 0o755); err != nil {
		return nil, fmt.Errorf("assemble %s: %w", comp, err)
	}

	// Migration ladder: arch-independent, so it is staged ONCE here and added
	// to every target's zip alongside install.sh — mirrors tools/release.sh's
	// stage_migrations_for, which stages into each per-target assemble dir
	// before that dir is zipped (release.sh:658, called once per TARGETS
	// iteration; the content is identical every time because the source
	// ladder does not vary by os/arch).
	migContents, err := stageMigrations(comp, srcDir, filepath.Join(zipDir, ".migrations-"+comp))
	if err != nil {
		return nil, fmt.Errorf("assemble %s: %w", comp, err)
	}

	var zips []string
	for _, k := range targets {
		contents := append([]pack.Content{}, byTarget[k]...)
		contents = append(contents, pack.Content{Src: installSh, Name: "install.sh"})
		contents = append(contents, migContents...)
		zp := filepath.Join(zipDir, fmt.Sprintf("clawee-%s-%s.zip", comp, k))
		if err := pack.Zip(pack.Spec{Out: zp, Contents: contents}); err != nil {
			return nil, fmt.Errorf("assemble %s: zip %s: %w", comp, k, err)
		}
		zips = append(zips, zp)
	}
	return zips, nil
}

// stageMigrations copies claweed's migration ladder into dstDir by invoking
// the DAEMON's own install/stage_migrations.sh — the single source of truth
// for what "migrations/" contains, also sourced by the daemon's own
// build-local.sh and install_test.sh (see that file's header) and by this
// repo's tools/release.sh (stage_migrations_for, tools/release.sh:~516-530,
// which this mirrors). Reusing the shell rule via exec, rather than
// reimplementing the *.sh/*_test.sh/lib.sh-mode selection logic in Go, keeps
// there being exactly one place that rule can drift.
//
// clawee has no ladder — returns (nil, nil) immediately for any component
// other than "claweed".
//
// Fail-closed: even though stage_migrations itself already checks its own
// inputs and output, this asserts again on what actually landed in dstDir
// (mirroring release.sh:527-528's "ASSERT ON THE ARTIFACT" reasoning) — a
// deleted call site here would otherwise produce a build that "succeeds"
// with no migrations/ at all, which is exactly how the ladder went missing
// the first time.
func stageMigrations(comp, srcDir, dstDir string) ([]pack.Content, error) {
	if comp != "claweed" {
		return nil, nil
	}

	src := filepath.Join(srcDir, "install", "migrations")
	rule := filepath.Join(srcDir, "install", "stage_migrations.sh")
	if _, err := os.Stat(rule); err != nil {
		return nil, fmt.Errorf("stage migrations: %s missing — cannot stage the migration ladder (wrong --src / CLAWEE_SRC_CLAWEED?): %w", rule, err)
	}

	// `sh -c '. "$1" && stage_migrations "$2" "$3"' sh <rule> <src> <dst>`:
	// positional params, not string-interpolated into the script body, so
	// none of these paths need shell quoting/escaping.
	cmd := exec.Command("sh", "-c", `. "$1" && stage_migrations "$2" "$3"`, "sh", rule, src, dstDir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return nil, fmt.Errorf("stage migrations: %w: %s", err, msg)
		}
		return nil, fmt.Errorf("stage migrations: %w", err)
	}

	entries, err := os.ReadDir(dstDir)
	if err != nil {
		return nil, fmt.Errorf("stage migrations: read %s: %w", dstDir, err)
	}
	contents := make([]pack.Content, 0, len(entries))
	haveRunSh := false
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		contents = append(contents, pack.Content{Src: filepath.Join(dstDir, e.Name()), Name: "migrations/" + e.Name()})
		if e.Name() == "run.sh" {
			haveRunSh = true
		}
	}

	runSh := filepath.Join(dstDir, "run.sh")
	info, statErr := os.Stat(runSh)
	if !haveRunSh || statErr != nil || info.Mode()&0o111 == 0 {
		return nil, fmt.Errorf("stage migrations: staged %d file(s) but migrations/run.sh is missing or not executable", len(contents))
	}
	return contents, nil
}
