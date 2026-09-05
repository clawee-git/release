package publish

// The release body: what a person reads on the GitHub release page, and the
// commands they run if they want to verify a download by hand.
//
// This is the surface feature 01's cut lost when publishing moved out of the
// kit — tools/test-checksum-verify.sh pinned the shipped verify block against
// a stub pre-2016 `shasum`, and nothing was writing that block any more. It is
// restored here, at the one place that now creates a GitHub release, and
// notes_test.go executes the emitted block against the same stub.
//
// The recipe is deliberately the SAME chain the installers run, in the same
// order — signature first, then the checksum taken from the now-trusted sums
// file. A "verify your download" section that checked the sha256 without
// checking who signed the sums file would be teaching people a ritual that
// proves nothing.

import (
	"fmt"
	"strings"

	"github.com/clawee-git/release/internal/manage/catalog"
	"github.com/clawee-git/release/internal/manage/intake"
	"github.com/clawee-git/release/internal/manage/store"
)

// ReleaseNotes renders the body of the GitHub release for row.
func ReleaseNotes(row *store.ReleaseVersion, zipNames []string) string {
	pubkey, err := intake.ReleasePubkeyLine()
	if err != nil {
		// A build whose embedded key does not parse is a broken build, but a
		// promote is not the place to die over the release NOTES: publish the
		// release, say the key is missing, and let the operator see it.
		pubkey = "<the release public key could not be read from this build>"
	}
	var b strings.Builder

	fmt.Fprintf(&b, "**%s %s** — `%s`\n\n", row.Component, row.Version, row.Stamp)
	if row.Channel == catalog.ChannelBeta {
		b.WriteString("This is a **beta** release. Beta hosts resolve the newest of " +
			"beta-or-stable, with the tie going to stable.\n\n")
	}

	b.WriteString("## Verify a download\n\n")
	b.WriteString("Every artifact is covered by `SHA256SUMS.txt`, which is signed with the " +
		"Clawee release key. Check the signature FIRST — the checksums are only worth " +
		"anything once you know who wrote them.\n\n")
	b.WriteString("```sh\n")
	b.WriteString("# 1. the signature over the sums file, against the release public key\n")
	fmt.Fprintf(&b, "minisign -V -P '%s' \\\n    -m SHA256SUMS.txt -x SHA256SUMS.txt.minisig\n\n", pubkey)
	b.WriteString("# 2. the checksum of the file you downloaded, from the now-trusted sums file.\n")
	b.WriteString("#    Pick the line by exact filename rather than `shasum -c --ignore-missing`:\n")
	b.WriteString("#    that flag is a 2016 addition and the stock shasum on an older macOS\n")
	b.WriteString("#    rejects it outright, which reads as a checksum mismatch on a good file.\n")
	fmt.Fprintf(&b, "ZIP='%s'\n", firstOr(zipNames, "clawee-<component>-<os>-<arch>.zip"))
	b.WriteString("want=\"$(awk -v f=\"$ZIP\" '{ n = $2; sub(/^\\*/, \"\", n); if (n == f) { print $1; exit } }' SHA256SUMS.txt)\"\n")
	b.WriteString("got=\"$(shasum -a 256 \"$ZIP\" 2>/dev/null | awk '{print $1}')\"\n")
	b.WriteString("[ -z \"$got\" ] && got=\"$(sha256sum \"$ZIP\" | awk '{print $1}')\"\n")
	b.WriteString("[ -n \"$want\" ] && [ \"$want\" = \"$got\" ] && echo OK || echo MISMATCH\n")
	b.WriteString("```\n\n")

	b.WriteString("## Artifacts\n\n")
	for _, z := range zipNames {
		fmt.Fprintf(&b, "- `%s`\n", z)
	}
	b.WriteString("- `SHA256SUMS.txt`, `SHA256SUMS.txt.minisig`\n\n")
	b.WriteString("Installers resolve the channel manifest, not this page: " +
		"the GitHub release list is the fallback for when the manifest host is unreachable.\n")
	return b.String()
}

func firstOr(items []string, fallback string) string {
	if len(items) > 0 {
		return items[0]
	}
	return fallback
}
