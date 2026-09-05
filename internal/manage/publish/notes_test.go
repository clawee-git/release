package publish

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clawee-git/release/internal/manage/catalog"
	"github.com/clawee-git/release/internal/manage/store"
)

func notesFor(channel string) string {
	row := &store.ReleaseVersion{
		Component: catalog.ComponentCLI, Channel: channel,
		Version: "0.2.28", Stamp: stampFor(channel, 28),
	}
	return ReleaseNotes(row, []string{"clawee-clawee-darwin-arm64.zip", "clawee-clawee-linux-amd64.zip"})
}

func TestReleaseNotesCarryTheChainInOrder(t *testing.T) {
	notes := notesFor(catalog.ChannelStable)
	sig := strings.Index(notes, "minisign -V -P")
	sum := strings.Index(notes, "shasum -a 256")
	if sig < 0 || sum < 0 {
		t.Fatalf("the recipe is missing a step:\n%s", notes)
	}
	if sig > sum {
		// A "verify your download" section that checks the sha256 without
		// first checking who signed the sums file teaches a ritual that
		// proves nothing.
		t.Fatal("the recipe checks the checksum before the signature")
	}
	if !strings.Contains(notes, "clawee-clawee-darwin-arm64.zip") {
		t.Fatal("the notes do not list the artifacts")
	}
	if strings.Contains(notes, "could not be read from this build") {
		t.Fatal("the embedded release public key did not render")
	}
	if !strings.Contains(notes, "channel manifest") {
		t.Fatal("the notes do not say the GitHub list is the fallback, not the source")
	}
}

func TestBetaNotesSayTheyAreBeta(t *testing.T) {
	if !strings.Contains(notesFor(catalog.ChannelBeta), "**beta**") {
		t.Fatal("a beta release's notes do not say so")
	}
	if strings.Contains(notesFor(catalog.ChannelStable), "**beta**") {
		t.Fatal("a stable release's notes claim beta")
	}
}

// shBlock extracts the fenced sh block from the notes.
func shBlock(t *testing.T, notes string) string {
	t.Helper()
	start := strings.Index(notes, "```sh\n")
	if start < 0 {
		t.Fatal("the notes carry no sh block")
	}
	rest := notes[start+len("```sh\n"):]
	end := strings.Index(rest, "```")
	if end < 0 {
		t.Fatal("the sh block is unterminated")
	}
	return rest[:end]
}

// TestVerifyRecipeRunsAgainstAPre2016Shasum is the surface tools/test-checksum-verify.sh
// pinned before publishing moved out of the kit: the emitted recipe is
// EXECUTED against a stub shasum that rejects the flags an older macOS does
// not have. `shasum -c --ignore-missing` is a 2016 addition, and its refusal
// came back through the pipeline as "checksum mismatch" — every install on
// such a host accused a perfectly good zip of tampering.
func TestVerifyRecipeRunsAgainstAPre2016Shasum(t *testing.T) {
	for _, tool := range []string{"sh", "awk"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	os.MkdirAll(binDir, 0o755)

	// A stub shasum with a 2015-era flag set: it computes -a 256 and REFUSES
	// anything else, so a recipe reaching for --ignore-missing or -c fails
	// here exactly as it failed in the field.
	stub := `#!/bin/sh
for a in "$@"; do
  case "$a" in
    -a|256|-) ;;
    --ignore-missing|-c|--check) echo "Unknown option: ${a#--}" >&2; exit 1 ;;
    -*) echo "Unknown option: ${a#-}" >&2; exit 1 ;;
  esac
done
f="$3"
printf '%s  %s\n' "$(cat "$f.sha256")" "$(basename "$f")"
`
	if err := os.WriteFile(filepath.Join(binDir, "shasum"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	// sha256sum must NOT exist, so the recipe's shasum branch is the one taken.
	if err := os.WriteFile(filepath.Join(binDir, "minisign"),
		[]byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	zipName := "clawee-clawee-darwin-arm64.zip"
	body := []byte("the release zip bytes")
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	os.WriteFile(filepath.Join(dir, zipName), body, 0o644)
	os.WriteFile(filepath.Join(dir, zipName+".sha256"), []byte(digest), 0o644)
	os.WriteFile(filepath.Join(dir, "SHA256SUMS.txt"),
		[]byte(fmt.Sprintf("%s  %s\n%s  other.zip\n", digest, zipName, strings.Repeat("b", 64))), 0o644)
	os.WriteFile(filepath.Join(dir, "SHA256SUMS.txt.minisig"), []byte("sig"), 0o644)

	run := func() (string, error) {
		script := filepath.Join(dir, "verify.sh")
		os.WriteFile(script, []byte(shBlock(t, notesFor(catalog.ChannelStable))), 0o700)
		cmd := exec.Command("sh", script)
		cmd.Dir = dir
		// PATH is ONLY the stub dir plus the shell's own tools: a real shasum
		// on the machine would hide the very failure this test exists for.
		cmd.Env = append(os.Environ(), "PATH="+binDir+":/usr/bin:/bin")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	out, err := run()
	if err != nil {
		t.Fatalf("the recipe failed against a pre-2016 shasum: %v\n%s", err, out)
	}
	if !strings.Contains(out, "OK") || strings.Contains(out, "MISMATCH") {
		t.Fatalf("the recipe did not verify a good file:\n%s", out)
	}

	// And it still catches a bad one.
	os.WriteFile(filepath.Join(dir, zipName+".sha256"), []byte(strings.Repeat("c", 64)), 0o644)
	out, _ = run()
	if !strings.Contains(out, "MISMATCH") {
		t.Fatalf("the recipe accepted a mismatched checksum:\n%s", out)
	}
}
