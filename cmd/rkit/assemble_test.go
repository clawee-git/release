package main

import (
	"archive/zip"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/burrowee-git/release-kit/build"
)

func TestAssembleFlatZip(t *testing.T) {
	root := t.TempDir()
	// fake per-target artifacts (2 bins x 1 target for brevity).
	mk := func(p, data string) string {
		full := filepath.Join(root, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(data), 0o755)
		return full
	}
	arts := []build.Artifact{
		{OS: "linux", Arch: "arm64", Path: mk("bin/clawee", "A")},
		{OS: "linux", Arch: "arm64", Path: mk("bin/clawee-updater", "B")},
	}
	installSh := mk("install.sh", "#!/bin/sh\n")

	zips, err := assemble("clawee", "v0.1.90.x", root, installSh, arts)
	if err != nil {
		t.Fatal(err)
	}
	if len(zips) != 1 {
		t.Fatalf("want 1 zip, got %d", len(zips))
	}
	if base := filepath.Base(zips[0]); base != "clawee-clawee-linux-arm64.zip" {
		t.Fatalf("zip name = %s", base)
	}
	// zip contains clawee, clawee-updater, install.sh (flat).
	r, err := zip.OpenReader(zips[0])
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var names []string
	for _, f := range r.File {
		names = append(names, f.Name)
	}
	sort.Strings(names)
	want := []string{"clawee", "clawee-updater", "install.sh"}
	if len(names) != len(want) {
		t.Fatalf("entries = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("entries = %v, want %v", names, want)
		}
	}
}
