package catalog

import "testing"

func TestStampMatchesChannel(t *testing.T) {
	const stable = "v0.2.28.2026.09.04.deadbeef"
	const beta = "v0.3.0.beta.2026.09.04.deadbeef"

	cases := []struct {
		stamp, channel string
		want           bool
	}{
		{stable, ChannelStable, true},
		{stable, ChannelBeta, false},
		{beta, ChannelBeta, true},
		{beta, ChannelStable, false},
		// An unknown channel matches nothing, so a router bug cannot turn into
		// an accepted row.
		{stable, "nightly", false},
		// Shapes that are close but wrong: a tag-prefixed stamp, a short sha,
		// an uppercase sha.
		{"clawee/" + stable, ChannelStable, false},
		{"v0.2.28.2026.09.04.dead", ChannelStable, false},
		{"v0.2.28.2026.09.04.DEADBEEF", ChannelStable, false},
		{"", ChannelStable, false},
	}
	for _, c := range cases {
		if got := StampMatchesChannel(c.stamp, c.channel); got != c.want {
			t.Errorf("StampMatchesChannel(%q, %q) = %v, want %v", c.stamp, c.channel, got, c.want)
		}
	}
}

func TestChannelOfStampDefaultsStable(t *testing.T) {
	if got := ChannelOfStamp(""); got != ChannelStable {
		t.Fatalf("ChannelOfStamp(\"\") = %q, want %q", got, ChannelStable)
	}
	if got := ChannelOfStamp("v0.3.0.beta.2026.09.04.deadbeef"); got != ChannelBeta {
		t.Fatalf("beta stamp read as %q", got)
	}
}

func TestVocabularyIsClosed(t *testing.T) {
	if ValidChannel("nightly") || ValidComponent("clawee-cli") || ValidState("draft") {
		t.Fatal("a name outside the closed vocabulary validated")
	}
	for _, c := range Channels {
		if !ValidChannel(c) {
			t.Fatalf("channel %q is in Channels but does not validate", c)
		}
	}
	for _, c := range Components {
		if !ValidComponent(c) {
			t.Fatalf("component %q is in Components but does not validate", c)
		}
	}
	// Channels and components must stay disjoint: URL shapes read a path
	// segment as one or the other, which is unambiguous only while they are.
	for _, ch := range Channels {
		if ValidComponent(ch) {
			t.Fatalf("%q names both a channel and a component", ch)
		}
	}
}
