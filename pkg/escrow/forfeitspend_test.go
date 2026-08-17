package escrow

import (
	"bytes"
	"testing"

	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/crypto/blake256"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/wire"
	"github.com/karamble/dcrgaming-sdk/pkg/forfeit"
)

const forfeitSpendMatch = "5a8e2c17f9b04d63a1e85c29d7f04b361c8e5a2d9f7b0e43612d8f5a9c0e7b3d"

// forfeitDraft is a spendable draft over the given bond, paying a pinned
// stand-in destination. Output scripts are not executed by input validation,
// so the literal only has to be distinctive.
func forfeitDraft(bond []byte) ForfeitDraft {
	var prev chainhash.Hash
	copy(prev[:], bytes.Repeat([]byte{0x22}, chainhash.HashSize))
	return ForfeitDraft{
		Bond:       bond,
		Prevout:    wire.OutPoint{Hash: prev, Index: 1, Tree: wire.TxTreeRegular},
		ValueAtoms: 1_000_000,
		PayScript:  append([]byte{0x76, 0xa9, 0x14}, append(bytes.Repeat([]byte{0x33}, 20), 0x88, 0xac)...),
		FeeAtoms:   10_000,
	}
}

// The pilot's whole pipeline in one test: the owner equivocates at a log
// position, the wronged player recovers the log key from the two signatures,
// assembles the branch secret, and the trio turns it into a spend the real
// consensus engine accepts - with no timelock waited out.
func TestARecoveredKeyTakesAForfeitedBond(t *testing.T) {
	privs, pubs := memberKeys(t, 3)
	ownerPub := pubs[0]
	logPriv, punisher := privs[1], privs[2]

	br := forfeit.Branch{Match: forfeitSpendMatch, Seat: pubs[2]}
	fPub, err := forfeit.ForfeitKey(br, logPriv.PubKey(), punisher.PubKey())
	if err != nil {
		t.Fatalf("branch key: %v", err)
	}
	bond, err := ForfeitableBondScript(ownerPub, [][]byte{fPub.SerializeCompressed()}, testBondLock)
	if err != nil {
		t.Fatalf("build bond: %v", err)
	}

	// The owner tells two stories at one position and hands over its key.
	pos := forfeit.Position{Match: forfeitSpendMatch, Domain: forfeit.DomainEntry, Seq: 7}
	hashA := blake256.Sum256([]byte("the fleet is at anchor"))
	hashB := blake256.Sum256([]byte("the fleet has sailed"))
	sigA, err := forfeit.Sign(logPriv, pos, hashA[:])
	if err != nil {
		t.Fatalf("first signature: %v", err)
	}
	sigB, err := forfeit.Sign(logPriv, pos, hashB[:])
	if err != nil {
		t.Fatalf("second signature: %v", err)
	}
	recovered, err := forfeit.Recover(logPriv.PubKey(), hashA[:], sigA, hashB[:], sigB)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	spendKey, err := forfeit.ForfeitPrivKey(br, recovered, punisher)
	if err != nil {
		t.Fatalf("branch secret: %v", err)
	}

	d := forfeitDraft(bond)
	tx, err := BuildForfeit(d)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(tx.TxIn) != 1 || len(tx.TxOut) != 1 {
		t.Fatalf("built %d inputs and %d outputs, want 1 and 1", len(tx.TxIn), len(tx.TxOut))
	}
	if tx.TxIn[0].Sequence != 0 {
		t.Fatalf("the punishment branch carries a sequence of %d, and it has no timelock", tx.TxIn[0].Sequence)
	}
	if tx.TxOut[0].Value != 990_000 {
		t.Fatalf("the spend pays %d, want %d", tx.TxOut[0].Value, 990_000)
	}
	if !bytes.Equal(tx.TxOut[0].PkScript, d.PayScript) {
		t.Fatal("the spend pays somewhere other than the pinned destination")
	}

	sig, err := SignForfeitableSpend(tx, bond, spendKey)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	terms, err := ParseForfeitableBond(bond)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	index, err := ForfeitIndex(terms, fPub.SerializeCompressed())
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	final, err := FinishForfeit(tx, bond, sig, index, chaincfg.TestNet3Params())
	if err != nil {
		t.Fatalf("a recovered key could not take the bond it was owed: %v", err)
	}
	if len(final.TxIn[0].SignatureScript) == 0 {
		t.Fatal("the finished spend carries no witness")
	}
	if len(tx.TxIn[0].SignatureScript) != 0 {
		t.Fatal("finishing wrote through to the caller's transaction")
	}
}

