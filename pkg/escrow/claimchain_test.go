package escrow

import (
	"bytes"
	"testing"

	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/wire"
)

const takeFee = 2_000

func accuseDraftFor(t *testing.T, b *tableBond, value int64) AccuseDraft {
	t.Helper()
	return AccuseDraft{
		Bond:       b.script,
		Prevout:    wire.OutPoint{Hash: [32]byte{9}, Index: 0, Tree: wire.TxTreeRegular},
		ValueAtoms: value,
		FeeAtoms:   takeFee,
		Params:     chaincfg.TestNet3Params(),
	}
}

// An accusation pays into the claimed bond and nowhere else, and the check is the
// only thing that says so - no script can require it.
func TestAnAccusationPaysIntoTheClaimedBond(t *testing.T) {
	b := postTableBond(t, 3)
	d := accuseDraftFor(t, b, 100_000)

	tx, err := BuildAccuse(d)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := CheckAccuseDraft(tx, d); err != nil {
		t.Fatalf("an accusation this draft built was refused: %v", err)
	}

	claimed, err := d.ClaimedScript()
	if err != nil {
		t.Fatalf("claimed script: %v", err)
	}
	_, want, err := Address(claimed, d.Params)
	if err != nil {
		t.Fatalf("address: %v", err)
	}
	if !bytes.Equal(tx.TxOut[0].PkScript, want) {
		t.Fatal("an accusation paid somewhere other than into the claimed bond")
	}
}

// The check is where a covenant would be, so it has to catch an accusation that
// pays its author instead.
func TestAnAccusationPayingTheAccusersIsRefused(t *testing.T) {
	b := postTableBond(t, 3)
	d := accuseDraftFor(t, b, 100_000)

	tx, err := BuildAccuse(d)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// The shape of a claim under the old rules: straight to somebody.
	_, theirs, err := Address(b.script, d.Params)
	if err != nil {
		t.Fatalf("address: %v", err)
	}
	tx.TxOut[0].PkScript = theirs
	if err := CheckAccuseDraft(tx, d); err == nil {
		t.Fatal("an accusation paying somewhere else was accepted, and nothing else would have stopped it")
	}
}

// The whole dispute, through the consensus engine: accuse, then answer, and the
// answer leaves the bond where it started so the next accusation can spend it.
func TestADisputeComposesAndTheBondSurvivesIt(t *testing.T) {
	b := postTableBond(t, 3)
	value := int64(100_000)
	d := accuseDraftFor(t, b, value)

	accuse, err := BuildAccuse(d)
	if err != nil {
		t.Fatalf("build accusation: %v", err)
	}
	claimed, err := d.ClaimedScript()
	if err != nil {
		t.Fatalf("claimed script: %v", err)
	}

	// The accusation spends the bond's all-members branch. Every member signs
	// it in advance, the owner included - that is the trade.
	accuse.TxIn[0].SignatureScript = mustAliveSig(t, b, accuse)
	if err := execute(t, b.script, accuse, csvFlags); err != nil {
		t.Fatalf("a pre-agreed accusation could not spend the bond: %v", err)
	}

	// The answer: the owner alone, no wait.
	answer, err := BuildAnswer(AnswerDraft{
		Claimed:    claimed,
		Bond:       b.script,
		Prevout:    wire.OutPoint{Hash: accuse.TxHash(), Index: 0, Tree: wire.TxTreeRegular},
		ValueAtoms: accuse.TxOut[0].Value,
		FeeAtoms:   takeFee,
		Params:     d.Params,
	})
	if err != nil {
		t.Fatalf("build answer: %v", err)
	}
	ownerSig := signInput(t, privFor(t, b.privs, b.terms.Owner), claimed, answer)
	sig, err := AnswerSigScript(claimed, ownerSig)
	if err != nil {
		t.Fatalf("answer sigscript: %v", err)
	}
	answer.TxIn[0].SignatureScript = sig
	if err := execute(t, claimed, answer, csvFlags); err != nil {
		t.Fatalf("the owner could not answer: %v", err)
	}

	// And it landed back in a bond identical to the one it came from, which is
	// what keeps the seat bonded and lets the chain continue.
	_, back, err := Address(b.script, d.Params)
	if err != nil {
		t.Fatalf("address: %v", err)
	}
	if !bytes.Equal(answer.TxOut[0].PkScript, back) {
		t.Fatal("answering did not leave the bond where it started")
	}
}

