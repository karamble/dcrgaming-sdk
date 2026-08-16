package escrow

import (
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

const testOutpoint = "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0:1"

func bondFor(t *testing.T, priv *secp256k1.PrivateKey) []byte {
	t.Helper()
	bond, err := BondScript(priv.PubKey().SerializeCompressed(), MinBondBlocks)
	if err != nil {
		t.Fatalf("bond script: %v", err)
	}
	return bond
}

func TestAProofVerifiesAgainstTheBondsOwnerKey(t *testing.T) {
	owner, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	bond := bondFor(t, owner)
	holder := []byte("player-one")

	sig, err := SignBondPoP(testOutpoint, holder, owner)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := VerifyBondPoP(bond, testOutpoint, holder, sig); err != nil {
		t.Fatalf("a genuine proof did not verify: %v", err)
	}
}

// The point of the whole exercise: pointing at a bond proves nothing, because a
// bond is public. Only the key it pays out to can claim it.
func TestSomebodyElsesBondCannotBeClaimed(t *testing.T) {
	owner, _ := secp256k1.GeneratePrivateKey()
	thief, _ := secp256k1.GeneratePrivateKey()
	bond := bondFor(t, owner)
	holder := []byte("the thief")

	sig, err := SignBondPoP(testOutpoint, holder, thief)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := VerifyBondPoP(bond, testOutpoint, holder, sig); err == nil {
		t.Fatal("a bond was claimed by a key that does not own it")
	}
}

// A proof travels in public inside a join, so one that named only the outpoint
// would be a token anybody could lift. Naming the holder makes a stolen proof a
// proof about somebody else.
func TestAProofCannotBeLiftedByAnotherHolder(t *testing.T) {
	owner, _ := secp256k1.GeneratePrivateKey()
	bond := bondFor(t, owner)

	sig, err := SignBondPoP(testOutpoint, []byte("the real player"), owner)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := VerifyBondPoP(bond, testOutpoint, []byte("somebody else"), sig); err == nil {
		t.Fatal("a proof made for one holder was accepted for another")
	}
}

// And it is about one deposit. Reusing it for a different outpoint would let
// one bond back a claim on coin it has nothing to do with.
func TestAProofCannotBeReusedForAnotherOutpoint(t *testing.T) {
	owner, _ := secp256k1.GeneratePrivateKey()
	bond := bondFor(t, owner)
	holder := []byte("player-one")

	sig, err := SignBondPoP(testOutpoint, holder, owner)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	other := "1111111111111111111111111111111111111111111111111111111111111111:0"
	if err := VerifyBondPoP(bond, other, holder, sig); err == nil {
		t.Fatal("a proof about one deposit was accepted for another")
	}
}

func TestAProofIsRefusedWhenItNamesNothing(t *testing.T) {
	owner, _ := secp256k1.GeneratePrivateKey()
	if _, err := BondPoPDigest("", []byte("holder")); err == nil {
		t.Fatal("a proof with no outpoint was allowed")
	}
	if _, err := BondPoPDigest(testOutpoint, nil); err == nil {
		t.Fatal("a proof with no holder was allowed")
	}
	if _, err := SignBondPoP(testOutpoint, []byte("holder"), nil); err == nil {
		t.Fatal("a proof was signed with no key")
	}

	bond := bondFor(t, owner)
	if err := VerifyBondPoP(bond, testOutpoint, []byte("holder"), []byte{1, 2, 3}); err == nil {
		t.Fatal("a truncated proof was accepted")
	}
	if err := VerifyBondPoP([]byte{0x51}, testOutpoint, []byte("holder"), make([]byte, 64)); err == nil {
		t.Fatal("a proof against something that is not a bond was accepted")
	}
}
