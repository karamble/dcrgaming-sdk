package membership

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
	"github.com/karamble/dcrgaming-sdk/pkg/forfeit"
	"github.com/karamble/dcrgaming-sdk/pkg/gamelog"
)

// The point of putting log keys in joins: a peer that has only ever received
// signed joins can build the roster gamelog verifies entries against, without a
// second message and without trusting anybody who relayed one.

// A table forms, and every peer independently derives the same log roster - then
// a hand signed under it actually verifies.
func TestAPeerBuildsTheLogRosterFromJoinsAlone(t *testing.T) {
	terms := testTerms(3)
	privs := players(t, 3)
	creds := make([]Credentials, len(privs))
	for i, p := range privs {
		creds[i] = testCreds(t, p)
	}

	// Each peer runs its own formation and hears the others' joins relayed.
	forms := make([]*Formation, len(creds))
	for i := range creds {
		f, err := NewFormation(terms, creds[i])
		if err != nil {
			t.Fatalf("formation %d: %v", i, err)
		}
		forms[i] = f
	}
	for i, f := range forms {
		for j := range forms {
			if i == j {
				continue
			}
			if err := f.AddJoin(forms[j].Ours()); err != nil {
				t.Fatalf("peer %d could not take peer %d's join: %v", i, j, err)
			}
		}
		f.CloseWindow()
		if _, err := f.Bind(); err != nil {
			t.Fatalf("peer %d could not bind: %v", i, err)
		}
	}
	for i, f := range forms {
		for j := range forms {
			c, err := forms[j].Bind()
			if err != nil {
				t.Fatalf("commit: %v", err)
			}
			if err := f.AddCommit(c); err != nil {
				t.Fatalf("peer %d could not take peer %d's commit: %v", i, j, err)
			}
		}
		if err := f.SetBeacon(testBeacon()); err != nil {
			t.Fatalf("peer %d beacon: %v", i, err)
		}
	}

	// Every peer must derive the same log roster, from joins alone.
	var want map[uint32][]byte
	for i, f := range forms {
		got, ok := f.LogSeats()
		if !ok {
			t.Fatalf("peer %d could not derive a log roster", i)
		}
		if want == nil {
			want = got
			continue
		}
		if len(got) != len(want) {
			t.Fatalf("peer %d derived %d log seats, want %d", i, len(got), len(want))
		}
		for seat, key := range want {
			if !bytes.Equal(got[seat], key) {
				t.Fatalf("peer %d put a different key at seat %d", i, seat)
			}
		}
	}

	// A log key must never be the session key that holds the stake.
	sessions, _ := forms[0].Seats()
	for seat, logKey := range want {
		if bytes.Equal(logKey, sessions[seat]) {
			t.Fatalf("seat %d's log key is its session key", seat)
		}
	}

	// And the roster has to actually work: a hand signed under it verifies.
	match := MatchID("table1", terms.SID)
	chain, err := gamelog.NewChain(match, gamelog.Roster(want))
	if err != nil {
		t.Fatalf("new chain: %v", err)
	}
	seat, ok := forms[0].OurSeat()
	if !ok {
		t.Fatal("peer 0 has no seat")
	}
	key, err := forfeit.LogKeyFrom(creds[0].Log, match)
	if err != nil {
		t.Fatalf("log key: %v", err)
	}
	e := chain.Next(seat, 1, gamelog.StreetPreFlop, gamelog.ActionCheck, 0)
	if err := e.Sign(key); err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := chain.Append(e); err != nil {
		t.Fatalf("an entry signed under the derived roster was refused: %v", err)
	}
}

// The join's own signature is the binding, so swapping the log key on a relayed
// join has to break it. Otherwise any member could enrol a log key of their own
// choosing for somebody else's seat and sign that seat's actions.
func TestALogKeySwappedInFlightIsCaught(t *testing.T) {
	terms := testTerms(2)
	priv := players(t, 1)[0]
	j, err := SignJoin(terms, testCreds(t, priv))
	if err != nil {
		t.Fatalf("sign join: %v", err)
	}
	if err := j.Verify(terms); err != nil {
		t.Fatalf("an honest join did not verify: %v", err)
	}

	theirs, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	swapped := *j
	swapped.LogKey = theirs.PubKey().SerializeCompressed()
	if err := swapped.Verify(terms); err == nil {
		t.Fatal("a join with its log key swapped in flight still verified")
	}

	// And it cannot be lifted into another table, where the log key would be
	// expected to be fresh.
	other := testTerms(2)
	other.SID = "another-session"
	if err := j.Verify(other); err == nil {
		t.Fatal("a join verified at a table it was not made for")
	}
}

