package schema

import (
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/karamble/dcrgaming-sdk/pkg/escrow"
	"github.com/karamble/dcrgaming-sdk/pkg/membership"
)

// testCreds gives a key a bond, so it can join. Whether the deposit is really
// on chain is membership.CheckBond's question, not this package's.
func testCreds(t *testing.T, priv *secp256k1.PrivateKey) membership.Credentials {
	t.Helper()
	bond, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate bond key: %v", err)
	}
	script, err := escrow.BondScript(bond.PubKey().SerializeCompressed(), escrow.MinBondBlocks)
	if err != nil {
		t.Fatalf("bond script: %v", err)
	}
	logKey, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate log key: %v", err)
	}
	return membership.Credentials{
		Session: priv, Log: logKey, Bond: bond, BondOutpoint: "beef:0", BondScript: script,
	}
}

func testTerms() membership.Terms {
	return membership.Terms{
		Game:       "poker",
		GameVer:    5,
		SID:        "0123456789abcdef",
		BuyInAtoms: 10_000_000,
		Seats:      2,
		CSVBlocks:  64,
		Until:      900000,
	}
}

// A join has to survive the wire unchanged, because what proves it genuine is a
// signature over the bytes it names. A conversion that dropped or reshaped a
// field would leave every relayed join unverifiable.
func TestAJoinSurvivesTheWireAndStillVerifies(t *testing.T) {
	terms := testTerms()
	priv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	original, err := membership.SignJoin(terms, testCreds(t, priv))
	if err != nil {
		t.Fatalf("sign join: %v", err)
	}

	blob, err := Encode(5, KindJoin, "match1", JoinFrom(original))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	msg, err := Decode(5, blob)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if msg.Kind != KindJoin {
		t.Fatalf("kind is %q, want %q", msg.Kind, KindJoin)
	}

	var body Join
	if err := msg.Into(&body); err != nil {
		t.Fatalf("into: %v", err)
	}
	back, err := body.Into()
	if err != nil {
		t.Fatalf("read join back: %v", err)
	}
	if err := back.Verify(terms); err != nil {
		t.Fatalf("a join that went over the wire no longer verifies: %v", err)
	}
}

func TestACommitSurvivesTheWireAndStillVerifies(t *testing.T) {
	terms := testTerms()
	priv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	var roster [32]byte
	roster[0], roster[31] = 0xab, 0xcd
	original, err := membership.SignCommit(terms, roster, priv)
	if err != nil {
		t.Fatalf("sign commit: %v", err)
	}

	blob, err := Encode(5, KindCommit, "match1", CommitFrom(original))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	msg, err := Decode(5, blob)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var body Commit
	if err := msg.Into(&body); err != nil {
		t.Fatalf("into: %v", err)
	}
	back, err := body.Into()
	if err != nil {
		t.Fatalf("read commit back: %v", err)
	}
	if back.Roster != roster {
		t.Fatal("the roster hash changed crossing the wire")
	}
	if err := back.Verify(terms); err != nil {
		t.Fatalf("a commit that went over the wire no longer verifies: %v", err)
	}
}

// The point of carrying joins in a roster assertion is that a peer who missed
// one can check its way to the same membership. That only works if they arrive
// verifiable.
func TestARosterCarriesJoinsThatStillVerify(t *testing.T) {
	terms := testTerms()
	joins := make([]*membership.Join, 0, 2)
	for range 2 {
		priv, err := secp256k1.GeneratePrivateKey()
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}
		j, err := membership.SignJoin(terms, testCreds(t, priv))
		if err != nil {
			t.Fatalf("sign join: %v", err)
		}
		joins = append(joins, j)
	}

	seats := map[uint32][]byte{0: joins[0].Key, 1: joins[1].Key}
	blob, err := Encode(5, KindRoster, "match1", RosterFrom(terms, seats, joins, nil))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	msg, err := Decode(5, blob)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var body Roster
	if err := msg.Into(&body); err != nil {
		t.Fatalf("into: %v", err)
	}

	if len(body.Joins) != len(joins) {
		t.Fatalf("carried %d joins, want %d", len(body.Joins), len(joins))
	}
	for i, wire := range body.Joins {
		back, err := wire.Into()
		if err != nil {
			t.Fatalf("join %d: %v", i, err)
		}
		if err := back.Verify(terms); err != nil {
			t.Fatalf("relayed join %d does not verify: %v", i, err)
		}
	}

	gotSeats, err := body.SeatKeys()
	if err != nil {
		t.Fatalf("seat keys: %v", err)
	}
	if len(gotSeats) != len(seats) {
		t.Fatalf("carried %d seats, want %d", len(gotSeats), len(seats))
	}

	if body.Terms == nil {
		t.Fatal("a roster assertion should say what terms it was computed under")
	}
	if body.Terms.Into() != terms {
		t.Fatal("the terms changed crossing the wire")
	}
}
