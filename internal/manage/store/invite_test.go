package store

import (
	"testing"
	"time"

	"github.com/clawee-git/release/internal/manage/catalog"
)

func TestInviteRecordAndLiveness(t *testing.T) {
	s := open(t)
	row := stage(t, s, catalog.ComponentCLI, catalog.ChannelBeta, 1, base)
	id, err := s.CreateInvite(Invite{
		RowID: row, MintedBy: "ada", ScriptKey: "invites/abc/install.sh",
		URL: "https://example.invalid/presigned", CreatedAt: base, ExpiresAt: base.Add(48 * time.Hour),
	})
	if err != nil || id == 0 {
		t.Fatalf("CreateInvite = %d, %v", id, err)
	}
	list, err := s.ListInvites()
	if err != nil || len(list) != 1 {
		t.Fatalf("ListInvites = %v, %v", list, err)
	}
	inv := list[0]
	if !inv.Live(base.Add(47 * time.Hour)) {
		t.Fatal("a link 47h into its 48h window reports dead")
	}
	if inv.Live(base.Add(48 * time.Hour)) {
		t.Fatal("a link at its expiry reports live — the listing would offer copy-again for a URL that 403s")
	}
}

func TestInviteRequiresARealRow(t *testing.T) {
	s := open(t)
	// The foreign key is on: an invite pointing at no row would be an audit
	// entry nobody can resolve back to a release.
	if _, err := s.CreateInvite(Invite{RowID: 4242, MintedBy: "ada", CreatedAt: base, ExpiresAt: base}); err == nil {
		t.Fatal("an invite against a non-existent release row was recorded")
	}
}
