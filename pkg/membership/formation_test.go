package membership

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/decred/dcrd/chaincfg/v3"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
)

func testTerms(seats uint32) Terms {
	return Terms{
		Game:       "poker",
		GameVer:    1,
		SID:        "0123456789abcdef",
		BuyInAtoms: 10_000_000,
		Seats:      seats,
		CSVBlocks:  64,
		Until:      900000,
		BondAtoms:  1000000, BondLockBlocks: 2016,
	}
}

// players returns n keys in ascending canonical order, so a test can say
// "the lowest key" and mean it.
func players(t *testing.T, n int) []*secp256k1.PrivateKey {
	t.Helper()
	out := make([]*secp256k1.PrivateKey, 0, n)
	for len(out) < n {
		priv, err := secp256k1.GeneratePrivateKey()
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}
		out = append(out, priv)
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			a := out[i].PubKey().SerializeCompressed()
			b := out[j].PubKey().SerializeCompressed()
			if bytes.Compare(a, b) > 0 {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// testCreds gives a key a bond, so it can join anything. The deposit is made up
// - whether it is really on chain is CheckBond's question, not SignJoin's.
func testCreds(t *testing.T, priv *secp256k1.PrivateKey) Credentials {
	t.Helper()
	bond, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate bond key: %v", err)
	}
	seed := sha256.Sum256(append(priv.Serialize(), []byte("test-financial-key")...))
	recovery := secp256k1.PrivKeyFromBytes(seed[:])
	script, err := escrow.BridgeBondScript(bond.PubKey().SerializeCompressed(), recovery.PubKey().SerializeCompressed(), 2016)
	if err != nil {
		t.Fatalf("bond script: %v", err)
	}
	logKey, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate log key: %v", err)
	}
	return Credentials{
		Session:      priv,
		Log:          logKey,
		Bond:         bond,
		BondOutpoint: hex.EncodeToString(priv.PubKey().SerializeCompressed()[:32]) + ":0",
		BondScript:   script,
	}
}

func joinsFor(t *testing.T, terms Terms, privs []*secp256k1.PrivateKey) []*Join {
	t.Helper()
	out := make([]*Join, 0, len(privs))
	for _, p := range privs {
		j, err := SignJoin(terms, testCreds(t, p))
		if err != nil {
			t.Fatalf("sign join: %v", err)
		}
		out = append(out, j)
	}
	return out
}

// settleAll runs a full table to Settled: everyone joins, everyone binds,
// everyone hears everyone's commit.
func settleAll(t *testing.T, terms Terms, privs []*secp256k1.PrivateKey) []*Formation {
	t.Helper()
	all := joinsFor(t, terms, privs)

	fs := make([]*Formation, 0, len(privs))
	for _, p := range privs {
		f, err := NewFormation(terms, testCreds(t, p))
		if err != nil {
			t.Fatalf("new formation: %v", err)
		}
		for _, j := range all {
			if err := f.AddJoin(j); err != nil {
				t.Fatalf("add join: %v", err)
			}
		}
		fs = append(fs, f)
	}

	commits := make([]*Commit, 0, len(fs))
	for _, f := range fs {
		c, err := f.Bind()
		if err != nil {
			t.Fatalf("bind: %v", err)
		}
		commits = append(commits, c)
	}
	for _, f := range fs {
		for _, c := range commits {
			if err := f.AddCommit(c); err != nil {
				t.Fatalf("add commit: %v", err)
			}
		}
	}
	return fs
}

func TestAWholeTableSettlesOnOneMembership(t *testing.T) {
	terms := testTerms(6)
	fs := settleAll(t, terms, players(t, 6))

	want, ok := fs[0].RosterHash()
	if !ok {
		t.Fatal("no roster hash after settling")
	}
	for i, f := range fs {
		if f.State() != Settled {
			t.Fatalf("peer %d is %s, want settled (%s)", i, f.State(), f.Reason())
		}
		got, _ := f.RosterHash()
		if got != want {
			t.Fatalf("peer %d settled a different membership", i)
		}
		if id, _ := f.MatchID(); id == "" {
			t.Fatalf("peer %d settled with no match id", i)
		}
	}
}

