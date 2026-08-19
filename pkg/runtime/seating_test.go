package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/decred/dcrd/chaincfg/v3"

	"github.com/karamble/dcrgaming-sdk/pkg/gaming/bridgetest"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/connect"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/gamingpb"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/schema"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/transport"
	"github.com/karamble/dcrgaming-sdk/pkg/identity"
	"github.com/karamble/dcrgaming-sdk/pkg/spend"
)

const testGCID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// invite renders an invitation the way one arrives in a chat message.
func invite(t *testing.T, mut func(*schema.Invite)) string {
	t.Helper()
	inv := schema.Invite{
		Game: "battleships", Kind: schema.InviteKindTable, SID: "abcdef01",
		BuyInAtoms: 5_000_000, Seats: 2, CSVBlocks: 2048, Until: 900,
	}
	if mut != nil {
		mut(&inv)
	}
	link, err := inv.String()
	if err != nil {
		t.Fatalf("render the invitation: %v", err)
	}
	return link
}

func accept(rt *Runtime, link, gcid string) (string, error) {
	return rt.acceptInvite(context.Background(), &gamingpb.AcceptInvite{Invite: link, Gcid: gcid})
}

func TestAcceptingAnInvitationFormsATable(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	sid, err := accept(rt, invite(t, nil), testGCID)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if sid != "abcdef01" {
		t.Fatalf("joined %q", sid)
	}
	rt.mu.Lock()
	tbl, ok := rt.tables[sid]
	rt.mu.Unlock()
	if !ok {
		t.Fatal("no table was formed")
	}
	if tbl.form == nil || tbl.form.Ours() == nil {
		t.Fatal("this seat has no join of its own")
	}
	if tbl.gcID != testGCID {
		t.Fatalf("the table remembers group chat %q", tbl.gcID)
	}
}

// Accepting twice is the operator pressing the button again, not an error.
func TestAcceptingTwiceIsTheSameAnswer(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	link := invite(t, nil)
	first, err := accept(rt, link, testGCID)
	if err != nil {
		t.Fatalf("first accept: %v", err)
	}
	second, err := accept(rt, link, testGCID)
	if err != nil {
		t.Fatalf("second accept: %v", err)
	}
	if first != second {
		t.Fatalf("two answers: %q then %q", first, second)
	}
	rt.mu.Lock()
	n := len(rt.tables)
	rt.mu.Unlock()
	if n != 1 {
		t.Fatalf("accepting twice made %d tables", n)
	}
}

// The invitation decides what it states. A game that disagrees refuses, because
// two players reading different invitations must fail to form a table rather
// than form one and discover it at settlement.
func TestAGameRefusesTermsItDoesNotPlay(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	for _, tc := range []struct {
		name string
		mut  func(*schema.Invite)
	}{
		{"a different buy-in", func(i *schema.Invite) { i.BuyInAtoms = 9_000_000 }},
		{"a different seat count", func(i *schema.Invite) { i.Seats = 4 }},
		{"a shorter refund timelock", func(i *schema.Invite) { i.CSVBlocks = 288 }},
		{"a different admission deadline", func(i *schema.Invite) { i.Until = 5000 }},
	} {
		if _, err := accept(rt, invite(t, tc.mut), testGCID); err == nil {
			t.Errorf("%s: sat at a table on terms this game does not play", tc.name)
		}
	}
}

// What the invitation leaves out, the game fills in - and the bond it cannot
// state at all, because schema.Invite has no field for one.
func TestTheGameFillsInWhatTheInvitationLeavesOut(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	sid, err := accept(rt, invite(t, func(i *schema.Invite) { i.CSVBlocks = 0 }), testGCID)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	rt.mu.Lock()
	tm := rt.tables[sid].form.Terms()
	rt.mu.Unlock()
	if tm.CSVBlocks != 2048 {
		t.Fatalf("the game's refund timelock was not used: %d", tm.CSVBlocks)
	}
	if tm.BondAtoms == 0 || tm.BondLockBlocks != 4032 {
		t.Fatalf("the game's bond terms were not carried: %d atoms / %d blocks",
			tm.BondAtoms, tm.BondLockBlocks)
	}
}

