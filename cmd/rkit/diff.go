package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Report is the outcome of comparing two directories of assembled release
// zips — the live release.sh oracle vs orchestrate's candidate — for
// PAYLOAD-CONTENT equivalence. Zip envelope bytes (entry order, timestamps,
// compression) and the checksum manifest are deliberately ignored.
type Report struct {
	Targets []TargetResult
}

// TargetResult is the comparison outcome for one
// burrowee-<comp>-<os>-<arch>.zip, matched by basename across the two dirs.
type TargetResult struct {
	Target         string
	BinMismatch    []string
	InstallShEqual bool
	FileSetEqual   bool
	OK             bool
}

// ignoredEntry excludes envelope artifacts from the payload comparison — the
// checksum manifest and its signature cover the zip bytes themselves, not
// the component payload, so they never factor into equivalence.
func ignoredEntry(name string) bool {
	return name == "SHA256SUMS.txt" || strings.HasSuffix(name, ".minisig")
}

// comparePayloads matches zips in refZipDir (the release.sh oracle) and
// candZipDir (orchestrate's output) by basename and diffs each pair's
// payload: per-file sha256 for everything but install.sh, install.sh by byte
// equality, and the file set (sorted basenames, minus SHA256SUMS.txt/
// *.minisig).
func comparePayloads(refZipDir, candZipDir string) (Report, error) {
	refZips, err := zipsIn(refZipDir)
	if err != nil {
		return Report{}, fmt.Errorf("ref zip dir %s: %w", refZipDir, err)
	}
	candZips, err := zipsIn(candZipDir)
	if err != nil {
		return Report{}, fmt.Errorf("cand zip dir %s: %w", candZipDir, err)
	}

	seen := map[string]bool{}
	var names []string
	for name := range refZips {
		names = append(names, name)
		seen[name] = true
	}
	for name := range candZips {
		if !seen[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	report := Report{}
	for _, name := range names {
		refPath, refOK := refZips[name]
		candPath, candOK := candZips[name]
		if !refOK || !candOK {
			// present on only one side: total mismatch, nothing to diff.
			report.Targets = append(report.Targets, TargetResult{Target: name})
			continue
		}
		tr, err := compareZipPair(name, refPath, candPath)
		if err != nil {
			return Report{}, err
		}
		report.Targets = append(report.Targets, tr)
	}
	return report, nil
}

// compareZipPair diffs one matched ref/cand zip pair's payload.
func compareZipPair(target, refPath, candPath string) (TargetResult, error) {
	refFiles, err := readZipEntries(refPath)
	if err != nil {
		return TargetResult{}, fmt.Errorf("%s: read ref zip: %w", target, err)
	}
	candFiles, err := readZipEntries(candPath)
	if err != nil {
		return TargetResult{}, fmt.Errorf("%s: read cand zip: %w", target, err)
	}

	result := TargetResult{Target: target}
	result.FileSetEqual = equalNames(payloadNames(refFiles), payloadNames(candFiles))

	if refInstall, ok := refFiles["install.sh"]; ok {
		if candInstall, ok2 := candFiles["install.sh"]; ok2 {
			result.InstallShEqual = string(refInstall) == string(candInstall)
		}
	}

	for name, refData := range refFiles {
		if name == "install.sh" || ignoredEntry(name) {
			continue
		}
		candData, ok := candFiles[name]
		if !ok {
			continue // already reflected in FileSetEqual
		}
		// darwin-normalization extension point: darwin binaries are ad-hoc
		// signed by orchestrate (sign.AdHocSigner) and by release.sh's build
		// step independently, which can perturb the Mach-O signature load
		// command without changing payload semantics. This compares raw
		// sha256 for every OS today (linux-strict, per the plan); if darwin
		// shows spurious mismatches, Task 7 re-derives the sha256 here after
		// stripping the code-signature load command (or compares pre-sign
		// copies) before falling back to a mismatch.
		if sha256Hex(refData) != sha256Hex(candData) {
			result.BinMismatch = append(result.BinMismatch, name)
		}
	}
	sort.Strings(result.BinMismatch)

	result.OK = len(result.BinMismatch) == 0 && result.InstallShEqual && result.FileSetEqual
	return result, nil
}

// zipsIn returns basename -> full path for every *.zip file directly under dir.
func zipsIn(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".zip") {
			continue
		}
		out[e.Name()] = filepath.Join(dir, e.Name())
	}
	return out, nil
}

// readZipEntries reads every non-directory entry of a zip into memory,
// keyed by its in-zip name.
func readZipEntries(path string) (map[string][]byte, error) {
	r, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	out := map[string][]byte{}
	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, err
		}
		out[f.Name] = data
	}
	return out, nil
}

// payloadNames returns the sorted basenames of files, excluding envelope
// artifacts (SHA256SUMS.txt / *.minisig).
func payloadNames(files map[string][]byte) []string {
	var names []string
	for name := range files {
		if ignoredEntry(name) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func equalNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