// Joins arrive over a group chat, which has no ordering at all. If the
// membership depended on arrival order, two members of one table would derive
// different deposit addresses and the money would go to two places.
func TestTheMembershipIsTheSameWhateverOrderJoinsArriveIn(t *testing.T) {
	terms := testTerms(5)
	privs := players(t, 5)
	all := joinsFor(t, terms, privs)

	forward, err := NewFormation(terms, testCreds(t, privs[0]))
	if err != nil {
		t.Fatalf("new formation: %v", err)
	}
	for _, j := range all {
		_ = forward.AddJoin(j)
	}

	backward, err := NewFormation(terms, testCreds(t, privs[0]))
	if err != nil {
		t.Fatalf("new formation: %v", err)
	}
	for i := len(all) - 1; i >= 0; i-- {
		_ = backward.AddJoin(all[i])
	}

	a, ok := forward.RosterHash()
	if !ok {
		t.Fatal("forward order did not form")
	}
	b, ok := backward.RosterHash()
	if !ok {
		t.Fatal("reverse order did not form")
	}
	if a != b {
		t.Fatal("arrival order changed the membership")
	}
}

// The counterexample that killed "the lowest N keys of whatever has arrived".
//
// Two seats; three players join; C's key sorts lowest. Under the old rule A and
// B would agree on {A,B} and A would settle, then C's join would reach B alone
// and B would move to {A,C} - leaving A bound to a membership nobody else would
// ever sign for. Exact fill makes B abort instead, and A never reaches Settled
// because B never commits. The table fails, which is the correct outcome; what
// must never happen is A believing it succeeded.
func TestALateLowKeyCannotStrandAPeerThatAlreadySettled(t *testing.T) {
	terms := testTerms(2)
	privs := players(t, 3)
	c, a, b := privs[0], privs[1], privs[2] // c sorts lowest
	joins := joinsFor(t, terms, []*secp256k1.PrivateKey{a, b, c})
	joinA, joinB, joinC := joins[0], joins[1], joins[2]

	// A sees only A and B.
	fa, err := NewFormation(terms, testCreds(t, a))
	if err != nil {
		t.Fatalf("new formation: %v", err)
	}
	_ = fa.AddJoin(joinB)
	if fa.State() != Formed {
		t.Fatalf("A is %s, want formed", fa.State())
	}
	if _, err := fa.Bind(); err != nil {
		t.Fatalf("A bind: %v", err)
	}

	// B sees A, B and C.
	fb, err := NewFormation(terms, testCreds(t, b))
	if err != nil {
		t.Fatalf("new formation: %v", err)
	}
	_ = fb.AddJoin(joinA)
	_ = fb.AddJoin(joinC)
	if fb.State() != Aborted {
		t.Fatalf("B is %s, want aborted for oversubscription", fb.State())
	}

	// A is bound but must not settle: nobody else bound to its membership.
	if fa.State() == Settled {
		t.Fatal("A settled a membership its counterparty abandoned")
	}
	if fa.State() != Committed {
		t.Fatalf("A is %s, want committed and waiting", fa.State())
	}
}

func TestOversubscriptionAbortsRatherThanChoosing(t *testing.T) {
	terms := testTerms(2)
	privs := players(t, 3)

	f, err := NewFormation(terms, testCreds(t, privs[0]))
	if err != nil {
		t.Fatalf("new formation: %v", err)
	}
	for _, j := range joinsFor(t, terms, privs[1:]) {
		_ = f.AddJoin(j)
	}
	if f.State() != Aborted {
		t.Fatalf("state is %s, want aborted", f.State())
	}
	if f.Reason() == "" {
		t.Fatal("aborted without saying why")
	}
}

// More players than a table can ever seat must abort, not error out of the
// selection. escrow.CanonicalMembers refuses more than MaxMembers keys, so a
// rule that canonicalised everything seen would fail at exactly the case it
// was meant to handle.
func TestMoreKeysThanAnyTableCanSeatStillAborts(t *testing.T) {
	terms := testTerms(uint32(escrow.MaxMembers))
	privs := players(t, escrow.MaxMembers+2)

	f, err := NewFormation(terms, testCreds(t, privs[0]))
	if err != nil {
		t.Fatalf("new formation: %v", err)
	}
	for _, j := range joinsFor(t, terms, privs[1:]) {
		if err := f.AddJoin(j); err != nil {
			t.Fatalf("add join: %v", err)
		}
	}
	if f.State() != Aborted {
		t.Fatalf("state is %s, want aborted", f.State())
	}
}

