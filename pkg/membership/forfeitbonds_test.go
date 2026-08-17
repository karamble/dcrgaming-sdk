package membership

import (
	"encoding/hex"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
	"github.com/karamble/dcrgaming-sdk/pkg/forfeit"
)

const (
	forfeitMatch = "b5e1c7a92d4f80361e8a5c2d9f7b4e03612d8f5a9c0e7b3d41f6a2c8e5d9b071"
	forfeitLock  = uint32(2016)
)

// forfeitRoster draws a fresh n-seat roster: session, log and punishment key
// per seat, in the three maps ForfeitableBonds takes.
func forfeitRoster(t *testing.T, n int) (seats, logs, punish map[uint32][]byte) {
	t.Helper()
	seats = make(map[uint32][]byte, n)
	logs = make(map[uint32][]byte, n)
	punish = make(map[uint32][]byte, n)
	for i := range n {
		for _, m := range []map[uint32][]byte{seats, logs, punish} {
			priv, err := secp256k1.GeneratePrivateKey()
			if err != nil {
				t.Fatalf("generate key: %v", err)
			}
			m[uint32(i)] = priv.PubKey().SerializeCompressed()
		}
	}
	return seats, logs, punish
}

// The golden roster: every scalar an independent literal, so the branch
// derivation cannot drift without failing here, byte for byte.
func goldenForfeitRoster(t *testing.T) (seats, logs, punish map[uint32][]byte) {
	t.Helper()
	seats = map[uint32][]byte{
		0: privFromHex(t, "0101010101010101010101010101010101010101010101010101010101010101").PubKey().SerializeCompressed(),
		1: privFromHex(t, "0202020202020202020202020202020202020202020202020202020202020202").PubKey().SerializeCompressed(),
	}
	logs = map[uint32][]byte{
		0: privFromHex(t, "0303030303030303030303030303030303030303030303030303030303030303").PubKey().SerializeCompressed(),
		1: privFromHex(t, "0404040404040404040404040404040404040404040404040404040404040404").PubKey().SerializeCompressed(),
	}
	punish = map[uint32][]byte{
		0: privFromHex(t, "0505050505050505050505050505050505050505050505050505050505050505").PubKey().SerializeCompressed(),
		1: privFromHex(t, "0606060606060606060606060606060606060606060606060606060606060606").PubKey().SerializeCompressed(),
	}
	return seats, logs, punish
}

// The heads-up fixture is pinned whole. These two scripts are what both peers
// must derive from this roster forever; a derivation that moves one byte sends
// a bond to an address the other peer will refuse.
func TestTheForfeitableBondFixtureIsPinned(t *testing.T) {
	const (
		goldenBondSeat0 = "6321036ed45e3bdec230c3d84e33280cbfd3ab7a0940ccb84911157a795609b02a940652bf6702e007b27521031b84c5567b126440995d3ed5aaba0565d71e1834604819ff9c17f5e9d5dd078f52bf6851"
		goldenBondSeat1 = "632102d15c1f542f8fe9535c5d7ff2a9e4c4a60207b61e4f9fa69608195a552d7725ee52bf6702e007b27521024d4b6cd1361032ca9bd2aeb9d900aa4d45d9ead80ac9423374c451a7254d076652bf6851"
	)

	seats, logs, punish := goldenForfeitRoster(t)
	bonds, err := ForfeitableBonds(forfeitMatch, seats, logs, punish, forfeitLock, testParams())
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if len(bonds) != 2 {
		t.Fatalf("derived %d bonds, want 2", len(bonds))
	}
	if bonds[0].Seat != 0 || bonds[1].Seat != 1 {
		t.Fatalf("bonds came back for seats %d and %d, want 0 and 1", bonds[0].Seat, bonds[1].Seat)
	}
	if bonds[0].ScriptHex != goldenBondSeat0 {
		t.Fatalf("seat 0's bond is\n%s\nwant the pinned\n%s", bonds[0].ScriptHex, goldenBondSeat0)
	}
	if bonds[1].ScriptHex != goldenBondSeat1 {
		t.Fatalf("seat 1's bond is\n%s\nwant the pinned\n%s", bonds[1].ScriptHex, goldenBondSeat1)
	}
}

