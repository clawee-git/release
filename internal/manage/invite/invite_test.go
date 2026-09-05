package invite

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clawee-git/release/internal/manage/backend/backendtest"
	"github.com/clawee-git/release/internal/manage/catalog"
	"github.com/clawee-git/release/internal/manage/store"
	"github.com/clawee-git/release/internal/register"
)

var base = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

const (
	comp  = catalog.ComponentCLI
	stamp = "v0.3.0.beta.2026.09.04.deadbeef"
)

type fixture struct {
	t       *testing.T
	st      *store.Store
	staging *backendtest.Staging
	rec     *backendtest.Recorder
	deps    Deps
	row     *store.ReleaseVersion
	// zips maps the object key to the bytes served for it.
	zips map[string][]byte
}

// zipFor builds a real zip carrying an inner install.sh, so the execution test
// exercises the real unzip and the real hand-off.
func zipFor(t *testing.T, marker string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	f, err := w.Create("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(f, "#!/bin/sh\necho '%s'\n", marker)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func newFixture(t *testing.T, channel string) *fixture {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	rec := &backendtest.Recorder{}
	staging := backendtest.NewStaging(rec, "clawee-staging")
	f := &fixture{t: t, st: st, staging: staging, rec: rec, zips: map[string][]byte{}}

	keyBase := register.KeyBase(comp, channel, stamp)
	var artifacts []register.Artifact
	var sums bytes.Buffer
	for _, plat := range []string{"darwin-arm64", "darwin-amd64", "linux-arm64", "linux-amd64"} {
		name := "clawee-" + comp + "-" + plat + ".zip"
		body := zipFor(t, "INNER INSTALLER RAN")
		key := keyBase + "/" + name
		staging.Objects[key] = body
		f.zips[key] = body
		sum := sha256.Sum256(body)
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(sum[:]), name)
		artifacts = append(artifacts, register.Artifact{
			Platform: strings.Replace(plat, "-", "/", 1),
			Key:      key, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(body)),
		})
	}
	sumsKey := keyBase + "/" + register.SumsName
	minisigKey := keyBase + "/" + register.MinisigName
	staging.Objects[sumsKey] = sums.Bytes()
	staging.Objects[minisigKey] = []byte("untrusted comment: a signature\nAAAA\n")

	artifactsJSON, _ := json.Marshal(artifacts)
	id, err := st.Stage(store.ReleaseVersion{
		Component: comp, Channel: channel, Version: "0.3.0", Stamp: stamp,
		ArtifactsJSON: string(artifactsJSON), SumsKey: sumsKey, MinisigKey: minisigKey,
		CreatedAt: base,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.row, err = st.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	f.deps = Deps{Store: st, Staging: staging, Now: func() time.Time { return base }}
	return f
}

func TestMintPresignsEverythingRecordsItAndReturnsACommand(t *testing.T) {
	f := newFixture(t, catalog.ChannelBeta)
	got, err := Mint(context.Background(), f.deps, f.row, "ada")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if got.ExpiresAt != base.Add(48*time.Hour) {
		t.Fatalf("expires_at = %s, want base+48h", got.ExpiresAt)
	}
	if !strings.HasPrefix(got.Command, "curl -fsSL '") || !strings.HasSuffix(got.Command, "' | sh") {
		t.Fatalf("command = %q", got.Command)
	}
	if !strings.Contains(got.Command, got.URL) {
		t.Fatal("the command does not carry the URL")
	}

	// Six artifacts presigned, plus the script itself: four zips, the sums
	// file, its signature. A link whose script outlived its artifacts would
	// download a verifier and then 403.
	presigns := 0
	for _, c := range f.rec.Calls() {
		if c.Op == "staging.presign" {
			presigns++
		}
	}
	if presigns != 7 {
		t.Fatalf("%d presigns, want 7 (4 zips + sums + minisig + the script)", presigns)
	}
	for _, u := range []string{got.URL} {
		if !strings.Contains(u, "ttl=172800") {
			t.Fatalf("presigned URL %q does not carry the 48h TTL", u)
		}
	}

	// The script landed in the PRIVATE bucket under a random key.
	if !f.rec.Has("staging.put") {
		t.Fatal("the script was never uploaded")
	}
	var scriptKey string
	for _, c := range f.rec.Calls() {
		if c.Op == "staging.put" {
			scriptKey = c.Key
		}
	}
	if !strings.HasPrefix(scriptKey, "invites/") || !strings.HasSuffix(scriptKey, "/install.sh") {
		t.Fatalf("script key = %q", scriptKey)
	}
	if len(strings.Split(scriptKey, "/")[1]) != 32 {
		t.Fatalf("script key %q does not carry a 32-hex random segment", scriptKey)
	}

	// The audit row is the whole control surface; it must exist.
	invites, err := f.st.ListInvites()
	if err != nil || len(invites) != 1 {
		t.Fatalf("ListInvites = %v, %v", invites, err)
	}
	if invites[0].MintedBy != "ada" || invites[0].RowID != f.row.ID {
		t.Fatalf("audit row = %+v", invites[0])
	}
	if !invites[0].Live(base.Add(47*time.Hour)) || invites[0].Live(base.Add(49*time.Hour)) {
		t.Fatal("the audit row's liveness window is not 48h")
	}

	// Two mints never share a key.
	second, err := Mint(context.Background(), f.deps, f.row, "ada")
	if err != nil {
		t.Fatal(err)
	}
	if second.URL == got.URL {
		t.Fatal("two mints produced the same script URL")
	}
}

func TestMintRefusesAYankedOrExpiredRow(t *testing.T) {
	for _, state := range []string{catalog.StateYanked, catalog.StateExpired} {
		f := newFixture(t, catalog.ChannelBeta)
		// Drive the row into the state through the real transitions where
		// possible, otherwise set it directly for the terminal one.
		f.row.State = state
		if _, err := Mint(context.Background(), f.deps, f.row, "ada"); !errors.Is(err, ErrNotMintable) {
			t.Fatalf("%s: err = %v, want ErrNotMintable", state, err)
		}
		if f.rec.Has("staging.presign") {
			t.Fatalf("%s: a refused mint presigned something anyway", state)
		}
	}
}

func TestMintRefusesARowWithNoArtifacts(t *testing.T) {
	f := newFixture(t, catalog.ChannelBeta)
	f.row.ArtifactsJSON = "[]"
	if _, err := Mint(context.Background(), f.deps, f.row, "ada"); err == nil {
		t.Fatal("a row with no artifacts was minted")
	}
}

// A quote in a URL would break out of the script's single quoting.
func TestRenderRefusesAQuoteInAnyEmbeddedValue(t *testing.T) {
	d := scriptData{
		Component: comp, Version: "0.3.0", ExpiresAt: "soon", Pubkey: "RWS...",
		SumsURL: "https://x/sums'; rm -rf /", MinisigURL: "https://x/sig",
		Platforms: []platformScript{{Platform: "darwin-arm64", File: "a.zip", URL: "https://x/a.zip"}},
	}
	if _, err := renderScript(d); err == nil {
		t.Fatal("a script was rendered around a value containing a single quote")
	}
}

// ── The execution test ───────────────────────────────────────────────────

// runScript executes a rendered install.sh with a stubbed minisign on PATH and
// returns its combined output and exit error.
func runScript(t *testing.T, script string, minisignExit int) (string, error) {
	t.Helper()
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The stub stands in for minisign itself: this test is about the CHAIN —
	// that a signature failure stops everything, and that a checksum mismatch
	// stops everything even when the signature is fine — not about
	// re-verifying minisign's own arithmetic.
	stub := fmt.Sprintf("#!/bin/sh\necho 'stub minisign' >&2\nexit %d\n", minisignExit)
	if err := os.WriteFile(filepath.Join(binDir, "minisign"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "install.sh")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", path)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		// The presigned URLs point at a TLS httptest server with a
		// self-signed certificate. Trusting it through curl's own CA
		// variable keeps the script's --proto '=https' intact — the
		// alternative, an escape hatch in the script, would be a hole in
		// the thing under test.
		"CURL_CA_BUNDLE="+writeCert(t, dir))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

var testCert []byte

func writeCert(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(p, testCert, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// serveArtifacts starts a TLS server handing out the staging objects by key,
// and points the fake's presigner at it.
func (f *fixture) serveArtifacts(t *testing.T, tamper string) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/")
		body, ok := f.staging.Objects[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if tamper != "" && strings.Contains(key, tamper) {
			// One byte is enough: the point is that ANY difference is caught.
			body = append(append([]byte(nil), body...), 'X')
		}
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	testCert = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	f.staging.PresignPrefix = srv.URL + "/"
}

func requireTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"sh", "curl", "unzip", "awk"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed; the invite script cannot be executed here", tool)
		}
	}
	if _, err := exec.LookPath("shasum"); err != nil {
		if _, err := exec.LookPath("sha256sum"); err != nil {
			t.Skip("neither shasum nor sha256sum is installed")
		}
	}
}

// TestRenderedScriptInstallsWhatTheReleaseKeySigned is the acceptance test for
// the invite: the REAL rendered script, executed, against a local server
// standing in for the staging bucket.
func TestRenderedScriptInstallsWhatTheReleaseKeySigned(t *testing.T) {
	requireTools(t)
	f := newFixture(t, catalog.ChannelBeta)
	f.serveArtifacts(t, "")
	got, err := Mint(context.Background(), f.deps, f.row, "ada")
	if err != nil {
		t.Fatal(err)
	}
	script := string(f.staging.Objects[scriptKeyOf(t, f)])
	if script == "" {
		t.Fatal("no script was uploaded")
	}
	_ = got

	out, err := runScript(t, script, 0)
	if err != nil {
		t.Fatalf("the script failed on good bytes: %v\n%s", err, out)
	}
	if !strings.Contains(out, "minisign signature valid") ||
		!strings.Contains(out, "sha256 matches the signed sums file") {
		t.Fatalf("the verification steps did not both report success:\n%s", out)
	}
	if !strings.Contains(out, "INNER INSTALLER RAN") {
		t.Fatalf("the verified inner installer never ran:\n%s", out)
	}
}

// The tamper case: the signature stub still says yes, but the zip's bytes no
// longer match the signed sums file. The script must refuse, and must not
// reach the inner installer.
func TestRenderedScriptRefusesATamperedZip(t *testing.T) {
	requireTools(t)
	f := newFixture(t, catalog.ChannelBeta)
	f.serveArtifacts(t, ".zip")
	if _, err := Mint(context.Background(), f.deps, f.row, "ada"); err != nil {
		t.Fatal(err)
	}
	script := string(f.staging.Objects[scriptKeyOf(t, f)])

	out, err := runScript(t, script, 0)
	if err == nil {
		t.Fatalf("the script accepted a tampered zip:\n%s", out)
	}
	if !strings.Contains(out, "checksum mismatch") {
		t.Fatalf("the refusal does not name the checksum:\n%s", out)
	}
	if strings.Contains(out, "INNER INSTALLER RAN") {
		t.Fatalf("the inner installer ran despite the mismatch:\n%s", out)
	}
}

// A signature failure stops everything BEFORE the checksum step: the sums file
// is only trusted because it is signed, so an unverified one must never be
// read for hashes.
func TestRenderedScriptRefusesABadSignature(t *testing.T) {
	requireTools(t)
	f := newFixture(t, catalog.ChannelBeta)
	f.serveArtifacts(t, "")
	if _, err := Mint(context.Background(), f.deps, f.row, "ada"); err != nil {
		t.Fatal(err)
	}
	script := string(f.staging.Objects[scriptKeyOf(t, f)])

	out, err := runScript(t, script, 1)
	if err == nil {
		t.Fatalf("the script ran with a failing signature check:\n%s", out)
	}
	if !strings.Contains(out, "signature verification failed") {
		t.Fatalf("the refusal does not name the signature:\n%s", out)
	}
	if strings.Contains(out, "sha256 matches") || strings.Contains(out, "INNER INSTALLER RAN") {
		t.Fatalf("the script continued past a failed signature:\n%s", out)
	}
}

func TestRenderedScriptRejectsArguments(t *testing.T) {
	requireTools(t)
	f := newFixture(t, catalog.ChannelBeta)
	f.serveArtifacts(t, "")
	if _, err := Mint(context.Background(), f.deps, f.row, "ada"); err != nil {
		t.Fatal(err)
	}
	script := string(f.staging.Objects[scriptKeyOf(t, f)])
	dir := t.TempDir()
	p := filepath.Join(dir, "install.sh")
	os.WriteFile(p, []byte(script), 0o700)
	out, err := exec.Command("sh", p, "--force").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "takes no arguments") {
		t.Fatalf("a stray argument was accepted: %v\n%s", err, out)
	}
}

