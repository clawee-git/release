package relconfig

import (
	"context"

	"github.com/burrowee-git/release-kit/version"
)

// Stamp reproduces `tools/version.sh <comp> --stamp` for srcDir, wrapping
// release-kit's version.Stamp with the library's DateVersionScheme
// (v<semver>.<dateUTC>.<sha>, dateUTC = YYYY.MM.DD) — byte-identical to
// version.sh's stamp(). No clawee-specific scheme func is needed.
func Stamp(ctx context.Context, semverFile, srcDir string) (string, error) {
	return version.Stamp(ctx, semverFile, srcDir, version.DateVersionScheme)
}
