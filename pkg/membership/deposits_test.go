package membership

import (
	"testing"

	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
)

const testOutpoint = "1def0123456789abcdef0123456789abcdef0123456789abcdef0123456789ab:1"

// A funding announcement establishes who said where their stake is - and
// nothing else. Whether that outpoint is real, big enough, or pays the right
// script are the chain's answers.
func TestAFundingAnnouncementNamesItsSigner(t *testing.T) {
	terms := testTerms(2)
	privs := players(t, 2)

	f, err := SignFunding(terms, 0, testOutpoint, privs[0])
	if err != nil {
		t.Fatalf("sign funding: %v", err)
	}
	if err := f.Verify(terms); err != nil {
		t.Fatalf("a freshly signed announcement did not verify: %v", err)
	}

	// Somebody else's signature over the same words.
	forged, err := SignFunding(terms, 0, testOutpoint, privs[1])
	if err != nil {
		t.Fatalf("sign funding: %v", err)
	}
	forged.Signer = f.Signer
	if err := forged.Verify(terms); err == nil {
		t.Fatal("an announcement verified against a key that did not sign it")
	}
}

// The seat and the outpoint are both signed, so neither can be edited in
// flight: pointing a member's stake at a different output, or moving it to
// another seat, invalidates the signature.
func TestAFundingAnnouncementCannotBeEdited(t *testing.T) {
	terms := testTerms(2)
	priv := players(t, 2)[0]

	signed, err := SignFunding(terms, 1, testOutpoint, priv)
	if err != nil {
		t.Fatalf("sign funding: %v", err)
	}

	moved := *signed
	moved.Seat = 0
	if err := moved.Verify(terms); err == nil {
		t.Fatal("an announcement verified after being moved to another seat")
	}

	repointed := *signed
	repointed.Outpoint = "0000000000000000000000000000000000000000000000000000000000000000:0"
	if err := repointed.Verify(terms); err == nil {
		t.Fatal("an announcement verified after being pointed at another output")
	}

	// And it belongs to one table: the terms are in the digest, so the same
	// words signed for one session say nothing about another.
	other := testTerms(2)
	other.SID = "beef02"
	if err := signed.Verify(other); err == nil {
		t.Fatal("an announcement for one table verified at another")
	}
}

// Terms carrying a refund timelock nothing could ever satisfy have to be
// refused when the invitation is read - not discovered after the stake is paid
// into a script with no way out.
func TestTermsRefuseATimelockNothingCouldSpend(t *testing.T) {
	terms := testTerms(2)
	terms.CSVBlocks = escrow.MaxCSVBlocks + 1
	if err := terms.Validate(); err == nil {
		t.Fatal("accepted a table whose refunds could never be spent")
	}

	// A join cannot be signed against them either, since signing hashes the
	// terms and hashing validates them.
	if _, err := terms.Hash(); err == nil {
		t.Fatal("hashed terms that describe an unspendable table")
	}

	terms.CSVBlocks = escrow.MaxCSVBlocks
	if err := terms.Validate(); err != nil {
		t.Fatalf("refused the largest lock that can actually be spent: %v", err)
	}
}

func TestAFundingAnnouncementNeedsAnOutpoint(t *testing.T) {
	terms := testTerms(2)
	priv := players(t, 2)[0]

	if _, err := SignFunding(terms, 0, "   ", priv); err == nil {
		t.Fatal("signed an announcement that names no output")
	}
}

// The whole point of deriving deposits locally: every peer has to arrive at
// byte-identical addresses. A one-byte disagreement is money paid to a script
// nobody can satisfy, and there is no referee here to be authoritative about
// which peer got it right.
func TestEveryPeerDerivesTheSameDeposits(t *testing.T) {
	terms := testTerms(3)
	privs := players(t, 3)
	fs := settleAll(t, terms, privs)
	for i, f := range fs {
		if err := f.SetBeacon(testBeacon()); err != nil {
			t.Fatalf("peer %d beacon: %v", i, err)
		}
	}

	want, err := fs[0].Deposits(testParams())
	if err != nil {
		t.Fatalf("deposits: %v", err)
	}
	if len(want) != 3 {
		t.Fatalf("derived %d deposits for a 3 seat table", len(want))
	}

	for i, f := range fs[1:] {
		got, err := f.Deposits(testParams())
		if err != nil {
			t.Fatalf("peer %d deposits: %v", i+1, err)
		}
		if len(got) != len(want) {
			t.Fatalf("peer %d derived %d deposits, peer 0 derived %d", i+1, len(got), len(want))
		}
		for j := range got {
			if got[j] != want[j] {
				t.Fatalf("peer %d derived a different deposit for seat %d:\n got %+v\nwant %+v",
					i+1, want[j].Seat, got[j], want[j])
			}
		}
	}

	// Every seat pays somewhere different, or one member could satisfy the
	// settlement branch with somebody else's stake.
	seen := make(map[string]bool, len(want))
	for _, d := range want {
		if seen[d.DepositAddr] {
			t.Fatalf("two seats share the deposit address %s", d.DepositAddr)
		}
		seen[d.DepositAddr] = true
	}

	// And each peer knows which one is its own to pay.
	held := make(map[uint32]bool, len(fs))
	for i, f := range fs {
		seat, ok := f.OurSeat()
		if !ok {
			t.Fatalf("peer %d holds no seat at a table it settled", i)
		}
		if held[seat] {
			t.Fatalf("two peers hold seat %d", seat)
		}
		held[seat] = true
	}
}

// A deposit belongs to a seat, and until the beacon is drawn there are no seats
// to attribute one to. Deriving anyway would mean paying an address whose
// script the table has not agreed.
func TestDepositsWaitForTheSeating(t *testing.T) {
	terms := testTerms(2)
	fs := settleAll(t, terms, players(t, 2))

	if _, err := fs[0].Deposits(testParams()); err == nil {
		t.Fatal("derived deposits for a table that has not been seated")
	}
	if _, ok := fs[0].OurSeat(); ok {
		t.Fatal("reported a seat before the seating was drawn")
	}
}

// The stake pays a script naming the whole table, so a different membership is
// a different address. That is what makes funding roster-first necessary.
func TestADifferentMembershipPaysElsewhere(t *testing.T) {
	terms := testTerms(2)

	first := settleAll(t, terms, players(t, 2))
	second := settleAll(t, terms, players(t, 2))
	for _, f := range append(append([]*Formation{}, first...), second...) {
		if err := f.SetBeacon(testBeacon()); err != nil {
			t.Fatalf("beacon: %v", err)
		}
	}

	a, err := first[0].Deposits(testParams())
	if err != nil {
		t.Fatalf("deposits: %v", err)
	}
	b, err := second[0].Deposits(testParams())
	if err != nil {
		t.Fatalf("deposits: %v", err)
	}
	for _, x := range a {
		for _, y := range b {
			if x.DepositAddr == y.DepositAddr {
				t.Fatalf("two different tables share the deposit address %s", x.DepositAddr)
			}
		}
	}
}