// Binding is the point of no return. A join that turns up afterwards cannot
// move what this peer signed, or the signature would mean nothing.
func TestAJoinAfterBindingCannotChangeTheMembership(t *testing.T) {
	terms := testTerms(2)
	privs := players(t, 3)
	a, b, late := privs[1], privs[2], privs[0]

	f, err := NewFormation(terms, testCreds(t, a))
	if err != nil {
		t.Fatalf("new formation: %v", err)
	}
	_ = f.AddJoin(joinsFor(t, terms, []*secp256k1.PrivateKey{b})[0])
	before, _ := f.RosterHash()
	if _, err := f.Bind(); err != nil {
		t.Fatalf("bind: %v", err)
	}

	if err := f.AddJoin(joinsFor(t, terms, []*secp256k1.PrivateKey{late})[0]); err != nil {
		t.Fatalf("add late join: %v", err)
	}
	if f.State() != Committed {
		t.Fatalf("state is %s, want it to stay committed", f.State())
	}
	after, _ := f.RosterHash()
	if after != before {
		t.Fatal("a late join moved a membership that had been bound")
	}
}

// Rejection has to be wholesale. A key that never joined must not be admitted
// because it arrived alongside real ones.
func TestAKeyThatNeverJoinedIsNeverAdmitted(t *testing.T) {
	terms := testTerms(3)
	privs := players(t, 3)

	f, err := NewFormation(terms, testCreds(t, privs[0]))
	if err != nil {
		t.Fatalf("new formation: %v", err)
	}
	before := len(f.Joins())

	// A join signed for another table's terms, replayed here.
	other := testTerms(3)
	other.SID = "abcdef0123456789"
	forged := joinsFor(t, other, privs[1:2])[0]
	if err := f.AddJoin(forged); err == nil {
		t.Fatal("a join for another table was admitted")
	}

	// A join whose signature is simply wrong.
	bad := joinsFor(t, terms, privs[2:3])[0]
	bad.Sig[0] ^= 0xff
	if err := f.AddJoin(bad); err == nil {
		t.Fatal("a join with a broken signature was admitted")
	}

	if got := len(f.Joins()); got != before {
		t.Fatalf("holding %d joins, want the original %d", got, before)
	}
}

// Two players who read different invitations must fail to form a table rather
// than form one and discover at settlement that they staked different amounts.
func TestPeersWhoReadDifferentInvitesNeverForm(t *testing.T) {
	base := testTerms(2)
	privs := players(t, 2)

	for _, tc := range []struct {
		name  string
		alter func(t Terms) Terms
	}{
		{"buy-in", func(t Terms) Terms { t.BuyInAtoms *= 10; return t }},
		{"seats", func(t Terms) Terms { t.Seats = 3; return t }},
		{"timelock", func(t Terms) Terms { t.CSVBlocks = 1; return t }},
		{"session", func(t Terms) Terms { t.SID = "beef"; return t }},
		{"deadline", func(t Terms) Terms { t.Until = 999999; return t }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := NewFormation(base, testCreds(t, privs[0]))
			if err != nil {
				t.Fatalf("new formation: %v", err)
			}
			theirs := joinsFor(t, tc.alter(base), privs[1:])[0]
			if err := f.AddJoin(theirs); err == nil {
				t.Fatal("a join under different terms was admitted")
			}
			if f.State() != Joining {
				t.Fatalf("state is %s, want still joining", f.State())
			}
		})
	}
}

// Silence is never punished and never attested. A table that does not fill just
// does not happen.
func TestATableShortOneCommitNeverSettles(t *testing.T) {
	terms := testTerms(3)
	privs := players(t, 3)
	all := joinsFor(t, terms, privs)

	f, err := NewFormation(terms, testCreds(t, privs[0]))
	if err != nil {
		t.Fatalf("new formation: %v", err)
	}
	for _, j := range all {
		_ = f.AddJoin(j)
	}
	if _, err := f.Bind(); err != nil {
		t.Fatalf("bind: %v", err)
	}

	// Only one of the other two ever binds.
	other, err := NewFormation(terms, testCreds(t, privs[1]))
	if err != nil {
		t.Fatalf("new formation: %v", err)
	}
	for _, j := range all {
		_ = other.AddJoin(j)
	}
	c, err := other.Bind()
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := f.AddCommit(c); err != nil {
		t.Fatalf("add commit: %v", err)
	}

	if f.State() != Committed {
		t.Fatalf("state is %s, want committed and waiting", f.State())
	}
	if _, ok := f.Roster(); ok {
		t.Fatal("handed out a roster without every member bound to it")
	}
}

