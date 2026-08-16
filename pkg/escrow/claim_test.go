package escrow

import (
	"bytes"
	"testing"

	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/txscript/v4"
)

// Building a spend more than one person signs has a failure mode that a single
// signer does not: two peers can build almost the same transaction, and the
// signatures then do not fit. That looks like a disagreement about the facts and
// is really a rounding difference, so most of what follows is about byte
// equality rather than about poker.

const (
	testBondAtoms = int64(10_000_000) // 0.1 DCR
	testBondFee   = int64(10_000)
)

// payTo builds a plausible payout script for each of n seats.
func payTo(t *testing.T, n int) [][]byte {
	t.Helper()
	out := make([][]byte, 0, n)
	for i := range n {
		s, err := txscript.NewScriptBuilder().
			AddOp(txscript.OP_DUP).
			AddOp(txscript.OP_HASH160).
			AddData(bytes.Repeat([]byte{byte(i + 1)}, 20)).
			AddOp(txscript.OP_EQUALVERIFY).
			AddOp(txscript.OP_CHECKSIG).
			Script()
		if err != nil {
			t.Fatalf("pay script: %v", err)
		}
		out = append(out, s)
	}
	return out
}

func TestTheSplitIsExactAndDeterministic(t *testing.T) {
	for _, tc := range []struct {
		total int64
		n     int
		want  []int64
	}{
		{300_000, 3, []int64{100_000, 100_000, 100_000}},
		{300_001, 3, []int64{100_001, 100_000, 100_000}},
		{300_002, 3, []int64{100_001, 100_001, 100_000}},
		{100_000, 1, []int64{100_000}},
	} {
		got, err := Shares(tc.total, tc.n)
		if err != nil {
			t.Fatalf("%d/%d: %v", tc.total, tc.n, err)
		}
		var sum int64
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%d/%d: share %d is %d, want %d", tc.total, tc.n, i, got[i], tc.want[i])
			}
			sum += got[i]
		}
		if sum != tc.total {
			t.Fatalf("%d/%d: shares total %d, so %d atoms went missing",
				tc.total, tc.n, sum, tc.total-sum)
		}
	}

	// A bond too small to divide is refused rather than split into dust that
	// would make the whole transaction unrelayable.
	if _, err := Shares(1000, 6); err == nil {
		t.Fatal("a bond was split into dust")
	}
	if _, err := Shares(0, 2); err == nil {
		t.Fatal("nothing was divided between two seats")
	}
}

func TestTheBackstopBuildsThroughTheOrdinarySpendBuilder(t *testing.T) {
	b := postTableBond(t, 3)
	pay := payTo(t, 1)[0]

	tx, err := BuildTimelockedSpend(Spend{
		Key:        b.privs[0],
		Script:     b.script,
		Prevout:    testPrevout(),
		ValueAtoms: testBondAtoms,
		CSVBlocks:  b.terms.LockBlocks,
		PayScript:  pay,
		FeeAtoms:   testBondFee,
		SigScript:  func(script, sig []byte) ([]byte, error) { return BackstopSigScript(script, sig) },
		Params:     chaincfg.TestNet3Params(),
	})
	if err != nil {
		t.Fatalf("the owner could not reclaim a stranded bond: %v", err)
	}
	if tx.TxOut[0].Value != testBondAtoms-testBondFee {
		t.Fatalf("backstop paid %d, want %d", tx.TxOut[0].Value, testBondAtoms-testBondFee)
	}

	// And not a block early.
	if _, err := BuildTimelockedSpend(Spend{
		Key: b.privs[0], Script: b.script, Prevout: testPrevout(),
		ValueAtoms: testBondAtoms, CSVBlocks: b.terms.LockBlocks - 1,
		PayScript: pay, FeeAtoms: testBondFee,
		SigScript: func(script, sig []byte) ([]byte, error) { return BackstopSigScript(script, sig) },
		Params:    chaincfg.TestNet3Params(),
	}); err == nil {
		t.Fatal("the backstop was spent a block before it matured")
	}
}