// renderWithPubkey renders the invite script for the fixture's row against an
// arbitrary public key. Mint always bakes the RELEASE key, which no test can
// hold the private half of; this goes through the same pure renderer with the
// test-only key so the verification chain can be exercised for real.
func (f *fixture) renderWithPubkey(t *testing.T, pubkey string) string {
	t.Helper()
	var artifacts []register.Artifact
	if err := json.Unmarshal([]byte(f.row.ArtifactsJSON), &artifacts); err != nil {
		t.Fatal(err)
	}
	d := scriptData{
		Component: f.row.Component, Version: f.row.Version,
		ExpiresAt: base.Add(TTL).Format("2006-01-02 15:04 UTC"), Pubkey: pubkey,
	}
	for _, a := range artifacts {
		url, err := f.staging.Presign(a.Key, TTL)
		if err != nil {
			t.Fatal(err)
		}
		d.Platforms = append(d.Platforms, platformScript{
			Platform: strings.ReplaceAll(a.Platform, "/", "-"),
			File:     filepath.Base(a.Key),
			URL:      url,
		})
	}
	var err error
	if d.SumsURL, err = f.staging.Presign(f.row.SumsKey, TTL); err != nil {
		t.Fatal(err)
	}
	if d.MinisigURL, err = f.staging.Presign(f.row.MinisigKey, TTL); err != nil {
		t.Fatal(err)
	}
	script, err := renderScript(d)
	if err != nil {
		t.Fatal(err)
	}
	return script
}