// One key bound to two memberships is a contradiction anyone can check, and it
// is the only thing in forming a table that is somebody's fault.
func TestOneKeyBoundToTwoMembershipsIsAProof(t *testing.T) {
	terms := testTerms(2)
	privs := players(t, 2)
	all := joinsFor(t, terms, privs)

	f, err := NewFormation(terms, testCreds(t, privs[0]))
	if err != nil {
		t.Fatalf("new formation: %v", err)
	}
	for _, j := range all {
		_ = f.AddJoin(j)
	}

	honest, err := SignCommit(terms, mustHash(t, f), privs[1])
	if err != nil {
		t.Fatalf("sign commit: %v", err)
	}
	var elsewhere [32]byte
	elsewhere[0] = 0xff
	liar, err := SignCommit(terms, elsewhere, privs[1])
	if err != nil {
		t.Fatalf("sign commit: %v", err)
	}

	_ = f.AddCommit(honest)
	_ = f.AddCommit(liar)

	if f.State() != Aborted {
		t.Fatalf("state is %s, want aborted", f.State())
	}
	proof := f.Conflict()
	if proof == nil {
		t.Fatal("aborted on a contradiction without keeping the proof")
	}
	if err := proof.Verify(terms); err != nil {
		t.Fatalf("the proof does not verify: %v", err)
	}

	// Showing the same commit twice proves nothing.
	if err := (ConflictingCommits{A: honest, B: honest}).Verify(terms); err == nil {
		t.Fatal("one commit shown twice was accepted as a contradiction")
	}
	// Nor do two different keys disagreeing, which is not a contradiction:
	// they are just two peers who saw different things.
	mine, err := SignCommit(terms, elsewhere, privs[0])
	if err != nil {
		t.Fatalf("sign commit: %v", err)
	}
	if err := (ConflictingCommits{A: mine, B: honest}).Verify(terms); err == nil {
		t.Fatal("two different keys were accepted as a contradiction")
	}
}

// A settled table stays settled. It is still sound: the equivocator bound
// itself to our membership too, so our escrows still need every signature they
// always did, and making its other table real would cost it a second stake.
func TestASettledTableIsNotUnwoundByLaterEquivocation(t *testing.T) {
	terms := testTerms(2)
	privs := players(t, 2)
	fs := settleAll(t, terms, privs)
	f := fs[0]

	var elsewhere [32]byte
	elsewhere[0] = 0xaa
	liar, err := SignCommit(terms, elsewhere, privs[1])
	if err != nil {
		t.Fatalf("sign commit: %v", err)
	}
	if err := f.AddCommit(liar); err != nil {
		t.Fatalf("add commit: %v", err)
	}

	if f.State() != Settled {
		t.Fatalf("state is %s, want it to stay settled", f.State())
	}
	proof := f.Conflict()
	if proof == nil {
		t.Fatal("the contradiction was seen and not kept")
	}
	if err := proof.Verify(terms); err != nil {
		t.Fatalf("the proof does not verify: %v", err)
	}
}

func TestSeatsAndCanonicalOrderAreBothReported(t *testing.T) {
	terms := testTerms(4)
	fs := settleAll(t, terms, players(t, 4))

	members, ok := fs[0].Members()
	if !ok {
		t.Fatal("no members after settling")
	}
	// Seating waits for the block it is drawn from; the membership does not.
	if err := fs[0].SetBeacon(testBeacon()); err != nil {
		t.Fatalf("beacon: %v", err)
	}
	seats, ok := fs[0].Seats()
	if !ok {
		t.Fatal("no seats after the draw")
	}
	if len(seats) != len(members) {
		t.Fatalf("%d seats for %d members", len(seats), len(members))
	}
	roster, ok := fs[0].Roster()
	if !ok {
		t.Fatal("no roster after settling")
	}
	if !roster.Closed() {
		t.Fatal("a settled roster should be closed")
	}
	if roster.Pending() != 0 {
		t.Fatalf("%d seats still pending on a settled roster", roster.Pending())
	}
}

func TestTermsMustDescribeATableThatCouldExist(t *testing.T) {
	for _, tc := range []struct {
		name  string
		terms Terms
	}{
		{"no game", func() Terms { x := testTerms(2); x.Game = ""; return x }()},
		{"no session", func() Terms { x := testTerms(2); x.SID = " "; return x }()},
		{"one seat", func() Terms { x := testTerms(2); x.Seats = 1; return x }()},
		{"too many seats", func() Terms {
			x := testTerms(2)
			x.Seats = uint32(escrow.MaxMembers) + 1
			return x
		}()},
		{"no timelock", func() Terms { x := testTerms(2); x.CSVBlocks = 0; return x }()},
		{"no deadline", func() Terms { x := testTerms(2); x.Until = 0; return x }()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.terms.Validate(); err == nil {
				t.Fatal("expected a refusal")
			}
			if _, err := NewFormation(tc.terms, testCreds(t, players(t, 1)[0])); err == nil {
				t.Fatal("formed a table on terms that cannot exist")
			}
		})
	}
}

