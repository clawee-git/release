package main

import (
	"archive/zip"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

// writeZip fabricates a zip at path from name->content, writing each entry
// with the given modTime — used to force two zips holding identical payload
// bytes to still differ at the envelope level (entry order, timestamps).
func writeZip(t *testing.T, path string, files map[string][]byte, modTime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := zip.NewWriter(f)

	var names []string
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		hdr := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: modTime}
		hdr.SetMode(0o755)
		fw, err := w.CreateHeader(hdr)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(files[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

// samplePayload returns a baseline set of zip entries mirroring an assembled
// clawee-<comp>-<os>-<arch>.zip: two component bins, the dispatcher,
// install.sh, and (as ref-only envelope noise) a SHA256SUMS.txt/.minisig pair
// that must never affect the comparison.
func samplePayload() map[string][]byte {
	return map[string][]byte{
		"clawee-cli":         []byte("cli-binary-bytes"),
		"clawee-cli-updater": []byte("updater-binary-bytes"),
		"clawee":             []byte("dispatcher-binary-bytes"),
		"install.sh":         []byte("#!/bin/sh\necho install\n"),
	}
}

func TestComparePayloadsRezippedEnvelopeOK(t *testing.T) {
	refDir, candDir := t.TempDir(), t.TempDir()
	payload := samplePayload()

	// Same payload bytes, but re-zipped with a different envelope: different
	// entry order and a different Modified timestamp.
	writeZip(t, filepath.Join(refDir, "clawee-cli-linux-amd64.zip"), payload, time.Unix(1000, 0))
	writeZip(t, filepath.Join(candDir, "clawee-cli-linux-amd64.zip"), payload, time.Unix(2000, 0))
	// ref-side envelope noise sitting next to the zip (SHA256SUMS.txt +
	// .minisig, as dist/<stamp>/ actually has) — not a *.zip, so zipsIn must
	// never pick it up as a target of its own.
	if err := os.WriteFile(filepath.Join(refDir, "SHA256SUMS.txt"), []byte("noise"), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := comparePayloads(refDir, candDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Targets) != 1 {
		t.Fatalf("got %d targets, want 1: %+v", len(report.Targets), report.Targets)
	}
	tr := report.Targets[0]
	if tr.Target != "clawee-cli-linux-amd64.zip" {
		t.Errorf("Target = %q", tr.Target)
	}
	if len(tr.BinMismatch) != 0 {
		t.Errorf("BinMismatch = %v, want none", tr.BinMismatch)
	}
	if !tr.InstallShEqual {
		t.Error("InstallShEqual = false, want true")
	}
	if !tr.FileSetEqual {
		t.Error("FileSetEqual = false, want true")
	}
	if !tr.OK {
		t.Error("OK = false, want true (envelope-only differences must be ignored)")
	}
}

func TestComparePayloadsBinaryByteMismatch(t *testing.T) {
	refDir, candDir := t.TempDir(), t.TempDir()
	ref := samplePayload()
	cand := samplePayload()
	cand["clawee-cli"] = []byte("DIFFERENT-cli-binary-bytes")

	writeZip(t, filepath.Join(refDir, "clawee-cli-linux-amd64.zip"), ref, time.Unix(1000, 0))
	writeZip(t, filepath.Join(candDir, "clawee-cli-linux-amd64.zip"), cand, time.Unix(1000, 0))

	report, err := comparePayloads(refDir, candDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Targets) != 1 {
		t.Fatalf("got %d targets, want 1", len(report.Targets))
	}
	tr := report.Targets[0]
	if !reflect.DeepEqual(tr.BinMismatch, []string{"clawee-cli"}) {
		t.Errorf("BinMismatch = %v, want [clawee-cli]", tr.BinMismatch)
	}
	if !tr.InstallShEqual {
		t.Error("InstallShEqual = false, want true (install.sh untouched)")
	}
	if !tr.FileSetEqual {
		t.Error("FileSetEqual = false, want true (same file set)")
	}
	if tr.OK {
		t.Error("OK = true, want false (binary mismatch)")
	}
}

func TestComparePayloadsInstallShMismatch(t *testing.T) {
	refDir, candDir := t.TempDir(), t.TempDir()
	ref := samplePayload()
	cand := samplePayload()
	cand["install.sh"] = []byte("#!/bin/sh\necho DIFFERENT\n")

	writeZip(t, filepath.Join(refDir, "clawee-cli-linux-amd64.zip"), ref, time.Unix(1000, 0))
	writeZip(t, filepath.Join(candDir, "clawee-cli-linux-amd64.zip"), cand, time.Unix(1000, 0))

	report, err := comparePayloads(refDir, candDir)
	if err != nil {
		t.Fatal(err)
	}
	tr := report.Targets[0]
	if tr.InstallShEqual {
		t.Error("InstallShEqual = true, want false")
	}
	if len(tr.BinMismatch) != 0 {
		t.Errorf("BinMismatch = %v, want none", tr.BinMismatch)
	}
	if !tr.FileSetEqual {
		t.Error("FileSetEqual = false, want true (same file set)")
	}
	if tr.OK {
		t.Error("OK = true, want false (install.sh mismatch)")
	}
}

func TestComparePayloadsFileSetMismatch(t *testing.T) {
	refDir, candDir := t.TempDir(), t.TempDir()
	ref := samplePayload()
	cand := samplePayload()
	cand["clawee-cli-extra-helper"] = []byte("unexpected-extra-file")

	writeZip(t, filepath.Join(refDir, "clawee-cli-linux-amd64.zip"), ref, time.Unix(1000, 0))
	writeZip(t, filepath.Join(candDir, "clawee-cli-linux-amd64.zip"), cand, time.Unix(1000, 0))

	report, err := comparePayloads(refDir, candDir)
	if err != nil {
		t.Fatal(err)
	}
	tr := report.Targets[0]
	if tr.FileSetEqual {
		t.Error("FileSetEqual = true, want false")
	}
	if tr.OK {
		t.Error("OK = true, want false (file set mismatch)")
	}
}

func TestComparePayloadsOneSidedTargetFailsClosed(t *testing.T) {
	refDir, candDir := t.TempDir(), t.TempDir()
	payload := samplePayload()

	// Present on the ref side only — no candidate zip for this target at all.
	writeZip(t, filepath.Join(refDir, "clawee-cli-linux-amd64.zip"), payload, time.Unix(1000, 0))

	report, err := comparePayloads(refDir, candDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Targets) != 1 {
		t.Fatalf("got %d targets, want 1: %+v", len(report.Targets), report.Targets)
	}
	tr := report.Targets[0]
	if tr.Target != "clawee-cli-linux-amd64.zip" {
		t.Errorf("Target = %q", tr.Target)
	}
	if tr.OK {
		t.Error("OK = true, want false (target present on only one side must fail closed)")
	}
}

func TestComparePayloadsOneSidedTargetCandOnlyFailsClosed(t *testing.T) {
	refDir, candDir := t.TempDir(), t.TempDir()
	payload := samplePayload()

	// Present on the candidate side only — no ref (oracle) zip for this target.
	writeZip(t, filepath.Join(candDir, "clawee-cli-linux-amd64.zip"), payload, time.Unix(1000, 0))

	report, err := comparePayloads(refDir, candDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Targets) != 1 {
		t.Fatalf("got %d targets, want 1: %+v", len(report.Targets), report.Targets)
	}
	tr := report.Targets[0]
	if tr.OK {
		t.Error("OK = true, want false (target present on only one side must fail closed)")
	}
}

func TestComparePayloadsMultipleTargetsSortedByName(t *testing.T) {
	refDir, candDir := t.TempDir(), t.TempDir()
	payload := samplePayload()

	for _, name := range []string{"clawee-cli-linux-amd64.zip", "clawee-cli-darwin-arm64.zip"} {
		writeZip(t, filepath.Join(refDir, name), payload, time.Unix(1000, 0))
		writeZip(t, filepath.Join(candDir, name), payload, time.Unix(1000, 0))
	}

	report, err := comparePayloads(refDir, candDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Targets) != 2 {
		t.Fatalf("got %d targets, want 2", len(report.Targets))
	}
	want := []string{"clawee-cli-darwin-arm64.zip", "clawee-cli-linux-amd64.zip"}
	var got []string
	for _, tr := range report.Targets {
		got = append(got, tr.Target)
		if !tr.OK {
			t.Errorf("target %s: OK = false, want true", tr.Target)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("target order = %v, want %v", got, want)
	}
}