func scriptKeyOf(t *testing.T, f *fixture) string {
	t.Helper()
	for _, c := range f.rec.Calls() {
		if c.Op == "staging.put" {
			return c.Key
		}
	}
	t.Fatal("no script was uploaded")
	return ""
}

// Both protocol flags must be rendered. This is the DISCRIMINATING test for
// the fix: which flag actually carries the guarantee depends on the host's
// curl (see the template's comment), so only the presence of both can be
// asserted here rather than in behaviour.
func TestRenderedScriptPinsRedirectsToHTTPS(t *testing.T) {
	f := newFixture(t, catalog.ChannelBeta)
	f.serveArtifacts(t, "")
	if _, err := Mint(context.Background(), f.deps, f.row, "ada"); err != nil {
		t.Fatal(err)
	}
	script := string(f.staging.Objects[scriptKeyOf(t, f)])
	if !strings.Contains(script, "--proto-redir '=https'") {
		t.Fatalf("the fetch helper does not pin redirects:\n%s", script)
	}
	if !strings.Contains(script, "--proto '=https'") {
		t.Fatal("the fetch helper does not pin the initial protocol")
	}
}

// …and end to end, curl refuses the hop. NOTE: on the curl this repo is
// developed against (8.7) `--proto '=https'` alone already refuses a redirect
// onto http, so this test passes with or without --proto-redir and is NOT a
// regression test for that flag — the assertion above is. It is here for the
// property itself: whatever the host's curl does with the flags, a rendered
// invite must not fetch anything over plaintext.
func TestRenderedScriptRefusesARedirectOntoPlaintext(t *testing.T) {
	requireTools(t)
	f := newFixture(t, catalog.ChannelBeta)

	// A plaintext server that would happily serve the sums file, and a TLS
	// server that redirects to it. Only --proto-redir can stop the second hop.
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("bytes from a plaintext hop"))
	}))
	t.Cleanup(plain.Close)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	testCert = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	f.staging.PresignPrefix = srv.URL + "/"

	if _, err := Mint(context.Background(), f.deps, f.row, "ada"); err != nil {
		t.Fatal(err)
	}
	script := string(f.staging.Objects[scriptKeyOf(t, f)])
	out, err := runScript(t, script, 0)
	if err == nil {
		t.Fatalf("the script followed a redirect onto plaintext:\n%s", out)
	}
	if !strings.Contains(out, "download failed") {
		t.Fatalf("the refusal does not read as a download failure:\n%s", out)
	}
	if strings.Contains(out, "INNER INSTALLER RAN") {
		t.Fatalf("the installer ran off plaintext bytes:\n%s", out)
	}
}
