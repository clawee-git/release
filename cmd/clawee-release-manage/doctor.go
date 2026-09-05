package main

// The `doctor` verb: wire the real seams to internal/manage/doctor and print
// the report.
//
// Everything the pass needs is a flag this binary already has, so `doctor`
// takes the SAME store flags `serve` does. That is the point — a doctor run
// with different flags than the service is a doctor checking a different
// deployment, and the failure it would then miss is the one where the unit
// names one bucket and the operator's shell names another.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/clawee-git/release/internal/manage/auth"
	"github.com/clawee-git/release/internal/manage/backend"
	"github.com/clawee-git/release/internal/manage/doctor"
	"github.com/clawee-git/release/internal/manage/intake"
	"github.com/clawee-git/release/internal/manage/store"
)

type doctorOpts struct {
	commonOpts
	storeOpts
	secretKey  string
	kitRoot    string
	checkWrite bool
	asJSON     bool
}

func (o *doctorOpts) register(fs *flag.FlagSet) {
	o.registerDataDir(fs)
	o.storeOpts.register(fs)
	fs.StringVar(&o.secretKey, "secret-key", "",
		"`path` to the service secret key; defaults to secret.key inside --data-dir")
	fs.StringVar(&o.kitRoot, "kit-root", "",
		"a release kit checkout `dir` on this host; the signing key is compared against its pubkey and bootstraps")
	fs.BoolVar(&o.checkWrite, "check-write", false,
		"also require that the GitHub token's permissions would allow publishing a release; nothing is created either way")
	fs.BoolVar(&o.asJSON, "json", false,
		"print the report as JSON instead of one line per check")
}

func runDoctor(e *env, n *node, args []string) error {
	var o doctorOpts
	fs := flag.NewFlagSet(toolName+" "+pathOf(n), flag.ContinueOnError)
	o.register(fs)
	handled, err := parseVerbFlags(e, n, fs, args)
	if handled || err != nil {
		return err
	}
	if err := rejectResiduals(n, fs); err != nil {
		return err
	}
	if err := requireDataDir(n, o.dataDir); err != nil {
		return err
	}
	deps, err := o.deps(n)
	if err != nil {
		return err
	}

	report := doctor.Run(context.Background(), deps)
	if err := printReport(e, report, o.asJSON); err != nil {
		return err
	}
	if report.Failed() {
		// A plain error, so the tool exits 1: the checks ran and the
		// deployment is wrong, which is not the same thing as a typo (2).
		return fmt.Errorf("%d of %d checks failed", report.Failures(), len(report.Checks))
	}
	return nil
}

// deps builds the real seams. Anything unconfigured stays nil, and its check
// reports skipped — the service is brought up in stages, so a doctor that
// failed on a stage nobody has reached yet would be one nobody reads.
func (o *doctorOpts) deps(n *node) (doctor.Deps, error) {
	secretKey := o.secretKey
	if secretKey == "" {
		secretKey = filepath.Join(o.dataDir, auth.SecretKeyFile)
	}
	abs, err := filepath.Abs(secretKey)
	if err != nil {
		return doctor.Deps{}, fmt.Errorf("resolve secret key path: %w", err)
	}
	key, err := intake.ReleasePubkeyLine()
	if err != nil {
		return doctor.Deps{}, err
	}

	d := doctor.Deps{
		CheckWrite:     o.checkWrite,
		DataDir:        o.dataDir,
		SecretKeyPath:  abs,
		KitRoot:        o.kitRoot,
		EmbeddedKey:    key,
		WantMigrations: store.LatestMigration(),
		UID:            os.Getuid(),
		Catalog: func() ([]string, error) {
			st, err := store.Open(o.dataDir)
			if err != nil {
				return nil, err
			}
			defer st.Close()
			return st.AppliedMigrations()
		},
	}

	backends, err := o.backends(n)
	if err != nil {
		return doctor.Deps{}, err
	}
	if backends.Staging != nil {
		st := backends.Staging
		d.Staging = &doctor.Bucket{Name: st.Bucket(), List: st.List}
		d.AnonGet = anonGet(o.r2Account)
	}
	if backends.Public != nil {
		pub := backends.Public
		d.Public = &doctor.Bucket{Name: o.publicBucket, List: pub.List}
	}
	if gh, ok := backends.GitHub.(*backend.GitHubClient); ok && gh != nil {
		d.Repo = func(ctx context.Context) (doctor.RepoInfo, error) {
			info, err := gh.ReadRepo(ctx)
			return doctor.RepoInfo{FullName: info.FullName, CanPublishReleases: info.CanPublishReleases}, err
		}
	}
	return d, nil
}

// anonGet asks the bucket what it says to a caller holding NOTHING: no
// signature, no credentials, no session.
//
// The account id is closed over rather than read from anywhere global: two
// deployments checked from one process would otherwise share it, and the
// second would probe the first's endpoint and report ITS answer. The probe
// goes through the download guard like every other outbound request, so it
// cannot be redirected onto a private address, and it reads only the status —
// the body of a bucket that wrongly answers IS the leak, and there is no
// reason to pull it.
func anonGet(account string) func(context.Context, string, string) (int, error) {
	return func(ctx context.Context, bucket, key string) (int, error) {
		guard := &backend.Guard{}
		u := fmt.Sprintf("https://%s.r2.cloudflarestorage.com/%s/%s",
			account, url.PathEscape(bucket), escapeKey(key))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return 0, err
		}
		if err := guard.CheckURL(req.URL); err != nil {
			return 0, err
		}
		resp, err := guard.Client().Do(req)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		return resp.StatusCode, nil
	}
}

// escapeKey escapes each segment of an object key, leaving the separators.
// A key is `<comp>/<channel>/<stamp>/<file>` and encoding the slashes would
// probe an object that does not exist — which answers 404 and would read as
// "private" for a bucket that is not.
func escapeKey(key string) string {
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// printReport renders the pass. The text form is one line per check with the
// status first, so a failure is greppable and the name is stable enough to be
// named in the runbook.
func printReport(e *env, r doctor.Report, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(e.stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	width := 0
	for _, c := range r.Checks {
		if len(c.Name) > width {
			width = len(c.Name)
		}
	}
	for _, c := range r.Checks {
		mark := "✓"
		switch c.Status {
		case doctor.StatusFail:
			mark = "✗"
		case doctor.StatusSkipped:
			mark = "–"
		}
		fmt.Fprintf(e.stdout, "%s %-*s  %s\n", mark, width, c.Name, c.Detail)
	}
	if r.Failed() {
		fmt.Fprintf(e.stdout, "\n%d of %d checks failed.\n", r.Failures(), len(r.Checks))
	}
	return nil
}
