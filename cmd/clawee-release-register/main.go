// Command clawee-release-register records a staged cut with the manage
// service: it measures the artifacts in dist/<stamp>/, builds the catalog row,
// fetches a single-use nonce, signs the row (nonce included) with the release
// signing key and POSTs it.
//
// It is the step that makes a staged upload PROMOTABLE. Bytes in the private
// staging bucket with no row are a stranded artifact — nobody can find them,
// nobody can promote them — which is why the cut refuses BEFORE uploading when
// the manage URL is unset, and fails AFTER uploading when the service refuses:
// in the first case nothing has happened yet, in the second the bytes are
// inert and the operator needs to see why no row exists.
//
// Usage:
//
//	clawee-release-register --manage-url <url> --comp <clawee|claweed> \
//	    --channel <stable|beta> --version <X.Y.Z> --stamp <v…stamp> \
//	    --stage-dir dist/<stamp> --key <minisign secret key> [--dry-run]
//
// --dry-run prints the payload that WOULD be signed and posted and makes no
// network call. The signing key is never printed, in any mode.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/clawee-git/release/internal/register"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "✗ clawee-release-register: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	manageURL := flag.String("manage-url", "", "base URL of the manage service")
	comp := flag.String("comp", "", "component (clawee | claweed)")
	channel := flag.String("channel", "", "channel (stable | beta)")
	version := flag.String("version", "", "human semver, e.g. 0.2.28")
	stamp := flag.String("stamp", "", "full release stamp")
	stageDir := flag.String("stage-dir", "", "the dist/<stamp> directory that was uploaded")
	keyPath := flag.String("key", "", "minisign secret key file (the release signing key)")
	dryRun := flag.Bool("dry-run", false, "print the payload and make no network call")
	flag.Parse()

	for _, f := range []struct{ name, val string }{
		{"comp", *comp}, {"channel", *channel}, {"version", *version},
		{"stamp", *stamp}, {"stage-dir", *stageDir}, {"key", *keyPath},
	} {
		if f.val == "" {
			return fmt.Errorf("missing required flag --%s", f.name)
		}
	}
	switch *comp {
	case "clawee", "claweed":
	default:
		return fmt.Errorf("unknown component %q (want clawee | claweed)", *comp)
	}
	switch *channel {
	case "stable", "beta":
	default:
		return fmt.Errorf("unknown channel %q (want stable | beta)", *channel)
	}

	payload, err := register.BuildPayload(*stageDir, *comp, *channel, *version, *stamp)
	if err != nil {
		return err
	}

	if *dryRun {
		// The dry-run payload deliberately carries no nonce and no signature:
		// a nonce is single-use and issued by the service, so printing a
		// fabricated one would show a shape that never goes on the wire.
		body, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			return err
		}
		fmt.Printf("dry-run: would POST to %s/api/v1/releases/register\n", *manageURL)
		fmt.Printf("dry-run: payload (nonce + signature filled in from the live handshake):\n%s\n", body)
		return nil
	}

	if *manageURL == "" {
		return fmt.Errorf("missing required flag --manage-url")
	}
	key, err := register.LoadSigningKey(*keyPath)
	if err != nil {
		return err
	}
	client := register.NewClient(*manageURL)
	_, rowURL, err := client.Register(context.Background(), payload, key)
	if err != nil {
		return err
	}
	fmt.Printf("✓ registered %s %s (%s) as staged\n", *comp, *stamp, *channel)
	fmt.Printf("  row: %s\n", rowURL)
	return nil
}
