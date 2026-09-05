// Command r2-mirror uploads a per-stamp release dist directory to a
// Cloudflare R2 bucket. It uploads every top-level *.zip plus SHA256SUMS.txt +
// SHA256SUMS.txt.minisig from the stage dir to <comp>/[<prefix>/]<stamp>/<file>,
// and — unless --no-manifest — writes <comp>/latest.json pointing at them.
//
// TWO CALLERS, TWO POSTURES. The bucket, the key prefix and the manifest are
// all flags because a cut and a go-live are different acts
// (~/.agents/guidelines/release-management.md §3):
//
//   - the CUT stages into the PRIVATE bucket under <comp>/<channel>/<stamp>/
//     with --no-manifest — nothing public changes, so nothing may name the new
//     stamp as current;
//   - PROMOTE copies the same bytes into the PUBLIC bucket under the public
//     layout and writes latest.json, which is what makes a release reachable.
//
// A manifest write is therefore the go-live, not a bookkeeping detail: writing
// one from the cut path would publish a staged build to every installer.
//
// Usage:
//
//	r2-mirror --account <id> --bucket <bucket> --stage-dir dist/<stamp> \
//	          --comp <clawee|claweed> --version <X.Y.Z> --stamp <v…stamp> \
//	          [--prefix <channel>] [--no-manifest] \
//	          --creds <r2.key> [--dry-run]
//
// The S3 credentials (access_key_id + secret_access_key) are read from the TOML
// file at --creds and are NEVER printed. --dry-run prints the planned keys and
// uploads nothing (no creds required).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/clawee-git/release/internal/r2"
)

const (
	sumsName    = "SHA256SUMS.txt"
	minisigName = "SHA256SUMS.txt.minisig"
)

// latestManifest is the <comp>/latest.json schema. Fields are declared in
// alphabetical order so json.Marshal emits them in the same order as the live
// bucket's hand-uploaded manifest (a stable, diff-friendly shape).
type latestManifest struct {
	Component  string   `json:"component"`
	Minisig    string   `json:"minisig"`
	Path       string   `json:"path"`
	SHA256Sums string   `json:"sha256sums"`
	Stamp      string   `json:"stamp"`
	Updated    string   `json:"updated"`
	Version    string   `json:"version"`
	Zips       []string `json:"zips"`
}

type config struct {
	account  string
	bucket   string
	stageDir string
	comp     string
	version  string
	stamp      string
	prefix     string
	noManifest bool
	creds      string
	dryRun     bool
}

// keyBase is the object-key directory this cut's artifacts land in:
// <comp>/<stamp> with no prefix (the public layout), <comp>/<prefix>/<stamp>
// with one (the staging layout, where prefix is the channel). Surrounding
// slashes on --prefix are tolerated so a caller passing "beta/" or "/beta"
// cannot produce a doubled or leading separator in a key.
func (c config) keyBase() string {
	prefix := strings.Trim(strings.TrimSpace(c.prefix), "/")
	if prefix == "" {
		return c.comp + "/" + c.stamp
	}
	return c.comp + "/" + prefix + "/" + c.stamp
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "✗ r2-mirror: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var cfg config
	flag.StringVar(&cfg.account, "account", "", "Cloudflare R2 account id")
	flag.StringVar(&cfg.bucket, "bucket", "", "R2 bucket name (e.g. clawee-downloads)")
	flag.StringVar(&cfg.stageDir, "stage-dir", "", "per-stamp dist directory to mirror")
	flag.StringVar(&cfg.comp, "comp", "", "component name (clawee | claweed)")
	flag.StringVar(&cfg.version, "version", "", "human semver, e.g. 0.1.66")
	flag.StringVar(&cfg.stamp, "stamp", "", "full release stamp, e.g. v0.1.66.2026.06.28.12e6b0fc")
	flag.StringVar(&cfg.prefix, "prefix", "", "key prefix between component and stamp (e.g. the channel: stable | beta); empty = the flat public layout")
	flag.BoolVar(&cfg.noManifest, "no-manifest", false, "upload the artifacts only — do not write <comp>/latest.json (a manifest write is the go-live)")
	flag.StringVar(&cfg.creds, "creds", "", "path to the r2.key TOML (access_key_id + secret_access_key)")
	flag.BoolVar(&cfg.dryRun, "dry-run", false, "print the planned keys and upload nothing")
	flag.Parse()

	return execute(cfg, os.Stdout)
}

