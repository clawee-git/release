// Command rkit drives clawee release cuts on release-kit: `build` produces the
// signed (and, with --apple, notarized) artifact set into dist/<stamp>/;
// `harness` validates that build against the live release.sh --dry-run.
// Distribution stays in release.sh --distribute-only.
package main

import (
	"fmt"
	"os"
)

func usage() string {
	return "usage: rkit <build|harness> --component <clawee|claweed> [flags]"
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage())
		os.Exit(2)
	}
	switch os.Args[1] {
	case "build":
		if err := runBuild(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "✗", err)
			os.Exit(1)
		}
	case "harness":
		if err := runHarness(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "✗", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, usage())
		os.Exit(2)
	}
}