// The rebuild has to be exact at every table size the roster allows: the whole
// verification model is a peer recomputing its neighbour's script and comparing
// bytes, so any nondeterminism is an honest bond refused.
func TestForfeitableBondsRebuildByteForByte(t *testing.T) {
	for n := 2; n <= escrow.MaxMembers; n++ {
		seats, logs, punish := forfeitRoster(t, n)
		first, err := ForfeitableBonds(forfeitMatch, seats, logs, punish, forfeitLock, testParams())
		if err != nil {
			t.Fatalf("%d seats: %v", n, err)
		}

		// The same roster through maps built in reverse insertion order.
		rs := make(map[uint32][]byte, n)
		rl := make(map[uint32][]byte, n)
		rp := make(map[uint32][]byte, n)
		for i := n - 1; i >= 0; i-- {
			rs[uint32(i)] = seats[uint32(i)]
			rl[uint32(i)] = logs[uint32(i)]
			rp[uint32(i)] = punish[uint32(i)]
		}
		again, err := ForfeitableBonds(forfeitMatch, rs, rl, rp, forfeitLock, testParams())
		if err != nil {
			t.Fatalf("%d seats, rebuilt: %v", n, err)
		}
		if len(again) != len(first) {
			t.Fatalf("%d seats: %d bonds, then %d", n, len(first), len(again))
		}
		for i := range first {
			if first[i] != again[i] {
				t.Fatalf("%d seats: seat %d's bond differs between two derivations", n, first[i].Seat)
			}
		}
	}
}

// Every bond must read back under the forfeitable parser with exactly the
// terms it was derived from - and never under the table-bond parser, because
// telling the two kinds apart is what the separate type exists for.
func TestForfeitableBondsParseBack(t *testing.T) {
	for _, n := range []int{2, escrow.MaxMembers} {
		seats, logs, punish := forfeitRoster(t, n)
		bonds, err := ForfeitableBonds(forfeitMatch, seats, logs, punish, forfeitLock, testParams())
		if err != nil {
			t.Fatalf("%d seats: %v", n, err)
		}
		if len(bonds) != n {
			t.Fatalf("%d seats derived %d bonds", n, len(bonds))
		}
		for _, b := range bonds {
			script, err := hex.DecodeString(b.ScriptHex)
			if err != nil {
				t.Fatalf("seat %d: %v", b.Seat, err)
			}
			terms, err := escrow.ParseForfeitableBond(script)
			if err != nil {
				t.Fatalf("seat %d's bond does not parse back: %v", b.Seat, err)
			}
			if string(terms.Owner) != string(seats[b.Seat]) {
				t.Fatalf("seat %d's bond parsed to the wrong owner", b.Seat)
			}
			if terms.LockBlocks != forfeitLock {
				t.Fatalf("seat %d's bond carries a lock of %d, want %d", b.Seat, terms.LockBlocks, forfeitLock)
			}
			if len(terms.Forfeit) != n-1 {
				t.Fatalf("seat %d's bond has %d branches, want %d", b.Seat, len(terms.Forfeit), n-1)
			}
			// Each opponent finds its own branch under the key it rebuilds.
			for opp, member := range seats {
				if opp == b.Seat {
					continue
				}
				lp, err := secp256k1.ParsePubKey(logs[b.Seat])
				if err != nil {
					t.Fatalf("log key: %v", err)
				}
				pp, err := secp256k1.ParsePubKey(punish[opp])
				if err != nil {
					t.Fatalf("punishment key: %v", err)
				}
				fkey, err := forfeit.ForfeitKey(forfeit.Branch{Match: forfeitMatch, Seat: member}, lp, pp)
				if err != nil {
					t.Fatalf("branch key: %v", err)
				}
				idx, err := escrow.ForfeitIndex(terms, fkey.SerializeCompressed())
				if err != nil {
					t.Fatalf("seat %d's bond has no branch for seat %d: %v", b.Seat, opp, err)
				}
				if n == 2 && idx != 0 {
					t.Fatalf("a heads-up bond found its one branch at %d", idx)
				}
			}
			if _, err := escrow.ParseTableBond(script); err == nil {
				t.Fatalf("seat %d's forfeitable bond parsed as a table bond", b.Seat)
			}
		}
	}
}