// The forfeiture, once nobody answered.
func TestTheAccusersTakeWhatWasNeverAnswered(t *testing.T) {
	b := postTableBond(t, 3)
	d := accuseDraftFor(t, b, 100_000)
	claimed, err := d.ClaimedScript()
	if err != nil {
		t.Fatalf("claimed script: %v", err)
	}
	terms, err := ParseClaimedBond(claimed)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	pays := make([][]byte, 0, len(terms.Others))
	for range terms.Others {
		_, p, err := Address(b.script, d.Params)
		if err != nil {
			t.Fatalf("address: %v", err)
		}
		pays = append(pays, p)
	}
	take, err := BuildTake(TakeDraft{
		Claimed:    claimed,
		Prevout:    wire.OutPoint{Hash: [32]byte{7}, Index: 0, Tree: wire.TxTreeRegular},
		ValueAtoms: 90_000,
		PayScripts: pays,
		FeeAtoms:   takeFee,
	})
	if err != nil {
		t.Fatalf("build take: %v", err)
	}
	if got := take.TxIn[0].Sequence; got != terms.ClaimBlocks {
		t.Fatalf("a take carries a sequence of %d, and the branch needs %d", got, terms.ClaimBlocks)
	}

	sigs := make([][]byte, 0, len(terms.Others))
	for _, k := range terms.Others {
		sigs = append(sigs, signInput(t, privFor(t, b.privs, k), claimed, take))
	}
	sig, err := TakeSigScript(claimed, sigs)
	if err != nil {
		t.Fatalf("take sigscript: %v", err)
	}
	take.TxIn[0].SignatureScript = sig
	if err := execute(t, claimed, take, csvFlags); err != nil {
		t.Fatalf("the accusers could not take an unanswered bond: %v", err)
	}
}

// Every accusation the chain may need, agreed at once - each one spending the
// bond that answering the previous one produces.
func TestTheAccusationChainSpendsWhatAnsweringLeaves(t *testing.T) {
	b := postTableBond(t, 3)
	d := accuseDraftFor(t, b, 500_000)

	chain, err := BuildAccuseChain(d, 4)
	if err != nil {
		t.Fatalf("build chain: %v", err)
	}
	if len(chain) != 4 {
		t.Fatalf("chain is %d long, want 4", len(chain))
	}
	if chain[0].TxIn[0].PreviousOutPoint != d.Prevout {
		t.Fatal("the first accusation does not spend the bond as it stands")
	}
	claimed, err := d.ClaimedScript()
	if err != nil {
		t.Fatalf("claimed script: %v", err)
	}

	for i := 1; i < len(chain); i++ {
		// Where the previous accusation's answer would leave the bond.
		answer, err := BuildAnswer(AnswerDraft{
			Claimed:    claimed,
			Bond:       b.script,
			Prevout:    wire.OutPoint{Hash: chain[i-1].TxHash(), Index: 0, Tree: wire.TxTreeRegular},
			ValueAtoms: chain[i-1].TxOut[0].Value,
			FeeAtoms:   takeFee,
			Params:     d.Params,
		})
		if err != nil {
			t.Fatalf("answer %d: %v", i-1, err)
		}
		want := wire.OutPoint{Hash: answer.TxHash(), Index: 0, Tree: wire.TxTreeRegular}
		if chain[i].TxIn[0].PreviousOutPoint != want {
			t.Fatalf("accusation %d does not spend what answering %d leaves", i, i-1)
		}
		if chain[i].TxIn[0].ValueIn != answer.TxOut[0].Value {
			t.Fatalf("accusation %d states a different value than answering %d leaves", i, i-1)
		}
	}
}

// mustAliveSig signs an accusation with every member, which is what the bond's
// all-members branch needs.
func mustAliveSig(t *testing.T, b *tableBond, tx *wire.MsgTx) []byte {
	t.Helper()
	sig, err := AliveSigScript(b.script, b.sign(t, tx, b.terms.Members))
	if err != nil {
		t.Fatalf("alive sigscript: %v", err)
	}
	return sig
}

// A transaction's identity does not depend on its signatures, which is the
// property the whole pre-agreed chain rests on: if signing changed the txid,
// nothing spending an accusation's output could be agreed before it was signed.
func TestATransactionsIdentityDoesNotDependOnItsSignatures(t *testing.T) {
	b := postTableBond(t, 3)
	d := accuseDraftFor(t, b, 100_000)
	tx, err := BuildAccuse(d)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	before := tx.TxHash()

	tx.TxIn[0].SignatureScript = mustAliveSig(t, b, tx)
	if got := tx.TxHash(); got != before {
		t.Fatalf("signing changed the transaction id from %s to %s", before, got)
	}
}