func mustHash(t *testing.T, f *Formation) [32]byte {
	t.Helper()
	h, ok := f.RosterHash()
	if !ok {
		t.Fatal("no roster hash")
	}
	return h
}

// The deadline is what makes "no more joins are coming" a fact. A table short
// of its seats when admission shuts cannot form, and waiting longer would only
// be hoping.
func TestATableShortOfSeatsAtTheDeadlineCannotForm(t *testing.T) {
	terms := testTerms(3)
	privs := players(t, 3)

	f, err := NewFormation(terms, testCreds(t, privs[0]))
	if err != nil {
		t.Fatalf("new formation: %v", err)
	}
	_ = f.AddJoin(joinsFor(t, terms, privs[1:2])[0])
	if f.State() != Joining {
		t.Fatalf("state is %s, want still joining", f.State())
	}

	f.CloseWindow()
	if f.State() != Aborted {
		t.Fatalf("state is %s, want aborted", f.State())
	}
	if f.Reason() == "" {
		t.Fatal("aborted without saying why")
	}
}

// Once admission shuts, a join is not merely late - it is one the peers who saw
// the deadline pass have already refused. Accepting it would put this peer at a
// table nobody else is at.
func TestAJoinAfterTheDeadlineIsNotAdmitted(t *testing.T) {
	terms := testTerms(2)
	privs := players(t, 3)

	f, err := NewFormation(terms, testCreds(t, privs[1]))
	if err != nil {
		t.Fatalf("new formation: %v", err)
	}
	_ = f.AddJoin(joinsFor(t, terms, privs[2:3])[0])
	if f.State() != Formed {
		t.Fatalf("state is %s, want formed", f.State())
	}
	before, _ := f.RosterHash()

	f.CloseWindow()
	if f.State() != Formed {
		t.Fatalf("a full table was disturbed by the deadline: %s", f.State())
	}
	if err := f.AddJoin(joinsFor(t, terms, privs[0:1])[0]); err != nil {
		t.Fatalf("add late join: %v", err)
	}
	if f.State() != Formed {
		t.Fatalf("a join past the deadline changed the state to %s", f.State())
	}
	if after, _ := f.RosterHash(); after != before {
		t.Fatal("a join past the deadline changed the membership")
	}
}

// A full table whose members all say they hold the same thing can bind without
// waiting out its deadline, which is the difference between a lobby that forms
// in seconds and one nobody watches for ten minutes.
func TestAFullTableThatAgreesNeedNotWaitForTheDeadline(t *testing.T) {
	terms := testTerms(3)
	privs := players(t, 3)
	all := joinsFor(t, terms, privs)

	mine, err := NewFormation(terms, testCreds(t, privs[0]))
	if err != nil {
		t.Fatalf("new formation: %v", err)
	}
	for _, j := range all {
		_ = mine.AddJoin(j)
	}
	if mine.State() != Formed {
		t.Fatalf("state is %s, want formed", mine.State())
	}
	if mine.Agreed() {
		t.Fatal("agreement was reported before anybody else said anything")
	}

	roster, _ := mine.RosterHash()
	for _, p := range privs[1:] {
		a, err := SignAssertion(terms, roster, p)
		if err != nil {
			t.Fatalf("sign assertion: %v", err)
		}
		if err := mine.AddAssertion(a, all); err != nil {
			t.Fatalf("add assertion: %v", err)
		}
	}
	// Our own counts too, and we have not said it yet.
	if mine.Agreed() {
		t.Fatal("agreement was reported without this peer having asserted")
	}
	ours, err := mine.Assertion()
	if err != nil {
		t.Fatalf("assertion: %v", err)
	}
	if err := mine.AddAssertion(ours, all); err != nil {
		t.Fatalf("add own assertion: %v", err)
	}
	if !mine.Agreed() {
		t.Fatal("every member said the same thing and it was not enough")
	}
	if mine.WindowClosed() {
		t.Fatal("agreement should not have needed the deadline")
	}
}

