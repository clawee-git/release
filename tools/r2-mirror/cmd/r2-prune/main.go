// Command r2-prune applies retention to the public Cloudflare R2 bucket behind
// downloads.clawee.org: it keeps the newest N per-stamp directories for each
// component and deletes every object beneath the older ones.
//
// R2 is the install-time fallback mirror — GitHub Releases stay primary — so it
// accumulated every stamp ever cut. This is the pass that bounds it.
//
// Usage:
//
//	r2-prune [--comp clawee|claweed|all] [--keep 10] [--execute]
//	         [--dir ~/.clawee/release] [--account id] [--bucket name] [--creds path]
//
// Dry-run by default: it prints the planned deletions and removes nothing.
// --execute performs them. Account id and bucket come from <dir>/config.toml
// (r2_account_id / r2_bucket) and the S3 credentials from <dir>/r2.key, each
// overridable by flag; the secret is never printed.
//
// ORDERING: run tools/prune-releases.sh (the GitHub side) BEFORE this. That
// script refuses to delete any release tag not mirrored on R2, so draining R2
// first makes the corresponding GitHub tags permanently un-prunable.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"clawee-release-r2-mirror/prune"
	"clawee-release-r2-mirror/r2"
)

// components is the full set r2-mirror publishes, and so the full set retention
// applies to.
var components = []string{"clawee", "claweed"}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "✗ r2-prune: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	home, _ := os.UserHomeDir()
	dir := flag.String("dir", filepath.Join(home, ".clawee", "release"), "directory holding config.toml and r2.key")
	account := flag.String("account", "", "Cloudflare R2 account id (default: r2_account_id from <dir>/config.toml)")
	bucket := flag.String("bucket", "", "R2 bucket name (default: r2_bucket from <dir>/config.toml)")
	creds := flag.String("creds", "", "path to the r2.key TOML (default: <dir>/r2.key)")
	comp := flag.String("comp", "all", "component: clawee | claweed | all")
	keep := flag.Int("keep", prune.DefaultKeep, "stamps to retain per component")
	execute := flag.Bool("execute", false, "actually delete (default: dry-run)")
	flag.Parse()

	if flag.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q (this command takes flags only)", flag.Arg(0))
	}

	comps := components
	if *comp != "all" {
		if !contains(components, *comp) {
			return fmt.Errorf("unknown component %q (want clawee | claweed | all)", *comp)
		}
		comps = []string{*comp}
	}

	cfgPath := filepath.Join(*dir, "config.toml")
	if *account == "" {
		*account = tomlValue(cfgPath, "r2_account_id")
	}
	if *bucket == "" {
		*bucket = tomlValue(cfgPath, "r2_bucket")
	}
	if *creds == "" {
		*creds = filepath.Join(*dir, "r2.key")
	}
	if *account == "" {
		return fmt.Errorf("no r2_account_id in %s and no --account given", cfgPath)
	}
	if *bucket == "" {
		return fmt.Errorf("no r2_bucket in %s and no --bucket given", cfgPath)
	}

	accessKeyID, secret, err := readCreds(*creds)
	if err != nil {
		return err
	}

	client := r2.New(*account, *bucket, accessKeyID, secret, nil)
	ctx := context.Background()

	mode := "DRY-RUN"
	if *execute {
		mode = "EXECUTE"
	}
	fmt.Printf("bucket=%s  keep=%d  components=[%s]  mode=%s\n\n", *bucket, *keep, strings.Join(comps, " "), mode)

	total := 0
	for _, c := range comps {
		n, err := prune.Prune(ctx, client, c, *keep, *execute, os.Stdout)
		total += n
		if err != nil {
			return fmt.Errorf("prune %s: %w", c, err)
		}
	}

	fmt.Println()
	if *execute {
		fmt.Printf("✓ done — removed %d object(s); kept newest %d stamp(s) per component.\n", total, *keep)
	} else {
		fmt.Printf("DRY-RUN: %d object(s) would be removed. Re-run with --execute to apply.\n", total)
	}
	return nil
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// tomlValue returns the value of key from a minimal TOML file
// (`key = "value"` or `key = value`, one per line; '#' comments allowed),
// or "" if the file or the key is absent — the caller reports the missing
// setting with the context to fix it.
func tomlValue(path, key string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != key {
			continue
		}
		return strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return ""
}

// readCreds parses access_key_id + secret_access_key from the r2.key TOML. The
// secret is returned to the caller and never logged. Same shape as the one in
// r2-mirror's main.go — the two binaries read the same file.
func readCreds(path string) (accessKeyID, secret string, err error) {
	accessKeyID = tomlValue(path, "access_key_id")
	secret = tomlValue(path, "secret_access_key")
	if accessKeyID == "" || secret == "" {
		return "", "", fmt.Errorf("creds %q: missing access_key_id or secret_access_key", path)
	}
	return accessKeyID, secret, nil
}
