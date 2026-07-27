package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeAppleConfig creates repoDir/config/<name> holding body.
func writeAppleConfig(t *testing.T, repoDir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(repoDir, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "config", name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// clearAppleEnv unsets every variable the resolution reads, restoring after.
func clearAppleEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"APPLE_ACCOUNT", "APPLE_ACCOUNT_DIR", "APPLE_HOME", "HOME"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

// appleHome makes a plugin root holding one folder per account name.
func appleHome(t *testing.T, accounts ...string) string {
	t.Helper()
	home := t.TempDir()
	for _, a := range accounts {
		if err := os.MkdirAll(filepath.Join(home, a), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

// TestLoadAppleAccountResolvesFromConfig is the happy path: both values come
// from the repo's own config, so a cut needs nothing exported, and every
// variable the signer reads is set.
func TestLoadAppleAccountResolvesFromConfig(t *testing.T) {
	clearAppleEnv(t)
	repo := t.TempDir()
	home := appleHome(t, "AcmeCorp")
	writeAppleConfig(t, repo, "apple-account", "# the project account\n\nAcmeCorp\n")
	writeAppleConfig(t, repo, "apple-home", home+"\n")

	if err := loadAppleAccount(repo); err != nil {
		t.Fatalf("loadAppleAccount: %v", err)
	}
	if got := os.Getenv("APPLE_ACCOUNT"); got != "AcmeCorp" {
		t.Errorf("APPLE_ACCOUNT = %q, want AcmeCorp", got)
	}
	if got := os.Getenv("APPLE_HOME"); got != home {
		t.Errorf("APPLE_HOME = %q, want %q from config/apple-home", got, home)
	}
	if got, want := os.Getenv("APPLE_ACCOUNT_DIR"), filepath.Join(home, "AcmeCorp"); got != want {
		t.Errorf("APPLE_ACCOUNT_DIR = %q, want %q", got, want)
	}
}

// TestLoadAppleAccountEnvBeatsConfig: an operator cutting under a second
// account, or against a second plugin root, exports the variable — and that
// must win over the repo's file for BOTH values.
func TestLoadAppleAccountEnvBeatsConfig(t *testing.T) {
	clearAppleEnv(t)
	repo := t.TempDir()
	envHome := appleHome(t, "OtherLLC")
	writeAppleConfig(t, repo, "apple-account", "AcmeCorp\n")
	writeAppleConfig(t, repo, "apple-home", appleHome(t, "AcmeCorp")+"\n")
	t.Setenv("APPLE_ACCOUNT", "OtherLLC")
	t.Setenv("APPLE_HOME", envHome)

	if err := loadAppleAccount(repo); err != nil {
		t.Fatalf("loadAppleAccount: %v", err)
	}
	if got := os.Getenv("APPLE_ACCOUNT"); got != "OtherLLC" {
		t.Errorf("APPLE_ACCOUNT = %q, want the exported OtherLLC", got)
	}
	if got, want := os.Getenv("APPLE_ACCOUNT_DIR"), filepath.Join(envHome, "OtherLLC"); got != want {
		t.Errorf("APPLE_ACCOUNT_DIR = %q, want %q", got, want)
	}
}

// TestLoadAppleAccountDirSettlesBoth: APPLE_ACCOUNT_DIR names the plugin folder
// itself, so neither config file is consulted — but a stale one pointing at a
// folder that no longer exists must abort rather than fail later in the signer.
func TestLoadAppleAccountDirSettlesBoth(t *testing.T) {
	clearAppleEnv(t)
	repo := t.TempDir() // deliberately no config files at all
	dir := filepath.Join(appleHome(t, "AcmeCorp"), "AcmeCorp")
	t.Setenv("APPLE_ACCOUNT_DIR", dir)

	if err := loadAppleAccount(repo); err != nil {
		t.Fatalf("loadAppleAccount with APPLE_ACCOUNT_DIR and no config: %v", err)
	}

	t.Setenv("APPLE_ACCOUNT_DIR", filepath.Join(dir, "gone"))
	if err := loadAppleAccount(repo); err == nil {
		t.Fatal("a stale APPLE_ACCOUNT_DIR pointing at a missing folder must abort")
	}
}

// TestLoadAppleAccountFailsClosed covers every mode that used to return
// silently, leaving the Developer-ID path running with no account plugin — an
// ad-hoc signed build the operator believes is Developer-ID signed.
func TestLoadAppleAccountFailsClosed(t *testing.T) {
	cases := []struct {
		name        string
		account     string // "" = write no config/apple-account
		appleHomeIn string // "" = no config/apple-home; "SET" = a real one; else literal
		wantErrSub  string
	}{
		{
			name:       "nothing configured and nothing exported",
			wantErrSub: "APPLE_ACCOUNT is unresolved",
		},
		{
			name:        "account config holds only comments and blanks",
			account:     "# nothing here\n\n   \n",
			appleHomeIn: "SET",
			wantErrSub:  "holds no value",
		},
		{
			name:       "no home anywhere — nothing derived from $HOME",
			account:    "AcmeCorp\n",
			wantErrSub: "APPLE_HOME is unresolved",
		},
		{
			name:        "home is relative",
			account:     "AcmeCorp\n",
			appleHomeIn: "Workstation/Apple",
			wantErrSub:  "not an absolute path",
		},
		{
			name:        "account plugin folder missing",
			account:     "MissingAccount\n",
			appleHomeIn: "SET",
			wantErrSub:  "account plugin folder points at",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearAppleEnv(t)
			repo := t.TempDir()
			if c.account != "" {
				writeAppleConfig(t, repo, "apple-account", c.account)
			}
			switch c.appleHomeIn {
			case "":
			case "SET":
				writeAppleConfig(t, repo, "apple-home", appleHome(t, "AcmeCorp")+"\n")
			default:
				writeAppleConfig(t, repo, "apple-home", c.appleHomeIn+"\n")
			}

			err := loadAppleAccount(repo)
			if err == nil {
				t.Fatalf("loadAppleAccount returned nil; APPLE_ACCOUNT_DIR=%q", os.Getenv("APPLE_ACCOUNT_DIR"))
			}
			if !strings.Contains(err.Error(), c.wantErrSub) {
				t.Fatalf("error = %v, want it to mention %q", err, c.wantErrSub)
			}
		})
	}
}

// TestAppleErrorsNameTheFix: every failure has to be self-sufficient. An
// operator reading only the error should learn what was requested, why it
// refused, and each of the three ways to supply the missing value — otherwise
// the message costs a runbook lookup at exactly the wrong moment.
func TestAppleErrorsNameTheFix(t *testing.T) {
	clearAppleEnv(t)
	repo := t.TempDir()
	writeAppleConfig(t, repo, "apple-account", "AcmeCorp\n") // account fine, home missing

	err := loadAppleAccount(repo)
	if err == nil {
		t.Fatal("want an error when APPLE_HOME is unresolved")
	}
	for _, want := range []string{
		"--apple/--public",             // what was requested
		"AD-HOC",                       // what refusing protects
		"config/apple-home",            // the file to create
		"$APPLE_HOME",                  // the variable to export
		"$APPLE_ACCOUNT_DIR",           // the way that settles both
		"one folder per Apple account", // what the value holds
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing %q — it must stand alone in a build log.\ngot: %v", want, err)
		}
	}
}

// TestLoadAppleAccountCarriesNoOperatorLayout: this repo is PUBLIC. No
// operator's personal directory tree may be baked into its source as a default.
// Scans every non-test source file so the guard survives the code moving files.
func TestLoadAppleAccountCarriesNoOperatorLayout(t *testing.T) {
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	scanned := 0
	for _, path := range sources {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		for _, banned := range []string{`"Workstation"`, "Workstation/Apple"} {
			if strings.Contains(string(src), banned) {
				t.Errorf("%s bakes an operator machine layout (%s) into a public repo — "+
					"the plugin root must come from config/apple-home or the environment", path, banned)
			}
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no source files — the glob is wrong, so this guard proves nothing")
	}
}
