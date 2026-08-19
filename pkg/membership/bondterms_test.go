package membership

import (
	"encoding/hex"
	"testing"

	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
)

// legacyTerms is a table of the shape dcrpoker has been forming for years: no
// bond terms, because they did not exist.
func legacyTerms() Terms {
	return Terms{
		Game: "poker", GameVer: 5, SID: "abc123",
		BuyInAtoms: 5_000_000, Seats: 2, CSVBlocks: 288, Until: 900,
	}
}

// The load-bearing test of this change. dcrpoker's live tables carry joins and
// commits signed over this digest; if adding bond terms moved it, every one of
// those signatures would stop verifying.
//
// The literal was captured from the implementation before bond terms existed
// and is pinned here on purpose - it must never be regenerated from the code it
// is checking.
func TestATableWithoutBondTermsHashesExactlyAsItAlwaysDid(t *testing.T) {
	const before = "0c11359ed2485221522d4a6bc9cf2dc00883ea0ac90d8e79d4bb38cdfef41176"

	h, err := legacyTerms().Hash()
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if got := hex.EncodeToString(h[:]); got != before {
		t.Fatalf("the digest of a table with no bond terms moved.\n got %s\nwant %s\n"+
			"Every join and commit signed at a live table binds to this.", got, before)
	}
}

func TestStatingBondTermsChangesTheDigest(t *testing.T) {
	plain, err := legacyTerms().Hash()
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	bonded := legacyTerms()
	bonded.BondAtoms = escrow.MinBondAtoms
	bonded.BondLockBlocks = 4032
	bonded.AccuseFeeAtoms = 10_000
	got, err := bonded.Hash()
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if got == plain {
		t.Fatal("a table with bond terms hashes the same as one without, so nobody signs them")
	}
}

// Each term has to change the digest on its own, or two different tables agree
// on a hash and one seat builds a bond the other never agreed to.
func TestEveryBondTermIsInTheDigest(t *testing.T) {
	base := legacyTerms()
	base.BondAtoms = escrow.MinBondAtoms
	base.BondLockBlocks = 4032
	base.AccuseFeeAtoms = 10_000
	h0, err := base.Hash()
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	for _, tc := range []struct {
		name string
		mut  func(*Terms)
	}{
		{"the amount", func(tm *Terms) { tm.BondAtoms = escrow.MinBondAtoms * 2 }},
		{"the lock", func(tm *Terms) { tm.BondLockBlocks = 4033 }},
		{"the accusation fee", func(tm *Terms) { tm.AccuseFeeAtoms = 20_000 }},
	} {
		other := base
		tc.mut(&other)
		h1, err := other.Hash()
		if err != nil {
			t.Fatalf("%s: hash: %v", tc.name, err)
		}
		if h1 == h0 {
			t.Errorf("changing %s did not change the digest", tc.name)
		}
	}
}

// Half a bond term is refused, but by the floors rather than by a rule of its
// own: the missing half is zero and zero is under both floors. There is
// deliberately no separate pair check, because it would be unreachable.
func TestBondTermsComeInPairsAndClearTheEscrowFloors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		atoms  uint64
		blocks uint32
		fee    uint64
		ok     bool
	}{
		{"neither, the legacy table", 0, 0, 0, true},
		{"battleships, above both floors", escrow.MinBondAtoms, 4032, 10_000, true},
		{"exactly on both floors", escrow.MinBondAtoms, escrow.MinBondBlocks, 10_000, true},
		{"an amount with no lock", escrow.MinBondAtoms, 0, 10_000, false},
		{"a lock with no amount", 0, 4032, 10_000, false},
		{"under the amount floor", escrow.MinBondAtoms - 1, 4032, 10_000, false},
		{"under the lock floor", escrow.MinBondAtoms, escrow.MinBondBlocks - 1, 10_000, false},
		{"bonds but no accusation fee", escrow.MinBondAtoms, 4032, 0, false},
	} {
		tm := legacyTerms()
		tm.BondAtoms, tm.BondLockBlocks, tm.AccuseFeeAtoms = tc.atoms, tc.blocks, tc.fee
		err := tm.Validate()
		if tc.ok && err != nil {
			t.Errorf("%s: refused good terms: %v", tc.name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: accepted terms a seat could not build a bond from", tc.name)
		}
	}
}

// Both games' real bond terms have to be expressible, which is the whole point
// of moving them into the shared vocabulary.
func TestBothGamesBondTermsAreExpressible(t *testing.T) {
	for _, tc := range []struct {
		game   string
		atoms  uint64
		blocks uint32
		fee    uint64
	}{
		// dcrpoker states none and uses the escrow floors directly.
		{"poker", 0, 0, 0},
		// battleships: TableBondAtoms is the escrow floor, lock 4032,
		// AccuseFeeAtoms 10,000.
		{"battleships", escrow.MinBondAtoms, 4032, 10_000},
	} {
		tm := legacyTerms()
		tm.Game = tc.game
		tm.BondAtoms, tm.BondLockBlocks, tm.AccuseFeeAtoms = tc.atoms, tc.blocks, tc.fee
		if err := tm.Validate(); err != nil {
			t.Errorf("%s: %v", tc.game, err)
		}
		if _, err := tm.Hash(); err != nil {
			t.Errorf("%s: hash: %v", tc.game, err)
		}
	}
}
