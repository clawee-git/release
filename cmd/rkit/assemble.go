package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/burrowee-git/release-kit/build"
	"github.com/burrowee-git/release-kit/pack"
)

// assemble builds one flat zip per target: component bins + install.sh. Zips
// land at outRoot/stamp/clawee-<comp>-<os>-<arch>.zip in sorted-target order.
// Clawee has no dispatcher and no extra payload — updaters/spawn are regular
// bins already in compArts.
func assemble(comp, stamp, outRoot, installSh string, compArts []build.Artifact) ([]string, error) {
	byTarget := map[string][]pack.Content{}
	for _, a := range compArts {
		k := a.OS + "-" + a.Arch
		byTarget[k] = append(byTarget[k], pack.Content{Src: a.Path})
	}

	targets := make([]string, 0, len(byTarget))
	for k := range byTarget {
		targets = append(targets, k)
	}
	sort.Strings(targets)

	zipDir := filepath.Join(outRoot, stamp)
	if err := os.MkdirAll(zipDir, 0o755); err != nil {
		return nil, fmt.Errorf("assemble %s: %w", comp, err)
	}

	var zips []string
	for _, k := range targets {
		contents := append(byTarget[k], pack.Content{Src: installSh, Name: "install.sh"})
		zp := filepath.Join(zipDir, fmt.Sprintf("clawee-%s-%s.zip", comp, k))
		if err := pack.Zip(pack.Spec{Out: zp, Contents: contents}); err != nil {
			return nil, fmt.Errorf("assemble %s: zip %s: %w", comp, k, err)
		}
		zips = append(zips, zp)
	}
	return zips, nil
}