// Somebody holding a different membership is not agreement, and must not be
// counted as it - that is the case the whole gate exists to notice.
func TestAPeerHoldingSomethingElseIsNotAgreement(t *testing.T) {
	terms := testTerms(2)
	privs := players(t, 2)
	all := joinsFor(t, terms, privs)

	mine, err := NewFormation(terms, testCreds(t, privs[0]))
	if err != nil {
		t.Fatalf("new formation: %v", err)
	}
	for _, j := range all {
		_ = mine.AddJoin(j)
	}

	var elsewhere [32]byte
	elsewhere[0] = 0xaa
	for _, p := range privs {
		a, err := SignAssertion(terms, elsewhere, p)
		if err != nil {
			t.Fatalf("sign assertion: %v", err)
		}
		if err := mine.AddAssertion(a, all); err != nil {
			t.Fatalf("add assertion: %v", err)
		}
	}
	if mine.Agreed() {
		t.Fatal("assertions naming another membership were counted as agreement")
	}
}

// An unsigned assertion would let anyone manufacture agreement - telling every
// peer that the others concur with whatever it happens to hold, and so driving
// peers with different join sets to bind different memberships.
func TestAnAssertionNobodySignedIsRefused(t *testing.T) {
	terms := testTerms(2)
	privs := players(t, 2)
	all := joinsFor(t, terms, privs)

	mine, err := NewFormation(terms, testCreds(t, privs[0]))
	if err != nil {
		t.Fatalf("new formation: %v", err)
	}
	for _, j := range all {
		_ = mine.AddJoin(j)
	}
	roster, _ := mine.RosterHash()

	forged, err := SignAssertion(terms, roster, privs[1])
	if err != nil {
		t.Fatalf("sign assertion: %v", err)
	}
	forged.Sig[0] ^= 0xff
	if err := mine.AddAssertion(forged, all); err == nil {
		t.Fatal("an assertion with a broken signature was admitted")
	}
	if mine.Agreed() {
		t.Fatal("a forged assertion counted toward agreement")
	}
}

func bondFacts(t *testing.T, c Credentials) BondFacts {
	t.Helper()
	_, pkScript, err := escrow.BondAddress(c.BondScript, chaincfg.SimNetParams())
	if err != nil {
		t.Fatalf("bond address: %v", err)
	}
	return BondFacts{
		Found:         true,
		ValueAtoms:    int64(escrow.MinBondAtoms),
		PkScriptHex:   hex.EncodeToString(pkScript),
		Confirmations: int64(escrow.BondConfirmations),
	}
}

// Without a bond a seat is free, and a stranger can fill a table or spoil it as
// often as they like. This is the check that makes it cost something.
func TestARealBondIsAccepted(t *testing.T) {
	terms := testTerms(2)
	creds := testCreds(t, players(t, 1)[0])
	j, err := SignJoin(terms, creds)
	if err != nil {
		t.Fatalf("sign join: %v", err)
	}
	if err := j.Verify(terms); err != nil {
		t.Fatalf("a genuine join did not verify: %v", err)
	}
	if err := CheckBond(j, bondFacts(t, creds), chaincfg.SimNetParams()); err != nil {
		t.Fatalf("a genuine bond was refused: %v", err)
	}
}

func TestABondThatIsNotWhatItClaimsIsRefused(t *testing.T) {
	terms := testTerms(2)
	creds := testCreds(t, players(t, 1)[0])
	j, err := SignJoin(terms, creds)
	if err != nil {
		t.Fatalf("sign join: %v", err)
	}
	params := chaincfg.SimNetParams()

	for _, tc := range []struct {
		name   string
		break_ func(f BondFacts) BondFacts
	}{
		{"no coin there", func(f BondFacts) BondFacts { f.Found = false; return f }},
		{"pays something else", func(f BondFacts) BondFacts {
			f.PkScriptHex = "a914" + strings.Repeat("00", 20) + "87"
			return f
		}},
		{"too small", func(f BondFacts) BondFacts {
			f.ValueAtoms = int64(escrow.MinBondAtoms) - 1
			return f
		}},
		{"not buried yet", func(f BondFacts) BondFacts {
			f.Confirmations = int64(escrow.BondConfirmations) - 1
			return f
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := CheckBond(j, tc.break_(bondFacts(t, creds)), params); err == nil {
				t.Fatal("expected a refusal")
			}
		})
	}
}

// A bond is public - coin at an outpoint anyone can name - so a join carrying
// somebody else's deposit has to fail on its face, before any chain is asked.
func TestAJoinCannotCiteSomebodyElsesBond(t *testing.T) {
	terms := testTerms(2)
	privs := players(t, 2)
	mine, theirs := testCreds(t, privs[0]), testCreds(t, privs[1])

	// Take their deposit and script, keep our own session key: the proof of
	// possession is over their bond key, which we do not have.
	stolen := mine
	stolen.BondOutpoint, stolen.BondScript = theirs.BondOutpoint, theirs.BondScript
	j, err := SignJoin(terms, stolen)
	if err != nil {
		t.Fatalf("sign join: %v", err)
	}
	if err := j.Verify(terms); err == nil {
		t.Fatal("a join citing another player's bond verified")
	}
}

