// Package catalog is the closed vocabulary of the release catalog: which
// components exist, which channels exist, which states a row can be in, and
// what a stamp for a given channel looks like.
//
// It is deliberately its own package with no dependencies. The store validates
// against it, the register endpoint validates against it, and the router
// DERIVES its per-channel paths from Channels — a channel a validator accepts
// but no route serves is a 405 wearing a 200's clothes, so the two are not
// allowed to be written down separately (release-management.md §2).
package catalog

import (
	"regexp"
	"slices"
	"strings"
)

// The channels. Stable first: it is the default everywhere a channel is
// implied, and the tab order on the manage page reads from this slice.
const (
	ChannelStable = "stable"
	ChannelBeta   = "beta"
)

// Channels is THE list. ValidChannel answers from it and the router builds
// "/api/v1/manage/releases/<ch>/versions" from it.
var Channels = []string{ChannelStable, ChannelBeta}

// The components this kit cuts. Both are shipped from clawee-git/cli and
// clawee-git/daemon; the kit's own zip naming (clawee-<comp>-<os>-<arch>.zip)
// is where these spellings come from.
const (
	ComponentCLI    = "clawee"
	ComponentDaemon = "claweed"
)

// Components is THE list, in the order the manage page renders its cards.
var Components = []string{ComponentCLI, ComponentDaemon}

// The row states (release-management.md §2). The transitions are
// staged→public→yanked, and staged|public→expired by retention.
const (
	StateStaged  = "staged"
	StatePublic  = "public"
	StateYanked  = "yanked"
	StateExpired = "expired"
)

// States is THE list, in lifecycle order.
var States = []string{StateStaged, StatePublic, StateYanked, StateExpired}

// ValidChannel reports whether s names a channel this service serves.
func ValidChannel(s string) bool { return slices.Contains(Channels, s) }

// ValidComponent reports whether s names a component this service catalogs.
func ValidComponent(s string) bool { return slices.Contains(Components, s) }

// ValidState reports whether s names a row state.
func ValidState(s string) bool { return slices.Contains(States, s) }

// Stamp shapes are exclusive by construction: a stable stamp has no ".beta."
// segment and a beta stamp has exactly one, between the semver and the date.
// The kit's tools/version.sh is what produces both.
var (
	stableStampRe = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+\.[0-9]{4}\.[0-9]{2}\.[0-9]{2}\.[0-9a-f]{8}$`)
	betaStampRe   = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+\.beta\.[0-9]{4}\.[0-9]{2}\.[0-9]{2}\.[0-9a-f]{8}$`)
)

// StampMatchesChannel is the boundary check the register endpoint runs: the
// cut says which channel it cut on, and the stamp must agree.
//
// It matters because retention and every installer read the CHANNEL, not the
// stamp — so a beta build registered as stable is a beta build that retention
// keeps ten of and that installers hand to every stable host. The stamp is the
// one part of the row the kit cannot spell freely, so it is the part that can
// contradict the claim.
func StampMatchesChannel(stamp, channel string) bool {
	switch channel {
	case ChannelStable:
		return stableStampRe.MatchString(stamp)
	case ChannelBeta:
		return betaStampRe.MatchString(stamp)
	}
	return false
}

// ChannelOfStamp derives the channel a stamp was cut on. Malformed input is
// stable, matching the field rule that a node reporting nothing is never moved
// onto beta; callers that need to REJECT malformed input use StampMatchesChannel.
func ChannelOfStamp(stamp string) string {
	if strings.Contains(stamp, ".beta.") {
		return ChannelBeta
	}
	return ChannelStable
}
