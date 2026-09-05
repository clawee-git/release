package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clawee-git/release/internal/staticsurface"
)

// recorder stands in for ssh, scp and cp. NOTHING in this suite may run any of
// them: the verb's whole job is to write another machine's web root, and a
// test that actually did so would be a test that publishes.
type recorder struct{ cmds []string }

func (r *recorder) run(name string, args ...string) error {
	r.cmds = append(r.cmds, name+" "+strings.Join(args, " "))
	return nil
}

// kitDir builds a checkout carrying exactly the declared static surface.
func kitDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range staticsurface.Files() {
		p := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("# "+f+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestPublishStaticCopiesTheWholeDeclaredSurface(t *testing.T) {
	root := kitDir(t)
	rec := &recorder{}
	var out bytes.Buffer
	e := &env{stdout: &out, stderr: &out}

	if err := publishStatic(e, publishStaticOpts{
		dest: "release-host:/srv/static", run: rec.run,
	}, root); err != nil {
		t.Fatal(err)
	}

	// One mkdir up front, then one scp per file — and every file on the list,
	// because a partial publish leaves a static root where some bootstraps are
	// new and some are old, and each still verifies its own download.
	if len(rec.cmds) != len(staticsurface.Files())+1 {
		t.Fatalf("ran %d commands for %d files:\n%s",
			len(rec.cmds), len(staticsurface.Files()), strings.Join(rec.cmds, "\n"))
	}
	if !strings.HasPrefix(rec.cmds[0], "ssh release-host mkdir -p ") {
		t.Errorf("the directories are not created first: %s", rec.cmds[0])
	}
	for _, want := range []string{"'/srv/static/clawee'", "'/srv/static/claweed'"} {
		if !strings.Contains(rec.cmds[0], want) {
			t.Errorf("mkdir does not cover %s: %s", want, rec.cmds[0])
		}
	}
	for _, f := range staticsurface.Files() {
		want := "scp -q " + filepath.Join(root, filepath.FromSlash(f)) + " release-host:/srv/static/" + f
		if !containsCmd(rec.cmds, want) {
			t.Errorf("missing: %s", want)
		}
	}
}

// The site's pages are NOT static any more. A publish that still carried an
// index.html would put a second, frozen answer to "what is current" on the
// host beside the one the service renders — which is the whole defect this
// feature removed.
func TestPublishStaticCarriesNoSitePage(t *testing.T) {
	for _, f := range staticsurface.Files() {
		if strings.HasSuffix(f, ".html") {
			t.Errorf("%s is on the static surface; the pages are rendered from the catalog now", f)
		}
	}
}

func TestPublishStaticRefusesAPartialKitAndCopiesNothing(t *testing.T) {
	root := kitDir(t)
	if err := os.Remove(filepath.Join(root, "clawee", "beta.install.sh")); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	var out bytes.Buffer
	err := publishStatic(&env{stdout: &out, stderr: &out}, publishStaticOpts{
		dest: "release-host:/srv/static", run: rec.run,
	}, root)
	if err == nil {
		t.Fatal("a kit missing a generated file published anyway")
	}
	if !strings.Contains(err.Error(), "clawee/beta.install.sh") {
		t.Errorf("the refusal does not name the missing file: %v", err)
	}
	if !strings.Contains(err.Error(), "gen-bootstraps.sh") {
		t.Errorf("the refusal does not say how to produce it: %v", err)
	}
	if len(rec.cmds) != 0 {
		t.Errorf("the check must happen BEFORE anything is copied; ran: %v", rec.cmds)
	}
}

func TestPublishStaticDryRunRunsNothing(t *testing.T) {
	root := kitDir(t)
	rec := &recorder{}
	var out bytes.Buffer
	if err := publishStatic(&env{stdout: &out, stderr: &out}, publishStaticOpts{
		dest: "release-host:/srv/static", dryRun: true, run: rec.run,
	}, root); err != nil {
		t.Fatal(err)
	}
	if len(rec.cmds) != 0 {
		t.Fatalf("--dry-run ran commands: %v", rec.cmds)
	}
	body := out.String()
	if !strings.Contains(body, "would copy: clawee/beta.install.sh -> release-host:/srv/static/clawee/beta.install.sh") {
		t.Errorf("the plan does not name each copy:\n%s", body)
	}
	if !strings.Contains(body, "nothing was copied") {
		t.Errorf("the plan does not say it copied nothing:\n%s", body)
	}
}

// A dest with no host is a local copy — the shape this takes once the service
// runs on the release host itself and there is nothing to ssh to.
func TestPublishStaticWithNoHostCopiesLocally(t *testing.T) {
	root := kitDir(t)
	dest := filepath.Join(t.TempDir(), "static")
	rec := &recorder{}
	var out bytes.Buffer
	if err := publishStatic(&env{stdout: &out, stderr: &out}, publishStaticOpts{
		dest: dest, run: rec.run,
	}, root); err != nil {
		t.Fatal(err)
	}
	for _, c := range rec.cmds {
		if strings.HasPrefix(c, "ssh ") || strings.HasPrefix(c, "scp ") {
			t.Fatalf("a local destination reached for ssh: %s", c)
		}
	}
	if _, err := os.Stat(filepath.Join(dest, "clawee")); err != nil {
		t.Errorf("the local target directories were not created: %v", err)
	}
}

// "host:" is one keystroke from "host:/srv/static" and lands every file at the
// remote filesystem root — outside the web root, where nginx cannot serve them
// and nobody thinks to look.
func TestPublishStaticRefusesAHostWithNoDirectory(t *testing.T) {
	var out bytes.Buffer
	e := &env{stdout: &out, stderr: &out}
	if got := run(e, []string{"publish-static", "--root", ".", "--dest", "release-host:"}); got != exitUsage {
		t.Fatalf("exit %d, want %d", got, exitUsage)
	}
	if !strings.Contains(out.String(), "no directory") {
		t.Errorf("the refusal does not say what is wrong:\n%s", out.String())
	}
}

func TestPublishStaticRequiresItsTwoFlags(t *testing.T) {
	n, _, _ := find([]string{"publish-static"})
	if n == nil {
		t.Fatal("no publish-static node")
	}
	for _, args := range [][]string{
		{"--dest", "h:/srv"},
		{"--root", "."},
	} {
		var out bytes.Buffer
		e := &env{stdout: &out, stderr: &out}
		if got := run(e, append([]string{"publish-static"}, args...)); got != exitUsage {
			t.Errorf("%v: exit %d, want %d (a missing required flag is an invocation error)", args, got, exitUsage)
		}
	}
}

func containsCmd(cmds []string, want string) bool {
	for _, c := range cmds {
		if c == want {
			return true
		}
	}
	return false
}
