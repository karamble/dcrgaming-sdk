package escrow

import (
	"bytes"
	"testing"

	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/txscript/v4"
	"github.com/decred/dcrd/wire"
)

func testPrevout() wire.OutPoint {
	var h chainhash.Hash
	copy(h[:], bytes.Repeat([]byte{0x11}, chainhash.HashSize))
	return wire.OutPoint{Hash: h, Index: 0, Tree: wire.TxTreeRegular}
}

func testPayScript(t *testing.T) []byte {
	t.Helper()
	s, err := txscript.NewScriptBuilder().AddOp(txscript.OP_TRUE).Script()
	if err != nil {
		t.Fatalf("pay script: %v", err)
	}
	return s
}

// A depositor reclaiming their own stake, through the branch that names only
// them. The builder verifies against the real engine before returning, so
// getting a transaction back at all is the assertion.
func TestARefundSpendsItsOwnBranch(t *testing.T) {
	privs, pubs := memberKeys(t, 3)
	owner := pubs[0]
	redeem, err := RedeemScript(owner, pubs, testCSVBlocks)
	if err != nil {
		t.Fatalf("redeem script: %v", err)
	}

	tx, err := BuildTimelockedSpend(Spend{
		Key:        privFor(t, privs, owner),
		Script:     redeem,
		Prevout:    testPrevout(),
		ValueAtoms: testInAtoms,
		CSVBlocks:  testCSVBlocks,
		PayScript:  testPayScript(t),
		FeeAtoms:   testInAtoms - testOutAtoms,
		SigScript:  RefundSigScript,
		Params:     chaincfg.TestNet3Params(),
	})
	if err != nil {
		t.Fatalf("build refund: %v", err)
	}
	if got := tx.TxIn[0].Sequence; got != testCSVBlocks {
		t.Fatalf("sequence is %d, want the lock %d - the branch cannot be satisfied otherwise", got, testCSVBlocks)
	}
	if got := tx.TxIn[0].ValueIn; got != testInAtoms {
		t.Fatalf("ValueIn is %d, want %d - the signature commits to it", got, testInAtoms)
	}
	if got := tx.TxOut[0].Value; got != testOutAtoms {
		t.Fatalf("pays %d, want %d", got, testOutAtoms)
	}
	if tx.Version < 2 {
		t.Fatalf("version %d, and OP_CHECKSEQUENCEVERIFY refuses anything under 2", tx.Version)
	}
}

// The refund branch names one member. Another member of the same table holds a
// key the settlement branch accepts and this one must not.
func TestARefundRefusesAnotherMembersKey(t *testing.T) {
	privs, pubs := memberKeys(t, 3)
	redeem, err := RedeemScript(pubs[0], pubs, testCSVBlocks)
	if err != nil {
		t.Fatalf("redeem script: %v", err)
	}

	if _, err := BuildTimelockedSpend(Spend{
		Key:        privFor(t, privs, pubs[1]),
		Script:     redeem,
		Prevout:    testPrevout(),
		ValueAtoms: testInAtoms,
		CSVBlocks:  testCSVBlocks,
		PayScript:  testPayScript(t),
		FeeAtoms:   testInAtoms - testOutAtoms,
		SigScript:  RefundSigScript,
		Params:     chaincfg.TestNet3Params(),
	}); err == nil {
		t.Fatal("built a refund of somebody else's stake")
	}
}

// The check before returning has to run with the sequence rule enabled.
//
// Without it OP_CHECKSEQUENCEVERIFY degrades to a no-op and the verification
// proves only that the signature is well formed - so a builder that checks
// without the rule hands back a transaction the network then refuses. Build one
// whose sequence is under the script's lock and require it to be caught here
// rather than at broadcast.
func TestASpendShortOfTheLockIsCaughtBeforeBroadcast(t *testing.T) {
	privs, pubs := memberKeys(t, 2)
	owner := pubs[0]
	redeem, err := RedeemScript(owner, pubs, testCSVBlocks)
	if err != nil {
		t.Fatalf("redeem script: %v", err)
	}

	if _, err := BuildTimelockedSpend(Spend{
		Key:        privFor(t, privs, owner),
		Script:     redeem,
		Prevout:    testPrevout(),
		ValueAtoms: testInAtoms,
		CSVBlocks:  testCSVBlocks - 1, // a block short of the lock
		PayScript:  testPayScript(t),
		FeeAtoms:   testInAtoms - testOutAtoms,
		SigScript:  RefundSigScript,
		Params:     chaincfg.TestNet3Params(),
	}); err == nil {
		t.Fatal("returned a spend whose sequence does not satisfy the lock")
	}
}

// A bond reclaimed by its owner. Same transaction, different witness: the bond
// script has no branch, so nothing selects one.
func TestABondSweepsToItsOwner(t *testing.T) {
	privs, pubs := memberKeys(t, 2)
	bond, err := BondScript(pubs[0], MinBondBlocks)
	if err != nil {
		t.Fatalf("bond script: %v", err)
	}

	tx, err := BuildTimelockedSpend(Spend{
		Key:        privFor(t, privs, pubs[0]),
		Script:     bond,
		Prevout:    testPrevout(),
		ValueAtoms: testInAtoms,
		CSVBlocks:  MinBondBlocks,
		PayScript:  testPayScript(t),
		FeeAtoms:   testInAtoms - testOutAtoms,
		SigScript:  BondSigScript,
		Params:     chaincfg.TestNet3Params(),
	})
	if err != nil {
		t.Fatalf("build sweep: %v", err)
	}
	if got := tx.TxIn[0].Sequence; got != MinBondBlocks {
		t.Fatalf("sequence is %d, want %d", got, MinBondBlocks)
	}

	// Nobody else, ever - the whole point of the instrument.
	if _, err := BuildTimelockedSpend(Spend{
		Key:        privFor(t, privs, pubs[1]),
		Script:     bond,
		Prevout:    testPrevout(),
		ValueAtoms: testInAtoms,
		CSVBlocks:  MinBondBlocks,
		PayScript:  testPayScript(t),
		FeeAtoms:   testInAtoms - testOutAtoms,
		SigScript:  BondSigScript,
		Params:     chaincfg.TestNet3Params(),
	}); err == nil {
		t.Fatal("swept a bond belonging to somebody else")
	}
}

// A fee that leaves nothing is a mistake worth refusing rather than broadcasting.
func TestASpendRefusesToPayEverythingToTheMiner(t *testing.T) {
	privs, pubs := memberKeys(t, 2)
	redeem, err := RedeemScript(pubs[0], pubs, testCSVBlocks)
	if err != nil {
		t.Fatalf("redeem script: %v", err)
	}

	for _, fee := range []int64{testInAtoms, testInAtoms + 1} {
		if _, err := BuildTimelockedSpend(Spend{
			Key:        privFor(t, privs, pubs[0]),
			Script:     redeem,
			Prevout:    testPrevout(),
			ValueAtoms: testInAtoms,
			CSVBlocks:  testCSVBlocks,
			PayScript:  testPayScript(t),
			FeeAtoms:   fee,
			SigScript:  RefundSigScript,
			Params:     chaincfg.TestNet3Params(),
		}); err == nil {
			t.Fatalf("built a spend paying a fee of %d out of %d", fee, testInAtoms)
		}
	}
}
