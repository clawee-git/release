package main

// The `publish-static` verb: copy the kit's generated static files to the
// release host's nginx root.
//
// This used to be five scp lines inside tools/release.sh, and it ran on every
// cut. Both facts were wrong. Serving a file is a PUBLICATION, and a cut
// publishes nothing (release-management.md §1) — but more practically, the
// files it copied embed no version: the bootstraps resolve one at install
// time, and the pubkey is the pubkey. Copying them per cut was a network round
// trip to write bytes that had not changed, on the one host where an
// accidental write is visible to everyone.
//
// So it is a verb, and it runs when the KIT changes — a new bootstrap
// template, a regenerated badge, a rotated signing key. Not per release.
//
// WHAT IT DOES NOT COPY: the site's pages. They are rendered by this service
// from the promoted catalog now, so there is nothing static to publish and
// nothing that could go stale between here and the catalog.

import (
	"flag"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"

	"github.com/clawee-git/release/internal/staticsurface"
)

type publishStaticOpts struct {
	root   string
	dest   string
	dryRun bool
	// run executes one command. It is a field so the tests can watch the plan
	// without ever running ssh or scp: nothing in this suite may reach a host,
	// and a seam is the only way to assert on the commands themselves rather
	// than on their effects.
	run func(name string, args ...string) error
}

func (o *publishStaticOpts) register(fs *flag.FlagSet) {
	fs.StringVar(&o.root, "root", "",
		"the release kit checkout `dir` holding the generated files (required)")
	fs.StringVar(&o.dest, "dest", "",
		"where to publish: `[user@host:]dir`, the nginx static root. With no host it is a local copy (required)")
	fs.BoolVar(&o.dryRun, "dry-run", false,
		"print the plan and copy nothing")
}

func runPublishStatic(e *env, n *node, args []string) error {
	o := publishStaticOpts{run: execCommand}
	fs := flag.NewFlagSet(toolName+" "+pathOf(n), flag.ContinueOnError)
	o.register(fs)
	handled, err := parseVerbFlags(e, n, fs, args)
	if handled || err != nil {
		return err
	}
	if err := rejectResiduals(n, fs); err != nil {
		return err
	}
	if strings.TrimSpace(o.root) == "" {
		return usagef(n, "--root is required: it is the kit checkout whose generated files are published, and there is no sensible default for a directory whose contents become the trust anchor")
	}
	if strings.TrimSpace(o.dest) == "" {
		return usagef(n, "--dest is required; the host and path live in the sealed release config, never in this binary")
	}
	// A trailing colon is a host with no directory, and joinDest would then
	// build "host:/clawee/install.sh" — the remote FILESYSTEM ROOT. The typo
	// is one keystroke from the correct spelling and its result is a bootstrap
	// written outside the web root, where nginx cannot serve it and the
	// operator has no reason to look.
	if _, dir := splitDest(o.dest); strings.TrimSpace(dir) == "" {
		return usagef(n, "--dest %q names a host with no directory; the files would land at the remote filesystem root, not in the static root", o.dest)
	}
	root, err := filepath.Abs(o.root)
	if err != nil {
		return fmt.Errorf("resolve --root: %w", err)
	}
	return publishStatic(e, o, root)
}

// publishStatic checks the whole file set BEFORE copying anything, then copies.
//
// Checking first is the point. These files are what a `curl … | sh` fetches,
// so a partial publish is a static root where some bootstraps are new and some
// are old — and every one of them still verifies its own download, so nothing
// downstream notices.
func publishStatic(e *env, o publishStaticOpts, root string) error {
	files := staticsurface.Files()
	var missing []string
	for _, f := range files {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(f))); err != nil {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s is missing %d of the %d static files (%s).\n"+
			"They are GENERATED — regenerate them in the kit and commit, rather than publishing a partial set:\n"+
			"  tools/gen-bootstraps.sh      the install/upgrade bootstraps and their beta twins\n"+
			"  tools/gen-version-jsonp.sh   the per-channel version badges",
			root, len(missing), len(files), strings.Join(missing, ", "))
	}

	host, dir := splitDest(o.dest)
	// Every directory the set needs, deduplicated and created up front: scp
	// does not create one, and a missing directory is the commonest way this
	// half-publishes.
	dirs := targetDirs(files, dir)

	if o.dryRun {
		fmt.Fprintf(e.stdout, "would publish %d files from %s to %s\n", len(files), root, o.dest)
		fmt.Fprintf(e.stdout, "would ensure: %s\n", strings.Join(dirs, " "))
		for _, f := range files {
			fmt.Fprintf(e.stdout, "would copy: %s -> %s\n", f, joinDest(host, dir, f))
		}
		fmt.Fprintln(e.stdout, "✓ dry run: nothing was copied")
		return nil
	}

	if host != "" {
		if err := o.run("ssh", host, "mkdir -p "+strings.Join(quoteAll(dirs), " ")); err != nil {
			return fmt.Errorf("create the target directories on %s: %w", host, err)
		}
	} else {
		for _, d := range dirs {
			if err := os.MkdirAll(d, 0o755); err != nil {
				return fmt.Errorf("create %s: %w", d, err)
			}
		}
	}
	for _, f := range files {
		src := filepath.Join(root, filepath.FromSlash(f))
		dst := joinDest(host, dir, f)
		var err error
		if host != "" {
			err = o.run("scp", "-q", src, dst)
		} else {
			err = o.run("cp", src, dst)
		}
		if err != nil {
			return fmt.Errorf("copy %s: %w", f, err)
		}
		fmt.Fprintf(e.stdout, "✓ %s\n", dst)
	}
	fmt.Fprintf(e.stdout, "published %d files to %s\n", len(files), o.dest)
	return nil
}

// splitDest separates "[user@host:]dir". A path with no colon is local, which
// is the shape this takes once the service itself runs on the release host.
func splitDest(dest string) (host, dir string) {
	if h, d, ok := strings.Cut(dest, ":"); ok {
		return h, d
	}
	return "", dest
}

// joinDest is where one file lands. The separator is "/" on both sides: the
// remote is POSIX, and the local case is too on every platform this ships to.
func joinDest(host, dir, file string) string {
	p := strings.TrimSuffix(dir, "/") + "/" + file
	if host == "" {
		return p
	}
	return host + ":" + p
}

// targetDirs is every directory the file set needs, deduplicated, parents
// first — so one mkdir -p covers the whole publish.
func targetDirs(files []string, dir string) []string {
	base := strings.TrimSuffix(dir, "/")
	seen := map[string]bool{base: true}
	out := []string{base}
	for _, f := range files {
		d := base
		if i := strings.LastIndex(f, "/"); i >= 0 {
			d = base + "/" + f[:i]
		}
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out
}

// quoteAll single-quotes each path for the remote shell. The values are the
// operator's own --dest plus this binary's own constants, so this is hygiene
// rather than a boundary — but a static root with a space in it should publish,
// not silently create two directories.
func quoteAll(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, "'"+strings.ReplaceAll(p, "'", `'\''`)+"'")
	}
	return out
}

// execCommand is the real runner. Its output goes to this process's, because
// an ssh that asks for a passphrase must be able to.
func execCommand(name string, args ...string) error {
	cmd := osexec.Command(name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
