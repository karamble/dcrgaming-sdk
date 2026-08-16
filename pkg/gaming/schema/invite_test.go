package schema

import (
	"strings"
	"testing"
)

func TestInviteRoundTrips(t *testing.T) {
	want := Invite{
		Game: Game, Kind: InviteKindTable,
		BuyInAtoms: 10_000_000, Seats: 6, SID: "0123456789abcdef",
	}
	link, err := want.String()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got, err := ParseInvite(link)
	if err != nil {
		t.Fatalf("parse %q: %v", link, err)
	}
	if got != want {
		t.Fatalf("round trip lost something: %+v != %+v", got, want)
	}
}

// An invite with no terms is still an invitation, and must stay renderable.
func TestInviteWithoutTerms(t *testing.T) {
	link, err := Invite{Game: Game, Kind: InviteKindTable}.String()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if link != "gaming://poker/table" {
		t.Fatalf("link is %q", link)
	}
	got, err := ParseInvite(link)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.BuyInAtoms != 0 || got.Seats != 0 || got.SID != "" {
		t.Fatalf("terms invented from nothing: %+v", got)
	}
}

func TestInviteRejectsMalformed(t *testing.T) {
	for name, inv := range map[string]Invite{
		"no game":        {Kind: InviteKindTable},
		"no kind":        {Game: Game},
		"game not a key": {Game: "Poker!", Kind: InviteKindTable},
	} {
		if _, err := inv.String(); err == nil {
			t.Errorf("%s should not render", name)
		}
	}

	for name, link := range map[string]string{
		"another scheme":     "https://poker/table",
		"no game":            "gaming:///table",
		"no kind":            "gaming://poker",
		"buyin not a number": "gaming://poker/table?buyin=lots",
		"seats not a number": "gaming://poker/table?seats=many",
		"not a link":         "hello",
	} {
		if _, err := ParseInvite(link); err == nil {
			t.Errorf("%s should not parse: %q", name, link)
		}
	}
}

// The host's renderer matches on this shape, so a change here that the host
// does not follow stops invitations rendering. This pins the literal.
func TestInviteLinkShapeIsStable(t *testing.T) {
	link, err := Invite{
		Game: Game, Kind: InviteKindTable,
		BuyInAtoms: 10_000_000, Seats: 6, SID: "abc123",
	}.String()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	const want = "gaming://poker/table?buyin=10000000&seats=6&sid=abc123"
	if link != want {
		t.Fatalf("invite link is %q, want %q - the host's parser matches on this", link, want)
	}
}

// An invitation nobody could act on should be refused when it is read. Left to
// later, a bad session id fails inside wire.Encode at the moment somebody tries
// to send their first frame, which is a long way from the mistake.
func TestInviteRefusesTermsNoTableCouldHave(t *testing.T) {
	for _, link := range []string{
		"gaming://poker/table?sid=NOTHEX",
		"gaming://poker/table?sid=" + strings.Repeat("ab", 17), // 34 chars
		"gaming://poker/table?seats=1",
		"gaming://poker/table?seats=7",
		"gaming://poker/table?csv=0",
		"gaming://poker/table?csv=soon",
	} {
		if inv, err := ParseInvite(link); err == nil {
			t.Errorf("%s was accepted as %+v", link, inv)
		}
	}
}

func TestInviteCarriesTheRefundTimelock(t *testing.T) {
	inv := Invite{Game: "poker", Kind: "table", BuyInAtoms: 10000000, Seats: 6, SID: "abc123", CSVBlocks: 64}
	link, err := inv.String()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	back, err := ParseInvite(link)
	if err != nil {
		t.Fatalf("parse %q: %v", link, err)
	}
	if back != inv {
		t.Fatalf("round trip changed the invite:\n got %+v\nwant %+v", back, inv)
	}
	if back.CSVBlocks != 64 {
		t.Fatalf("refund timelock came back as %d", back.CSVBlocks)
	}
}