// Neither the owner nor a punisher holding only its own half can finish a
// forfeit spend, which is the bond holding in both directions.
func TestHalfAKeyDoesNotFinishAForfeitSpend(t *testing.T) {
	s := postBond(t, 2)
	tx, err := BuildForfeit(forfeitDraft(s.script))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, who := range []struct {
		name string
		key  *secp256k1.PrivateKey
	}{
		{"the owner", s.owner},
		{"the leaked log key alone", s.log},
		{"a punisher's own half alone", s.punish[0]},
	} {
		sig, err := SignForfeitableSpend(tx, s.script, who.key)
		if err != nil {
			t.Fatalf("%s: sign: %v", who.name, err)
		}
		if _, err := FinishForfeit(tx, s.script, sig, 0, chaincfg.TestNet3Params()); err == nil {
			t.Fatalf("%s finished a forfeit spend", who.name)
		}
	}
}

// The builder has to refuse what it cannot make safe, and the first refusal
// is the one the type exists for: a table bond is not a forfeitable bond.
func TestBuildForfeitRefusesWhatItCannotMakeSafe(t *testing.T) {
	s := postBond(t, 1)
	_, pubs := memberKeys(t, 2)
	tableBond, err := TableBondScript(pubs[0], pubs, testBondLock)
	if err != nil {
		t.Fatalf("table bond: %v", err)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*ForfeitDraft)
	}{
		{"a table bond", func(d *ForfeitDraft) { d.Bond = tableBond }},
		{"no bond at all", func(d *ForfeitDraft) { d.Bond = nil }},
		{"nowhere to pay", func(d *ForfeitDraft) { d.PayScript = nil }},
		{"an empty bond output", func(d *ForfeitDraft) { d.ValueAtoms = 0 }},
		{"a negative fee", func(d *ForfeitDraft) { d.FeeAtoms = -1 }},
		{"a fee that leaves dust", func(d *ForfeitDraft) { d.FeeAtoms = d.ValueAtoms - MinShareAtoms + 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := forfeitDraft(s.script)
			tc.mutate(&d)
			if _, err := BuildForfeit(d); err == nil {
				t.Fatal("built a forfeit spend that should have been refused")
			}
		})
	}
}

// The two signers gate on opposite parsers, and both gates must hold: the gap
// this file closes existed because SignBondSpend refuses forfeitable bonds.
func TestTheForfeitSignerGatesOnTheBondKind(t *testing.T) {
	s := postBond(t, 1)
	_, pubs := memberKeys(t, 2)
	tableBond, err := TableBondScript(pubs[0], pubs, testBondLock)
	if err != nil {
		t.Fatalf("table bond: %v", err)
	}
	tx, err := BuildForfeit(forfeitDraft(s.script))
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	if _, err := SignForfeitableSpend(tx, tableBond, s.owner); err == nil {
		t.Fatal("the forfeit signer accepted a table bond")
	}
	if _, err := SignBondSpend(tx, s.script, s.owner); err == nil {
		t.Fatal("the table-bond signer accepted a forfeitable bond, so this file is closing a gap that no longer exists")
	}
	if _, err := SignForfeitableSpend(nil, s.script, s.owner); err == nil {
		t.Fatal("signed a transaction that is not there")
	}
	if _, err := SignForfeitableSpend(tx, s.script, nil); err == nil {
		t.Fatal("signed with a key that is not there")
	}
}

// What the finisher must refuse: a stray output smuggled past the builder, a
// branch the bond does not have, and a signature of the wrong shape.
func TestFinishForfeitRefusesAMalformedSpend(t *testing.T) {
	s := postBond(t, 1)
	tx, err := BuildForfeit(forfeitDraft(s.script))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	sig, err := SignForfeitableSpend(tx, s.script, s.spend[0])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	extra := tx.Copy()
	extra.AddTxOut(wire.NewTxOut(1, []byte{0x51}))
	if _, err := FinishForfeit(extra, s.script, sig, 0, chaincfg.TestNet3Params()); err == nil {
		t.Fatal("finished a forfeit spend with a second output")
	}
	if _, err := FinishForfeit(nil, s.script, sig, 0, chaincfg.TestNet3Params()); err == nil {
		t.Fatal("finished a transaction that is not there")
	}
	if _, err := FinishForfeit(tx, s.script, sig, 1, chaincfg.TestNet3Params()); err == nil {
		t.Fatal("finished through a branch the bond does not have")
	}
	if _, err := FinishForfeit(tx, s.script, sig[:SigLen-1], 0, chaincfg.TestNet3Params()); err == nil {
		t.Fatal("finished with a truncated signature")
	}
}
