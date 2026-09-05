package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clawee-git/release/internal/staticsurface"
)

// releaseVhost names the committed vhost and the values it was rendered with.
// They are the release host's own — already committed in that file and in
// ops/README.md — and they live in exactly one place here so the gate below
// can regenerate the file rather than have a human keep it in step.
const (
	releaseVhostFile = "../../ops/nginx/release.clawee.org.conf"
	releaseVhostHost = "release.clawee.org"
	releaseStaticDir = "/ebs_storage/apps/release.clawee.org/static"
)

// testOpts is a fully-wired render with values that are obviously fake. No
// test in this suite may carry a real bucket, token path or account.
func testOpts() opsRenderOpts {
	o := opsRenderOpts{
		Out: "", Exec: "/usr/local/bin/clawee-release-manage",
		User: defaultServiceUser, DataDir: "/srv/clawee-release/data",
		Listen: defaultListen, BaseURL: "https://manage.example.org",
		RetainAt: defaultRetainAt, Host: "manage.example.org",
		StaticDir: "/srv/static", TLSProtocols: defaultTLSProtocols,
	}
	o.r2Account = "ACCOUNT"
	o.r2Creds = "/etc/clawee-release/r2.key"
	o.stagingBucket = "staging-example"
	o.publicBucket = "public-example"
	o.githubRepo = "example-org/example-repo"
	o.githubToken = "/etc/clawee-release/github.token"
	o.publicBaseURL = "https://downloads.example.org"
	return o
}

func rendered(t *testing.T, o opsRenderOpts) map[string]string {
	t.Helper()
	files, err := renderOps(o)
	if err != nil {
		t.Fatalf("renderOps: %v", err)
	}
	out := map[string]string{}
	for _, f := range files {
		out[f.name] = f.body
	}
	return out
}

// The unit is the whole point of rendering rather than documenting: an
// operator who types a unit by hand types the run-as user, the flags and the
// hardening lines, and every one of those is a line whose absence looks
// exactly like a working service.
func TestServiceUnitRunsAsTheDedicatedUserWithTheRenderedFlags(t *testing.T) {
	unit := rendered(t, testOpts())["systemd/"+serviceUnitFile]

	if !strings.Contains(unit, "\nUser=clawee-release\n") {
		t.Errorf("unit does not run as the dedicated user:\n%s", unit)
	}
	// An unset User= in a system unit is a service running as ROOT. It is the
	// one omission here that is silently accepted by systemd and changes what
	// a compromise of the process is worth.
	for _, want := range []string{
		`ExecStart="/usr/local/bin/clawee-release-manage" "serve"`,
		`"--data-dir" "/srv/clawee-release/data"`,
		`"--listen" "127.0.0.1:8787"`,
		`"--base-url" "https://manage.example.org"`,
		`"--staging-bucket" "staging-example"`,
		`"--public-bucket" "public-example"`,
		`"--github-repo" "example-org/example-repo"`,
		`"--public-base-url" "https://downloads.example.org"`,
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit is missing %s:\n%s", want, unit)
		}
	}
}

// The hardening lines are asserted individually because ProtectSystem=strict
// without ReadWritePaths is a service that starts, serves every read-only
// page, and fails at the first catalog write — which reads as a database
// problem, not as a unit setting.
func TestServiceUnitIsHardenedAndWritesOnlyTheDataDir(t *testing.T) {
	unit := rendered(t, testOpts())["systemd/"+serviceUnitFile]
	for _, want := range []string{
		"NoNewPrivileges=yes",
		"ProtectSystem=strict",
		"ReadWritePaths=/srv/clawee-release/data",
		"PrivateTmp=yes",
		"RestrictSUIDSGID=yes",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit is missing %q:\n%s", want, unit)
		}
	}
	if n := strings.Count(unit, "ReadWritePaths="); n != 1 {
		t.Errorf("unit re-opens %d paths for writing; the data dir is the complete list", n)
	}
}

// An optional store flag with no value must be OMITTED, not rendered empty:
// `serve` refuses a half-configured store, so a unit carrying
// `--staging-bucket ""` is a service that will not start at all.
func TestUnitOmitsUnsetStoreFlagsRatherThanRenderingThemEmpty(t *testing.T) {
	o := testOpts()
	o.storeOpts = storeOpts{}
	unit := rendered(t, o)["systemd/"+serviceUnitFile]
	for _, absent := range []string{"--staging-bucket", "--public-bucket", "--github-repo", "--r2-account"} {
		if strings.Contains(unit, absent) {
			t.Errorf("unit renders %s with no value:\n%s", absent, unit)
		}
	}
}

func TestRetainTimerRunsNightlyAndCatchesUpAfterADownHost(t *testing.T) {
	files := rendered(t, testOpts())
	timer := files["systemd/"+retainTimerFile]
	for _, want := range []string{
		"OnCalendar=*-*-* 03:20:00",
		"Persistent=true",
		"Unit=" + retainUnitFile,
		"WantedBy=timers.target",
	} {
		if !strings.Contains(timer, want) {
			t.Errorf("timer is missing %q:\n%s", want, timer)
		}
	}
	svc := files["systemd/"+retainUnitFile]
	if !strings.Contains(svc, "Type=oneshot") {
		t.Errorf("the retention pass is not a oneshot:\n%s", svc)
	}
	if !strings.Contains(svc, `"retain" "--data-dir" "/srv/clawee-release/data"`) {
		t.Errorf("the retention pass does not run retain against the data dir:\n%s", svc)
	}
	// The timer and the service must name the same store, or the nightly pass
	// prunes a bucket the service never publishes to.
	if !strings.Contains(svc, `"--public-bucket" "public-example"`) {
		t.Errorf("the retention pass names a different store than the service:\n%s", svc)
	}
}

