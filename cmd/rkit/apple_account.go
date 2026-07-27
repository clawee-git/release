package main

// Apple account plugin resolution for `rkit build --apple/--public`. Split out
// of build.go, which crossed 400 lines; its tests are apple_account_test.go.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// appleAccountFiles are the config file names, in precedence order, that name
// the Apple account plugin folder (one line: the folder name).
var appleAccountFiles = []string{"config/apple-account", "config/apple.account"}

// appleHomeFiles are the config file names, in precedence order, that hold the
// absolute directory containing one folder per Apple account (one line).
var appleHomeFiles = []string{"config/apple-home", "config/apple.home"}

// loadAppleAccount sets APPLE_ACCOUNT / APPLE_ACCOUNT_DIR / APPLE_HOME from the
// repo's config so modernech-sign picks the project account plugin. It is called
// only when --apple/--public is set, so every failure is fatal: entering the
// Developer-ID path with no account resolved produces an AD-HOC signed build
// while the operator believes it is Developer-ID signed and notarized.
// It used to return silently on every failure mode and runBuild ignored that it
// had done nothing.
//
// Both values resolve the same way — an explicit environment variable first,
// then a per-product config file:
//
//	account   APPLE_ACCOUNT → config/apple-account
//	home      APPLE_HOME    → config/apple-home
//
// or APPLE_ACCOUNT_DIR names the account's folder directly and settles both.
//
// The config files are operator-local and gitignored. That is what lets this
// PUBLIC repo resolve a machine path without carrying one: no $HOME-derived
// default is baked in here, both because no operator's layout belongs in public
// source and because such a default silently becomes a RELATIVE path when HOME
// is unset (launchd, cron, a detached harness session).
//
// tools/apple_sign.sh is release.sh's independent copy of this resolution and
// shares the same precedence — neither entry point invokes the other, so what
// they share is a contract, not an inherited environment.
func loadAppleAccount(repoDir string) error {
	if dir := strings.TrimSpace(os.Getenv("APPLE_ACCOUNT_DIR")); dir != "" {
		if err := checkAccountDir(dir, "APPLE_ACCOUNT_DIR"); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "→ Apple account dir (from APPLE_ACCOUNT_DIR): %s\n", dir)
		return nil
	}
	account, err := resolveAppleValue(repoDir, "APPLE_ACCOUNT", appleAccountFiles,
		"the Apple account plugin folder name")
	if err != nil {
		return err
	}
	home, err := resolveAppleValue(repoDir, "APPLE_HOME", appleHomeFiles,
		"the absolute directory holding one folder per Apple account")
	if err != nil {
		return err
	}
	if !filepath.IsAbs(home) {
		return appleErrorf("APPLE_HOME resolved to %q, which is not an absolute path", home)
	}
	accountDir := filepath.Join(home, account)
	if err := checkAccountDir(accountDir, "the account plugin folder"); err != nil {
		return err
	}
	_ = os.Setenv("APPLE_ACCOUNT", account)
	_ = os.Setenv("APPLE_HOME", home)
	_ = os.Setenv("APPLE_ACCOUNT_DIR", accountDir)
	fmt.Fprintf(os.Stderr, "→ Apple account: %s (%s)\n", account, accountDir)
	return nil
}

// resolveAppleValue returns one value from the environment or, failing that,
// from the first readable config file. holds describes the value, so an
// unresolved one names what the operator is being asked for.
func resolveAppleValue(repoDir, env string, files []string, holds string) (string, error) {
	// An operator cutting under a second account, or against a second plugin
	// root, exports the variable; honour it rather than overriding it from the
	// repo's config file.
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		return v, nil
	}
	for _, name := range files {
		body, err := os.ReadFile(filepath.Join(repoDir, name))
		if err != nil {
			continue
		}
		if v := firstConfigLine(string(body)); v != "" {
			return v, nil
		}
		return "", appleErrorf("%s holds no value — only blank or comment lines.\n%s",
			filepath.Join(repoDir, name), appleFixHint(repoDir, env, files, holds))
	}
	return "", appleErrorf("%s is unresolved.\n%s", env, appleFixHint(repoDir, env, files, holds))
}

// firstConfigLine returns the first non-blank, non-comment line, trimmed.
func firstConfigLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return line
	}
	return ""
}

// appleFixHint lists every way to supply the value, each with what it holds, so
// an operator can get unstuck from the error alone without opening the runbook.
func appleFixHint(repoDir, env string, files []string, holds string) string {
	return fmt.Sprintf("  supply it as one of:\n"+
		"    %s\n        one line: %s\n"+
		"    $%s\n        the same value, from the environment\n"+
		"    $APPLE_ACCOUNT_DIR\n        the account's folder itself, which settles account and home together",
		filepath.Join(repoDir, files[0]), holds, env)
}

// checkAccountDir reports a path that is meant to be an account plugin folder
// but is not a usable directory. label names where the path came from.
func checkAccountDir(dir, label string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return appleErrorf("%s points at %s, which cannot be read: %v", label, dir, err)
	}
	if !info.IsDir() {
		return appleErrorf("%s points at %s, which is not a directory", label, dir)
	}
	return nil
}

// appleErrorf prefixes every failure with what was requested and what refusing
// protects, so the message stands on its own in a build log.
func appleErrorf(format string, args ...any) error {
	return fmt.Errorf("apple signing requested (--apple/--public) but "+format+
		"\n  refusing to continue: signing with no account plugin produces an AD-HOC build "+
		"that looks Developer-ID signed", args...)
}