// The bond that has been ground down as far as it goes can still be answered,
// and cannot be accused again.
//
// Every round of a disagreement costs the accused bond two fees, one for the
// accusation and one for the answer, so a long wedge walks the bond down its
// ladder. AffordableDepth bounds how many rounds are agreed, and it bounds them
// on the accusation side: what it reserves is enough for a forfeiture to pay
// every taking seat its minimum. The question that decides whether this
// exchange can cost somebody a bond rather than only fees is the other side of
// that boundary - at the last rung the ladder reaches, is the owner still able
// to answer?
//
// The order of the two halves is the point. Answerable first: an owner who
// cannot build an answer at the last rung loses the bond to a window it could
// not act inside. Unaccusable second: one rung further on there is nothing left
// to pay a taker, so no accusation is agreed and the grinding simply stops.
func TestTheLastAffordableRungIsAnswerableAndThenUnaccusable(t *testing.T) {
	// The live shape: two seats, so one taker, and the plugin's own fee.
	const fee int64 = 10_000
	b := postTableBond(t, 2)
	terms, err := ParseTableBond(b.script)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(terms.Others) != 1 {
		t.Fatalf("%d takers at a two seat table, want 1", len(terms.Others))
	}

	start := int64(MinBondAtoms)
	depth := AffordableDepth(start, fee, len(terms.Others))
	if depth < 1 {
		t.Fatalf("a bond of %d funds no accusation at all", start)
	}

	d := accuseDraftFor(t, b, start)
	d.FeeAtoms = fee
	chain, err := BuildAccuseChain(d, depth)
	if err != nil {
		t.Fatalf("build the whole ladder: %v", err)
	}
	if len(chain) != depth {
		t.Fatalf("the ladder is %d long, want the %d that were affordable", len(chain), depth)
	}
	claimed, err := d.ClaimedScript()
	if err != nil {
		t.Fatalf("claimed script: %v", err)
	}

	// The last rung: the owner answers it, with what that accusation left.
	last := chain[len(chain)-1]
	answer, err := BuildAnswer(AnswerDraft{
		Claimed:    claimed,
		Bond:       b.script,
		Prevout:    wire.OutPoint{Hash: last.TxHash(), Index: 0, Tree: wire.TxTreeRegular},
		ValueAtoms: last.TxOut[0].Value,
		FeeAtoms:   fee,
		Params:     d.Params,
	})
	if err != nil {
		t.Fatalf("the owner cannot answer the last accusation the ladder agreed, "+
			"so grinding a bond down takes it: %v", err)
	}

	// And the accusers could have taken it instead, each paid its minimum -
	// which is what AffordableDepth reserved and what makes the last rung a
	// real accusation rather than one nobody could collect on.
	_, pay, err := Address(b.script, d.Params)
	if err != nil {
		t.Fatalf("address: %v", err)
	}
	take, err := BuildTake(TakeDraft{
		Claimed:    claimed,
		Prevout:    wire.OutPoint{Hash: last.TxHash(), Index: 0, Tree: wire.TxTreeRegular},
		ValueAtoms: last.TxOut[0].Value,
		PayScripts: [][]byte{pay},
		FeeAtoms:   fee,
	})
	if err != nil {
		t.Fatalf("the last accusation could never be collected on: %v", err)
	}
	if got := take.TxOut[0].Value; got < int64(MinShareAtoms) {
		t.Fatalf("taking the last rung pays %d, under the %d minimum", got, MinShareAtoms)
	}

	// And the grinding stops one rung further on, for one of two reasons
	// depending on which bound the bond is under. Saying which matters: the
	// two look the same from the bond's side and are not the same fact.
	next := answer.TxOut[0].Value
	if depth == AccuseDepth {
		// Bounded by policy. There is value left and it is not spent, because
		// the ladder is only ever derived from where the bond was posted and
		// is this long - the rung past it carries no accusation at all, which
		// is what stops it. A bond big enough to reach here has stopped
		// depending on its own size for how long a dispute can run.
		if got := AffordableDepth(next, fee, len(terms.Others)); got == 0 {
			t.Fatal("the cap was reached and the arithmetic had run out anyway, " +
				"so this is not testing the capped regime")
		}
		return
	}
	// Bounded by arithmetic: nothing further is affordable, so the ladder ends
	// exactly where the bond does.
	if got := AffordableDepth(next, fee, len(terms.Others)); got != 0 {
		t.Fatalf("a bond of %d past the last rung still funds %d accusations", next, got)
	}
}
