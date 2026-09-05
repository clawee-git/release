package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// TestGeneratedReferenceIsCommitted is the build gate. It lives in the normal
// suite, so any pipeline that runs the tests runs the gate — there is no extra
// CI step to forget, and a stale reference fails the build rather than the
// review.
func TestGeneratedReferenceIsCommitted(t *testing.T) {
	var want bytes.Buffer
	writeReference(&want)

	got, err := os.ReadFile("../../" + referenceFile)
	if err != nil {
		t.Fatalf("read %s: %v\nregenerate with: go run ./cmd/%s docs > %s", referenceFile, err, toolName, referenceFile)
	}
	if !bytes.Equal(got, want.Bytes()) {
		t.Fatalf("%s is stale.\nregenerate with:\n  go run ./cmd/%s docs > %s", referenceFile, toolName, referenceFile)
	}
}

// The reference is produced by the SAME renderer as --help, so every page in
// it is byte-identical to what the binary prints.
func TestReferenceEmbedsTheExactHelpPages(t *testing.T) {
	var doc bytes.Buffer
	writeReference(&doc)
	out, _, _ := exec(t, "admin", "add", "--help")
	if !strings.Contains(doc.String(), out) {
		t.Fatalf("the reference does not carry `admin add --help` verbatim")
	}
	// Every command has a heading.
	walkNodes(func(n *node) {
		if !strings.Contains(doc.String(), "`"+toolName+" "+pathOf(n)+"`") {
			t.Errorf("the reference has no section for %q", pathOf(n))
		}
	})
}

func TestDocsVerbRejectsArguments(t *testing.T) {
	_, errb, code := exec(t, "docs", "stray")
	if code != exitUsage || !strings.Contains(errb, `unexpected argument "stray"`) {
		t.Fatalf("docs with a stray argument: code %d, stderr %q", code, errb)
	}
}