// The bond is signed over, so a relayed join cannot have its deposit swapped
// for another on the way.
func TestABondCannotBeSwappedInARelayedJoin(t *testing.T) {
	terms := testTerms(2)
	privs := players(t, 2)
	mine, theirs := testCreds(t, privs[0]), testCreds(t, privs[1])

	j, err := SignJoin(terms, mine)
	if err != nil {
		t.Fatalf("sign join: %v", err)
	}
	j.Bond.Outpoint = theirs.BondOutpoint
	if err := j.Verify(terms); err == nil {
		t.Fatal("a join whose deposit was swapped still verified")
	}
}

// A lock short enough to roll over between tables deters nothing, so a bond on
// those terms is not one.
func TestABondLockedTooBrieflyIsRefused(t *testing.T) {
	terms := testTerms(2)
	priv := players(t, 1)[0]
	bond, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate bond key: %v", err)
	}
	// BondScript itself refuses a short lock, so the script has to be built
	// at the minimum and the join checked against a raised one.
	seed := sha256.Sum256(append(priv.Serialize(), []byte("test-financial-key")...))
	recovery := secp256k1.PrivKeyFromBytes(seed[:])
	script, err := escrow.BridgeBondScript(bond.PubKey().SerializeCompressed(), recovery.PubKey().SerializeCompressed(), 2016)
	if err != nil {
		t.Fatalf("bond script: %v", err)
	}
	logKey, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate log key: %v", err)
	}
	j, err := SignJoin(terms, Credentials{
		Session: priv, Log: logKey, Bond: bond, BondOutpoint: "beef:0", BondScript: script,
	})
	if err != nil {
		t.Fatalf("sign join: %v", err)
	}
	if err := j.Verify(terms); err != nil {
		t.Fatalf("a bond at the minimum lock was refused: %v", err)
	}
}

func TestJoiningNeedsCredentialsThatCouldWork(t *testing.T) {
	terms := testTerms(2)
	full := testCreds(t, players(t, 1)[0])

	for _, tc := range []struct {
		name  string
		creds Credentials
	}{
		{"no session key", Credentials{Log: full.Log, Bond: full.Bond, BondOutpoint: full.BondOutpoint, BondScript: full.BondScript}},
		{"no log key", Credentials{Session: full.Session, Bond: full.Bond, BondOutpoint: full.BondOutpoint, BondScript: full.BondScript}},
		{"no bond key", Credentials{Session: full.Session, Log: full.Log, BondOutpoint: full.BondOutpoint, BondScript: full.BondScript}},
		{"no deposit", Credentials{Session: full.Session, Log: full.Log, Bond: full.Bond, BondScript: full.BondScript}},
		{"no script", Credentials{Session: full.Session, Log: full.Log, Bond: full.Bond, BondOutpoint: full.BondOutpoint}},
		// The one that would put the stake at risk of forfeiture.
		{"the session key as the log key", Credentials{Session: full.Session, Log: full.Session, Bond: full.Bond, BondOutpoint: full.BondOutpoint, BondScript: full.BondScript}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := SignJoin(terms, tc.creds); err == nil {
				t.Fatal("expected a refusal")
			}
			if _, err := NewFormation(terms, tc.creds); err == nil {
				t.Fatal("a table was joined without a bond")
			}
		})
	}
}

func testBeacon() []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = byte(i * 7)
	}
	return b
}

// Every member draws the same table from the same block. If they did not, they
// would disagree about who acts when, which is the one thing seats decide.
func TestEverySeatDrawIsTheSameFromTheSameBlock(t *testing.T) {
	terms := testTerms(6)
	fs := settleAll(t, terms, players(t, 6))

	var want map[uint32][]byte
	for i, f := range fs {
		if f.Seated() {
			t.Fatalf("peer %d was seated before the block was drawn", i)
		}
		if _, ok := f.Seats(); ok {
			t.Fatalf("peer %d had seats before the block was drawn", i)
		}
		if err := f.SetBeacon(testBeacon()); err != nil {
			t.Fatalf("peer %d: %v", i, err)
		}
		seats, ok := f.Seats()
		if !ok {
			t.Fatalf("peer %d has no seats after the draw", i)
		}
		if want == nil {
			want = seats
			continue
		}
		if len(seats) != len(want) {
			t.Fatalf("peer %d seated %d, want %d", i, len(seats), len(want))
		}
		for seat, key := range want {
			if !bytes.Equal(seats[seat], key) {
				t.Fatalf("peer %d put a different key in seat %d", i, seat)
			}
		}
	}
}