func TestAnInvitationThisGameCannotSitAtIsRefused(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	for _, tc := range []struct {
		name string
		link string
		gcid string
	}{
		{"not a link at all", "hello", testGCID},
		{"another game's table", invite(t, func(i *schema.Invite) { i.Game = "poker" }), testGCID},
		{"not an invitation to a table", invite(t, func(i *schema.Invite) { i.Kind = "tournament" }), testGCID},
		{"no session", invite(t, func(i *schema.Invite) { i.SID = "" }), testGCID},
		{"a group chat id that is not one", invite(t, nil), "nope"},
		{"a group chat id of the wrong length", invite(t, nil), strings.Repeat("a", 63)},
	} {
		if _, err := accept(rt, tc.link, tc.gcid); err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
	}
}

// Until a table exists, nobody may allocate state for it; once it does, the
// table's own messages are admitted.
func TestFormingATableIsWhatOpensItToTraffic(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	if rt.authorized("abcdef01", "anyone") {
		t.Fatal("state was allocated for a table nobody had joined")
	}
	if _, err := accept(rt, invite(t, nil), testGCID); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if !rt.authorized("abcdef01", "anyone") {
		t.Fatal("the table this game just joined does not admit its own traffic")
	}
}

// A join or commit for a table this game is not at is refused rather than
// silently making one.
func TestFormationTrafficForAnUnknownTableIsRefused(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	if err := rt.addJoin(context.Background(), "no-such-table", nil); err == nil {
		t.Error("a join for an unknown table was taken")
	}
	if err := rt.addCommit(context.Background(), "no-such-table", nil); err == nil {
		t.Error("a commit for an unknown table was taken")
	}
}

// Seating waits for the beacon height and does nothing before it, so no seat
// can be assigned from a block that has not been mined.
func TestATableIsNotSeatedBeforeItsBeaconHeight(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, err := accept(rt, invite(t, nil), testGCID)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	fake.SetHeight(10)
	if err := rt.seatIfReady(context.Background(), sid); err != nil {
		t.Fatalf("seat: %v", err)
	}
	rt.mu.Lock()
	seated := rt.tables[sid].form.Seated()
	rt.mu.Unlock()
	if seated {
		t.Fatal("a table was seated before its beacon block existed")
	}
	if _, ok := rt.Seats(sid); ok {
		t.Fatal("seats were reported for a table that is not seated")
	}
}

func TestSeatingAnUnknownTableIsRefused(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	if err := rt.seatIfReady(context.Background(), "no-such-table"); err == nil {
		t.Fatal("seated a table that does not exist")
	}
}

// The names an operator sets are merged, and an empty one removes an entry, so
// a console can correct a single name without restating the rest.
func TestNamesAreMergedAndAnEmptyOneRemoves(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	rt.setNames(map[string]string{"aa": "Ann", "bb": "Bob"})
	rt.setNames(map[string]string{"bb": "Bobby"})
	got := rt.Names()
	if got["aa"] != "Ann" {
		t.Errorf("correcting one name dropped another: %v", got)
	}
	if got["bb"] != "Bobby" {
		t.Errorf("the correction did not land: %v", got)
	}
	rt.setNames(map[string]string{"aa": ""})
	if _, still := rt.Names()["aa"]; still {
		t.Error("an empty name did not remove the entry")
	}
}

// A seat with no bond has nothing to stake against its word, and a join names
// its bond, so it cannot join at all.
func TestASeatWithNoBondCannotJoin(t *testing.T) {
	fake := bridgetest.New(bridgetest.Options{
		Game: "battleships", Network: "mainnet",
		Params: chaincfg.TestNet3Params(), Height: 1000,
	})
	srv, err := fake.Serve("seat0")
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	g := &trivialGame{}
	conn, err := srv.Dial(ctx, "seat0", func(cfg *transport.BridgeConfig) { connect.Stamp(cfg, g.Identity()) })
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	book, err := spend.OpenBook(spend.MemStore())
	if err != nil {
		t.Fatalf("book: %v", err)
	}
	// Deliberately no SetBondDeposit.
	seed, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	rt, err := New(Config{Rules: g, Bridge: conn, Book: book, Identity: seed, SeatTags: testTags, Params: chaincfg.TestNet3Params()})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	_, err = accept(rt, invite(t, nil), testGCID)
	if err == nil {
		t.Fatal("a seat with nothing to forfeit joined a table")
	}
	// membership would refuse this anyway, with "no bond deposit". The
	// runtime's own check exists for the sentence after it, which tells a
	// developer what to do about it - so that is what is pinned.
	if !strings.Contains(err.Error(), "fund one before joining a table") {
		t.Fatalf("refused without telling the developer what to do: %v", err)
	}
}