// The vhost's split is the feature: every static path stays a file, and
// EVERYTHING else — the pages and the operator's /manage — is the service's.
func TestVhostKeepsEveryStaticLocationAndProxiesTheRest(t *testing.T) {
	conf := rendered(t, testOpts())["nginx/manage.example.org.conf"]

	for _, loc := range []string{
		`location ~ \.sh$`,
		`location ~ \.js$`,
		`location = /clawee-release.pub`,
		`location ~ \.(txt|minisig)$`,
	} {
		if !strings.Contains(conf, loc) {
			t.Errorf("vhost lost the static location %q:\n%s", loc, conf)
		}
	}
	// Every file publish-static writes must be matched by one of those
	// locations — a static file the vhost proxies is a 404 from a service that
	// has no such route.
	for _, f := range staticsurface.Files() {
		switch {
		case strings.HasSuffix(f, ".sh"), strings.HasSuffix(f, ".js"),
			f == staticsurface.Pubkey:
		default:
			t.Errorf("static file %q is matched by no location in the vhost", f)
		}
	}
	if !strings.Contains(conf, "location / {") || !strings.Contains(conf, "proxy_pass         http://127.0.0.1:8787;") {
		t.Errorf("vhost does not proxy / to the service:\n%s", conf)
	}
	// /manage is not its own location ON PURPOSE: it falls through `location /`
	// like every other page. A separate block would be a second place for the
	// proxy settings to drift.
	if strings.Contains(conf, "location /manage") {
		t.Errorf("vhost gives /manage its own block; it belongs to `location /`:\n%s", conf)
	}
	if !strings.Contains(conf, "proxy_buffering    off;") {
		t.Errorf("vhost buffers the promote progress stream:\n%s", conf)
	}
}

// The committed vhost IS this template's output — the same gate docs/cli-help.md
// has, and for the same reason: a hand-edited conf is one the renderer will
// silently overwrite the next time an operator runs `ops render`.
func TestCommittedVhostIsTheRenderedOne(t *testing.T) {
	o := testOpts()
	o.storeOpts = storeOpts{}
	o.Host = releaseVhostHost
	o.StaticDir = releaseStaticDir

	want, err := os.ReadFile(releaseVhostFile)
	if err != nil {
		t.Fatalf("read the committed vhost: %v", err)
	}
	got := rendered(t, o)["nginx/"+releaseVhostHost+".conf"]
	if got != string(want) {
		t.Errorf("%s is stale. Regenerate it:\n"+
			"  go run ./cmd/%s ops render --out <tmp> --host %s --static-dir %s \\\n"+
			"      --data-dir <DATA_DIR> --base-url https://%s\n"+
			"  cp <tmp>/nginx/%s.conf ops/nginx/",
			releaseVhostFile, toolName, releaseVhostHost, releaseStaticDir,
			releaseVhostHost, releaseVhostHost)
	}
}

func TestOpsRenderWritesTheWholeSetAndInstallsNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "rendered")
	stdout, stderr, code := exec(t, "ops", "render", "--out", dir,
		"--host", "manage.example.org", "--static-dir", "/srv/static",
		"--data-dir", "/srv/clawee-release/data", "--base-url", "https://manage.example.org")
	if code != 0 {
		t.Fatalf("ops render exited %d: %s", code, stderr)
	}
	for _, f := range []string{
		"systemd/" + serviceUnitFile,
		"systemd/" + retainUnitFile,
		"systemd/" + retainTimerFile,
		"nginx/manage.example.org.conf",
	} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f))); err != nil {
			t.Errorf("ops render did not write %s: %v", f, err)
		}
	}
	// The output must say that installing is the operator's, because a
	// renderer that reads as an installer is one somebody runs on the host.
	if !strings.Contains(stdout, "Nothing was installed") {
		t.Errorf("ops render does not say it installed nothing:\n%s", stdout)
	}
}

func TestOpsRenderRefusesWithoutTheValuesItWillNotGuess(t *testing.T) {
	for _, missing := range []string{"--out", "--data-dir", "--base-url", "--host", "--static-dir"} {
		args := []string{"ops", "render",
			"--out", t.TempDir(), "--host", "manage.example.org",
			"--static-dir", "/srv/static", "--data-dir", "/d",
			"--base-url", "https://manage.example.org"}
		var kept []string
		for i := 0; i < len(args); i++ {
			if args[i] == missing {
				i++
				continue
			}
			kept = append(kept, args[i])
		}
		_, stderr, code := exec(t, kept...)
		if code != exitUsage {
			t.Errorf("without %s: exit %d, want %d", missing, code, exitUsage)
		}
		if !strings.Contains(stderr, strings.TrimPrefix(missing, "--")) {
			t.Errorf("without %s the refusal does not name it: %s", missing, stderr)
		}
	}
}

// A data root with a space in it is one argument, not two. Without the
// quoting the unit starts against a directory nobody named — a second, empty
// catalog, which looks like a service that lost its releases.
func TestExecStartQuotesEveryArgument(t *testing.T) {
	o := testOpts()
	o.DataDir = "/srv/clawee release/data"
	unit := rendered(t, o)["systemd/"+serviceUnitFile]
	if !strings.Contains(unit, `"--data-dir" "/srv/clawee release/data"`) {
		t.Errorf("ExecStart does not quote a path with a space:\n%s", unit)
	}
}