// A seat that announces an opponent's log key, or its negation, gets no branch
// in that opponent's bond and the bond builds from the remaining seats - but
// heads-up there are no remaining seats, and the build must refuse.
func TestADegenerateAnnouncementLosesItsBranch(t *testing.T) {
	seats, logs, punish := forfeitRoster(t, 3)

	// Seat 2 announces seat 0's log key as its punishment key.
	punish[2] = append([]byte(nil), logs[0]...)
	bonds, err := ForfeitableBonds(forfeitMatch, seats, logs, punish, forfeitLock, testParams())
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	counts := map[uint32]int{}
	for _, b := range bonds {
		script, err := hex.DecodeString(b.ScriptHex)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		terms, err := escrow.ParseForfeitableBond(script)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		counts[b.Seat] = len(terms.Forfeit)
	}
	if counts[0] != 1 || counts[1] != 2 || counts[2] != 2 {
		t.Fatalf("branch counts %v, want seat 0 down to 1 and the others at 2", counts)
	}

	// The negation is the same announcement wearing the other parity byte.
	neg := append([]byte(nil), logs[0]...)
	neg[0] ^= 0x01
	punish[2] = neg
	bonds, err = ForfeitableBonds(forfeitMatch, seats, logs, punish, forfeitLock, testParams())
	if err != nil {
		t.Fatalf("derive with the negated key: %v", err)
	}
	script, err := hex.DecodeString(bonds[0].ScriptHex)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	terms, err := escrow.ParseForfeitableBond(script)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(terms.Forfeit) != 1 {
		t.Fatalf("seat 0's bond kept %d branches against a negated announcement, want 1", len(terms.Forfeit))
	}

	// Heads-up the same announcement leaves nothing to build from.
	seats2, logs2, punish2 := forfeitRoster(t, 2)
	punish2[1] = append([]byte(nil), logs2[0]...)
	if _, err := ForfeitableBonds(forfeitMatch, seats2, logs2, punish2, forfeitLock, testParams()); err == nil {
		t.Fatal("built a heads-up bond with no branches at all")
	}
}

func TestForfeitableBondsRefuseBadRosters(t *testing.T) {
	seats, logs, punish := forfeitRoster(t, 2)

	short := map[uint32][]byte{0: seats[0]}
	missingLog := map[uint32][]byte{0: logs[0]}
	missingPunish := map[uint32][]byte{0: punish[0]}
	extraLog := map[uint32][]byte{0: logs[0], 1: logs[1], 2: logs[0]}
	badKey := map[uint32][]byte{0: logs[0], 1: {0x02, 0x03}}
	big, bigLogs, bigPunish := forfeitRoster(t, escrow.MaxMembers+1)

	for _, tc := range []struct {
		name                string
		match               string
		seats, logs, punish map[uint32][]byte
		lock                uint32
	}{
		{"one seat", forfeitMatch, short, logs, punish, forfeitLock},
		{"a missing log key", forfeitMatch, seats, missingLog, punish, forfeitLock},
		{"a missing punishment key", forfeitMatch, seats, logs, missingPunish, forfeitLock},
		{"a log roster with an extra entry", forfeitMatch, seats, extraLog, punish, forfeitLock},
		{"a malformed log key", forfeitMatch, seats, badKey, punish, forfeitLock},
		{"a malformed punishment key", forfeitMatch, seats, logs, badKey, forfeitLock},
		{"no match", "", seats, logs, punish, forfeitLock},
		{"a lock under the minimum", forfeitMatch, seats, logs, punish, escrow.MinBondBlocks - 1},
		{"too many seats", forfeitMatch, big, bigLogs, bigPunish, forfeitLock},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ForfeitableBonds(tc.match, tc.seats, tc.logs, tc.punish, tc.lock, testParams()); err == nil {
				t.Fatal("derived bonds from a roster that should have been refused")
			}
		})
	}
}