// The whole point. Seating by canonical key order would hand seat zero - and
// with it the first hand's button - to whoever generated the lowest key.
func TestSeatingIsNotTheOrderTheScriptNamesMembersIn(t *testing.T) {
	terms := testTerms(6)
	creds := make([]Credentials, 0, 6)
	for _, p := range players(t, 6) {
		creds = append(creds, testCreds(t, p))
	}

	// Draw the same membership under many different blocks: if seating
	// followed the key order, seat zero would never move.
	firsts := map[string]int{}
	for i := range 32 {
		f, err := NewFormation(terms, creds[0])
		if err != nil {
			t.Fatalf("new formation: %v", err)
		}
		for _, c := range creds[1:] {
			j, err := SignJoin(terms, c)
			if err != nil {
				t.Fatalf("sign join: %v", err)
			}
			if err := f.AddJoin(j); err != nil {
				t.Fatalf("add join: %v", err)
			}
		}
		beacon := testBeacon()
		beacon[0] = byte(i)
		if err := f.SetBeacon(beacon); err != nil {
			t.Fatalf("beacon: %v", err)
		}
		seats, ok := f.Seats()
		if !ok {
			t.Fatal("no seats after the draw")
		}
		firsts[string(seats[0])]++
	}

	if len(firsts) < 2 {
		t.Fatal("seat zero never moved, so it is still something a key can win")
	}
}

// Seating is drawn once. A second block would reseat a table that may already
// have been dealt.
func TestSeatingIsDrawnOnce(t *testing.T) {
	terms := testTerms(3)
	fs := settleAll(t, terms, players(t, 3))
	f := fs[0]

	if err := f.SetBeacon(testBeacon()); err != nil {
		t.Fatalf("beacon: %v", err)
	}
	first, _ := f.Seats()

	other := testBeacon()
	other[0] ^= 0xff
	if err := f.SetBeacon(other); err != nil {
		t.Fatalf("second beacon: %v", err)
	}
	again, _ := f.Seats()

	for seat, key := range first {
		if !bytes.Equal(again[seat], key) {
			t.Fatal("a second block reseated the table")
		}
	}
	if err := f.SetBeacon(nil); err == nil {
		t.Fatal("an empty beacon was accepted")
	}
}

// The block is one from after everybody committed, so it did not exist when
// anyone was choosing a key.
func TestTheDrawingBlockIsPastTheDeadline(t *testing.T) {
	terms := testTerms(2)
	if got := BeaconHeight(terms); got <= terms.Until {
		t.Fatalf("seating is drawn at height %d, which is not past the deadline %d", got, terms.Until)
	}
}

// Abandoning is for a table this key already bound itself to - Committed or
// Settled - and for nothing earlier. At Committed the membership must survive
// the abort exactly as it does at Settled: the commit was this key's
// irrevocable word, and no money can have moved yet because funding needs a
// seating, which needs the table settled.
func TestAbandonEndsACommittedTableAndKeepsItsWord(t *testing.T) {
	terms := testTerms(2)
	privs := players(t, 2)
	all := joinsFor(t, terms, privs)

	f, err := NewFormation(terms, testCreds(t, privs[0]))
	if err != nil {
		t.Fatalf("new formation: %v", err)
	}

	// Joining: nothing to give up on yet.
	f.Abandon("too early")
	if f.State() != Joining {
		t.Fatalf("abandoning a joining table left it %v", f.State())
	}
	for _, j := range all {
		if err := f.AddJoin(j); err != nil {
			t.Fatalf("add join: %v", err)
		}
	}

	// Formed: still nothing bound, still not abandonable.
	f.Abandon("still too early")
	if f.State() != Formed {
		t.Fatalf("abandoning a formed table left it %v", f.State())
	}

	if _, err := f.Bind(); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if f.State() != Committed {
		t.Fatalf("state is %v after binding alone", f.State())
	}
	canonical := f.canonical
	if canonical == nil {
		t.Fatal("no canonical membership at committed")
	}

	f.Abandon("the other commitments never arrived")
	if f.State() != Aborted {
		t.Fatalf("a committed table would not abandon: %v", f.State())
	}
	if f.Reason() == "" {
		t.Fatal("abandoned with no reason recorded")
	}
	if f.canonical == nil {
		t.Fatal("abandoning forgot the membership, which is where the scripts come from")
	}
}
