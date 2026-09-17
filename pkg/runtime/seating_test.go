package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/gamingpb"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/schema"
	"github.com/karamble/dcrgaming-sdk/pkg/gaming/wire"
	"github.com/karamble/dcrgaming-sdk/pkg/membership"
)

const testGCID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// invite renders an invitation the way one arrives in a chat message.
func invite(t *testing.T, mut func(*schema.Invite)) string {
	t.Helper()
	inv := schema.Invite{
		Game: "battleships", Kind: schema.InviteKindTable, SID: "abcdef01",
		BuyInAtoms: 5_000_000, Seats: 2, CSVBlocks: 2048, Until: 900,
		AdmissionAtoms: escrow.MinBondAtoms, AdmissionBlocks: 4032,
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

func TestAcceptingAnInvitationCreatesADurablePendingTableThenJoins(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
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
	waitFor(t, "the admission deposit to be announced", func() bool {
		return len(fake.Spends()) > 0 && tbl.formation() != nil
	})
	if tbl.formation() == nil || tbl.formation().Ours() == nil {
		t.Fatal("this seat has no join of its own")
	}
	if err := rt.checkAdmissionBond(context.Background(), tbl.terms, tbl.formation().Ours()); err == nil {
		t.Fatal("an unconfirmed admission bond passed the readiness check")
	}
	fake.SetHeight(802)
	if err := rt.checkAdmissionBond(context.Background(), tbl.terms, tbl.formation().Ours()); err != nil {
		t.Fatalf("confirmed admission bond: %v", err)
	}
	if tbl.gcID != testGCID {
		t.Fatalf("the table remembers group chat %q", tbl.gcID)
	}
}

func TestAFullTableDoesNotExpireWhileAnnouncedBondsConfirm(t *testing.T) {
	fake, rt, _ := stand(t, &trivialGame{})
	sid, err := accept(rt, invite(t, nil), testGCID)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	waitFor(t, "local bond announcement", func() bool {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		return rt.tables[sid] != nil && rt.tables[sid].formation() != nil
	})
	rt.mu.Lock()
	tbl := rt.tables[sid]
	rt.mu.Unlock()

	them := newPeer(t, tbl.terms)
	_, pkScript, err := escrow.BondAddress(them.bond, rt.params)
	if err != nil {
		t.Fatalf("peer bond address: %v", err)
	}
	fake.Place(strings.Repeat("bb22cc33", 8), 1, pkScript, int64(tbl.terms.BondAtoms), 901)
	if err = rt.addJoin(context.Background(), sid, them.form.Ours()); err != nil {
		t.Fatalf("take peer bond announcement: %v", err)
	}
	if got := tbl.formation().State(); got != membership.Formed {
		t.Fatalf("full table state is %s", got)
	}

	// Admission is closed, but the complete signed roster and both exact bond
	// outputs were already known. One output has only one confirmation here.
	fake.SetHeight(901)
	rt.Tick(context.Background(), 901)
	rt.mu.Lock()
	recoveryOnly := tbl.recoveryOnly
	rt.mu.Unlock()
	if recoveryOnly || tbl.formation().State() == membership.Aborted {
		t.Fatal("full table expired while its announced bond was confirming")
	}
	if err = rt.CheckAdmissionBonds(context.Background(), sid); err == nil {
		t.Fatal("one-confirmation bond passed the readiness gate")
	}
	fake.SetHeight(902)
	if err = rt.CheckAdmissionBonds(context.Background(), sid); err != nil {
		t.Fatalf("confirmed roster bonds: %v", err)
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

// Financial version 2 invitations must carry the complete operator-approved
// economics. The game cannot fill in missing bond terms after approval.
func TestAnInvitationCannotOmitFinancialTerms(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	link := "gaming://battleships/table?fv=2&sid=abcdef01&buyin=5000000&seats=2&csv=2048&until=900"
	if _, err := accept(rt, link, testGCID); err == nil {
		t.Fatal("accepted an invitation with no admission recovery terms")
	}
}

func TestAnInvitationThisGameCannotSitAtIsRefused(t *testing.T) {
	_, rt, _ := stand(t, &trivialGame{})
	valid := invite(t, nil)
	for _, tc := range []struct {
		name, link, gcid string
	}{
		{"not a link at all", "hello", testGCID},
		{"another game's table", strings.Replace(valid, "battleships", "poker", 1), testGCID},
		{"not an invitation to a table", strings.Replace(valid, "/table?", "/tournament?", 1), testGCID},
		{"no session", strings.Replace(valid, "sid=abcdef01", "sid=", 1), testGCID},
		{"a group chat id that is not one", valid, "nope"},
		{"a group chat id of the wrong length", valid, strings.Repeat("a", 63)},
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
	waitFor(t, "the admission deposit to confirm", func() bool {
		if len(fake.Spends()) > 0 && fake.Height() < 802 {
			fake.SetHeight(802)
		}
		rt.mu.Lock()
		defer rt.mu.Unlock()
		return rt.tables[sid].formation() != nil
	})
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

// Formation traffic outlives a relay backlog and nothing else does.
//
// A join queued behind a backlog and expiring in transit forms one table and
// aborts the other. Everything else describes where money is right now, is
// repeated while it still matters, and is worse than useless once stale.
func TestFormationOutlivesABacklogAndTheRestDoesNot(t *testing.T) {
	for _, kind := range []schema.Kind{
		schema.KindJoin, schema.KindCommit, KindRoster,
	} {
		if got := classOf(kind); got != wire.ClassDurable {
			t.Errorf("%s is sent as class %v, and formation must survive history replay", kind, got)
		}
	}
	for _, kind := range []schema.Kind{
		KindFunded, KindBonded, KindPayout,
	} {
		if got := classOf(kind); got != wire.ClassDurable {
			t.Errorf("%s is sent as class %v, and financial state must survive history replay",
				kind, got)
		}
	}
	// Every kind the runtime sends is named above, so a new one cannot be
	// added without deciding how long it should live.
	if got := classOf("something-nobody-added-here"); got != wire.ClassState {
		t.Errorf("an unnamed kind defaults to %v", got)
	}
}