// execute is run() past flag parsing, with the progress stream injected so the
// tests can read what a dry-run planned instead of scraping os.Stdout.
func execute(cfg config, out io.Writer) error {
	if err := cfg.validate(); err != nil {
		return err
	}

	artifacts, zips, err := collectArtifacts(cfg.stageDir, cfg.comp)
	if err != nil {
		return err
	}

	base := cfg.keyBase()
	var manifestBody []byte
	manifestKey := cfg.comp + "/latest.json"
	if !cfg.noManifest {
		manifest := buildManifest(cfg, zips)
		manifestBody, err = json.MarshalIndent(manifest, "", "  ")
		if err != nil {
			return fmt.Errorf("encode latest.json: %w", err)
		}
		manifestBody = append(manifestBody, '\n')
	}

	if cfg.dryRun {
		planned := len(artifacts)
		if !cfg.noManifest {
			planned++
		}
		fmt.Fprintf(out, "dry-run: would upload %d objects to bucket %q:\n", planned, cfg.bucket)
		for _, name := range artifacts {
			fmt.Fprintf(out, "  %s  (%s)\n", base+"/"+name, contentType(name))
		}
		if cfg.noManifest {
			fmt.Fprintf(out, "  (no manifest: %s is NOT written)\n", manifestKey)
		} else {
			fmt.Fprintf(out, "  %s  (%s)\n", manifestKey, contentType(manifestKey))
		}
		return nil
	}

	accessKeyID, secret, err := readCreds(cfg.creds)
	if err != nil {
		return err
	}

	ctx := context.Background()
	client := r2.New(cfg.account, cfg.bucket, accessKeyID, secret, nil)

	for _, name := range artifacts {
		key := base + "/" + name
		body, err := os.ReadFile(filepath.Join(cfg.stageDir, name))
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if err := client.Put(ctx, key, body, contentType(name)); err != nil {
			return err
		}
		fmt.Fprintf(out, "  uploaded %s (%d bytes)\n", key, len(body))
	}

	if cfg.noManifest {
		fmt.Fprintf(out, "✓ staged %s %s to bucket %q under %s/ (no manifest written)\n", cfg.comp, cfg.stamp, cfg.bucket, base)
		return nil
	}
	if err := client.Put(ctx, manifestKey, manifestBody, contentType(manifestKey)); err != nil {
		return err
	}
	fmt.Fprintf(out, "  uploaded %s (%d bytes)\n", manifestKey, len(manifestBody))
	fmt.Fprintf(out, "✓ mirrored %s %s to bucket %q\n", cfg.comp, cfg.stamp, cfg.bucket)
	return nil
}

