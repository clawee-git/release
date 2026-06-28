// Command r2-mirror publishes a per-stamp release dist directory to the public
// Cloudflare R2 bucket behind downloads.clawee.org (the install-time fallback
// mirror for GitHub Releases). It uploads every top-level *.zip plus
// SHA256SUMS.txt + SHA256SUMS.txt.minisig from the stage dir to
// <comp>/<stamp>/<file>, then writes <comp>/latest.json pointing at them.
//
// R2 is a MIRROR: GitHub Releases stay primary. The release script invokes this
// after a successful GitHub publish and treats any failure here as non-fatal.
//
// Usage:
//
//	r2-mirror --account <id> --bucket clawee-downloads --stage-dir dist/<stamp> \
//	          --comp <clawee|claweed> --version <X.Y.Z> --stamp <v…stamp> \
//	          --creds ~/.burrowee/release/r2.key [--dry-run]
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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"clawee-release-r2-mirror/r2"
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
	stamp    string
	creds    string
	dryRun   bool
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
	flag.StringVar(&cfg.creds, "creds", "", "path to the r2.key TOML (access_key_id + secret_access_key)")
	flag.BoolVar(&cfg.dryRun, "dry-run", false, "print the planned keys and upload nothing")
	flag.Parse()

	if err := cfg.validate(); err != nil {
		return err
	}

	artifacts, zips, err := collectArtifacts(cfg.stageDir)
	if err != nil {
		return err
	}

	manifest := buildManifest(cfg, zips)
	manifestBody, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode latest.json: %w", err)
	}
	manifestBody = append(manifestBody, '\n')
	manifestKey := cfg.comp + "/latest.json"

	if cfg.dryRun {
		fmt.Printf("dry-run: would upload %d objects to bucket %q:\n", len(artifacts)+1, cfg.bucket)
		for _, name := range artifacts {
			fmt.Printf("  %s  (%s)\n", cfg.comp+"/"+cfg.stamp+"/"+name, contentType(name))
		}
		fmt.Printf("  %s  (%s)\n", manifestKey, contentType(manifestKey))
		return nil
	}

	accessKeyID, secret, err := readCreds(cfg.creds)
	if err != nil {
		return err
	}

	ctx := context.Background()
	client := r2.New(cfg.account, cfg.bucket, accessKeyID, secret, nil)

	for _, name := range artifacts {
		key := cfg.comp + "/" + cfg.stamp + "/" + name
		body, err := os.ReadFile(filepath.Join(cfg.stageDir, name))
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if err := client.Put(ctx, key, body, contentType(name)); err != nil {
			return err
		}
		fmt.Printf("  uploaded %s (%d bytes)\n", key, len(body))
	}

	if err := client.Put(ctx, manifestKey, manifestBody, contentType(manifestKey)); err != nil {
		return err
	}
	fmt.Printf("  uploaded %s (%d bytes)\n", manifestKey, len(manifestBody))
	fmt.Printf("✓ mirrored %s %s to bucket %q\n", cfg.comp, cfg.stamp, cfg.bucket)
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

// collectArtifacts returns the top-level files to upload (sorted) and the subset
// that are zips (sorted) for the manifest. It requires at least one zip plus
// SHA256SUMS.txt and SHA256SUMS.txt.minisig — a release without them is broken.
func collectArtifacts(stageDir string) (artifacts, zips []string, err error) {
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
		return nil, nil, fmt.Errorf("no *.zip artifacts in %q", stageDir)
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
	base := cfg.comp + "/" + cfg.stamp
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