// A join that names its session key as its log key must be refused, because
// equivocating would then publish the key holding the stake rather than
// forfeiting the bond.
func TestAJoinCannotUseTheSessionKeyAsItsLogKey(t *testing.T) {
	terms := testTerms(2)
	priv := players(t, 1)[0]

	creds := testCreds(t, priv)
	creds.Log = creds.Session
	if _, err := SignJoin(terms, creds); err == nil {
		t.Fatal("signed a join whose log key is its session key")
	}

	// And forged after the fact, it fails verification rather than passing
	// quietly.
	good, err := SignJoin(terms, testCreds(t, priv))
	if err != nil {
		t.Fatalf("sign join: %v", err)
	}
	forged := *good
	forged.LogKey = forged.Key
	if err := forged.Verify(terms); err == nil {
		t.Fatal("a join naming its session key as its log key verified")
	}
}

// A join with no log key at all cannot be used to build a roster, rather than
// producing one with a hole in it.
func TestAJoinWithNoLogKeyIsRefused(t *testing.T) {
	terms := testTerms(2)
	priv := players(t, 1)[0]
	good, err := SignJoin(terms, testCreds(t, priv))
	if err != nil {
		t.Fatalf("sign join: %v", err)
	}
	stripped := *good
	stripped.LogKey = nil
	if err := stripped.Verify(terms); err == nil {
		t.Fatal("a join with no log key verified")
	}
}

// A table bond has to name the whole table, or it is a bond nobody can take.
//
// This is the check that cannot be skipped: a bond naming the wrong roster holds
// real coin, confirms on the chain, and looks identical from the outside to one
// that could actually be claimed. Every peer derives its neighbours' bonds from
// the roster it already agreed rather than believing what they announce, and
// finds its own branch in each.
func TestEverySeatsBondNamesTheWholeTable(t *testing.T) {
	terms := testTerms(3)
	privs := players(t, 3)
	fs := settleAll(t, terms, privs)
	for i, f := range fs {
		if err := f.SetBeacon(testBeacon()); err != nil {
			t.Fatalf("peer %d beacon: %v", i, err)
		}
	}

	var want []TableBond
	for i, f := range fs {
		bonds, err := f.TableBonds(testParams())
		if err != nil {
			t.Fatalf("peer %d: %v", i, err)
		}
		if len(bonds) != 3 {
			t.Fatalf("peer %d derived %d bonds for 3 seats", i, len(bonds))
		}
		if want == nil {
			want = bonds
			continue
		}
		for j := range bonds {
			if bonds[j] != want[j] {
				t.Fatalf("peer %d and peer 0 derived different bonds for seat %d",
					i, bonds[j].Seat)
			}
		}
	}

	seats, _ := fs[0].Seats()
	for _, b := range want {
		script, err := hex.DecodeString(b.ScriptHex)
		if err != nil {
			t.Fatalf("seat %d bond script: %v", b.Seat, err)
		}
		parsed, err := escrow.ParseTableBond(script)
		if err != nil {
			t.Fatalf("seat %d bond: %v", b.Seat, err)
		}
		if !bytes.Equal(parsed.Owner, seats[b.Seat]) {
			t.Fatalf("seat %d's bond is owned by another key", b.Seat)
		}
		if len(parsed.Others) != 2 {
			t.Fatalf("seat %d's bond can be taken by %d seats, want 2", b.Seat, len(parsed.Others))
		}
		// Every seat is a member, so each of them can help take it.
		for seat, key := range seats {
			if _, err := escrow.MemberIndex(parsed, key); err != nil {
				t.Fatalf("seat %d is not a member of seat %d's bond: %v", seat, b.Seat, err)
			}
		}
		// And the owner is not among the seats that can claim its own bond.
		for _, other := range parsed.Others {
			if bytes.Equal(other, seats[b.Seat]) {
				t.Fatalf("seat %d can help claim its own bond", b.Seat)
			}
		}
	}

	// A key not at the table is in nobody's bond, so a bond derived from the
	// wrong roster is caught rather than paid into.
	strangers := players(t, 1)
	script, err := hex.DecodeString(want[0].ScriptHex)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	parsed, err := escrow.ParseTableBond(script)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := escrow.MemberIndex(parsed, strangers[0].PubKey().SerializeCompressed()); err == nil {
		t.Fatal("a key not at the table is a member of its bonds")
	}
}

// A bond derived before the seating exists would name a membership that could
// still change, which is a bond paid into for a table that never happened.
func TestBondsWaitForTheSeating(t *testing.T) {
	terms := testTerms(2)
	privs := players(t, 2)
	fs := settleAll(t, terms, privs)
	if _, err := fs[0].TableBonds(testParams()); err == nil {
		t.Fatal("bonds were derived before the seating was drawn")
	}
}
