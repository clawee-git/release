// Package gofmtgate holds one test: the whole tree is gofmt-clean.
//
// It lives in the normal suite rather than in a CI step, for the same reason
// the CLI reference's byte-diff gate does — any pipeline that runs the tests
// runs it, and there is no separate job to forget. Three files had drifted
// before this existed, which is exactly how formatting rot starts: nothing
// fails, so nobody notices until a diff is half whitespace.
package gofmtgate

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestTreeIsGofmtClean(t *testing.T) {
	// gofmt ships with the toolchain, but a PATH that lacks it must not make
	// the gate silently pass: skipping is announced, and the reason names the
	// binary rather than pretending the tree is clean.
	gofmt, err := exec.LookPath("gofmt")
	if err != nil {
		gofmt = filepath.Join(runtime.GOROOT(), "bin", "gofmt")
		if _, statErr := exec.LookPath(gofmt); statErr != nil {
			t.Skipf("gofmt not found on PATH or in GOROOT (%v); the formatting gate did not run", err)
		}
	}

	// The repo root, from this file's location — not the working directory,
	// which is this package's own dir when `go test` runs it.
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(gofmt, "-l", root).CombinedOutput()
	if err != nil {
		t.Fatalf("gofmt -l %s: %v\n%s", root, err, out)
	}
	listed := strings.TrimSpace(string(out))
	if listed == "" {
		return
	}
	var files []string
	for _, line := range strings.Split(listed, "\n") {
		if rel, relErr := filepath.Rel(root, line); relErr == nil {
			line = rel
		}
		files = append(files, "  "+line)
	}
	t.Fatalf("these files are not gofmt-clean:\n%s\n\nrun:\n  gofmt -w .", strings.Join(files, "\n"))
}