func (c config) validate() error {
	missing := func(name, val string) error {
		if strings.TrimSpace(val) == "" {
			return fmt.Errorf("missing required flag --%s", name)
		}
		return nil
	}
	for _, f := range []struct{ name, val string }{
		{"comp", c.comp}, {"version", c.version}, {"stamp", c.stamp}, {"stage-dir", c.stageDir},
	} {
		if err := missing(f.name, f.val); err != nil {
			return err
		}
	}
	if c.comp != "clawee" && c.comp != "claweed" {
		return fmt.Errorf("unknown component %q (want clawee | claweed)", c.comp)
	}
	// An empty prefix is the FLAT PUBLIC LAYOUT, and --no-manifest is the cut's
	// posture — so the combination means "stage a private build into the public
	// key space", which is how a staged build ends up where retention and the
	// download page look for promoted ones. The caller cannot reach this state
	// deliberately; it reaches it when a channel string arrives empty because
	// something upstream failed without saying so. Refuse rather than quietly
	// pick a layout.
	if c.noManifest && strings.Trim(strings.TrimSpace(c.prefix), "/") == "" {
		return fmt.Errorf("--no-manifest with no --prefix would stage into the flat public layout %s/<stamp>/ — pass the channel as --prefix", c.comp)
	}
	info, err := os.Stat(c.stageDir)
	if err != nil {
		return fmt.Errorf("stage-dir %q: %w", c.stageDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("stage-dir %q is not a directory", c.stageDir)
	}
	if c.dryRun {
		return nil
	}
	for _, f := range []struct{ name, val string }{
		{"account", c.account}, {"bucket", c.bucket}, {"creds", c.creds},
	} {
		if err := missing(f.name, f.val); err != nil {
			return err
		}
	}
	return nil
}

// zipName is the release kit's zip naming, templated on the component:
// clawee-<comp>-<os>-<arch>.zip. The UPLOAD set and the CATALOG set must be the
// same set — internal/register applies the identical rule when it builds the
// row — because a file that is uploaded but not registered is a byte nobody can
// promote, and a file registered but not uploaded is a 404 at install time.
//
// This used to take every top-level *.zip, which is a wider rule than the
// catalog's: a stray zip was uploaded and then registration failed AFTER the
// bytes were up. Refusing here means the failure lands before anything moves.
var zipName = regexp.MustCompile(`^clawee-[a-z]+-[a-z0-9]+-[a-z0-9]+\.zip$`)

// collectArtifacts returns the top-level files to upload (sorted) and the subset
// that are zips (sorted) for the manifest. It requires at least one zip plus
// SHA256SUMS.txt and SHA256SUMS.txt.minisig — a release without them is broken.
func collectArtifacts(stageDir, comp string) (artifacts, zips []string, err error) {
	entries, err := os.ReadDir(stageDir)
	if err != nil {
		return nil, nil, fmt.Errorf("read stage-dir %q: %w", stageDir, err)
	}
	var hasSums, hasMinisig bool
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		switch {
		case strings.HasSuffix(name, ".zip"):
			if !zipName.MatchString(name) {
				return nil, nil, fmt.Errorf("zip %q does not match clawee-<comp>-<os>-<arch>.zip — refusing to upload a file the catalog cannot describe", name)
			}
			// Another component's zips legitimately share a stage dir; they are
			// that component's cut to upload, not this one's.
			if !strings.HasPrefix(name, "clawee-"+comp+"-") {
				continue
			}
			artifacts = append(artifacts, name)
			zips = append(zips, name)
		case name == sumsName:
			artifacts = append(artifacts, name)
			hasSums = true
		case name == minisigName:
			artifacts = append(artifacts, name)
			hasMinisig = true
		}
	}
	if len(zips) == 0 {
		return nil, nil, fmt.Errorf("no clawee-%s-*.zip artifacts in %q", comp, stageDir)
	}
	if !hasSums {
		return nil, nil, fmt.Errorf("%s missing from %q", sumsName, stageDir)
	}
	if !hasMinisig {
		return nil, nil, fmt.Errorf("%s missing from %q", minisigName, stageDir)
	}
	slices.Sort(artifacts)
	slices.Sort(zips)
	return artifacts, zips, nil
}

func buildManifest(cfg config, zips []string) latestManifest {
	base := cfg.keyBase()
	return latestManifest{
		Component:  cfg.comp,
		Version:    cfg.version,
		Stamp:      cfg.stamp,
		Path:       base,
		Zips:       zips,
		SHA256Sums: base + "/" + sumsName,
		Minisig:    base + "/" + minisigName,
		Updated:    time.Now().UTC().Format(time.RFC3339),
	}
}

func contentType(name string) string {
	switch {
	case strings.HasSuffix(name, ".zip"):
		return "application/zip"
	case strings.HasSuffix(name, ".json"):
		return "application/json"
	default:
		return "text/plain"
	}
}

// readCreds parses access_key_id + secret_access_key from a minimal TOML file
// (`key = "value"` or `key = value`, one per line; '#' comments allowed). The
// secret is returned to the caller and never logged.
func readCreds(path string) (accessKeyID, secret string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("read creds %q: %w", path, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		switch strings.TrimSpace(key) {
		case "access_key_id":
			accessKeyID = val
		case "secret_access_key":
			secret = val
		}
	}
	if accessKeyID == "" || secret == "" {
		return "", "", fmt.Errorf("creds %q: missing access_key_id or secret_access_key", path)
	}
	return accessKeyID, secret, nil
}
